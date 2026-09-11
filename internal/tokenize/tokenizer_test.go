package tokenize

import (
	"reflect"
	"testing"
)

func TestTokenize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "中英混排",
			in:   "使用 pgvector 做向量检索",
			want: []string{"使用", "pgvector", "做向", "向量", "量检", "检索"},
		},
		{
			name: "纯中文两字",
			in:   "检索",
			want: []string{"检索"},
		},
		{
			name: "纯中文三字",
			in:   "数据库",
			want: []string{"数据", "据库"},
		},
		{
			// 单字必须原样输出，否则用户搜"库"永远召回不到。
			name: "单字 CJK 必须回吐 unigram",
			in:   "库",
			want: []string{"库"},
		},
		{
			name: "纯英文转小写",
			in:   "Vector Search Engine",
			want: []string{"vector", "search", "engine"},
		},
		{
			name: "数字与字母混合不拆",
			in:   "Go1.26",
			want: []string{"go1", "26"},
		},
		{
			name: "标点作为分隔符",
			in:   "Hello, world!",
			want: []string{"hello", "world"},
		},
		{
			name: "空串",
			in:   "",
			want: []string{},
		},
		{
			name: "纯标点",
			in:   "，。！？——",
			want: []string{},
		},
		{
			name: "纯空白",
			in:   "   \n\t  ",
			want: []string{},
		},
		{
			// 全角 ＡＢＣ 必须和半角 ABC 归一成同一个 token。
			name: "全角字母归一",
			in:   "ＡＢＣ",
			want: []string{"abc"},
		},
		{
			name: "全角数字归一",
			in:   "１２３",
			want: []string{"123"},
		},
		{
			name: "全角空格当分隔符",
			in:   "中文　english",
			want: []string{"中文", "english"},
		},
		{
			// 汉字与拉丁字母相邻时必须断开，不能粘成一段。
			name: "汉字与字母紧邻",
			in:   "abc中文",
			want: []string{"abc", "中文"},
		},
		{
			name: "日文假名也走 bigram",
			in:   "全文検索",
			want: []string{"全文", "文検", "検索"},
		},
		{
			name: "平假名",
			in:   "すし",
			want: []string{"すし"},
		},
		{
			name: "标点切断 CJK 段",
			in:   "数据，库",
			want: []string{"数据", "库"},
		},
		{
			name: "英文词被标点拆开",
			in:   "post-graduate",
			want: []string{"post", "graduate"},
		},
		{
			name: "前后有空白",
			in:   "  hello  ",
			want: []string{"hello"},
		},
	}

	tk := New()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tk.Tokenize(tt.in)
			if got == nil {
				t.Fatal("Tokenize 不应返回 nil，调用方需要能直接 range")
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Tokenize(%q)\n  期望 %v\n  实际 %v", tt.in, tt.want, got)
			}
		})
	}
}

// TestTokenizeIsDeterministic 保证同一输入永远产出同一结果。
//
// 这不是废话：BM25 的倒排索引在索引端建立、在查询端复用，
// 两次结果不一致会导致召回莫名其妙地失败。
func TestTokenizeIsDeterministic(t *testing.T) {
	tk := New()
	const in = "ContextDock 是一个混合检索服务，支持 BM25 与向量检索。"
	first := tk.Tokenize(in)
	for i := 0; i < 100; i++ {
		got := tk.Tokenize(in)
		if !reflect.DeepEqual(first, got) {
			t.Fatalf("第 %d 次结果不一致:\n  %v\n  %v", i, first, got)
		}
	}
}

// TestTokenizerIsConcurrencySafe 验证分词器可以被并发调用。
//
// 文档里声明了 Tokenizer 是无状态的，这个测试守护那个声明。
// 用 go test -race 运行时才能真正验证。
func TestTokenizerIsConcurrencySafe(t *testing.T) {
	tk := New()
	const in = "并发测试 concurrent test 中文分词"
	want := tk.Tokenize(in)

	done := make(chan []string, 50)
	for i := 0; i < 50; i++ {
		go func() { done <- tk.Tokenize(in) }()
	}
	for i := 0; i < 50; i++ {
		got := <-done
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("并发调用结果不一致:\n  期望 %v\n  实际 %v", want, got)
		}
	}
}

// TestBigramCount 验证一个长度为 n 的 CJK 段产出 n-1 个 token。
//
// 这条规律是 BM25 文档长度 |D| 的基础——如果 token 数算错，
// 长度归一化就会失准，短文档会被系统性高估。
func TestBigramCount(t *testing.T) {
	tk := New()
	for n := 1; n <= 10; n++ {
		s := string(make([]rune, 0))
		for i := 0; i < n; i++ {
			s += "中"
		}
		got := tk.Tokenize(s)
		var want int
		if n == 1 {
			want = 1 // 单字回吐 unigram
		} else {
			want = n - 1
		}
		if len(got) != want {
			t.Errorf("%d 个连续汉字应产出 %d 个 token，实际 %d 个: %v", n, want, len(got), got)
		}
	}
}

// TestNoEmptyTokens 保证不会产出空字符串 token。
//
// 空 token 会污染 BM25 的倒排表（所有文档都"包含"它），
// 进而拉低它的 IDF，属于静默的质量下降。
func TestNoEmptyTokens(t *testing.T) {
	tk := New()
	inputs := []string{
		"，中文，", "a,,b", "   ", "中 a 文", "　abc　", "！！！",
	}
	for _, in := range inputs {
		for _, tok := range tk.Tokenize(in) {
			if tok == "" {
				t.Errorf("Tokenize(%q) 产出了空 token", in)
			}
		}
	}
}

// TestIsCJKUsesHanNotIdeographic 守护 isCJK 的判据选择。
//
// 代码注释里写着「必须用 unicode.Han，不能用 unicode.Ideographic」，
// 但常用汉字在两张表里完全一致——只有边缘码点才分叉。
// 如果不测这些边缘码点，把判据换成 Ideographic 也不会有任何测试失败，
// 那条注释就成了一句空话。
//
// 两边各自多出来的部分（实测）：
//
//	Han 有而 Ideographic 没有：康熙部首、CJK 部首补充、叠字号
//	Ideographic 有而 Han 没有：〆、西夏文、女书、契丹小字
func TestIsCJKUsesHanNotIdeographic(t *testing.T) {
	hanOnly := []struct {
		r    rune
		name string
	}{
		{0x2F00, "康熙部首 ⼀"},
		{0x2E80, "CJK 部首补充 ⺀"},
		{0x3005, "叠字号 々"},
	}
	ideoOnly := []struct {
		r    rune
		name string
	}{
		{0x3006, "〆"},
		{0x17000, "西夏文"},
		{0x1B170, "女书"},
	}

	for _, c := range hanOnly {
		if !isCJK(c.r) {
			t.Errorf("%s (U+%04X) 应被判定为 CJK —— 它是 Han 但不是 Ideographic",
				c.name, c.r)
		}
	}
	for _, c := range ideoOnly {
		if isCJK(c.r) {
			t.Errorf("%s (U+%04X) 不应被判定为 CJK —— 它是 Ideographic 但不是 Han。"+
				"如果这条失败，说明判据被换成了 unicode.Ideographic",
				c.name, c.r)
		}
	}
}

func BenchmarkTokenizeChinese(b *testing.B) {
	tk := New()
	const s = "ContextDock 是一个基于 Go 的混合检索 MCP 服务，为本地 Agent 提供向量与 BM25 混合召回。"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = tk.Tokenize(s)
	}
}

func BenchmarkTokenizeEnglish(b *testing.B) {
	tk := New()
	const s = "ContextDock is a hybrid retrieval MCP server written in Go for local agents."
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = tk.Tokenize(s)
	}
}
