package main

import (
	"strings"
	"testing"
)

// sampleEvalOut 是 cmd/eval 真实输出的一段缩影，用来钉住 sliceEval 的行为。
//
// 它是**手写的**，不是抓来的——所以它证明不了"cmd/eval 现在还这么打印"，
// 只证明切片的逻辑本身对。标记文案漂移的兜底是生成期报错，见 main.go 的说明。
const sampleEvalOut = `评测参数：切分 400/60
按子集分通道成绩：
组       条数  词法 recall
-------  ----  -----------
zh         15        0.767
总计       51        0.461
  说明：recall 按**期望条目**加权。

融合相比单路最优者的增益（recall）：
组       融合   单路最优      增益
总计     0.667  向量 0.647  +0.020

top-k 平均覆盖的不同文档数：
通道     文档数
`

func TestSliceEvalKeepsQualitySection(t *testing.T) {
	got, err := sliceEval(sampleEvalOut)
	if err != nil {
		t.Fatalf("切片失败: %v", err)
	}

	// 该留下的
	for _, want := range []string{"按子集分通道成绩：", "总计       51", "融合相比单路最优者的增益"} {
		if !strings.Contains(got, want) {
			t.Errorf("切片结果里应当包含 %q，实际:\n%s", want, got)
		}
	}
	// 该切掉的
	for _, unwanted := range []string{"top-k 平均覆盖", "评测参数："} {
		if strings.Contains(got, unwanted) {
			t.Errorf("切片结果里不该出现 %q，实际:\n%s", unwanted, got)
		}
	}
}

// TestSliceEvalFailsLoudlyOnMissingMarker 验证标记消失时**报错**而不是退化成整段输出。
//
// 这条是这张素材能被信任的关键：cmd/eval 的表头一旦改文案，
// 生成演示素材必须直接失败。如果这里退化成"找不到就不切了"，
// 结果是一张塞满无关小节、还得靠人眼发现不对的图。
func TestSliceEvalFailsLoudlyOnMissingMarker(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"没有起始标记", "完全无关的输出\ntop-k 平均覆盖的不同文档数：\n"},
		{"没有结束标记", "按子集分通道成绩：\n总计 0.667\n"},
		{"标记顺序颠倒", "top-k 平均覆盖的不同文档数：\n按子集分通道成绩：\n"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sliceEval(tc.in)
			if err == nil {
				t.Fatalf("应当报错，实际返回了:\n%s", got)
			}
			// 错误信息要指出该改哪里，否则下一个人只能猜
			if !strings.Contains(err.Error(), "cmd/demo") {
				t.Errorf("错误信息应当指明要同步改 cmd/demo 的常量，实际: %v", err)
			}
		})
	}
}

// TestRenderSVGEscapesContent 验证内容里的 XML 元字符被转义。
//
// 检索结果里出现 <、>、& 是常态（本项目自己的文档里就有 `--tags=integration`、
// `<` 比较等）。不转义的话生成的 SVG 是坏的 XML，
// 而浏览器只会给你一张空白图，不报错。
func TestRenderSVGEscapesContent(t *testing.T) {
	svg := RenderSVG("标题 <&>", []Line{
		Plain("a < b && c > d"),
		Query("query=\"x\" & <tag>"),
	}, "footer & more")

	if strings.Contains(svg, "a < b") {
		t.Error("正文里的 < 没有被转义")
	}
	if !strings.Contains(svg, "&lt;") || !strings.Contains(svg, "&amp;") {
		t.Error("应当出现 &lt; 和 &amp;")
	}
	// 转义之后仍然是结构完整的 SVG
	if !strings.HasPrefix(svg, "<svg ") || !strings.HasSuffix(svg, "</svg>\n") {
		t.Error("生成的不是完整的 SVG 文档")
	}
}

// TestRenderSVGPreservesLeadingSpaces 验证行首空格被保留。
//
// 表格靠空格对齐，而 SVG 默认会折叠行首空白。丢了 xml:space="preserve"
// 之后表格会整体左移错位——而且图上看起来"只是没对齐"，不像坏了。
func TestRenderSVGPreservesLeadingSpaces(t *testing.T) {
	svg := RenderSVG("t", []Line{Plain("      缩进六格")}, "")
	if !strings.Contains(svg, `xml:space="preserve"`) {
		t.Error("缺少 xml:space=\"preserve\"，行首空格会被折叠")
	}
	if !strings.Contains(svg, ">      缩进六格<") {
		t.Error("行首空格没有原样进入 text 节点")
	}
}

// TestDisplayWidthCountsCJKAsTwo 验证中文按两列计算。
//
// 画布宽度靠它决定。按 rune 数算会把带中文的行算窄一半，
// 结果是最右边的内容被裁掉。
func TestDisplayWidthCountsCJKAsTwo(t *testing.T) {
	if got := displayWidth("中文"); got != 4 {
		t.Errorf("两个汉字应占 4 列，实际 %d", got)
	}
	if got := displayWidth("ab"); got != 2 {
		t.Errorf("两个拉丁字母应占 2 列，实际 %d", got)
	}
	if got := displayWidth("中a"); got != 3 {
		t.Errorf("一汉字一字母应占 3 列，实际 %d", got)
	}
}

// TestTruncateKeepsCJKWhole 验证截断不会切出半个字符。
func TestTruncateKeepsCJKWhole(t *testing.T) {
	// 每字 2 列，上限 7 列 → 最多放 3 个字（6 列）再加省略号
	got := truncate("一二三四五六", 7)
	if got != "一二三…" {
		t.Errorf("期望 \"一二三…\"，实际 %q", got)
	}

	// 本来就够短就不该被动
	if got := truncate("短", 10); got != "短" {
		t.Errorf("短字符串不该被截断，实际 %q", got)
	}
}
