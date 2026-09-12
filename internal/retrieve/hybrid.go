package retrieve

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/XiaoleC05/ContextDock/internal/types"
)

var (
	// ErrBothRetrieversFailed 表示两路检索都失败了。
	//
	// 只有一路失败**不算**失败——那会降级成只返回另一路的结果。
	ErrBothRetrieversFailed = errors.New("retrieve: 关键词与向量两路检索都失败了")
)

const (
	// DefaultHybridTimeout 是单次混合检索的超时。
	//
	// 注意：两路检索都是纯内存计算，正常在毫秒级。
	// 这个超时防的不是慢查询本身，而是"某一路上游出了意料之外的问题"——
	// 宁可返回降级结果，也不能把 Agent 卡死。
	DefaultHybridTimeout = 5 * time.Second

	// DefaultCandidateMultiplier 是每路检索时的候选倍数。
	//
	// # 为什么是 1（曾经是 3）
	//
	// 原始理由是「只取 topK 的话，两路各自的第一名可能都不在对方的候选里，
	// 融合会非常单薄」。这个担心在**机制上**成立，但在**实测中被推翻**了：
	//
	//	候选倍数   融合 recall@10   （2026-09-12，43 条评测集）
	//	   1          0.791
	//	   2          0.721
	//	   3          0.628   ← 旧默认
	//	   4          0.651
	//	   8          0.674
	//
	// 放大候选确实让融合"看得更多"，但多出来的大部分是弱通道的噪声，
	// 而 RRF 按名次等权合并——噪声照样拿分，把强通道的相关结果挤下去。
	// 降到 1 之后融合不再输给向量单路（0.791 vs 0.767）。
	//
	// 代价要一并说清：倍数=1 有一个**结构性盲区**——
	// 一条排在向量第 15 名、词法第 25 名的结果，两路都不会把它送进融合。
	// 只是在这份评测里，噪声的伤害大于这个盲区。详见 BENCHMARKS.md。
	DefaultCandidateMultiplier = 1
)

// LexicalSearcher 是关键词检索的抽象。
//
// 虽然内存版 BM25 本身不会失败，接口仍然返回 error：
// 一来让两条路径对称、降级逻辑的每个分支都可达可测，
// 二来将来换成数据库支撑的关键词检索时不用改接口。
type LexicalSearcher interface {
	Search(query string, topK int) ([]types.SearchResult, error)
}

// VectorSearcher 是向量检索的抽象。VectorIndex 实现了它。
//
// # 为什么它带 ctx 而 LexicalSearcher 不带
//
// 向量检索**可能打到数据库**（见 store.EmbeddingSearcher）：那种实现
// 必须能被打断，否则一次慢查询会一直占着资源。内存版（VectorIndex）
// 用不上 ctx，但它也得收下——接口只能按最严格的那个实现来定。
//
// LexicalSearcher 暂时还是纯内存的 BM25，等它也下推到数据库时
// 要一并加 ctx（那时 hybrid 里两个 goroutine 的写法会对称）。
//
// # 返回值的契约
//
// 返回**至多** topK 条，按相似度降序。
//
// ⚠️ **不保证穷尽，也不保证包含真正的最近邻**——数据库后端的
// HNSW 是近似索引，返回条数可能少于 topK。调用方不得假设
// `len(results) == topK`。
//
// 这个契约对内存实现同样成立（它会跳过零模长的片段），
// 所以不是为近似索引临时开的口子，而是把既有事实写下来。
type VectorSearcher interface {
	Search(ctx context.Context, query []float32, topK int) ([]types.SearchResult, error)
}

// BM25Searcher 把 BM25 适配成 LexicalSearcher。
//
// BM25.Search 本身不返回 error（纯内存计算不会失败），
// 这个适配器补上接口要求的那一层。
type BM25Searcher struct{ *BM25 }

// Search 实现 LexicalSearcher。
func (s BM25Searcher) Search(query string, topK int) ([]types.SearchResult, error) {
	return s.BM25.Search(query, topK), nil
}

// Hybrid 并行执行两路检索，然后用 RRF 融合。
type Hybrid struct {
	lexical LexicalSearcher
	vector  VectorSearcher

	timeout   time.Duration
	k         int
	mult      int
	onPathErr func(types.Retriever, error)
}

// NewHybrid 创建一个混合检索器。
func NewHybrid(lexical LexicalSearcher, vector VectorSearcher) *Hybrid {
	return &Hybrid{
		lexical: lexical,
		vector:  vector,
		timeout: DefaultHybridTimeout,
		k:       types.RRFK,
		mult:    DefaultCandidateMultiplier,
	}
}

// WithTimeout 覆盖超时时长。
func (h *Hybrid) WithTimeout(d time.Duration) *Hybrid {
	if d > 0 {
		h.timeout = d
	}
	return h
}

// WithRRFK 覆盖 RRF 的平滑常数 k。
func (h *Hybrid) WithRRFK(k int) *Hybrid {
	if k > 0 {
		h.k = k
	}
	return h
}

// WithCandidateMultiplier 覆盖候选倍数。
func (h *Hybrid) WithCandidateMultiplier(n int) *Hybrid {
	if n > 0 {
		h.mult = n
	}
	return h
}

// WithErrorHandler 注册单路失败的回调。
//
// 降级是静默的——一路挂了照样返回结果。所以必须留一个观测口，
// 否则线上会出现"检索质量莫名下降"而没有任何信号。
// 本包不引日志库，把记录方式交给调用方决定。
func (h *Hybrid) WithErrorHandler(fn func(types.Retriever, error)) *Hybrid {
	h.onPathErr = fn
	return h
}

// Search 并行跑两路检索并融合。
//
// 降级规则：
//   - 两路都成功 → 正常融合
//   - 只有一路成功 → 返回那一路的结果（仍然走一遍 RRF，保持返回格式一致）
//   - 两路都失败 → 返回 ErrBothRetrieversFailed
//   - 超时 → 已拿到的那一路照样返回；一路都没拿到才返回 ctx 错误
//
// topK <= 0 时使用 defaultTopK。
func (h *Hybrid) Search(ctx context.Context, query string, queryVec []float32, topK int) ([]types.SearchResult, error) {
	return h.search(ctx, query, queryVec, topK, true)
}

// SearchAll 与 Search 完全相同，但**不截断**融合结果。
//
// # 什么时候需要它
//
// 调用方要在融合之后再加工（比如 #50 的相邻片段合并），加工完才截断。
//
// # ⚠️ 不要用「传一个更大的 topK」来达到同样目的
//
// 那个做法看起来等价，实际上**会改变排名**：每路取回的候选数
// `n = topK * mult` 会跟着变大，而 RRF 的排名**依赖候选池大小**——
// 候选一多，弱通道的噪声就能压过强通道的相关结果（这正是 #44 测出来的）。
//
// 实测后果：开了相邻片段合并之后 recall 从 0.837 掉到 0.767，
// 而 top-10 里换掉的是一整批不相干的结果，不是"少了几条冗余"。
func (h *Hybrid) SearchAll(ctx context.Context, query string, queryVec []float32, topK int) ([]types.SearchResult, error) {
	return h.search(ctx, query, queryVec, topK, false)
}

// search 是 Search / SearchAll 的共同实现。
//
// topK 决定**每路取多少候选**（n = topK * mult），truncate 决定
// 融合结果要不要截断到 topK。两者分开，是因为它们的含义完全不同。
func (h *Hybrid) search(ctx context.Context, query string, queryVec []float32, topK int, truncate bool) ([]types.SearchResult, error) {
	const defaultTopK = 10
	if topK <= 0 {
		topK = defaultTopK
	}
	fuseK := topK
	if !truncate {
		fuseK = 0 // FuseRRF 的 0 表示不截断
	}
	n := topK * h.mult

	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	type pathOut struct {
		run Run
		err error
	}

	// 缓冲 2：保证两个 goroutine 无论如何都能把结果写进去然后退出，
	// 不会因为我们提前 return 而永久阻塞在发送上（goroutine 泄漏）。
	out := make(chan pathOut, 2)

	go func() {
		res, err := h.lexical.Search(query, n)
		out <- pathOut{
			run: Run{Retriever: types.RetrieverLexical, Results: res},
			err: err,
		}
	}()

	go func() {
		res, err := h.vector.Search(ctx, queryVec, n)
		out <- pathOut{
			run: Run{Retriever: types.RetrieverVector, Results: res},
			err: err,
		}
	}()

	// ⚠️ 两个 goroutine 各自往自己的 channel 消息里写结果，**不共享任何变量**。
	// 如果让它们直接往同一个 map / slice 里写，就是典型的数据竞争，
	// 而且 `go test -race` 之外很难发现。
	var runs []Run
	var errs []error

	for i := 0; i < 2; i++ {
		select {
		case p := <-out:
			if p.err != nil {
				errs = append(errs, p.err)
				h.reportError(p.run.Retriever, p.err)
				continue
			}
			runs = append(runs, p.run)
		case <-ctx.Done():
			// 超时或取消：把手头已有的结果用上，能返回多少返回多少。
			if len(runs) == 0 {
				return nil, ctx.Err()
			}
			return FuseRRF(h.k, fuseK, runs...), nil
		}
	}

	if len(runs) == 0 {
		return nil, fmt.Errorf("%w: %v", ErrBothRetrieversFailed, errors.Join(errs...))
	}
	return FuseRRF(h.k, fuseK, runs...), nil
}

func (h *Hybrid) reportError(r types.Retriever, err error) {
	if h.onPathErr != nil {
		h.onPathErr(r, err)
	}
}
