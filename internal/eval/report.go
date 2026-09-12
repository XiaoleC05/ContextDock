package eval

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode"
)

// WriteJSON 输出可机读的报告。
//
// 缩进而不是紧凑格式：这份文件是给人看的也是给工具看的，
// 出问题时人得能直接打开对比，省掉一次 `jq .` 的往返。
func WriteJSON(w io.Writer, rep *Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	// 不转义非 ASCII：报表里全是中文，转义成 \uXXXX 之后没人读得下去。
	enc.SetEscapeHTML(false)
	return enc.Encode(rep)
}

// WriteTable 输出人读的表格。
func WriteTable(w io.Writer, rep *Report) error {
	o := rep.Options
	fmt.Fprintf(w, "评测参数：切分 %d/%d，RRF k=%d，候选倍数 %d，topK=%d，NDCG@%d\n",
		o.MaxRunes, o.Overlap, o.RRFK, o.Mult, o.TopK, o.NDCGK)

	parts := make([]string, 0, len(rep.Corpus))
	for _, c := range rep.Corpus {
		parts = append(parts, fmt.Sprintf("%s %d", c.Source, c.Chunks))
	}
	fmt.Fprintf(w, "语料：%d 份，共 %d 个片段（%s）\n",
		len(rep.Corpus), rep.TotalChunks, strings.Join(parts, " / "))
	// 索引体积与构建耗时放在这里，是因为切分扫描必须同时看它们：
	// 「切得越碎 recall 越高」几乎总是成立，只看 recall 会选出病态参数。
	fmt.Fprintf(w, "索引：正文 %s + 向量 %s = %s；导入并建索引耗时 %dms\n\n",
		humanBytes(rep.TextBytes), humanBytes(rep.VectorBytes),
		humanBytes(rep.TextBytes+rep.VectorBytes), rep.ImportMs)

	// ---- 按子集 ----
	fmt.Fprintln(w, "按子集分通道成绩：")
	writeGroupTable(w, rep)
	fmt.Fprintln(w)

	// ---- 融合 vs 单路 ----
	writeGainTable(w, rep)
	fmt.Fprintln(w)

	// ---- 精确 token 组：词法必须命中 ----
	writeLexicalRequired(w, rep)

	if rep.Embed.Hits+rep.Embed.Misses > 0 {
		n := rep.Embed.Hits + rep.Embed.Misses
		fmt.Fprintf(w, "嵌入缓存：命中 %d / %d（%.1f%%）\n",
			rep.Embed.Hits, n, float64(rep.Embed.Hits)/float64(n)*100)
	}
	return nil
}

func writeGroupTable(w io.Writer, rep *Report) {
	head := []string{"组", "条数", "词法 recall", "向量 recall", "融合 recall", "融合 MRR", "融合 NDCG"}
	rows := [][]string{}
	for _, g := range rep.Groups {
		rows = append(rows, []string{
			string(g.Group),
			fmt.Sprintf("%d", g.N),
			pct(g.ByChannel[ChannelLexical].Recall),
			pct(g.ByChannel[ChannelVector].Recall),
			pct(g.ByChannel[ChannelFused].Recall),
			pct(g.ByChannel[ChannelFused].MRR),
			pct(g.ByChannel[ChannelFused].NDCG),
		})
	}
	rows = append(rows, []string{
		"总计",
		fmt.Sprintf("%d", rep.Overall[ChannelFused].Queries),
		pct(rep.Overall[ChannelLexical].Recall),
		pct(rep.Overall[ChannelVector].Recall),
		pct(rep.Overall[ChannelFused].Recall),
		pct(rep.Overall[ChannelFused].MRR),
		pct(rep.Overall[ChannelFused].NDCG),
	})
	writeAligned(w, head, rows, 1)
	fmt.Fprintln(w, "  说明：recall 按**期望条目**加权（多跳查询只命中一半记 0.5），MRR / NDCG 按查询平均。")
}

func writeGainTable(w io.Writer, rep *Report) {
	fmt.Fprintln(w, "融合相比单路最优者的增益（recall）：")

	head := []string{"组", "融合", "单路最优", "增益"}
	rows := [][]string{}
	add := func(name string, fused, lex, vec Aggregate) {
		best := lex.Recall
		which := "词法"
		if vec.Recall > best {
			best, which = vec.Recall, "向量"
		}
		rows = append(rows, []string{
			name, pct(fused.Recall), fmt.Sprintf("%s %s", which, pct(best)),
			signed(fused.Recall - best),
		})
	}
	for _, g := range rep.Groups {
		add(string(g.Group),
			g.ByChannel[ChannelFused], g.ByChannel[ChannelLexical], g.ByChannel[ChannelVector])
	}
	add("总计",
		rep.Overall[ChannelFused], rep.Overall[ChannelLexical], rep.Overall[ChannelVector])
	writeAligned(w, head, rows, 3)
}

// writeLexicalRequired 单独报告「必须由词法命中」的那批查询。
//
// #35 要求评测报告能单独拉出这组的词法 / 向量对比。这些查询里是
// 错误码、版本号、函数名这类不能模糊匹配的 token——「靠向量蒙对」
// 不算通过，那恰恰说明向量通道没有在区分近似 token。
func writeLexicalRequired(w io.Writer, rep *Report) {
	var total, lexHit, onlyVector int
	for _, d := range rep.Detail {
		if !d.RequireLexical {
			continue
		}
		total++
		lexOK := d.ByChannel[ChannelLexical].Recall >= 1
		vecOK := d.ByChannel[ChannelVector].Recall >= 1
		switch {
		case lexOK:
			lexHit++
		case vecOK:
			onlyVector++
		}
	}
	if total == 0 {
		return
	}
	fmt.Fprintf(w, "精确 token 组（要求词法命中）：%d 条，词法实际命中 %d 条（%.1f%%）\n",
		total, lexHit, float64(lexHit)/float64(total)*100)
	if onlyVector > 0 {
		fmt.Fprintf(w, "  ⚠️ 其中 %d 条**词法没中、向量中了**——需要人工确认："+
			"向量很可能命中了语义相近但 token 不同的片段，那正是这组要暴露的问题。\n", onlyVector)
	}
}

// ---- 排版 ----

func pct(v float64) string { return fmt.Sprintf("%.3f", v) }

// humanBytes 把字节数写成方便读的形式。
//
// 用 1024 进制而不是 1000：这里的数字是给人估量级用的
// （"这参数会不会让索引爆炸"），而不是要拿去做精确计算。
func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func signed(v float64) string { return fmt.Sprintf("%+.3f", v) }

// writeAligned 打印一个按显示宽度对齐的表格。
//
// ⚠️ 不能用 %-8s 直接对齐：那是按**字节**还是按 rune 补空格取决于实现，
// 而中文在任何一种下都不是等宽的——表头「融合 recall」和值「0.933」
// 的显示宽度完全不同，列会歪。这里显式算显示宽度：
// CJK 与全角字符占 2 列，其余占 1 列。
func writeAligned(w io.Writer, head []string, rows [][]string, rightFrom int) {
	widths := make([]int, len(head))
	for i, h := range head {
		widths[i] = displayWidth(h)
	}
	for _, r := range rows {
		for i, c := range r {
			if i < len(widths) && displayWidth(c) > widths[i] {
				widths[i] = displayWidth(c)
			}
		}
	}

	line := func(cells []string) {
		var b strings.Builder
		for i, c := range cells {
			pad := widths[i] - displayWidth(c)
			if pad < 0 {
				pad = 0
			}
			if i >= rightFrom {
				b.WriteString(strings.Repeat(" ", pad))
				b.WriteString(c)
			} else {
				b.WriteString(c)
				b.WriteString(strings.Repeat(" ", pad))
			}
			if i < len(cells)-1 {
				b.WriteString("  ")
			}
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
	line(head)
	sep := make([]string, len(head))
	for i := range sep {
		sep[i] = strings.Repeat("-", widths[i])
	}
	line(sep)
	for _, r := range rows {
		line(r)
	}
}

// displayWidth 估算字符串在等宽终端里占几列。
//
// 只需覆盖本项目会出现的字符：中文（东亚宽字符）占 2 列，其余占 1 列。
// 不追求完整的 UAX#11 实现——那样要带一张几百行的区间表，
// 而这里只要让报告的中文列对齐就够了。
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
	case r >= 0x1100 && r <= 0x115F, // 韩文字母
		r >= 0x2E80 && r <= 0xA4CF, // CJK 部首、假名、汉字
		r >= 0xAC00 && r <= 0xD7A3, // 韩文音节
		r >= 0xF900 && r <= 0xFAFF, // CJK 兼容汉字
		r >= 0xFE30 && r <= 0xFE6F, // CJK 兼容形式
		r >= 0xFF00 && r <= 0xFF60, // 全角
		r >= 0xFFE0 && r <= 0xFFE6:
		return true
	}
	return unicode.Is(unicode.Han, r)
}
