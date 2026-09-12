package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Gate 是回归门禁用的一组指标。
//
// # 为什么门禁跑的是**假嵌入**而不是真实 API
//
// 三个理由，缺一不可：
//
//  1. **CI 里没有 API Key**。真实嵌入要么需要配置密钥（那 CI 就不再是
//     "任何人 fork 都能跑"的），要么会因为缺 Key 而失败。
//  2. **假嵌入是确定性的**。真实嵌入接口有微小抖动，每次跑数字都不同，
//     而"门禁"需要"同一份代码 → 同一份数字"才能定阈值。
//  3. **它足够抓住要抓的东西**。实测：把 RRF 的排序反转之后，
//     融合 recall 从 0.490 掉到 0.020、NDCG 从 0.407 掉到 0.006。
//     质量回归里最要命的那类（排序被改坏）它完全接得住。
//
// ⚠️ 代价要说清：**假嵌入的向量没有语义**，所以它测不出"语义召回变差了"。
// 它能守住的是"检索流程的排序行为没被改坏"。语义质量仍然要靠人跑真嵌入评测。
type Gate struct {
	// Note 说明这份基线的来历，供人核对。
	Note string `json:"note"`

	// Metrics 是指标名 → 值。
	Metrics map[string]float64 `json:"metrics"`
}

// BuildGate 从一次评测结果里提取门禁指标。
//
// 只取**跨子集的总计**：分子集会让门禁对"某一组抖了一下"过度敏感，
// 而那种抖动在 43 条查询的样本量下本来就没有意义。
func BuildGate(rep *Report, note string) Gate {
	get := func(ch Channel) Aggregate { return rep.Overall[ch] }
	return Gate{
		Note: note,
		Metrics: map[string]float64{
			"fused_recall":   get(ChannelFused).Recall,
			"fused_ndcg":     get(ChannelFused).NDCG,
			"fused_mrr":      get(ChannelFused).MRR,
			"lexical_recall": get(ChannelLexical).Recall,
			"vector_recall":  get(ChannelVector).Recall,
		},
	}
}

// CheckGate 把当前指标与基线比对，任何一项掉超过 tolerance 就报错。
//
// # 为什么用"相对基线 + 固定容忍度"，而不是写死一个绝对阈值
//
// 绝对阈值（比如"recall 必须 ≥ 0.45"）看着简单，但它**只在评测集和语料
// 完全不变时才有意义**。加一条查询、换一份语料、调一次切分参数，
// 那个数就得手调一次——而"忘了调"的表现是门禁失效，不报错。
//
// 相对基线让"下降"有明确含义：**与这次提交之前的自己比**。
// 容忍度写死是因为门禁跑的是假嵌入，数字完全确定，抖动只可能来自代码——
// 那种情况下 0.05 已经是很宽松的口子了。
//
// 只报**下降**，不报上升：上升可能是改进，值得更新基线，但不该拦人。
func CheckGate(base, cur Gate, tolerance float64) error {
	// 键排序后遍历：报错信息的顺序必须确定，否则同一份数据两次跑
	// 可能报出不同的"第一项"，对比输出时会以为改了东西。
	keys := make([]string, 0, len(base.Metrics))
	for k := range base.Metrics {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var bad []string
	for _, k := range keys {
		want, ok := base.Metrics[k]
		if !ok {
			continue
		}
		got, ok := cur.Metrics[k]
		if !ok {
			bad = append(bad, fmt.Sprintf("%s: 本次没有这个指标", k))
			continue
		}
		if want-got > tolerance {
			bad = append(bad, fmt.Sprintf("%s: %.3f → %.3f（下降 %.3f，超过容忍度 %.3f）",
				k, want, got, want-got, tolerance))
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("检索质量回归：\n  %s\n\n如果这是有意的改进，请更新基线：\n"+
		"  go run ./cmd/eval -fake-embed -write-gate eval/gate.json",
		strings.Join(bad, "\n  "))
}

// LoadGate 读取一份基线。
func LoadGate(path string) (Gate, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Gate{}, fmt.Errorf("eval: 读取基线 %s 失败: %w", path, err)
	}
	var g Gate
	if err := unmarshalStrict(raw, path, &g); err != nil {
		return Gate{}, err
	}
	if len(g.Metrics) == 0 {
		return Gate{}, fmt.Errorf("eval: 基线 %s 里没有任何指标", path)
	}
	return g, nil
}

// WriteGate 写出一份基线。
func WriteGate(path string, g Gate) error {
	out, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return fmt.Errorf("eval: 序列化基线失败: %w", err)
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
		return fmt.Errorf("eval: 写入基线失败: %w", err)
	}
	return nil
}
