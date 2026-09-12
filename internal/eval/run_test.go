package eval

import (
	"testing"

	"github.com/XiaoleC05/ContextDock/internal/config"
	"github.com/XiaoleC05/ContextDock/internal/retrieve"
	"github.com/XiaoleC05/ContextDock/internal/types"
)

func TestOptionsNormalizeFillsDefaults(t *testing.T) {
	o := Options{}.Normalize()
	if o.TopK != config.DefaultTopK {
		t.Errorf("TopK 应为 %d，实际 %d", config.DefaultTopK, o.TopK)
	}
	if o.RRFK != types.RRFK {
		t.Errorf("RRFK 应为 %d，实际 %d", types.RRFK, o.RRFK)
	}
	if o.MaxRunes != config.DefaultChunkMaxRunes {
		t.Errorf("MaxRunes 应为 %d，实际 %d", config.DefaultChunkMaxRunes, o.MaxRunes)
	}
	if o.NDCGK != NDCGK {
		t.Errorf("NDCGK 应为 %d，实际 %d", NDCGK, o.NDCGK)
	}
}

func TestOptionsNormalizeMultUsesProductionDefault(t *testing.T) {
	// 基线必须跑在**生产默认**的候选倍数上。
	//
	// 写成 1 的话，「融合比单路好多少」测的其实是「候选不够时融合好不好」——
	// 那样得到的基线是假的，后面所有改进都在和一个不存在的配置比。
	if got := (Options{}).Normalize().Mult; got != retrieve.DefaultCandidateMultiplier {
		t.Errorf("候选倍数应为生产默认值 %d，实际 %d",
			retrieve.DefaultCandidateMultiplier, got)
	}
}

func TestOptionsNormalizeOverlapZeroIsHonored(t *testing.T) {
	// ⚠️ 这条守的是参数扫描里 Overlap=0 那一列的真实性。
	//
	// 0 是合法的重叠值（完全不重叠）。如果 Normalize 写成 `<= 0 就填默认`，
	// `-overlap 0` 会被静默换成 60——那一整列数字都是「Overlap=60」的，
	// 而报告照样打印、看着完全正常。参数扫描的结论会因此出错，
	// 且没有任何迹象。
	o := Options{Overlap: 0}.Normalize()
	if o.Overlap != 0 {
		t.Errorf("Overlap=0 应当被遵守，实际被改成了 %d", o.Overlap)
	}

	// 负数才是「没传」。
	if n := (Options{Overlap: -1}).Normalize(); n.Overlap != config.DefaultChunkOverlap {
		t.Errorf("Overlap=-1 应回落成默认值 %d，实际 %d",
			config.DefaultChunkOverlap, n.Overlap)
	}
}

func TestPercentilesUseNearestRank(t *testing.T) {
	// 用最近秩而不是插值：插值出来的「P95」可能不是任何一次真实检索的耗时，
	// 而看这个数的人想知道的恰恰是「最慢的那几次有多慢」。
	ms := make([]float64, 100)
	for i := range ms {
		ms[i] = float64(i + 1) // 1..100
	}
	l := percentiles(ms)
	if l.P50Ms != 50 {
		t.Errorf("P50 = %v，期望 50（最近秩）", l.P50Ms)
	}
	if l.P95Ms != 95 {
		t.Errorf("P95 = %v，期望 95", l.P95Ms)
	}
	if l.MaxMs != 100 {
		t.Errorf("Max = %v，期望 100", l.MaxMs)
	}
}

func TestPercentilesDoesNotMutateInput(t *testing.T) {
	// 排序前必须复制：调用方传进来的切片被就地改掉的话，
	// 后续再用它的顺序就全乱了，而且不会有任何报错。
	ms := []float64{3, 1, 2}
	_ = percentiles(ms)
	if ms[0] != 3 || ms[1] != 1 || ms[2] != 2 {
		t.Errorf("输入切片被就地排序了：%v", ms)
	}
}

func TestPercentilesEmptyIsZero(t *testing.T) {
	if l := percentiles(nil); l.P50Ms != 0 || l.P95Ms != 0 || l.MaxMs != 0 {
		t.Errorf("空输入应返回零值，实际 %+v", l)
	}
}
