package retrieve

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/XiaoleC05/ContextDock/internal/types"
)

var (
	// ErrVectorDim 表示查询向量或文档向量的维度不正确。
	ErrVectorDim = errors.New("retrieve: 向量维度不正确")

	// ErrZeroVector 表示查询向量的模长为 0，余弦相似度无定义。
	ErrZeroVector = errors.New("retrieve: 查询向量是零向量，余弦相似度无定义")
)

// VectorIndex 是内存版的向量检索。
//
// 它同样是"建好后只读"：Index 重建内部状态，Search 只读且可并发调用。
type VectorIndex struct {
	chunks []types.Chunk
	vecs   [][]float32
	norms  []float32 // 预算好的模长，避免每次查询重复计算
}

// NewVectorIndex 创建一个空的向量索引。
func NewVectorIndex() *VectorIndex {
	return &VectorIndex{}
}

// Len 返回已索引的向量数。
func (v *VectorIndex) Len() int { return len(v.chunks) }

// Index 重建索引。
//
// 每个 chunk 必须已经带好 types.EmbeddingDim 维的向量——
// 维度不对会立刻报错，而不是等到查询时算出莫名其妙的结果。
// 还没有算过 embedding 的片段不应该传进来。
func (v *VectorIndex) Index(chunks []types.Chunk) error {
	v.chunks = make([]types.Chunk, 0, len(chunks))
	v.vecs = make([][]float32, 0, len(chunks))
	v.norms = make([]float32, 0, len(chunks))

	for i, c := range chunks {
		if len(c.Embedding) != types.EmbeddingDim {
			return fmt.Errorf("%w: 第 %d 个片段（key=%s）有 %d 维，期望 %d 维",
				ErrVectorDim, i, c.StableKey(), len(c.Embedding), types.EmbeddingDim)
		}
		v.chunks = append(v.chunks, c)
		v.vecs = append(v.vecs, c.Embedding)
		v.norms = append(v.norms, norm(c.Embedding))
	}
	return nil
}

// Search 返回与 query 向量最相似的前 topK 条（topK <= 0 表示不截断）。
//
// 相似度用**余弦相似度**。返回的 SearchResult 里填好了 VectorRank（1-based）
// 和 VectorScore（原始余弦值，范围 -1~1）。
//
// ctx 是 VectorSearcher 接口要求的。内存扫描本身是微秒级的、打断没有收益，
// 但**开头的 ctx.Err() 检查是必须的**：调用方（hybrid）传下来的是带超时的
// ctx，已经超时时应当立刻返回错误，而不是白扫一遍再把结果丢掉。
//
// ⚠️ 数据库后端（store.EmbeddingSearcher）走的是同一份契约：可能返回
// **少于 topK** 条。调用方不得假设 len(results) == topK。
func (v *VectorIndex) Search(ctx context.Context, query []float32, topK int) ([]types.SearchResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(query) != types.EmbeddingDim {
		return nil, fmt.Errorf("%w: 查询向量 %d 维，期望 %d 维",
			ErrVectorDim, len(query), types.EmbeddingDim)
	}
	if len(v.chunks) == 0 {
		return []types.SearchResult{}, nil
	}

	qNorm := norm(query)
	if qNorm == 0 {
		return nil, ErrZeroVector
	}

	items := make([]scored, 0, len(v.chunks))
	for i := range v.chunks {
		// 文档向量模长为 0 的情况在 Index 时不拦（可能是全零向量），
		// 这里跳过，否则会算出 NaN 污染整个排序。
		if v.norms[i] == 0 {
			continue
		}
		sim := float64(dot(query, v.vecs[i]) / (qNorm * v.norms[i]))
		items = append(items, scored{chunk: v.chunks[i], score: sim})
	}

	items = sortAndTrim(items, topK)

	out := make([]types.SearchResult, len(items))
	for i, it := range items {
		out[i] = types.SearchResult{
			Chunk:       it.chunk,
			Score:       it.score,
			VectorScore: it.score,
			VectorRank:  i + 1,
		}
	}
	return out, nil
}

// dot 是两个向量的点积。
//
// 循环里刻意先判断长度再取值：Go 的边界检查消除需要编译器能证明
// i < len(a) 且 i < len(b)，写成 `for i := range a` 加上显式长度断言
// 能让热点循环少一次检查。这里先用最直白的写法，优化留到 M7。
func dot(a, b []float32) float32 {
	var s float32
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

// norm 是向量的 L2 模长。
func norm(a []float32) float32 {
	var s float32
	for _, v := range a {
		s += v * v
	}
	return float32(math.Sqrt(float64(s)))
}
