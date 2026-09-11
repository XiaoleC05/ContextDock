// Package tokenize 提供按**书写系统**分流的分词器。
//
// 它是 BM25 和向量检索的共同上游：BM25 的倒排索引、文档长度、查询解析，
// 全部建立在这个包的输出上。索引端和查询端**必须使用同一套规则**，
// 否则召回会对不上——所以两边都只能调用 Tokenize()，不允许各写一套。
package tokenize

import (
	"strings"
	"unicode"
)

// Tokenizer 是无状态的，可以安全地被多个 goroutine 并发调用。
//
// 之所以做成结构体而不是裸函数：将来若要加入可配置项（比如是否输出
// unigram、自定义停用词表），可以在不破坏调用方代码的前提下扩展。
type Tokenizer struct{}

// New 创建一个分词器。
func New() *Tokenizer { return &Tokenizer{} }

// Tokenize 把文本切成 token 序列。
//
// 规则按**书写系统**分流，而不是按语言：
//
//	Han / 平假名 / 片假名  → 逐字 bigram（例："检索系统" → 检索 / 索系 / 系统）
//	拉丁字母 / 数字        → 按词切分并转小写（例："Vector Search" → vector / search）
//	其他字符（标点、空白）  → 作为分隔符，同时冲掉前面的缓冲
//
// 举例：
//
//	输入: 使用 pgvector 做向量检索
//	输出: [使用 pgvector 做向 向量 量检 检索]
//
// 为什么中文用 bigram 而不是分词库：中文没有空格，"检索系统" 对 BM25 来说
// 是一个不可再分的整体，直接用会让检索失效。bigram 把任意两个字组合起来，
// 天然绕开了「未登录词」（词典里没有的新词）问题，且零外部依赖。
// bleve 的 CJK 分析器内部就是同样的做法。
//
// ⚠️ bigram 只适用于 Han / 平假名 / 片假名。泰文、老挝文、高棉文、缅文
// 虽然也没有空格，但字母本身没有语义（两个字母拼起来无意义），
// 必须用词典分词或 LSTM。本项目第一版只处理中英混合，不涉及这些文种。
func (t *Tokenizer) Tokenize(s string) []string {
	if s == "" {
		return []string{}
	}
	s = normalizeWidth(s)

	// out 预分配一个粗略容量，减少扩容。按经验 token 数约为字节数的 1/3。
	out := make([]string, 0, len(s)/3+1)

	// 两个缓冲：拉丁词 和 CJK 段。它们互斥——遇到另一类字符就先冲掉对方。
	var word []rune
	var cjk []rune

	flushWord := func() {
		if len(word) == 0 {
			return
		}
		out = append(out, strings.ToLower(string(word)))
		word = word[:0]
	}

	flushCJK := func() {
		switch len(cjk) {
		case 0:
			// 无事可做
		case 1:
			// 单个汉字必须原样输出，否则单字查询永远召回不到。
			// （比如文档里有"库"，用户搜"库"）
			out = append(out, string(cjk))
		default:
			// 长度 n 的 CJK 段产出 n-1 个 bigram。
			for i := 0; i+1 < len(cjk); i++ {
				out = append(out, string(cjk[i:i+2]))
			}
		}
		cjk = cjk[:0]
	}

	for _, r := range s {
		switch {
		case isCJK(r):
			flushWord()
			cjk = append(cjk, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushCJK()
			word = append(word, r)
		default:
			// 标点、空白、符号：两者都冲掉。
			// 这一步不能省——否则 "abc中文" 会把 abc 和 中文 粘成一个段。
			flushCJK()
			flushWord()
		}
	}
	// 循环结束后别忘了把最后一段缓冲冲出来。
	flushCJK()
	flushWord()

	return out
}

// isCJK 判断一个字符是否属于「应该用 bigram 处理」的书写系统。
//
// ⚠️ 这里必须用 unicode.Han，**不能用 unicode.Ideographic**。
// 两者双向都不等价：
//   - Han 有而 Ideographic 没有：CJK 部首补充区（U+2E80–2EFF）、康熙部首
//   - Ideographic 有而 Han 没有：西夏文、女书、契丹小字
//
// 实测码点数：Han = 98408，Ideographic = 105854。
func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r)
}

// normalizeWidth 把全角字符归一成半角。
//
// 中文输入法打出的 ＡＢＣ１２３ 和 ABC123 应该被视为同一个词，
// 否则用户搜 "ABC" 找不到文档里写的 "ＡＢＣ"。
//
// 这里只处理两个区间，不引入 golang.org/x/text/unicode/norm：
//   - U+FF01–U+FF5E 全角 ASCII，与半角相差固定的 0xFEE0
//   - U+3000 全角空格 → 半角空格
//
// 之所以不引 x/text 做完整 NFKC：那会带来一个依赖，而本项目其他部分
// 刻意保持零依赖。上面两条覆盖了中文场景的绝大多数情况。
func normalizeWidth(s string) string {
	// 先扫一遍判断是否需要转换，避免无谓的分配。
	need := false
	for _, r := range s {
		if (r >= 0xFF01 && r <= 0xFF5E) || r == 0x3000 {
			need = true
			break
		}
	}
	if !need {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 0xFF01 && r <= 0xFF5E:
			b.WriteRune(r - 0xFEE0)
		case r == 0x3000:
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
