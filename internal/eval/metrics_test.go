package eval

import (
	"math"
	"testing"

	"github.com/XiaoleC05/ContextDock/internal/types"
)

// res 造一条落在源文档 doc.md 的 [start,end) 区间上的检索结果。
func res(start, end int) types.SearchResult {
	c := types.Chunk{StartOffset: start, EndOffset: end}
	c.SetMetadata(types.MetadataKeySource, "doc.md")
	return types.SearchResult{Chunk: c}
}

func expect(start, end int, grade Grade) Expect {
	return Expect{Source: "doc.md", Quote: "x", Start: start, End: end, Grade: grade}
}

func almost(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %.6f，期望 %.6f", name, got, want)
	}
}

// log2 的倒数累积：NDCG 的分母是 log2(名次+1)。
func ndcg1(gain float64) float64 { return gain / math.Log2(2) } // 名次 1
func ndcg2(gain float64) float64 { return gain / math.Log2(3) } // 名次 2

func TestScorePerfectHitAtRankOne(t *testing.T) {
	sc := Score([]Expect{expect(100, 120, GradeFull)}, []types.SearchResult{res(90, 130)}, 10)
	almost(t, "Recall", sc.Recall, 1)
	almost(t, "RR", sc.RR, 1)
	almost(t, "NDCG", sc.NDCG, 1)
	if len(sc.Matched) != 1 || sc.Matched[0] != 0 {
		t.Errorf("Matched = %v，期望 [0]", sc.Matched)
	}
}

func TestScoreHitAtRankTwo(t *testing.T) {
	// 手算：命中在名次 2，增益 3（grade 2）。
	//   DCG  = 3 / log2(3)
	//   IDCG = 3 / log2(2) = 3
	//   NDCG = DCG / IDCG
	sc := Score([]Expect{expect(100, 120, GradeFull)},
		[]types.SearchResult{res(0, 10), res(90, 130)}, 10)

	almost(t, "Recall", sc.Recall, 1)
	almost(t, "RR", sc.RR, 0.5)
	almost(t, "NDCG", sc.NDCG, ndcg2(3)/ndcg1(3))
}

func TestScoreMiss(t *testing.T) {
	sc := Score([]Expect{expect(100, 120, GradeFull)},
		[]types.SearchResult{res(0, 10), res(200, 300)}, 10)
	almost(t, "Recall", sc.Recall, 0)
	almost(t, "RR", sc.RR, 0)
	almost(t, "NDCG", sc.NDCG, 0)
	if len(sc.Matched) != 0 {
		t.Errorf("Matched = %v，期望空", sc.Matched)
	}
}

func TestScorePartialGradeGetsLowerGain(t *testing.T) {
	// 部分相关（grade 1，增益 1）命中在名次 1，NDCG 仍应为 1——
	// 因为理想排序里它也只有这一条，DCG 与 IDCG 同为 1。
	sc := Score([]Expect{expect(100, 120, GradePartial)}, []types.SearchResult{res(90, 130)}, 10)
	almost(t, "NDCG", sc.NDCG, 1)

	// 但同一条结果在两条期望下的增益不同：完全回答排在前面时，
	// 把部分相关挤到后面，NDCG 应当低于 1。
	two := Score(
		[]Expect{expect(100, 120, GradeFull), expect(300, 320, GradePartial)},
		[]types.SearchResult{res(300, 320), res(90, 130)}, 10)
	if two.NDCG >= 1 {
		t.Errorf("完全回答被排到后面，NDCG 应小于 1，实际 %.4f", two.NDCG)
	}
	// 反过来（完全回答在前）应当正好等于 1。
	rev := Score(
		[]Expect{expect(100, 120, GradeFull), expect(300, 320, GradePartial)},
		[]types.SearchResult{res(90, 130), res(300, 320)}, 10)
	almost(t, "理想排序 NDCG", rev.NDCG, 1)
}

func TestScoreMultiHopRecallIsFractional(t *testing.T) {
	// 多跳查询两条期望只命中一条 → recall 应为 0.5。
	// 按查询取平均（"至少命中一条就算 1"）会高估多跳场景，这里是那条防线。
	sc := Score(
		[]Expect{expect(100, 120, GradePartial), expect(900, 920, GradePartial)},
		[]types.SearchResult{res(90, 130)}, 10)
	almost(t, "Recall", sc.Recall, 0.5)
	if len(sc.Matched) != 1 || sc.Matched[0] != 0 {
		t.Errorf("Matched = %v，期望 [0]", sc.Matched)
	}
}

func TestScoreCountsOneResultOnlyOnce(t *testing.T) {
	// 一个片段同时压住两条期望时，增益只算最大的那一次。
	// 不这么做的话同一份内容会被算两遍，NDCG 虚高。
	//
	// 两条期望都在 [90,130) 这个区间内：返回这一条结果就够了，
	// 但增益应当是 max(3, 1) = 3，而不是 3+1。
	sc := Score(
		[]Expect{expect(100, 110, GradeFull), expect(115, 125, GradePartial)},
		[]types.SearchResult{res(90, 130)}, 10)

	almost(t, "Recall（两条都算命中）", sc.Recall, 1)
	// IDCG = 3/log2(2) + 1/log2(3) = 3 + 0.6309…
	// DCG  = 3/log2(2)                  = 3
	want := ndcg1(3) / (ndcg1(3) + ndcg2(1))
	almost(t, "NDCG", sc.NDCG, want)
	if sc.NDCG >= 1 {
		t.Error("NDCG 不应等于 1——部分相关那条没有被单独计入增益")
	}
}

func TestScoreIgnoresUnlocatedExpect(t *testing.T) {
	// 没定位过的期望 Start/End 都是 0。若不做防护，它会与文档开头
	// 的一切"重叠"，于是所有查询都轻松命中——这是最容易悄悄发生的一种失效。
	unlocated := Expect{Source: "doc.md"}
	sc := Score([]Expect{unlocated}, []types.SearchResult{res(0, 10)}, 10)
	almost(t, "Recall", sc.Recall, 0)
}

func TestScoreIgnoresExpectWithoutSource(t *testing.T) {
	// 没有来源的期望一律不命中，即使区间看起来完全吻合、
	// 且结果片段自己也没有 source 元数据（于是两个空来源"相等"）。
	//
	// 这条防护防的是**调用方直接构造 Expect** 的情况：正常走 Load
	// 的期望一定校验过 source 非空，但 Score 是导出的，别人可以
	// 手搓一个 Expect 进来。手搓出来的 Start/End 大概率是零值，
	// 而零值区间一旦被当成"压在文档开头"，所有查询都会轻松命中。
	noSource := types.SearchResult{Chunk: types.Chunk{StartOffset: 0, EndOffset: 10}}
	sc := Score([]Expect{{Quote: "x", Start: 0, End: 10, Grade: GradeFull}},
		[]types.SearchResult{noSource}, 10)
	almost(t, "Recall", sc.Recall, 0)
}

func TestScoreCountsRepeatedHitsOnlyOnce(t *testing.T) {
	// 两条结果都压住同一条期望时，增益只能算一次。
	//
	// 少了这条，`bestGain` 里的 matched 判断被删掉也测不出来——
	// 上面那条只返回**一条**结果的用例覆盖不到重复计数。
	// （这条测试就是变异测试逼出来的：它先报了 MISSED。）
	sc := Score([]Expect{expect(100, 120, GradeFull)},
		[]types.SearchResult{res(95, 115), res(105, 125)}, 10)

	almost(t, "Recall", sc.Recall, 1)
	// 只算一次时：DCG = 3/log2(2) = 3，IDCG = 3 → NDCG = 1。
	// 重复计入时：DCG = 3/log2(2) + 3/log2(3) ≈ 4.89 → NDCG ≈ 1.63。
	almost(t, "NDCG", sc.NDCG, 1)
	if sc.NDCG > 1 {
		t.Error("NDCG 超过 1，说明同一片段的增益被重复计入了")
	}
}

func TestScoreIgnoresOtherSource(t *testing.T) {
	// 区间相同但来自另一份语料，不该算命中。
	other := types.SearchResult{Chunk: types.Chunk{StartOffset: 90, EndOffset: 130}}
	other.Chunk.SetMetadata(types.MetadataKeySource, "别的文档.md")

	sc := Score([]Expect{expect(100, 120, GradeFull)}, []types.SearchResult{other}, 10)
	almost(t, "Recall", sc.Recall, 0)
}

func TestScoreHalfOpenBoundaryIsNotAHit(t *testing.T) {
	// 端点相接不算命中：切分恰好把答案切成两半时，
	// 相邻两段首尾相接是正常产物，算成两次命中会让 recall 虚高。
	sc := Score([]Expect{expect(100, 120, GradeFull)}, []types.SearchResult{res(80, 100)}, 10)
	almost(t, "Recall", sc.Recall, 0)
}

func TestScoreNDCGRespectsCutoff(t *testing.T) {
	// 命中落在截断之外时，NDCG 应当为 0，但 recall 仍然是 1
	// （结果确实返回了，只是排得靠后）——两者回答的是不同的问题。
	var results []types.SearchResult
	for i := 0; i < 9; i++ {
		results = append(results, res(i*10, i*10+5))
	}
	results = append(results, res(90, 130)) // 名次 10

	sc := Score([]Expect{expect(100, 120, GradeFull)}, results, 10)
	almost(t, "Recall", sc.Recall, 1)
	if sc.NDCG <= 0 {
		t.Error("名次 10 在截断内（NDCG@10），NDCG 不该为 0")
	}

	// 截断到 5 时，名次 10 落在外面。
	sc5 := Score([]Expect{expect(100, 120, GradeFull)}, results, 5)
	almost(t, "Recall（截断不影响 recall）", sc5.Recall, 1)
	almost(t, "NDCG@5", sc5.NDCG, 0)
}

func TestAggregateAverages(t *testing.T) {
	var a Aggregate
	a.Add(QueryScore{Recall: 1, RR: 1, NDCG: 1, Total: 1})
	a.Add(QueryScore{Recall: 0, RR: 0, NDCG: 0, Total: 1})
	m := a.Mean()
	if m.Queries != 2 {
		t.Errorf("Queries = %d，期望 2", m.Queries)
	}
	almost(t, "Recall", m.Recall, 0.5)
	almost(t, "MRR", m.MRR, 0.5)
	almost(t, "NDCG", m.NDCG, 0.5)
}

func TestAggregateEmptyMeanIsZero(t *testing.T) {
	var a Aggregate
	if m := a.Mean(); m.Queries != 0 || m.Recall != 0 {
		t.Errorf("空汇总的均值应为零值，实际 %+v", m)
	}
}
