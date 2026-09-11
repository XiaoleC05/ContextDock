package retrieve

import (
	"github.com/XiaoleC05/ContextDock/internal/types"
)

// Run 是一路检索的结果：谁检的、检出了什么（已按相关度排好序）。
type Run struct {
	Retriever types.Retriever
	Results   []types.SearchResult
}

// FuseRRF 用 RRF 把多路结果融合成一路。
//
// RRF(d) = Σ 1 / (k + rankᵢ(d))
//
// 用 RRF 而不是加权求和的原因：BM25 分和余弦相似度**不在同一个数量级**
// （0~20 vs -1~1），加权求和必须先归一化，而归一化本身没有标准答案。
// RRF 只用名次不用分数，不要求两路分数可比。详见 docs/DESIGN.md §6。
//
// 实现上有两条硬约束，都对应真实会踩的坑：
//
//  1. **去重必须用 Chunk.StableKey()，不能用 Chunk.ID**。
//     落库之前所有 chunk 的 ID 都是 0，用 ID 做键会把两个完全不同的片段
//     合并成一条，RRF 名次整体错乱——而且**不报任何错**，只是结果变差。
//
//  2. **算分必须用 SearchResult.RRFScore()，不要自己写公式**。
//     0 号哨兵（rank=0 表示未召回）的防护在那个方法里：
//     1/(60+0)=0.016667 比真正的第一名 1/(60+1)=0.016393 还大，
//     直接代入公式会让"未召回"拿到最高分。
//
// topK <= 0 表示不截断。
func FuseRRF(k, topK int, runs ...Run) []types.SearchResult {
	if k <= 0 {
		k = types.RRFK
	}

	// 先按 StableKey 把各路的行合并到同一条记录上。
	//
	// 这里用 map 存指针，因为同一段落会被多路命中，需要一个"就地累加名次"的地方。
	byKey := make(map[string]*types.SearchResult, 64)

	for _, run := range runs {
		for i, r := range run.Results {
			rank := i + 1 // 下标转 1-based 名次
			key := r.Chunk.StableKey()

			merged, ok := byKey[key]
			if !ok {
				cp := r
				// 名次由下面按通道重新赋值，先清零避免把上一路的值带进来。
				cp.LexicalRank = 0
				cp.VectorRank = 0
				cp.LexicalScore = 0
				cp.VectorScore = 0
				cp.Score = 0
				merged = &cp
				byKey[key] = merged
			}

			switch run.Retriever {
			case types.RetrieverLexical:
				merged.LexicalRank = rank
				merged.LexicalScore = r.LexicalScore
				if merged.LexicalScore == 0 {
					merged.LexicalScore = r.Score
				}
			case types.RetrieverVector:
				merged.VectorRank = rank
				merged.VectorScore = r.VectorScore
				if merged.VectorScore == 0 {
					merged.VectorScore = r.Score
				}
			}
		}
	}

	items := make([]scored, 0, len(byKey))
	for _, r := range byKey {
		// 用方法算分，不要在这里展开公式。
		s := *r
		s.Score = s.RRFScore(k)
		items = append(items, scored{chunk: s.Chunk, score: s.Score, result: &s})
	}

	items = sortAndTrim(items, topK)

	out := make([]types.SearchResult, 0, len(items))
	for _, it := range items {
		out = append(out, *it.result)
	}
	return out
}
