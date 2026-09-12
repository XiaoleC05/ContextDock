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

// Scheme 是 CJK 部分的分词方案。
//
// 拉丁字母部分在任何方案下都一样（按词切分 + 转小写）——那些语言有空格，
// 没有选择的余地。有争议的从来只有中文怎么切。
type Scheme string

const (
	// SchemeBigram 只产出字符 bigram。这是 v1.0.0 的默认，也是 DESIGN §3 的选型。
	SchemeBigram Scheme = "bigram"

	// SchemeUnigram 只产出单字。
	//
	// 它是最朴素的方案：召回几乎必然更高（每个字都成了检索点），
	// 代价是区分度低——常用字（的、是、在）到处都是，
	// 一条查询会撞上大量不相关的片段。
	SchemeUnigram Scheme = "unigram"

	// SchemeBoth 单字和 bigram 都产出。
	//
	// 直觉上「两个都要」应当最好，但它的词典会大一倍，
	// BM25 的长度归一化也会被单字的数量撑大。到底如何，由 #45 的实测说了算。
	SchemeBoth Scheme = "both"
)

// AllSchemes 是全部合法方案，顺序即报告里的展示顺序。
var AllSchemes = []Scheme{SchemeBigram, SchemeUnigram, SchemeBoth}

// Valid 判断是不是已知方案。
func (s Scheme) Valid() bool {
	for _, x := range AllSchemes {
		if s == x {
			return true
		}
	}
	return false
}

// Tokenizer 是无状态的，可以安全地被多个 goroutine 并发调用。
//
// 之所以做成结构体而不是裸函数：为可配置项留位置。
// #45 的对比实验正是靠这个扩展点做的，没有改动任何调用方的签名。
type Tokenizer struct {
	scheme Scheme
}

// New 创建一个使用默认方案（bigram）的分词器。
func New() *Tokenizer { return &Tokenizer{scheme: SchemeBigram} }

// NewWith 创建一个指定方案的分词器。
//
// 方案不合法或为空时**回落成 bigram**，而不是报错或返回空结果：
// 分词器是 BM25 的构造依赖，在这里报错会让整条检索链路起不来；
// 而"切不出 token"这种失效方式更糟——它不报错，只是什么都搜不到。
func NewWith(s Scheme) *Tokenizer {
	if !s.Valid() {
		s = SchemeBigram
	}
	return &Tokenizer{scheme: s}
}

// Scheme 返回当前方案。
func (t *Tokenizer) Scheme() Scheme {
	if t.scheme == "" {
		return SchemeBigram
	}
	return t.scheme
}

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
			// 长度 n 的 CJK 段：unigram 出 n 个，bigram 出 n-1 个。
			//
			// 两段的顺序是**先单字后 bigram**，这是刻意的：
			// 同一条查询里 token 的顺序会影响人读日志和调试时的直觉
			// （"[检 索 检索]" 比 "[检索 检 索]" 更容易看出切法）。
			// BM25 本身不看顺序，所以这纯粹是可读性考虑。
			if t.Scheme() != SchemeBigram {
				for _, r := range cjk {
					out = append(out, string(r))
				}
			}
			if t.Scheme() != SchemeUnigram {
				for i := 0; i+1 < len(cjk); i++ {
					out = append(out, string(cjk[i:i+2]))
				}
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
