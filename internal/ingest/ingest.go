// Package ingest 把「切分 → 嵌入 → 落库」串成一条流水线。
//
// 为什么单独一层：切分（chunk）、嵌入（embed）、存储（store）各自是零件，
// 把它们串起来是**业务编排**。这段逻辑如果内联进 MCP 的 handler，
// 业务逻辑就被塞进了传输层——传输层换协议（stdio 换 HTTP）时要重写一遍，
// 而且没法脱离 MCP 单独测试。
package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/XiaoleC05/ContextDock/internal/chunk"
	"github.com/XiaoleC05/ContextDock/internal/embed"
	"github.com/XiaoleC05/ContextDock/internal/store"
	"github.com/XiaoleC05/ContextDock/internal/types"
)

var (
	// ErrNilDocument 表示没传文档。
	ErrNilDocument = errors.New("ingest: 文档为空")

	// ErrChunkCountMismatch 表示切分产出的片段数与嵌入返回的向量数不一致。
	//
	// 这条不变量一旦不成立，向量就会和片段错位——第 N 个片段的向量是
	// 第 M 段的内容。检索结果会变得莫名其妙，而且**不报错**。
	ErrChunkCountMismatch = errors.New("ingest: 片段数与向量数不一致")
)

// Result 是一次导入的结果。
type Result struct {
	DocumentID  int64
	ChunkCount  int
	EmbeddedNum int // 成功生成了向量的片段数
}

// dedup 算好指纹与去重键；如果库里已有同一份，先把旧的删掉。
//
// # 替换语义：删掉重建，不是增量更新
//
// 旧文档连同它的全部片段一起删除（外键 ON DELETE CASCADE），
// 然后走正常的插入流程。**新文档会拿到一个新的 ID**。
//
// 为什么不做原地增量更新：片段是按切分参数切出来的，
// 而切分参数可能已经变了——保留旧片段的 ID 会让"新切出来的片段"
// 和"旧片段"混在同一篇文档里，编号也接不上。删掉重建是唯一能保证
// 「库里这份文档的片段与当前切分参数一致」的做法。
//
// 代价是 ID 会变。**调用方不该把 DocumentID 当作长期标识**——
// 要稳定就用 source 或内容指纹。
func (g *Ingester) dedup(ctx context.Context, doc *types.Document) error {
	doc.ContentHash = sha256Hex(doc.Content)
	doc.DedupKey = types.DedupKey(doc.Source, doc.ContentHash)

	old, err := g.store.DocumentByDedupKey(ctx, doc.DedupKey)
	if err != nil {
		if errors.Is(err, store.ErrDocumentNotFound) {
			return nil // 新文档，照常插入
		}
		// 查不动就**中止**，不能"当作没有"继续插入——
		// 那会在查库出故障时静默产生重复文档，而故障恢复后谁也不知道。
		return fmt.Errorf("ingest: 查重失败: %w", err)
	}

	if err := g.store.DeleteDocument(ctx, old.ID); err != nil {
		return fmt.Errorf("ingest: 替换旧文档失败: %w", err)
	}
	return nil
}

// sha256Hex 返回内容的 sha256（十六进制小写）。
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Ingester 编排一次完整的文档导入。
type Ingester struct {
	chunker  *chunk.Chunker
	embedder embed.Embedder
	store    store.Store
}

// New 创建一个导入器。三个参数都是接口/抽象，所以可以在没有网络、
// 没有数据库的情况下测全流程。
func New(chunker *chunk.Chunker, embedder embed.Embedder, st store.Store) *Ingester {
	return &Ingester{chunker: chunker, embedder: embedder, store: st}
}

// Ingest 导入一篇文档。
//
// 顺序：切分 → 落库拿到 ID → 生成向量 → 回填向量。
//
// ⚠️ 为什么不"先生成向量再一次落库"：
// 向量是网络调用，可能失败、可能限流。先生成再落库的话，一次 429
// 就让整篇文档白导；而先落库再回填，文档和片段已经在库里了，
// 重试只需要重算向量，而且**即使向量全失败，文档内容也没丢**。
func (g *Ingester) Ingest(ctx context.Context, doc *types.Document) (*Result, error) {
	if doc == nil {
		return nil, ErrNilDocument
	}
	if err := doc.Validate(); err != nil {
		return nil, err
	}

	// ---- 0. 算指纹与去重键，命中就替换 ----
	//
	// 顺序上必须**最先做**：先落库再发现重复的话，库里已经多出一份了，
	// 而且"删掉刚插的那份"和"删掉旧的那份"很难区分。
	if err := g.dedup(ctx, doc); err != nil {
		return nil, err
	}

	// ---- 1. 切分 ----
	// 切分器需要 DocumentID 来填充片段。新文档还没有 ID，
	// 这里先传一个占位值，落库时 store 会用真实的 ID 覆盖。
	// ⚠️ 用 1 而不是 0：Chunk.Validate() 会把 0 当作非法值。
	chunks, err := g.chunker.Split(placeholderDocID, doc.Content)
	if err != nil {
		return nil, fmt.Errorf("ingest: 切分失败: %w", err)
	}
	if len(chunks) == 0 {
		return nil, store.ErrNoChunks
	}

	// ---- 1.5. 把文档级来源下沉到每个片段 ----
	//
	// 检索返回的是片段，不是文档；SearchResult 内嵌的是 Chunk，
	// 而 Chunk 上并没有 Source 字段。不在这里存一份的话，
	// 检索路径就**永远无法**回答"这条内容出自哪儿"。
	//
	// 必须赶在 SaveDocument 之前写入——那一步就把 metadata 序列化落库了，
	// 之后再改内存里的副本不会影响库里已经写下的行。
	//
	// Source 为空时跳过，避免往 metadata 里塞一个空字符串。
	if doc.Source != "" {
		for i := range chunks {
			// 走 SetMetadata，它负责初始化 nil map。
			chunks[i].SetMetadata(types.MetadataKeySource, doc.Source)
		}
	}

	// ---- 2. 落库（拿到片段 ID）----
	saved, err := g.store.SaveDocument(ctx, doc, chunks)
	if err != nil {
		return nil, fmt.Errorf("ingest: 保存失败: %w", err)
	}

	res := &Result{DocumentID: doc.ID, ChunkCount: len(saved)}

	// ---- 3. 生成向量 ----
	// 送进去的文本必须走 IndexText() —— 它会把标题面包屑拼进正文。
	// 直接送 Content 的话，BM25 和向量搜的就不是同一个文本了。
	texts := make([]string, len(saved))
	for i, c := range saved {
		texts[i] = c.IndexText()
	}

	vecs, err := g.embedder.Embed(ctx, texts)
	if err != nil {
		// 向量失败不算导入失败：文档和片段已经落库了。
		// 调用方可以根据这个错误决定要不要重试向量部分。
		return res, fmt.Errorf("ingest: 文档已保存，但生成向量失败（可稍后重试）: %w", err)
	}
	if len(vecs) != len(saved) {
		return res, fmt.Errorf("%w: 片段 %d 个，向量 %d 个",
			ErrChunkCountMismatch, len(saved), len(vecs))
	}

	// ---- 4. 回填向量 ----
	for i := range saved {
		if len(vecs[i]) != types.EmbeddingDim {
			return res, fmt.Errorf("%w: 第 %d 个向量是 %d 维",
				store.ErrVectorDim, i, len(vecs[i]))
		}
		saved[i].Embedding = vecs[i]
	}
	if err := g.store.SaveEmbeddings(ctx, saved); err != nil {
		return res, fmt.Errorf("ingest: 文档已保存，但回填向量失败: %w", err)
	}

	res.EmbeddedNum = len(saved)
	return res, nil
}

// placeholderDocID 是切分阶段用的占位文档 ID。
//
// 用 1 而不是 0，因为 Chunk.Validate() 把 0 视作"没有所属文档"的非法值。
// 真正的 ID 在落库时由 store 覆盖。
const placeholderDocID = int64(1)
