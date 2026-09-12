package chunk

import (
	"errors"
	"strings"
	"testing"

	"github.com/XiaoleC05/ContextDock/internal/types"
)

// mustSplit 是测试辅助：切分失败直接 Fatal。
func mustSplit(t *testing.T, c *Chunker, docID int64, content string) []types.Chunk {
	t.Helper()
	got, err := c.Split(docID, content)
	if err != nil {
		t.Fatalf("Split 失败: %v", err)
	}
	return got
}

func newChunker(t *testing.T, cfg Config) *Chunker {
	t.Helper()
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return c
}

// TestContentMatchesOffsets 是全套测试里最重要的一个。
//
// 它守护的不变量是：Content == []rune(原文)[StartOffset:EndOffset]
//
// 一旦这条不成立，偏移就失去意义，而偏移丢失后「重新切分对齐」
// 和「上下文扩展」都会错位。而且偏移错位**不会报错**，只会让结果慢慢变怪。
func TestContentMatchesOffsets(t *testing.T) {
	c := newChunker(t, Config{MaxRunes: 50, OverlapRunes: 10})

	contents := []string{
		"这是一段中文内容。它包含多个句子。用来测试偏移是否正确。",
		"Mixed 中英 content with punctuation. 还有中文。And more English.",
		"没有任何标点的超长文本" + strings.Repeat("啊", 200),
		"短",
		"# 标题\n\n正文内容在这里。\n\n## 二级标题\n\n更多正文。",
		strings.Repeat("行\n", 100),
	}

	for i, content := range contents {
		runes := []rune(content)
		chunks := mustSplit(t, c, 1, content)
		for _, ch := range chunks {
			if ch.EndOffset > len(runes) {
				t.Errorf("用例 %d: EndOffset %d 超出原文长度 %d", i, ch.EndOffset, len(runes))
				continue
			}
			want := string(runes[ch.StartOffset:ch.EndOffset])
			if ch.Content != want {
				t.Errorf("用例 %d 片段 #%d: Content 与偏移不匹配\n  偏移 [%d,%d)\n  期望 %q\n  实际 %q",
					i, ch.Ordinal, ch.StartOffset, ch.EndOffset, want, ch.Content)
			}
		}
	}
}

// TestCoversAllBodyText 验证切分没有丢内容。
//
// 标题行不计入正文（它们只贡献面包屑），所以校验时排除掉标题行上的字符。
func TestCoversAllBodyText(t *testing.T) {
	c := newChunker(t, Config{MaxRunes: 60, OverlapRunes: 15})

	content := `# 安装指南

ContextDock 需要 Go 1.25 以上版本。请先安装 Go。

## 快速开始

运行 go build 即可编译。然后执行 go test 跑测试。

### 注意事项

数据库需要 pgvector 0.8.6 以上。否则 HNSW 索引建不起来。`

	runes := []rune(content)
	covered := make([]bool, len(runes))
	for _, ch := range mustSplit(t, c, 1, content) {
		for i := ch.StartOffset; i < ch.EndOffset; i++ {
			covered[i] = true
		}
	}

	// 标出标题行所占的字符（测试自己解析一遍，作为独立校验）
	inHeading := make([]bool, len(runes))
	lineStart := 0
	for i := 0; i <= len(runes); i++ {
		if i == len(runes) || runes[i] == '\n' {
			line := runes[lineStart:i]
			if _, _, ok := parseHeading(line); ok {
				for k := lineStart; k < i; k++ {
					inHeading[k] = true
				}
			}
			lineStart = i + 1
		}
	}

	for i, r := range runes {
		if inHeading[i] || isSpace(r) {
			continue
		}
		if !covered[i] {
			t.Errorf("第 %d 个字符 %q 没有被任何片段覆盖（丢内容了）", i, r)
		}
	}
}

func TestOrdinalIsSequentialFromZero(t *testing.T) {
	c := newChunker(t, Config{MaxRunes: 30, OverlapRunes: 5})
	chunks := mustSplit(t, c, 7, strings.Repeat("这是一些用来切分的中文内容。", 20))

	if len(chunks) < 3 {
		t.Fatalf("用例设计有误，应该切出多段，实际 %d 段", len(chunks))
	}
	for i, ch := range chunks {
		if ch.Ordinal != i {
			t.Errorf("第 %d 段的 Ordinal 应为 %d，实际 %d", i, i, ch.Ordinal)
		}
		if ch.DocumentID != 7 {
			t.Errorf("第 %d 段的 DocumentID 应为 7，实际 %d", i, ch.DocumentID)
		}
	}
}

func TestMaxRunesRespected(t *testing.T) {
	const maxRunes = 40
	c := newChunker(t, Config{MaxRunes: maxRunes, OverlapRunes: 8})

	// 无标点的长文本，强制走"硬切"路径
	chunks := mustSplit(t, c, 1, strings.Repeat("中", 500))
	for _, ch := range chunks {
		n := len([]rune(ch.Content))
		if n > maxRunes {
			t.Errorf("片段 #%d 长度 %d 超过上限 %d", ch.Ordinal, n, maxRunes)
		}
	}

	// 有标点的文本也不能超限
	chunks = mustSplit(t, c, 1, strings.Repeat("这是一个句子。", 50))
	for _, ch := range chunks {
		if n := len([]rune(ch.Content)); n > maxRunes {
			t.Errorf("带标点：片段 #%d 长度 %d 超过上限 %d", ch.Ordinal, n, maxRunes)
		}
	}
}

// TestOverlapBetweenAdjacentChunks 验证相邻片段确实有重叠。
//
// 没有重叠的话，答案正好落在接缝处就会被两个片段各切一半，
// 两边都不含完整信息。
func TestOverlapBetweenAdjacentChunks(t *testing.T) {
	c := newChunker(t, Config{MaxRunes: 100, OverlapRunes: 25})
	chunks := mustSplit(t, c, 1, strings.Repeat("这是用来测试重叠的中文句子。", 30))

	if len(chunks) < 3 {
		t.Fatalf("用例设计有误，应该切出多段，实际 %d 段", len(chunks))
	}
	for i := 0; i+1 < len(chunks); i++ {
		a, b := chunks[i], chunks[i+1]
		if b.StartOffset <= a.StartOffset {
			t.Errorf("片段 #%d 起点(%d) 没有前进，可能死循环", i+1, b.StartOffset)
		}
		overlap := a.EndOffset - b.StartOffset
		if overlap <= 0 {
			t.Errorf("片段 #%d 与 #%d 之间没有重叠：前者结束于 %d，后者开始于 %d",
				i, i+1, a.EndOffset, b.StartOffset)
		}
	}
}

func TestMarkdownBreadcrumb(t *testing.T) {
	c := newChunker(t, DefaultConfig())

	content := `# 安装指南

第一段正文。

## 快速开始

运行 go build。

### 注意事项

需要 pgvector。
`

	chunks := mustSplit(t, c, 1, content)

	// 收集每段对应的面包屑
	var got []string
	for _, ch := range chunks {
		got = append(got, ch.Metadata[types.MetadataKeyHeading])
	}

	want := []string{"安装指南", "安装指南 > 快速开始", "安装指南 > 快速开始 > 注意事项"}
	if len(got) != len(want) {
		t.Fatalf("片段数应为 %d，实际 %d：%v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 段面包屑:\n  期望 %q\n  实际 %q", i, want[i], got[i])
		}
	}
}

// TestBreadcrumbDropsStaleDeeperLevels 验证同级标题会清掉更深层级。
func TestBreadcrumbDropsStaleDeeperLevels(t *testing.T) {
	c := newChunker(t, DefaultConfig())
	content := `# A

## B

### C

正文一。

## D

正文二。
`
	chunks := mustSplit(t, c, 1, content)
	var got []string
	for _, ch := range chunks {
		got = append(got, ch.Metadata[types.MetadataKeyHeading])
	}
	// 第二段属于 ## D，不应该还带着 ### C
	last := got[len(got)-1]
	if last != "A > D" {
		t.Errorf("同级标题之后应丢掉更深层级，期望 %q，实际 %q", "A > D", last)
	}
}

func TestHeadingIsNotInContent(t *testing.T) {
	c := newChunker(t, DefaultConfig())
	content := "# 标题\n\n正文内容。\n"
	chunks := mustSplit(t, c, 1, content)

	for _, ch := range chunks {
		if strings.Contains(ch.Content, "# 标题") {
			t.Errorf("标题行不应出现在正文里（它只在 Metadata 中）: %q", ch.Content)
		}
	}
}

// TestHashWithoutSpaceIsNotHeading 验证 #hashtag 不会被误判成标题。
func TestHashWithoutSpaceIsNotHeading(t *testing.T) {
	c := newChunker(t, DefaultConfig())
	content := "#hashtag 这不是标题，是正文。"
	chunks := mustSplit(t, c, 1, content)

	if len(chunks) != 1 {
		t.Fatalf("应只切出 1 段，实际 %d 段", len(chunks))
	}
	if !strings.Contains(chunks[0].Content, "#hashtag") {
		t.Errorf("#hashtag 应保留在正文里，实际 %q", chunks[0].Content)
	}
	if chunks[0].Metadata[types.MetadataKeyHeading] != "" {
		t.Errorf("#hashtag 不应产生面包屑，实际 %q", chunks[0].Metadata[types.MetadataKeyHeading])
	}
}

// TestAllChunksValidate 验证切分产出能通过类型层的校验。
func TestAllChunksValidate(t *testing.T) {
	c := newChunker(t, Config{MaxRunes: 50, OverlapRunes: 10})
	for _, content := range []string{
		"普通中文内容。",
		"# 标题\n\n正文。",
		"English content here.",
		"中英 mixed 混合内容，测试 validate。",
	} {
		for _, ch := range mustSplit(t, c, 3, content) {
			if err := ch.Validate(); err != nil {
				t.Errorf("内容 %q 切出的片段未通过校验: %v", content, err)
			}
		}
	}
}

// TestNoEmptyContentChunks 保证不会切出空片段。
func TestNoEmptyContentChunks(t *testing.T) {
	c := newChunker(t, Config{MaxRunes: 20, OverlapRunes: 5})
	content := strings.Repeat("   \n\n   文字   \n\n   ", 20)
	for _, ch := range mustSplit(t, c, 1, content) {
		if strings.TrimSpace(ch.Content) == "" {
			t.Errorf("切出了空白片段: %q (偏移 [%d,%d))", ch.Content, ch.StartOffset, ch.EndOffset)
		}
	}
}

func TestSplitErrors(t *testing.T) {
	c := newChunker(t, DefaultConfig())

	t.Run("DocumentID 为 0", func(t *testing.T) {
		if _, err := c.Split(0, "有内容"); !errors.Is(err, ErrNoDocumentID) {
			t.Errorf("期望 ErrNoDocumentID，实际 %v", err)
		}
	})
	t.Run("空内容", func(t *testing.T) {
		if _, err := c.Split(1, ""); !errors.Is(err, ErrEmptyDocument) {
			t.Errorf("期望 ErrEmptyDocument，实际 %v", err)
		}
	})
	t.Run("纯空白", func(t *testing.T) {
		if _, err := c.Split(1, "  \n\t  "); !errors.Is(err, ErrEmptyDocument) {
			t.Errorf("期望 ErrEmptyDocument，实际 %v", err)
		}
	})
	t.Run("只有标题没有正文", func(t *testing.T) {
		if _, err := c.Split(1, "# 标题\n## 又一个标题\n"); !errors.Is(err, ErrEmptyDocument) {
			t.Errorf("期望 ErrEmptyDocument，实际 %v", err)
		}
	})
}

func TestNewRejectsBadConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		// ⚠️ 「OverlapRunes 为负」这条**被移除了**：负数是「未设置」的哨兵，
		// 会回落成默认值，不再是一个错误。见 TestZeroConfigSemantics。
		//
		// 边界必须留着：overlap 等于 maxRunes 时起点不前进，会死循环。
		{"OverlapRunes 等于 MaxRunes", Config{MaxRunes: 100, OverlapRunes: 100}},
		{"OverlapRunes 大于 MaxRunes", Config{MaxRunes: 100, OverlapRunes: 101}},
		{"MaxRunes 为负", Config{MaxRunes: -5}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.cfg); !errors.Is(err, ErrBadConfig) {
				t.Errorf("期望 ErrBadConfig，实际 %v", err)
			}
		})
	}
}

// TestZeroConfigSemantics 记录零值 Config 的语义。
//
// ⚠️ 这条测试**改过**，原来断言的是 `Config{} == DefaultConfig()`。
//
// 那个约定逼着 normalize 拿 0 当「未设置」的哨兵，代价是
// `OverlapRunes: 0`（完全不重叠——一个合法且有用的配置）
// 会被静默改成 60，**用户没有办法关掉重叠**。
//
// 违反的正是本项目自己写在 types.Chunk.Ordinal 上的规则：
// 0 是合法值，就不能拿它当"未设置"的哨兵。
//
// 新语义：MaxRunes 用 0 表示未设置（0 本来就不合法），
// OverlapRunes 用负数表示未设置，0 按字面解释。
func TestZeroConfigSemantics(t *testing.T) {
	c := newChunker(t, Config{})
	if c.cfg.MaxRunes != DefaultMaxRunes {
		t.Errorf("MaxRunes 应回落成默认值 %d，实际 %d", DefaultMaxRunes, c.cfg.MaxRunes)
	}
	if c.cfg.OverlapRunes != 0 {
		t.Errorf("OverlapRunes 应保持 0（不重叠），实际 %d", c.cfg.OverlapRunes)
	}

	// 想要默认重叠，得显式取 DefaultConfig()。
	if d := newChunker(t, DefaultConfig()); d.cfg.OverlapRunes != DefaultOverlapRunes {
		t.Errorf("DefaultConfig 的 OverlapRunes 应为 %d，实际 %d",
			DefaultOverlapRunes, d.cfg.OverlapRunes)
	}

	// 负数才是「未设置」。
	if n := newChunker(t, Config{MaxRunes: 100, OverlapRunes: -1}); n.cfg.OverlapRunes != DefaultOverlapRunes {
		t.Errorf("负的 OverlapRunes 应回落成默认值 %d，实际 %d",
			DefaultOverlapRunes, n.cfg.OverlapRunes)
	}
}

// TestOverlapZeroDisablesOverlap 守护「重叠可以真的设为 0」。
//
// 没有这条测试的话，normalize 里把 0 当哨兵的老行为回来了也不会有人发现——
// 而它返回来之后，参数扫描里 Overlap=0 那一列会悄悄变成「Overlap=60」，
// 报告照样打印，数字看着也正常。
func TestOverlapZeroDisablesOverlap(t *testing.T) {
	content := strings.Repeat("零重叠测试内容。", 60)

	c := newChunker(t, Config{MaxRunes: 100, OverlapRunes: 0})
	chunks := mustSplit(t, c, 1, content)

	if len(chunks) < 2 {
		t.Fatalf("应当切出多个片段，实际 %d 个", len(chunks))
	}
	for i := 1; i < len(chunks); i++ {
		if chunks[i].StartOffset < chunks[i-1].EndOffset {
			t.Errorf("第 %d 个片段与上一个重叠了：[%d,%d) 与 [%d,%d)——"+
				"OverlapRunes=0 没有被遵守",
				i, chunks[i-1].StartOffset, chunks[i-1].EndOffset,
				chunks[i].StartOffset, chunks[i].EndOffset)
		}
	}
}

// TestChunkerIsConcurrencySafe 守护「无状态可并发」的声明。
// 需要用 go test -race 运行才有意义。
func TestChunkerIsConcurrencySafe(t *testing.T) {
	c := newChunker(t, Config{MaxRunes: 80, OverlapRunes: 20})
	content := strings.Repeat("并发切分测试内容。", 40)
	want := mustSplit(t, c, 1, content)

	done := make(chan int, 20)
	for i := 0; i < 20; i++ {
		go func() { done <- len(mustSplitNoT(t, c, 1, content)) }()
	}
	for i := 0; i < 20; i++ {
		if n := <-done; n != len(want) {
			t.Errorf("并发切分结果数不一致: 期望 %d，实际 %d", len(want), n)
		}
	}
}

// mustSplitNoT 是给 goroutine 用的版本（t.Fatal 不能在非测试 goroutine 里调）。
func mustSplitNoT(t *testing.T, c *Chunker, docID int64, content string) []types.Chunk {
	got, err := c.Split(docID, content)
	if err != nil {
		return nil
	}
	return got
}

// TestBreakAtSentenceBoundary 验证片段尽量在句末标点处断开。
//
// 不在句子边界断开的话，一个完整的句子会被劈成两半，两个片段都变得
// 语义不完整——检索质量会下降，但**不会报错**，只有人工看结果才发现。
func TestBreakAtSentenceBoundary(t *testing.T) {
	// 每句 5 个字符（4 字 + 句号），MaxRunes 取 27 让它落在句子中间，
	// 这样"是否在标点处回退"才有可观测的差别。
	c := newChunker(t, Config{MaxRunes: 27, OverlapRunes: 5})
	content := strings.Repeat("第一句话。", 12)

	runes := []rune(content)
	chunks := mustSplit(t, c, 1, content)
	if len(chunks) < 3 {
		t.Fatalf("用例设计有误，应该切出多段，实际 %d 段", len(chunks))
	}

	// 除最后一段外（它是收尾，可能停在句子中间），每段都应以句末标点结尾。
	for i := 0; i < len(chunks)-1; i++ {
		r := runes[chunks[i].EndOffset-1]
		if !isSentenceEnd(r) {
			t.Errorf("片段 #%d 没有在句末标点断开，末尾字符是 %q\n  内容: %q",
				i, r, chunks[i].Content)
		}
	}

	// 而且必须确实发生了"回退"——否则这条测试等于没测。
	// 27 不是 5 的倍数，硬切的话第一段长度会是 27。
	if n := len([]rune(chunks[0].Content)); n%5 != 0 {
		t.Errorf("第一段长度 %d 不是句子长度的整数倍，说明没有在标点处断开", n)
	}
}

// TestUnicodeWhitespaceOnlyDocument 守护 isSpace 用 unicode.IsSpace 而非 ASCII 列表。
//
// 中文文档里全角空格（U+3000）很常见。如果 isSpace 只认 ASCII，
// 一个只含全角空格的文档会切出一个"看似有内容"的片段，
// 一路通过 Validate() 进数据库，然后永远召不回。
func TestUnicodeWhitespaceOnlyDocument(t *testing.T) {
	c := newChunker(t, DefaultConfig())

	blanks := map[string]string{
		"全角空格":    "　　　",
		"不换行空格":   "  ",
		"全角空格混换行": "　\n　",
		"制表与全角":   "\t　\t",
	}
	for name, content := range blanks {
		t.Run(name, func(t *testing.T) {
			got, err := c.Split(1, content)
			if !errors.Is(err, ErrEmptyDocument) {
				t.Errorf("只含空白字符的文档应当报 ErrEmptyDocument，实际 err=%v chunks=%v",
					err, got)
			}
		})
	}
}

// TestUnicodeWhitespaceIsTrimmedFromChunks 验证全角空白不会出现在片段两端。
func TestUnicodeWhitespaceIsTrimmedFromChunks(t *testing.T) {
	c := newChunker(t, DefaultConfig())
	content := "　　正文内容在这里。　　"
	chunks := mustSplit(t, c, 1, content)

	for _, ch := range chunks {
		r := []rune(ch.Content)
		if len(r) > 0 && isSpace(r[0]) {
			t.Errorf("片段开头有空白: %q", ch.Content)
		}
		if len(r) > 1 && isSpace(r[len(r)-1]) {
			t.Errorf("片段结尾有空白: %q", ch.Content)
		}
	}
}

func BenchmarkSplitChinese(b *testing.B) {
	c, _ := New(DefaultConfig())
	content := strings.Repeat("这是一个用于性能测试的中文句子，包含标点符号。", 100)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.Split(1, content); err != nil {
			b.Fatal(err)
		}
	}
}

// ---- 保护区：围栏代码块与表格 ----

// contentOf 把片段还原成原文，便于断言"整块有没有被保住"。
func contentOf(t *testing.T, content string, c types.Chunk) string {
	t.Helper()
	r := []rune(content)
	if c.EndOffset > len(r) {
		t.Fatalf("片段偏移越界：[%d,%d)，原文长 %d", c.StartOffset, c.EndOffset, len(r))
	}
	return string(r[c.StartOffset:c.EndOffset])
}

// TestFencedBlockStaysWholeWhenItFits 守护「放得下的围栏块必须整体保留」。
//
// 这是 #47 的核心：一张架构图被切成几片之后，每一片都不成形，
// 语义为空、向量是噪声，却会随机匹配到不相关的查询、占掉一个结果位。
func TestFencedBlockStaysWholeWhenItFits(t *testing.T) {
	block := "```text\n" +
		"┌──────────────┐\n" +
		"│  ContextDock │\n" +
		"└──────────────┘\n" +
		"```\n"
	content := "前置说明文字，用来把围栏块推到片段中部。\n\n" + block + "\n后置说明文字。\n"

	c := newChunker(t, Config{MaxRunes: 120, OverlapRunes: 20})
	chunks := mustSplit(t, c, 1, content)

	var found bool
	for _, ch := range chunks {
		if strings.Contains(contentOf(t, content, ch), block) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("放得下的围栏块应当被某个片段完整包含，实际切成了：")
		for i, ch := range chunks {
			t.Errorf("  片段 %d: %q", i, contentOf(t, content, ch))
		}
	}
}

// TestOversizedFencedBlockBreaksOnlyAtLineBoundaries 守护「超长围栏块退而求其次」。
//
// 块本身超过 MaxRunes 时物理上无法整体保留，此时**只能在行边界切**。
// 从一行中间切开会让两个片段都从半行开始——那是可见的质量问题，
// 而且它不报错。
func TestOversizedFencedBlockBreaksOnlyAtLineBoundaries(t *testing.T) {
	// 造一个远超上限的围栏块，每行长度不同，避免"恰好切在行首"的巧合。
	var b strings.Builder
	b.WriteString("```text\n")
	for i := 0; i < 30; i++ {
		b.WriteString(strings.Repeat("图", i%5+3))
		b.WriteString(" 一段图表内容\n")
	}
	b.WriteString("```\n")
	content := "开头。\n" + b.String() + "结尾。\n"

	c := newChunker(t, Config{MaxRunes: 80, OverlapRunes: 10})
	chunks := mustSplit(t, c, 1, content)

	fenceStart := strings.Index(content, "```text")
	r := []rune(content)
	for i, ch := range chunks {
		// 只查**结束**端点：那才是 cutPoint 决定的。
		//
		// ⚠️ 起点这里**不查**——起点由「结束位置减去重叠」得出，天然落在行中间。
		// 那是 #48（重叠起点吸附行边界）的范围。混在一起查的话，
		// 这条测试会在 #47 还没做完时就红，说不清是谁的问题。
		pos := ch.EndOffset
		if pos <= fenceStart || pos >= len(r) {
			continue
		}
		if r[pos-1] == '\n' || r[pos] == '\n' {
			continue // 行边界，合法
		}
		t.Errorf("片段 %d 的结束端点 %d 落在围栏块内的行中间：%q",
			i, pos, contentOf(t, content, ch))
	}
}

// TestTableRowsStayTogether 守护表格不被从中间切开。
func TestTableRowsStayTogether(t *testing.T) {
	table := "| 变量 | 默认值 | 说明 |\n" +
		"| --- | --- | --- |\n" +
		"| TOP_K | 10 | 返回条数 |\n" +
		"| TIMEOUT | 5s | 检索超时 |\n"
	content := "配置说明如下，请仔细阅读每一项的含义。\n\n" + table + "\n以上是全部配置。\n"

	c := newChunker(t, Config{MaxRunes: 90, OverlapRunes: 10})
	chunks := mustSplit(t, c, 1, content)

	// 片段末尾的空白会被裁掉，所以比对时也要裁掉表格自己的尾随换行——
	// 否则这条测试会因为"差一个 \n"而红，让人以为是切分的问题。
	want := strings.TrimRight(table, "\n")

	var found bool
	for _, ch := range chunks {
		if strings.Contains(contentOf(t, content, ch), want) {
			found = true
		}
	}
	if !found {
		t.Error("放得下的表格应当被某个片段完整包含，实际切成了：")
		for i, ch := range chunks {
			t.Errorf("  片段 %d: %q", i, contentOf(t, content, ch))
		}
	}
}

// TestFreestandingPipesAreNotTable 守护「≥3 行才算表格」那条阈值。
//
// 单独几行以 | 开头更可能是正文里恰好这么排版的内容（命令行管道、
// 或一行 Markdown 示例）。把它们当表格保护起来，反而会把正常段落切碎。
func TestFreestandingPipesAreNotTable(t *testing.T) {
	content := "示例命令：\n" +
		"cat a.txt | grep foo\n" +
		"ls -la | wc -l\n" +
		"\n" +
		"继续正文，这一段很长很长要保证它会被切分。" +
		strings.Repeat("补充内容。", 20) + "\n"

	c := newChunker(t, Config{MaxRunes: 60, OverlapRunes: 10})
	chunks := mustSplit(t, c, 1, content)
	if len(chunks) < 3 {
		t.Fatalf("这段内容应当被切成多个片段，实际 %d 个", len(chunks))
	}
	// 关键断言：保护逻辑不该吞掉任何内容。
	//
	// ⚠️ 不能断言「最后一个片段的 EndOffset == 原文长度」——
	// 末尾空白本来就会被裁掉，那是既有行为。要查的是**非空白字符**全覆盖。
	runes := []rune(content)
	covered := make([]bool, len(runes))
	for _, ch := range chunks {
		for i := ch.StartOffset; i < ch.EndOffset; i++ {
			covered[i] = true
		}
	}
	for i, r := range runes {
		if !isSpace(r) && !covered[i] {
			t.Errorf("第 %d 个字符 %q 没有被任何片段覆盖（丢内容了）", i, r)
		}
	}
}

// TestChunkAlwaysAdvances 守护「切分必须严格前进」。
//
// ⚠️ 这条测试来自一次真实事故：第一版保护区实现允许把切分点退到「任何
// region.start > pos」的位置，而下一片的起点是 end-overlap——
// 于是退到块首之后又退回到块首之前，两次调用算出同一个 end，
// 切分原地打转，全靠 `next = pos + 1` 兜底一个字符一个字符地挪。
//
// 症状是片段数暴涨（实测 196 → 313）而**不报任何错**，
// 表现成「改了切分之后检索变差」——根因在切分空转，不在检索。
func TestChunkAlwaysAdvances(t *testing.T) {
	// 一段"保护区紧贴着起点"的内容：块首离 pos 很近，
	// 退到块首会让本片几乎不前进。
	content := strings.Repeat("字", 30) + "\n" +
		"```text\n" + strings.Repeat("图", 300) + "\n```\n" +
		strings.Repeat("文", 200) + "\n"

	for _, cfg := range []Config{
		{MaxRunes: 100, OverlapRunes: 20},
		{MaxRunes: 100, OverlapRunes: 0},
		{MaxRunes: 400, OverlapRunes: 60},
	} {
		c := newChunker(t, cfg)
		chunks := mustSplit(t, c, 1, content)

		// 片段数不该远多于 content/MaxRunes——远超就说明在空转。
		runes := len([]rune(content))
		maxReasonable := runes/(cfg.MaxRunes-cfg.OverlapRunes) + 8
		if len(chunks) > maxReasonable {
			t.Errorf("cfg=%+v：切出 %d 个片段，内容 %d 字符、理论上限约 %d 个——切分在空转",
				cfg, len(chunks), runes, maxReasonable)
		}
		// 每个片段都必须真的覆盖一段内容。
		for i, ch := range chunks {
			if ch.EndOffset <= ch.StartOffset {
				t.Errorf("cfg=%+v：片段 %d 长度为 0", cfg, i)
			}
		}
	}
}

// ---- 重叠起点吸附行边界（#48）----

// ---- 重叠起点（#48：试过行边界吸附，实测有害，已回退）----
//
// 这里**故意没有**「片段必须从行首开始」这条断言。理由写在 chunker.go
// 的对应位置和 BENCHMARKS.md 里：两种吸附方向各跑 4 组切分参数，
// 没有一次比「盲退固定字符数」更好。
//
// 顺手记下这条**已知限制**：片段确实可能从半行开始
// （实测能见到以 "───────────┘" 开头的片段）。
// 它是可读性问题，不影响答案能不能被取到——而修它的两种尝试都伤了召回。

// TestOverlapNeverExceedsConfigured 守护「实际重叠不长于配置值」。
//
// 吸附的方向必须是**向后**（朝文末）找行首。向前找会让重叠比配置值更长，
// 而 OverlapRunes 这个数是按索引成本定下来的——重叠悄悄变长意味着
// 索引体积悄悄膨胀，报告上看不出来。
func TestOverlapNeverExceedsConfigured(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 80; i++ {
		b.WriteString("这一段有内容。\n")
	}
	content := b.String()

	const overlap = 25
	c := newChunker(t, Config{MaxRunes: 90, OverlapRunes: overlap})
	chunks := mustSplit(t, c, 1, content)

	for i := 0; i+1 < len(chunks); i++ {
		got := chunks[i].EndOffset - chunks[i+1].StartOffset
		if got > overlap {
			t.Errorf("片段 %d → %d 的实际重叠 %d 超过配置的 %d",
				i, i+1, got, overlap)
		}
	}
}

// TestOverlapOnSingleLongLine 守护「没有换行的超长文本照样切得动」。
//
// 整篇只有一行时，任何按行对齐的设想都无从谈起。这时不能硬猜位置，
// 只能按固定字符数回退——片段会从半行开始，但那是物理上无法避免的，
// 关键是不丢内容、不死循环。
func TestOverlapOnSingleLongLine(t *testing.T) {
	// 无换行的长文本：整篇只有一行。
	content := strings.Repeat("没有任何换行的超长文本内容", 40)

	c := newChunker(t, Config{MaxRunes: 100, OverlapRunes: 30})
	chunks := mustSplit(t, c, 1, content)

	if len(chunks) < 5 {
		t.Fatalf("应当切出多段，实际 %d 段", len(chunks))
	}
	// 覆盖性不能丢——退化路径不该吞掉内容。
	runes := []rune(content)
	covered := make([]bool, len(runes))
	for _, ch := range chunks {
		for i := ch.StartOffset; i < ch.EndOffset; i++ {
			covered[i] = true
		}
	}
	for i := range runes {
		if !covered[i] {
			t.Fatalf("第 %d 个字符没有被覆盖（退化路径吞了内容）", i)
		}
	}
}

// firstN 截取前 n 个字符，用于报错信息。按 rune 截，别把汉字切成半个。
func firstN(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
