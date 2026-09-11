// Package ingest 把「切分 → 嵌入 → 落库」串成一条流水线。
//
// 为什么单独一层：切分（chunk）、嵌入（embed）、存储（store）各自是零件，
// 把它们串起来是**业务编排**。这段逻辑如果内联进 MCP 的 handler，
// 业务逻辑就被塞进了传输层——传输层换协议（stdio 换 HTTP）时要重写一遍，
// 而且没法脱离 MCP 单独测试。
package ingest

import (
	"context"
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
