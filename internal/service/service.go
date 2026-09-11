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
	"sync"

	"github.com/XiaoleC05/ContextDock/internal/chunk"
	"github.com/XiaoleC05/ContextDock/internal/config"
	"github.com/XiaoleC05/ContextDock/internal/embed"
	"github.com/XiaoleC05/ContextDock/internal/ingest"
	"github.com/XiaoleC05/ContextDock/internal/retrieve"
	"github.com/XiaoleC05/ContextDock/internal/store"
	"github.com/XiaoleC05/ContextDock/internal/types"
)

// Service 是应用门面。
type Service struct {
	cfg      *config.Config
	embedder embed.Embedder
	store    store.Store
	ingester *ingest.Ingester

	// idxMu 保护下面两个索引。
	//
	// BM25 和 VectorIndex 都**不是**并发安全的：Index 会重建内部状态。
	// 检索时读、重建时写，用 RWMutex 隔开——否则并发检索会读到
	// 重建到一半的索引，结果不可预期而且 `-race` 之外很难发现。
	idxMu  sync.RWMutex
	bm25   *retrieve.BM25
	vecIdx *retrieve.VectorIndex

	// 检索到的统计信息，用于日志和诊断。
	indexedChunks int
	embeddedCount int
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

	return &Service{
		cfg:      cfg,
		embedder: emb,
		store:    st,
		ingester: ingest.New(chunker, emb, st),
		bm25:     retrieve.NewBM25(),
		vecIdx:   retrieve.NewVectorIndex(),
	}, nil
}

// Rebuild 从存储重建内存索引。
//
// 启动时必须调用一次。库里已有的文档不会自动出现在内存索引里。
//
// 单条片段没有向量时（比如上次导入在嵌入阶段失败了），
// 它仍然会进 BM25 索引——关键词检索不依赖向量。
func (s *Service) Rebuild(ctx context.Context) error {
	chunks, err := s.store.AllChunks(ctx)
	if err != nil {
		return fmt.Errorf("service: 读取全部片段失败: %w", err)
	}

	s.idxMu.Lock()
	defer s.idxMu.Unlock()

	s.bm25.Index(chunks)

	// 只有带向量的片段才能进向量索引。
	withVec := make([]types.Chunk, 0, len(chunks))
	for _, c := range chunks {
		if len(c.Embedding) == types.EmbeddingDim {
			withVec = append(withVec, c)
		}
	}
	if err := s.vecIdx.Index(withVec); err != nil {
		return fmt.Errorf("service: 重建向量索引失败: %w", err)
	}

	s.indexedChunks = len(chunks)
	s.embeddedCount = len(withVec)

	if len(chunks) > 0 && len(withVec) < len(chunks) {
		// 这条日志很重要：说明有一批片段没有向量，
		// 它们只能被关键词检索命中，融合质量会下降。
		log.Printf("注意：%d/%d 个片段没有向量，这些片段只能被关键词检索命中",
			len(chunks)-len(withVec), len(chunks))
	}
	return nil
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

	// ---- 读一次索引规模 ----
	s.idxMu.RLock()
	bm25Empty := s.bm25.Len() == 0
	vecEmpty := s.vecIdx.Len() == 0
	s.idxMu.RUnlock()

	out := &SearchOutput{Query: query, TopK: topK}

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

	// ---- 检索：全程持读锁 ----
	//
	// ⚠️ 锁必须一直覆盖到 Search 返回。
	//
	// 这里曾经提前 RUnlock 了，导致并发的 Rebuild（持写锁）可以在
	// Search 读索引的过程中重建索引——**真的数据竞争**。
	// 本机跑不了 -race 所以一直没暴露，直到 CI 上才报出来。
	//
	// 教训：注释里写「用 RWMutex 隔开」不等于代码真的隔开了。
	// 锁的作用域要盯到实际访问共享数据的那几行为止。
	s.idxMu.RLock()
	defer s.idxMu.RUnlock()

	if queryVec == nil {
		// 纯关键词路径
		out.Results = s.bm25.Search(query, topK)
		return out, nil
	}

	hybrid := retrieve.NewHybrid(
		retrieve.BM25Searcher{BM25: s.bm25},
		s.vecIdx,
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
