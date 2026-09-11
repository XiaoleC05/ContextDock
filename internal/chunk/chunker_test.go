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
		{"OverlapRunes 为负", Config{MaxRunes: 100, OverlapRunes: -1}},
		{"OverlapRunes 大于 MaxRunes", Config{MaxRunes: 100, OverlapRunes: 100}},
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

// TestZeroConfigUsesDefaults 验证零值 Config 等价于默认配置。
func TestZeroConfigUsesDefaults(t *testing.T) {
	a := newChunker(t, Config{})
	b := newChunker(t, DefaultConfig())
	if a.cfg != b.cfg {
		t.Errorf("零值配置应等价于默认配置: %+v vs %+v", a.cfg, b.cfg)
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
