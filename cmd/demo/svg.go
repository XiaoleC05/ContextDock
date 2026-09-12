package main

import (
	"fmt"
	"html"
	"strings"
)

// 终端样式的配色。刻意跟 README 的深色观感对齐，不跟随系统主题——
// 一张固定配色的图在 GitHub 的明暗两种主题下都可读，而跟随主题的
// SVG 要在 README 里做两套，收益不值这个复杂度。
const (
	colorBG      = "#0d1117" // 窗口背景
	colorChrome  = "#161b22" // 标题栏
	colorBorder  = "#30363d"
	colorText    = "#c9d1d9"
	colorDim     = "#8b949e" // 次要信息（说明行、计数）
	colorPrompt  = "#3fb950" // 提示符
	colorAccent  = "#58a6ff" // 查询、命令
	colorEmph    = "#d29922" // 反直觉的结论
	colorSuccess = "#3fb950"
)

// 排版常量。charWidth 取 0.6em 是等宽字体的常见比例；
// 只影响画布尺寸和留白，不影响对齐——对齐由终端那边用 displayWidth
// 补好的空格保证，这里原样保留。
const (
	fontSize   = 13.0
	lineHeight = 20.0
	charWidth  = fontSize * 0.6
	padX       = 18.0
	padTop     = 14.0
	chromeH    = 34.0
	padBottom  = 16.0
)

// Style 是一行的显示样式。
type Style int

const (
	StylePlain Style = iota
	StyleDim
	StylePrompt // 提示符行：$ 开头的命令
	StyleQuery  // 查询行
	StyleEmph   // 需要被注意到的结论
	StyleOK
)

func (s Style) color() string {
	switch s {
	case StyleDim:
		return colorDim
	case StylePrompt:
		return colorPrompt
	case StyleQuery:
		return colorAccent
	case StyleEmph:
		return colorEmph
	case StyleOK:
		return colorSuccess
	default:
		return colorText
	}
}

// Line 是终端里的一行。
type Line struct {
	Text  string
	Style Style

	// Bold 用于表头这类需要突出的行。不用 <strong>——
	// SVG 里直接切 font-weight 更省事，也不需要额外的字体声明。
	Bold bool
}

// Plain 造一个普通行。
func Plain(s string) Line { return Line{Text: s} }

// Dim 造一个次要信息行。
func Dim(s string) Line { return Line{Text: s, Style: StyleDim} }

// Prompt 造一个命令提示符行。
func Prompt(s string) Line { return Line{Text: s, Style: StylePrompt} }

// Query 造一个查询行。
func Query(s string) Line { return Line{Text: s, Style: StyleQuery} }

// Emph 造一个需要突出的结论行。
func Emph(s string) Line { return Line{Text: s, Style: StyleEmph} }

// OK 造一个成功行。
func OK(s string) Line { return Line{Text: s, Style: StyleOK} }

// Title 是终端窗口的标题栏文字。
//
// RenderSVG 把若干行渲染成一张终端窗口样式的 SVG。
//
// 它**只负责画框**：行内容原样输出，不做换行、不做截断、不重排。
// 这样素材里的每一个字符都来自真实命令的输出，渲染器不可能把
// 数字"美化"成别的东西——这是这张图能被当作证据的前提。
func RenderSVG(title string, lines []Line, footer string) string {
	// 画布宽度由最宽的一行决定。用 displayWidth 而不是 len()：
	// 中文占两列，按字节或按 rune 数算都会把画布裁窄。
	cols := 0
	for _, l := range lines {
		if w := displayWidth(l.Text); w > cols {
			cols = w
		}
	}
	if w := displayWidth(title); w > cols {
		cols = w
	}
	if w := displayWidth(footer); w > cols {
		cols = w
	}
	// 上限：再宽就该在源码里折行了，而不是把画布撑到没人能看清
	if cols > 118 {
		cols = 118
	}

	width := float64(cols)*charWidth + padX*2
	bodyH := float64(len(lines)) * lineHeight
	footerH := 0.0
	if footer != "" {
		footerH = lineHeight * 1.4
	}
	height := chromeH + padTop + bodyH + footerH + padBottom

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f" role="img" aria-label="ContextDock 演示">`,
		width, height, width, height)
	fmt.Fprintf(&b, "\n<title>%s</title>\n", html.EscapeString(title))

	// 窗口
	fmt.Fprintf(&b, `<rect x="0" y="0" width="%.0f" height="%.0f" rx="10" fill="%s" stroke="%s"/>`+"\n",
		width, height, colorBG, colorBorder)
	fmt.Fprintf(&b, `<path d="M0 10a10 10 0 0 1 10-10h%.0f a10 10 0 0 1 10 10v%.0f H0Z" fill="%s"/>`+"\n",
		width-20, chromeH-10, colorChrome)
	fmt.Fprintf(&b, `<line x1="0" y1="%.0f" x2="%.0f" y2="%.0f" stroke="%s"/>`+"\n", chromeH, width, chromeH, colorBorder)

	// 三个装饰圆点
	for i, c := range []string{"#ff5f56", "#ffbd2e", "#27c93f"} {
		fmt.Fprintf(&b, `<circle cx="%.0f" cy="%.0f" r="5.5" fill="%s"/>`+"\n",
			padX+float64(i)*18, chromeH/2, c)
	}

	// 标题带 —— 用等宽字体，长度用 displayWidth 估，居中对齐到圆点右侧
	fmt.Fprintf(&b, `<text x="%.1f" y="%.0f" font-family="%s" font-size="12" fill="%s" text-anchor="middle">%s</text>`+"\n",
		width/2, chromeH/2+4, fontStack(), colorDim, html.EscapeString(title))

	// 正文
	fmt.Fprintf(&b, `<g font-family="%s" font-size="%.0f">`+"\n", fontStack(), fontSize)
	for i, l := range lines {
		y := chromeH + padTop + float64(i)*lineHeight + fontSize
		weight := ""
		if l.Bold {
			weight = ` font-weight="600"`
		}
		// xml:space="preserve" 是必须的：没有它，渲染器会把行首的
		// 对齐空格吃掉，表格立刻错位。
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="%s"%s xml:space="preserve">%s</text>`+"\n",
			padX, y, l.Style.color(), weight, html.EscapeString(l.Text))
	}
	b.WriteString("</g>\n")

	if footer != "" {
		y := chromeH + padTop + bodyH + lineHeight*1.1
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" font-family="%s" font-size="11" fill="%s">%s</text>`+"\n",
			padX, y, fontStack(), colorDim, html.EscapeString(footer))
	}

	b.WriteString("</svg>\n")
	return b.String()
}

// fontStack 是等宽字体栈。
//
// 中文会回退到系统字体，而**回退字体的字宽不保证正好是拉丁字符的两倍**——
// 所以表格的对齐不能指望字体，得靠终端那边用 displayWidth 补好的空格。
// 这个前提在 cmd/eval 的 WriteTable 里成立（见 internal/eval/report.go）。
func fontStack() string {
	return "ui-monospace, SFMono-Regular, Menlo, Consolas, 'Cascadia Mono', 'Liberation Mono', monospace"
}

// displayWidth 估算字符串在等宽终端里占几列。
//
// 与 internal/eval/report.go 里的同名函数**刻意重复**：那个是包内私有，
// 而这里只需要同一套"中文占 2 列"的估算，为它导出一个 API 不划算。
// 两边都只服务于"让中文列对齐"这一个目的。
func displayWidth(s string) int {
	n := 0
	for _, r := range s {
		if isWide(r) {
			n += 2
		} else {
			n++
		}
	}
	return n
}

func isWide(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F,
		r >= 0x2E80 && r <= 0xA4CF,
		r >= 0xAC00 && r <= 0xD7A3,
		r >= 0xF900 && r <= 0xFAFF,
		r >= 0xFE30 && r <= 0xFE6F,
		r >= 0xFF00 && r <= 0xFF60,
		r >= 0xFFE0 && r <= 0xFFE6:
		return true
	}
	return false
}
