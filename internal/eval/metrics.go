package eval

import (
	"math"
	"sort"

	"github.com/XiaoleC05/ContextDock/internal/types"
)

// NDCGK 是 NDCG 的截断位置。
//
// 单独定成常量而不是复用 topK：NDCG 的量级会随截断位置变，
// 报数时必须说清是 NDCG@几。两处各写一个字面量迟早会漂移，
// 而漂移的表现是"两次报告的数字对不上"，看起来像检索变差了。
const NDCGK = 10

// QueryScore 是一条查询在三项指标上的得分。
type QueryScore struct {
	// Matched 是被命中的期望下标（对应 Query.Expect 的下标），升序。
	Matched []int `json:"matched"`

	// Total 是这条查询的期望总数。
	Total int `json:"total"`

	// Recall 是命中比例：len(Matched) / Total。
	Recall float64 `json:"recall"`

	// RR 是第一条命中结果的倒数名次；没有命中时为 0。
	RR float64 `json:"rr"`

	// NDCG 是按分级增益算的归一化折损累计增益。
	NDCG float64 `json:"ndcg"`
}

// Score 对一条查询的检索结果打分。
//
// # 命中怎么判
//
// 结果片段与期望引文的**原文区间重叠**（且出自同一份语料）即算命中。
// 不用"内容相似"之类的模糊判据：那会让评测跟着实现一起漂移，
// 而评测的全部价值在于它是一把不动的尺子。
//
// # 为什么一条结果只算一次
//
// 一个片段可能同时压住两条期望（尤其引文挨得近时）。这时取**增益最大的那条**
// 计入，其余标记为已命中。不这么做的话，同一份内容会被算两遍——
// NDCG 会虚高，而报告上看不出任何异常。
func Score(expects []Expect, results []types.SearchResult, ndcgK int) QueryScore {
	if ndcgK <= 0 {
		ndcgK = NDCGK
	}

	sc := QueryScore{Total: len(expects)}
	if sc.Total == 0 {
		return sc
	}

	matched := make([]bool, len(expects))
	dcg := 0.0

	for i, r := range results {
		rank := i + 1
		gain, hit := bestGain(r, expects, matched)
		if hit && sc.RR == 0 {
			sc.RR = 1 / float64(rank)
		}
		if rank <= ndcgK {
			dcg += gain / math.Log2(float64(rank)+1)
		}
	}

	for i, ok := range matched {
		if ok {
			sc.Matched = append(sc.Matched, i)
		}
	}
	sc.Recall = float64(len(sc.Matched)) / float64(sc.Total)

	// 理想排序：把全部期望的增益从大到小排，取前 ndcgK 个。
	idcg := 0.0
	gains := make([]float64, 0, len(expects))
	for _, e := range expects {
		gains = append(gains, gainOf(e))
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(gains)))
	for i := 0; i < len(gains) && i < ndcgK; i++ {
		idcg += gains[i] / math.Log2(float64(i+2))
	}
	if idcg > 0 {
		sc.NDCG = dcg / idcg
	}
	return sc
}

// bestGain 计算一条结果带来的增益，并把被它命中的期望标记为已命中。
//
// 已命中的期望不再参与：同一条内容被两条结果分别命中时，
// 增益只能算一次。
func bestGain(r types.SearchResult, expects []Expect, matched []bool) (float64, bool) {
	best := 0.0
	var hit []int
	for i := range expects {
		if matched[i] || !covers(r, expects[i]) {
			continue
		}
		hit = append(hit, i)
		if g := gainOf(expects[i]); g > best {
			best = g
		}
	}
	for _, i := range hit {
		matched[i] = true
	}
	return best, len(hit) > 0
}

// gainOf 把相关性分级换算成 NDCG 的增益。
//
// 用标准公式 gain = 2^grade - 1，于是：
//
//	grade 2（完全回答）→ 3
//	grade 1（部分相关）→ 1
//
// 刻意不自己发明一套权重：NDCG 是被广泛使用的指标，
// 换一套增益公式会让结果无法与任何外部数字对照。
func gainOf(e Expect) float64 {
	if e.Grade <= 0 {
		return 0
	}
	return math.Pow(2, float64(e.Grade)) - 1
}

// covers 判断一条检索结果是否命中某条期望。
//
// 两个条件缺一不可：**该期望有来源**，且两者区间重叠。
//
// 为什么只查来源、不查区间是否为空：`Span.Overlaps` 用的是
// max(a,c) < min(b,d)，空区间天然不与任何东西重叠，已经挡住了
// "未定位的期望"（Start/End 都是 0）那种情况。
//
// 这里曾经还写着一个 `e.End <= e.Start` 的判断。它是**死代码**——
// 变异测试把它整条删掉，13 个测试一个都没红。原因正是上面那句：
// Overlaps 修好之后，这层防护就不再被需要了。
// 留一句已经在别处保证的判断，只会让读代码的人以为它还在起作用。
func covers(r types.SearchResult, e Expect) bool {
	if e.Source == "" {
		return false
	}
	if r.Chunk.Metadata[types.MetadataKeySource] != e.Source {
		return false
	}
	return Span{r.Chunk.StartOffset, r.Chunk.EndOffset}.Overlaps(Span{e.Start, e.End})
}

// Aggregate 是一组查询的汇总指标。
type Aggregate struct {
	// Queries 是参与统计的查询条数。
	Queries int `json:"queries"`

	// Recall 是按**期望条目**加权的平均召回率。
	//
	// 加权而不是按查询平均：多跳查询有两条期望，只命中一条
	// 应当得 0.5 分，而按查询平均会把它算成 1（因为"至少命中了一条"），
	// 从而高估多跳场景的表现。
	Recall float64 `json:"recall"`

	// MRR 与 NDCG 按查询取平均——它们本来就是「一条查询排得好不好」。
	MRR  float64 `json:"mrr"`
	NDCG float64 `json:"ndcg"`
}

// Add 把一条查询的得分并入汇总。
func (a *Aggregate) Add(s QueryScore) {
	a.Queries++
	a.Recall += s.Recall
	a.MRR += s.RR
	a.NDCG += s.NDCG
}

// Mean 把累计值转成均值。
func (a *Aggregate) Mean() Aggregate {
	if a.Queries == 0 {
		return *a
	}
	n := float64(a.Queries)
	return Aggregate{
		Queries: a.Queries,
		Recall:  a.Recall / n,
		MRR:     a.MRR / n,
		NDCG:    a.NDCG / n,
	}
}
