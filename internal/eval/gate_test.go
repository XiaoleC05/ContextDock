package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func gate(m map[string]float64) Gate { return Gate{Metrics: m} }

func TestCheckGatePassesWhenMetricsHold(t *testing.T) {
	base := gate(map[string]float64{"fused_recall": 0.49, "fused_ndcg": 0.40})
	cur := gate(map[string]float64{"fused_recall": 0.49, "fused_ndcg": 0.40})
	if err := CheckGate(base, cur, 0.05); err != nil {
		t.Errorf("指标没变不该报错: %v", err)
	}
}

func TestCheckGateCatchesDrop(t *testing.T) {
	// 这条守的是门禁的**本职**：指标掉太多必须拦下来。
	// 掉 0.47（把 RRF 排序反转时实测的幅度）绝不能放过去。
	base := gate(map[string]float64{"fused_recall": 0.49})
	cur := gate(map[string]float64{"fused_recall": 0.02})
	err := CheckGate(base, cur, 0.05)
	if err == nil {
		t.Fatal("大幅下降必须报错")
	}
	// 报错要带上具体数字——只说"回归了"会让人无从下手。
	for _, want := range []string{"fused_recall", "0.490", "0.020"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息里应出现 %q，实际:\n%v", want, err)
		}
	}
}

func TestCheckGateAllowsSmallWobble(t *testing.T) {
	// 容忍度之内的波动不该拦人。
	base := gate(map[string]float64{"fused_recall": 0.49})
	cur := gate(map[string]float64{"fused_recall": 0.46})
	if err := CheckGate(base, cur, 0.05); err != nil {
		t.Errorf("0.03 的下降在 0.05 容忍度内，不该报错: %v", err)
	}
}

func TestCheckGateIgnoresImprovement(t *testing.T) {
	// 只报下降，不报上升。上升可能是改进，值得更新基线，但不该拦人——
	// 否则每次改进都要先改基线才能提交，而那会让人干脆把门禁关掉。
	base := gate(map[string]float64{"fused_recall": 0.49})
	cur := gate(map[string]float64{"fused_recall": 0.79})
	if err := CheckGate(base, cur, 0.05); err != nil {
		t.Errorf("指标上升不该报错: %v", err)
	}
}

func TestCheckGateReportsMissingMetric(t *testing.T) {
	// 基线里有、本次没有：说明指标被改名或删掉了。
	// 静默跳过的话，那条指标就**再也不设防**了，而门禁看起来一切正常。
	base := gate(map[string]float64{"fused_recall": 0.49, "fused_ndcg": 0.40})
	cur := gate(map[string]float64{"fused_recall": 0.49})
	err := CheckGate(base, cur, 0.05)
	if err == nil {
		t.Fatal("缺指标必须报错")
	}
	if !strings.Contains(err.Error(), "fused_ndcg") {
		t.Errorf("错误里应点名缺的是哪一项，实际:\n%v", err)
	}
	// ⚠️ 还要断言**说的是「缺」而不是「掉了」**。
	//
	// 少了这一条，把「缺指标」那个分支整段删掉也测不出来：那样 got 会取零值，
	// 于是报成"fused_ndcg 从 0.400 掉到 0.000"——同样包含 "fused_ndcg"、
	// 同样会报错，看起来门禁工作正常。而它其实把**指标被改名或删掉**
	// 误报成了"质量下降"，两者的排查方向完全不同。
	if !strings.Contains(err.Error(), "没有这个指标") {
		t.Errorf("缺指标应当报成「没有这个指标」，而不是当成下降：\n%v", err)
	}
}

func TestCheckGateIsDeterministic(t *testing.T) {
	// 报错顺序必须确定：map 遍历顺序随机的话，同一份数据两次跑
	// 可能报出不同的"第一项"，对比输出时会以为改了东西。
	base := gate(map[string]float64{"a": 1, "b": 1, "c": 1})
	cur := gate(map[string]float64{"a": 0, "b": 0, "c": 0})
	first := CheckGate(base, cur, 0.05).Error()
	for i := 0; i < 20; i++ {
		if got := CheckGate(base, cur, 0.05).Error(); got != first {
			t.Fatalf("两次报错内容不一致：\n%s\n---\n%s", first, got)
		}
	}
	if !strings.Contains(first, "  a: ") {
		t.Errorf("报错应按指标名排序（a 在最前），实际:\n%s", first)
	}
}

func TestGateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gate.json")
	want := gate(map[string]float64{"fused_recall": 0.49})

	if err := WriteGate(path, want); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	got, err := LoadGate(path)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got.Metrics["fused_recall"] != 0.49 {
		t.Errorf("读回来的值不对：%+v", got.Metrics)
	}
}

func TestLoadGateRejectsEmptyAndUnknownFields(t *testing.T) {
	dir := t.TempDir()

	// 没有任何指标：多半是文件写错了，静默通过等于门禁失效。
	p := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(p, []byte(`{"metrics":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadGate(p); err == nil {
		t.Error("空基线应当报错")
	}

	// 未知字段：手写基线时最常见的错误就是键名拼错，
	// 而标准解析器会静默忽略它——那条指标就不再设防了。
	p2 := filepath.Join(dir, "typo.json")
	if err := os.WriteFile(p2, []byte(`{"metirics":{"a":1}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadGate(p2); err == nil {
		t.Error("键名拼错应当报错，而不是当成空基线放过去")
	}
}
