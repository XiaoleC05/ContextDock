// Package service 是应用的核心：把配置、嵌入、检索、存储、导入编排在一起，
// 对上层（MCP 工具、将来的 HTTP 接口）暴露少量语义明确的方法。
//
// 它承担两件容易被忽略但很关键的事：
//
//  1. **启动时从数据库重建内存索引**。BM25 是内存索引，进程重启后就没了。
//     不重建的话，重启后检索会静默地只剩向量一路——结果变差但不报错。
//
//  2. **逐级降级**。嵌入接口挂了不该让检索完全不可用，
//     关键词检索应该还能顶上。
package service

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"

	"github.com/XiaoleC05/ContextDock/internal/chunk"
	"github.com/XiaoleC05/ContextDock/internal/config"
	"github.com/XiaoleC05/ContextDock/internal/embed"
	"github.com/XiaoleC05/ContextDock/internal/ingest"
	"github.com/XiaoleC05/ContextDock/internal/retrieve"
	"github.com/XiaoleC05/ContextDock/internal/store"
	"github.com/XiaoleC05/ContextDock/internal/tokenize"
	"github.com/XiaoleC05/ContextDock/internal/types"
)

// Service 是应用门面。
type Service struct {
	cfg      *config.Config
	embedder embed.Embedder
	store    store.Store
	ingester *ingest.Ingester

	// tok 是分词器。索引端与查询端共用同一套规则，
	// 重建时要用它造新的 BM25 实例。
	tok *tokenize.Tokenizer

	// idxMu 保护下面几个字段——**只保护「读指针 / 换指针」，不保护检索过程**。
	//
	// # 不变量：已发布的索引对象此后永不改写
	//
	// BM25 和 VectorIndex 都不是并发安全的（Index 会重建内部状态），
	// 所以重建的做法是「锁外构造全新实例，最后只拿写锁换指针」，
	// **绝不对已发布的实例再调 Index()**。
	//
	// 这条不变量比「用锁把检索整个盖住」更强，而且它是必需的：
	// 检索会在锁外长时间运行，超时那次尤其——`hybrid.go` 的 ctx.Done
	// 分支会直接 return，**不排空另一个还在读索引的 goroutine**。
	// 如果重建是原地改写，那个孤儿 goroutine 就会读到写了一半的状态。
	//
	// ⚠️ 所以：**不要把锁加回检索过程**。历史上这里出过一次事故
	// （提前解锁导致真实数据竞争，CI 上被 -race 抓到），当时的修法是
	// 扩大锁范围；但那个修法只是把窗口缩小，并没有消除——因为
	// Service.Search 的 defer RUnlock 在孤儿 goroutine 结束**之前**
	// 就执行了。真正的解法是让对象不可变，不是让锁盖得更久。
	//
	// ⚠️ 反过来，任何对已发布实例的原地修改都会让这条不变量失效。
	// 增量更新 BM25（改 df / avgdl）就是这类改动，做之前先看这条注释。
	idxMu  sync.RWMutex
	bm25   *retrieve.BM25
	vecIdx *retrieve.VectorIndex

	// byDoc 按 DocumentID 分组的片段，组内按 Ordinal 升序。
	//
	// 上下文扩展（#49）要用它取相邻片段。放在内存索引里而不是每次查库：
	// 那是 O(1) 查找，而每次检索都打一次库会把延迟从亚毫秒推到毫秒级——
	// 而检索结果已经在内存里了，再查一次库是纯粹的浪费。
	byDoc map[int64][]types.Chunk

	// 检索到的统计信息，用于日志和诊断。
	indexedChunks int
	embeddedCount int
	textBytes     int64
}

// IndexStats 是内存索引的规模。
//
// 单独开一个方法而不是往 Stats() 上加返回值：Stats() 的二元组
// 已经在别处被用着，改签名要动所有调用点，而那些调用点并不关心体积。
type IndexStats struct {
	// Chunks 是索引里的片段总数。
	Chunks int

	// Embedded 是其中带向量的片段数。
	Embedded int

	// TextBytes 是片段正文的字节总量（不含向量）。
	TextBytes int64

	// VectorBytes 是向量占用的字节数，按 float32 × 维度估算。
	//
	// ⚠️ 这是**进程内**的内存占用，不是数据库里的存储占用。
	// 两者在 pgvector 那边不完全相等（有行开销和索引），
	// 但对「切得越碎、索引涨多快」这个量级判断已经够用。
	VectorBytes int64
}

// IndexStats 返回当前内存索引的规模。
func (s *Service) IndexStats() IndexStats {
	s.idxMu.RLock()
	defer s.idxMu.RUnlock()
	return IndexStats{
		Chunks:      s.indexedChunks,
		Embedded:    s.embeddedCount,
		TextBytes:   s.textBytes,
		VectorBytes: int64(s.embeddedCount) * types.EmbeddingDim * 4,
	}
}

// New 组装一个 Service。
func New(cfg *config.Config, emb embed.Embedder, st store.Store) (*Service, error) {
	chunker, err := chunk.New(chunk.Config{
		MaxRunes:     cfg.ChunkMaxRunes,
		OverlapRunes: cfg.ChunkOverlap,
	})
	if err != nil {
		return nil, fmt.Errorf("service: 创建切分器失败: %w", err)
	}

	// 分词方案来自配置，索引端与查询端共用这一个实例——
	// 两边用不同的分词器会让召回静默对不上（见 tokenize 包的说明）。
	// 单独取出来是因为 Rebuild 每次都要造新的 BM25 实例，
	// 而新旧实例必须共用**同一个**分词器。
	tok := tokenize.NewWith(cfg.TokenizeScheme)

	return &Service{
		cfg:      cfg,
		embedder: emb,
		store:    st,
		ingester: ingest.New(chunker, emb, st),
		tok:      tok,
		bm25:     retrieve.NewBM25().WithTokenizer(tok),
		vecIdx:   retrieve.NewVectorIndex(),
	}, nil
}

// Rebuild 从存储重建内存索引。
//
// 启动时必须调用一次。库里已有的文档不会自动出现在内存索引里。
//
// 单条片段没有向量时（比如上次导入在嵌入阶段失败了），
// 它仍然会进 BM25 索引——关键词检索不依赖向量。
//
// # 锁外构造，最后换指针
//
// 全程**不在写锁下碰已有对象**：先在锁外把新实例全部造好，成功之后
// 只拿写锁做几个字段赋值。依据见 Service.idxMu 上的不变量说明。
//
// 这同时修掉了一处既有缺陷：原来的顺序是「先换 BM25 → 再 Index 向量」，
// 后者失败时返回错误、而 rebuildFromStore 只打一行日志继续跑，
// 于是留下**一半新一半旧**的部分重建，且没有任何信号。
// 现在要么整体成功、要么一个字段都不动。
func (s *Service) Rebuild(ctx context.Context) error {
	chunks, err := s.store.AllChunks(ctx)
	if err != nil {
		return fmt.Errorf("service: 读取全部片段失败: %w", err)
	}

	// ---- 以下全部在锁外构造 ----

	bm25 := retrieve.NewBM25().WithTokenizer(s.tok)
	bm25.Index(chunks)

	// 只有带向量的片段才能进向量索引。
	withVec := make([]types.Chunk, 0, len(chunks))
	for _, c := range chunks {
		if len(c.Embedding) == types.EmbeddingDim {
			withVec = append(withVec, c)
		}
	}
	vecIdx := retrieve.NewVectorIndex()
	if err := vecIdx.Index(withVec); err != nil {
		return fmt.Errorf("service: 重建向量索引失败: %w", err)
	}

	// 正文体积在重建时顺手算掉：评测要用它判断「切得越碎、索引涨多快」，
	// 而为此再遍历一遍全部片段不值得。
	var textBytes int64
	for _, c := range chunks {
		textBytes += int64(len(c.Content))
	}

	// 顺手按文档分组，供上下文扩展取相邻片段。
	// 组内顺序由 AllChunks 保证（它按 (document_id, ordinal) 读出），
	// 但这里仍然不假设，下面用 Ordinal 排序兜底。
	byDoc := make(map[int64][]types.Chunk, 16)
	for _, c := range chunks {
		byDoc[c.DocumentID] = append(byDoc[c.DocumentID], c)
	}
	for id := range byDoc {
		doc := byDoc[id]
		sort.Slice(doc, func(i, j int) bool { return doc[i].Ordinal < doc[j].Ordinal })
	}

	// ---- 到这里才动共享状态，且只做赋值 ----
	//
	// ⚠️ 绝不能在这里加回任何对 s.bm25 / s.vecIdx 的原地修改，
	// 那会让「已发布对象永不改写」失效。见 Service.idxMu 的说明。
	s.idxMu.Lock()
	s.bm25 = bm25
	s.vecIdx = vecIdx
	s.byDoc = byDoc
	s.indexedChunks = len(chunks)
	s.embeddedCount = len(withVec)
	s.textBytes = textBytes
	s.idxMu.Unlock()

	if len(chunks) > 0 && len(withVec) < len(chunks) {
		// 这条日志很重要：说明有一批片段没有向量，
		// 它们只能被关键词检索命中，融合质量会下降。
		log.Printf("注意：%d/%d 个片段没有向量，这些片段只能被关键词检索命中",
			len(chunks)-len(withVec), len(chunks))
	}
	return nil
}

// snapshot 在锁内取出当前索引的引用，供调用方在**锁外**使用。
//
// 返回的对象依据 Rebuild 的不变量保证不会再被改写，所以调用方
// 拿它们跑多久都不会与重建冲突。
func (s *Service) snapshot() (*retrieve.BM25, *retrieve.VectorIndex) {
	s.idxMu.RLock()
	defer s.idxMu.RUnlock()
	return s.bm25, s.vecIdx
}

// Stats 返回索引状态，供日志和诊断使用。
func (s *Service) Stats() (chunks, embedded int) {
	s.idxMu.RLock()
	defer s.idxMu.RUnlock()
	return s.indexedChunks, s.embeddedCount
}

// Import 导入一篇文档，并把它加进内存索引。
func (s *Service) Import(ctx context.Context, doc *types.Document) (*ingest.Result, error) {
	res, err := s.ingester.Ingest(ctx, doc)
	if err != nil {
		// ingest 在"已落库但向量失败"时会同时返回结果和错误。
		// 那种情况下索引还是要更新——片段内容已经可用，
		// 关键词检索应该能命中它。
		if res == nil {
			return nil, err
		}
		s.rebuildFromStore(ctx)
		return res, err
	}

	s.rebuildFromStore(ctx)
	return res, nil
}

// Search 执行混合检索。
//
// 降级策略（按顺序尝试）：
//  1. 两路都可用 → RRF 融合
//  2. 嵌入接口失败 → 只用关键词检索（**不返回错误**）
//  3. 向量索引为空 → 只用关键词检索
//
// 之所以嵌入失败也不报错：用户的问题是"找到相关文档"，
// 关键词检索能部分满足它。把整个查询失败掉，用户得到的是"什么都查不到"，
// 那比"结果差一点"更糟。
func (s *Service) Search(ctx context.Context, query string, topK int) (*SearchOutput, error) {
	if topK <= 0 {
		topK = s.cfg.TopK
	}

	// ---- 取快照：只在锁内读指针，检索全程不持锁 ----
	//
	// 安全性来自 Rebuild 的「已发布对象永不改写」不变量，
	// **不是**来自「锁覆盖住了检索」。见 Service.idxMu 的说明。
	bm25, vecIdx := s.snapshot()

	out := &SearchOutput{Query: query, TopK: topK}

	bm25Empty := bm25.Len() == 0
	vecEmpty := vecIdx.Len() == 0

	// 两路都不可用时没有降级空间，直接返回空。
	if bm25Empty && vecEmpty {
		out.Degraded = "索引为空"
		return out, nil
	}

	// ---- 生成查询向量：**刻意不持锁** ----
	//
	// 这是网络调用，可能耗时几百毫秒。如果在这里持读锁，
	// 并发的 Rebuild（要拿写锁）就得等这么久，而 Rebuild 期间
	// 所有检索都会被卡住。
	var queryVec []float32
	if !vecEmpty {
		vecs, err := s.embedder.Embed(ctx, []string{query})
		if err != nil {
			// 不返回错误——降级成纯关键词检索。
			out.Degraded = fmt.Sprintf("嵌入失败，已降级为关键词检索: %v", err)
		} else if len(vecs) != 1 || len(vecs[0]) != types.EmbeddingDim {
			out.Degraded = "嵌入返回的维度不正确，已降级为关键词检索"
		} else {
			queryVec = vecs[0]
		}
	} else {
		out.Degraded = "没有可用的向量索引，已降级为关键词检索"
	}

	// ---- 检索：用快照，**不持锁** ----
	//
	// ⚠️ 这里刻意不再持读锁。
	//
	// 历史：这里曾经在检索过程中提前 RUnlock，导致并发的 Rebuild
	// 可以在检索读索引时重建索引——真实数据竞争，CI 上被 -race 抓到。
	// 当时的修法是「把锁扩大到覆盖整个检索」，但那只是缩小了窗口：
	// hybrid.Search 内部有两个 goroutine，超时分支会**直接 return 而
	// 不排空另一个**，于是 Service.Search 的 defer RUnlock 会在那个
	// goroutine 还在读索引时就执行。
	//
	// 现在真正的保证换成了「已发布对象永不改写」（见 Service.idxMu）：
	// 快照拿到的那份索引是完整的、不会再变的，检索跑多久都行。
	// 锁只需要保护「读指针」这一瞬间。
	if queryVec == nil {
		// 纯关键词路径
		out.Results = bm25.Search(query, topK)
		return out, nil
	}

	hybrid := retrieve.NewHybrid(
		retrieve.BM25Searcher{BM25: bm25},
		vecIdx,
	).WithTimeout(s.cfg.SearchTimeout).
		WithErrorHandler(func(r types.Retriever, err error) {
			log.Printf("%s 检索失败，已降级: %v", r, err)
		})

	results, err := hybrid.Search(ctx, query, queryVec, topK)
	if err != nil {
		return nil, fmt.Errorf("service: 混合检索失败: %w", err)
	}
	out.Results = results
	return out, nil
}

// SearchOutput 是一次检索的完整结果。
type SearchOutput struct {
	Query   string
	TopK    int
	Results []types.SearchResult

	// Degraded 非空时说明发生了降级，内容是原因。
	//
	// 之所以要显式报出来：降级是静默的——结果照样返回，只是质量下降。
	// 不告诉调用方的话，"检索变差了"就成了一个没有任何信号的谜。
	Degraded string
}

// Close 释放资源。
func (s *Service) Close() error {
	return s.store.Close()
}

// rebuildFromStore 在导入之后刷新内存索引。
//
// 做法是"全量重建"而不是增量插入：增量插入需要在 BM25 里
// 维护 df 和 avgdl 的增量更新，很容错且难测；而单机场景下
// 片段数量不大，全量重建（毫秒级）完全够用。
//
// ⚠️ 数据量大到重建变慢时，这里是要改的地方。
func (s *Service) rebuildFromStore(ctx context.Context) {
	if err := s.Rebuild(ctx); err != nil {
		log.Printf("刷新索引失败（检索结果可能不包含刚导入的内容）: %v", err)
	}
}
