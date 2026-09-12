package main

import (
	"testing"

	"github.com/XiaoleC05/ContextDock/internal/types"
)

// TestPercentilesUsesNearestRank 钉住分位数的算法。
//
// ⚠️ 这里必须用**最近秩法**而不是插值。样本量不大时插值会给出一个
// 「比任何一次真实查询都快」的 P95——那是编出来的数，不是量出来的。
func TestPercentilesUsesNearestRank(t *testing.T) {
	// 1..100，最近秩下 P50=50、P95=95、Max=100。
	xs := make([]float64, 100)
	for i := range xs {
		xs[i] = float64(i + 1)
	}

	got := percentiles(xs)
	if got.P50 != 50 {
		t.Errorf("P50 应为 50（最近秩），实际 %v", got.P50)
	}
	if got.P95 != 95 {
		t.Errorf("P95 应为 95（最近秩，不是 95.05 那种插值值），实际 %v", got.P95)
	}
	if got.Max != 100 {
		t.Errorf("Max 应为 100，实际 %v", got.Max)
	}

	// 空输入不该 panic
	if percentiles(nil) != (Latency{}) {
		t.Error("空输入应当返回零值")
	}
}

// TestPercentilesDoesNotMutateInput 验证不会就地排序调用方的切片。
//
// 就地排序会让调用方手里的样本顺序变掉，而它可能还要用——
// 本项目在评测的 percentiles 上就踩过同一个坑（变异点
// 「eval: 算分位数时就地排序了输入切片」）。
func TestPercentilesDoesNotMutateInput(t *testing.T) {
	xs := []float64{5, 1, 4, 2, 3}
	_ = percentiles(xs)
	for i, want := range []float64{5, 1, 4, 2, 3} {
		if xs[i] != want {
			t.Fatalf("输入切片被改写了：%v（应当仍是 [5 1 4 2 3]）", xs)
		}
	}
}

// TestOverlapRatio 验证重合率按**片段身份**算，而不是按名次。
func TestOverlapRatio(t *testing.T) {
	mk := func(ids ...int64) []types.SearchResult {
		out := make([]types.SearchResult, len(ids))
		for i, id := range ids {
			out[i] = types.SearchResult{Chunk: types.Chunk{ID: id}}
		}
		return out
	}

	// 全部重合（哪怕顺序不同）
	if got := overlapRatio(mk(1, 2, 3), mk(3, 1, 2)); got != 1 {
		t.Errorf("同一批片段应当算 100%% 重合，实际 %v", got)
	}
	// 一半重合
	if got := overlapRatio(mk(1, 2, 3, 4), mk(1, 2, 9, 8)); got != 0.5 {
		t.Errorf("一半重合应当是 0.5，实际 %v", got)
	}
	// 基线为空时不除零
	if got := overlapRatio(mk(1, 2), nil); got != 0 {
		t.Errorf("基线为空应当是 0，实际 %v", got)
	}
}

// TestGenerateCorpusIsDeterministic 验证同一组参数跑出同一份语料。
//
// 不确定的语料会让两次测量的数字**不可比**，而那正是这个工具存在的意义。
func TestGenerateCorpusIsDeterministic(t *testing.T) {
	a := generateCorpus(100, 10)
	b := generateCorpus(100, 10)

	if len(a) != len(b) {
		t.Fatalf("文档数不一致：%d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Content != b[i].Content {
			t.Fatalf("第 %d 篇文档内容不一致——语料生成必须是确定的", i)
		}
		if a[i].Source != b[i].Source {
			t.Fatalf("第 %d 篇文档来源不一致", i)
		}
	}
}

// TestGenerateCorpusScales 验证片段数是随参数走的（松散的边界检查）。
//
// 不做精确断言：「n 个片段」是**目标**，实际数量取决于切分器的行为，
// 报告里也是以实际值为准的。这里只确认它大体成比例、且不会退化。
func TestGenerateCorpusScales(t *testing.T) {
	small := generateCorpus(100, 10)
	large := generateCorpus(1000, 10)

	if len(large) <= len(small) {
		t.Errorf("目标片段数变大时文档数应当变多：%d -> %d", len(small), len(large))
	}
	// 每篇文档都要有内容，否则导入会直接失败
	for i, d := range large {
		if d.Content == "" {
			t.Fatalf("第 %d 篇文档是空的", i)
		}
	}
}
