package chunk

// region 是原文里一段**不应该从中间切开**的区间，左闭右开，单位是 rune。
//
// 为什么需要它：围栏代码块和表格是**结构性**的内容，从中间切开产生的
// 片段两头都不成形——半张架构图、半张表。这些片段语义为空、向量是噪声，
// 却会随机匹配到不相关的查询，占掉一个结果位。
//
// ⚠️ 它保护的是"不要从中间切"，不是"必须整体保留"。
// 块长超过 MaxRunes 时物理上无法整体保留，那时退而求其次：
// 在块内的**行边界**上切。
type region struct{ start, end int }

// regionAt 返回包含 pos 的保护区。pos 落在区间的左端点算"在内"，
// 落在右端点算"在外"——这与半开区间 [start, end) 的语义一致。
func regionAt(regions []region, pos int) (region, bool) {
	for _, r := range regions {
		if r.start <= pos && pos < r.end {
			return r, true
		}
	}
	return region{}, false
}

// findRegions 找出 [from, to) 范围内全部保护区。
//
// 识别两类：
//
//  1. **围栏代码块**（``` 或 ~~~），含它们的开闭标记行
//  2. **表格**：连续 3 行以上以 `|` 开头的行
//
// 为什么表格要 ≥3 行：单独一两行以 `|` 开头更可能是正文里恰好这么排版的
// 一句话或一行命令行，把它们当表格保护起来反而会把正常段落切碎。
func findRegions(runes []rune, from, to int) []region {
	lines := lineSpans(runes, from, to)
	var out []region

	for i := 0; i < len(lines); i++ {
		ls := lines[i]
		if fence, ok := fenceOpen(runes[ls.start:ls.end]); ok {
			// 找闭合行。找不到就一路保护到节尾——
			// 未闭合的围栏在 Markdown 里会把后面全部内容都当作代码，
			// 我们的处理与渲染器保持一致。
			end := to
			j := i + 1
			for ; j < len(lines); j++ {
				if fenceClose(runes[lines[j].start:lines[j].end], fence) {
					end = lines[j].end
					break
				}
			}
			if j >= len(lines) {
				i = len(lines)
			} else {
				i = j
			}
			out = append(out, region{start: ls.start, end: end})
			continue
		}

		if isTableRow(runes[ls.start:ls.end]) {
			j := i
			for j < len(lines) && isTableRow(runes[lines[j].start:lines[j].end]) {
				j++
			}
			if j-i >= minTableRows {
				out = append(out, region{start: ls.start, end: lines[j-1].end})
			}
			i = j - 1
		}
	}
	return out
}

// minTableRows 是"多长的连续竖线行才算表格"。
const minTableRows = 3

// lineSpan 是一行在 rune 数组里的位置，**不含行尾换行符**，左闭右开。
type lineSpan struct{ start, end int }

// lineSpans 把 [from, to) 切成若干行。
//
// 与 strings.Split 的区别：这里保留每行在原文中的位置，
// 而保护区要记的正是位置。
func lineSpans(runes []rune, from, to int) []lineSpan {
	var out []lineSpan
	i := from
	for i < to {
		j := i
		for j < to && runes[j] != '\n' {
			j++
		}
		out = append(out, lineSpan{i, j})
		i = j + 1 // 跳过换行符
	}
	return out
}

// fenceOpen 判断一行是不是围栏代码块的起始行，返回围栏字符。
func fenceOpen(line []rune) (fenceChar byte, ok bool) {
	t := trimLeftSpaceRunes(line)
	if len(t) < 3 {
		return 0, false
	}
	var c byte
	switch t[0] {
	case '`':
		c = '`'
	case '~':
		c = '~'
	default:
		return 0, false
	}
	n := 0
	for n < len(t) && t[n] == rune(c) {
		n++
	}
	if n < 3 {
		return 0, false
	}
	return c, true
}

// fenceClose 判断一行是不是配对的闭合行。
//
// 与 CommonMark 一致：闭合行的围栏字符必须相同、且长度不短于起始行。
// 只认字符和长度，不比对缩进——缩进在代码块里有意义，
// 拿它参与判定会让某些合法的闭合行被漏掉，而漏掉的代价是整个块被吞掉。
func fenceClose(line []rune, fenceChar byte) bool {
	t := trimLeftSpaceRunes(line)
	n := 0
	for n < len(t) && t[n] == rune(fenceChar) {
		n++
	}
	if n < 3 {
		return false
	}
	// 闭合行后面只能有空白
	for _, r := range t[n:] {
		if !isSpace(r) {
			return false
		}
	}
	return true
}

// isTableRow 判断一行是不是表格行：去掉前导空白后以 `|` 开头。
func isTableRow(line []rune) bool {
	t := trimLeftSpaceRunes(line)
	return len(t) > 0 && t[0] == '|'
}

// trimLeftSpaceRunes 去掉左侧空白。
//
// 用 unicode.IsSpace（经 isSpace 包装）而不是手写 ASCII 列表：
// 中文全角空格 U+3000 在 Markdown 里很常见，只认 ASCII 会漏掉
// 那些用全角空格缩进的代码块。
func trimLeftSpaceRunes(line []rune) []rune {
	i := 0
	for i < len(line) && isSpace(line[i]) {
		i++
	}
	return line[i:]
}

// regionStartingAt 判断 pos 是否正好是某个保护区的起点。
//
// 与 regionAt 的区别：只认左端点，不认"落在区间内部"。
// 「切在块首」和「切在块中间」是两回事，前者是我们要的，后者是要避免的。
func regionStartingAt(regions []region, pos int) (region, bool) {
	for _, r := range regions {
		if r.start == pos {
			return r, true
		}
	}
	return region{}, false
}
