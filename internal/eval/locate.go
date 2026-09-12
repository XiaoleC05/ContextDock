package eval

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

var (
	// ErrQuoteEmpty 表示引文为空。
	ErrQuoteEmpty = errors.New("eval: 引文为空")

	// ErrQuoteNotFound 表示引文在原文里找不到。
	//
	// 两种可能，都必须人工判断：标注抄错了，或者语料已经不是标注时的那一版。
	// 这两种情况都该立刻停下——继续跑出来的数字没有意义。
	ErrQuoteNotFound = errors.New("eval: 引文在原文里找不到")

	// ErrQuoteAmbiguous 表示引文出现多次但没指定第几次。
	ErrQuoteAmbiguous = errors.New("eval: 引文在原文里出现多次，必须指定 occurrence")
)

// LocateQuote 在 content 里定位 quote 的第 occurrence 次出现，返回 rune 区间。
//
// 返回的 [start, end) 是**左闭右开**的 rune 下标，与 types.Chunk 的
// StartOffset / EndOffset 同一套单位——命中判定要跨这两者做区间比较，
// 单位不一致（一个 rune 一个 byte）会静默算错，而且错得很像那么回事：
// 命中率偏低一点，看起来只是「检索效果一般」。
//
// occurrence 从 1 开始，**0 或负数表示「没指定」**。
//
// 这个区分不是可有可无的：JSON 里的 omitempty 让「没写」落成 0，
// 而「没写」和「显式写了 1」必须区别对待——
// 没写时要检查引文是否唯一，写了 1 就是标注者明确表示「我要第一次出现」。
// 不区分的话，一段出现两次的引文永远标不了第一次。
//
// 重复出现的判定采取**不重叠**语义：在 "aaaa" 里找 "aa"，
// 第 1 次在 0、第 2 次在 2，不会给出 1。
// 这是标准且可预期的行为；重叠语义会让 occurrence 的含义变得难以解释。
func LocateQuote(content, quote string, occurrence int) (start, end int, err error) {
	if quote == "" {
		return 0, 0, ErrQuoteEmpty
	}
	explicit := occurrence >= 1
	if !explicit {
		occurrence = 1
	}

	idx := -1
	from := 0
	for i := 0; i < occurrence; i++ {
		j := strings.Index(content[from:], quote)
		if j < 0 {
			// 第 i+1 次就找不到了。要区分两种情形：
			//   - 连第一次都没有 → 引文根本不在原文里
			//   - 有第一次但没有第 occurrence 次 → occurrence 写大了
			if i == 0 {
				return 0, 0, ErrQuoteNotFound
			}
			return 0, 0, fmt.Errorf("%w: 只找到 %d 次", ErrQuoteAmbiguous, i)
		}
		idx = from + j
		from = idx + len(quote)
	}

	// 引文出现多次而标注者没显式指定 occurrence 时，**必须报错而不是取第一个**。
	//
	// 理由：标注者心里想的是 A 处，程序测的是 B 处，而结果看起来完全正常——
	// 这是那种能一路混到结论里去的错误。
	if !explicit && strings.Index(content[from:], quote) >= 0 {
		return 0, 0, fmt.Errorf("%w（至少出现 2 次）", ErrQuoteAmbiguous)
	}

	// byte 下标转 rune 下标。
	//
	// ⚠️ 这一步不能省。Go 的 string 索引是字节，一个汉字占 3 字节；
	// 直接把 idx 当字符位置用，中文文档上的偏移会偏大近 3 倍，
	// 而代码照样编译、照样运行、照样给出一个看起来合理的命中率。
	start = utf8.RuneCountInString(content[:idx])
	end = start + utf8.RuneCountInString(quote)
	return start, end, nil
}

// Span 是一段 rune 区间，左闭右开。
type Span struct {
	Start, End int
}

// Overlaps 判断两个区间是否有重叠。
//
// 用 max(a,c) < min(b,d) 而不是更常见的 a < d && c < b：
// 两者**只在区间非空时等价**。空区间 [5,5) 没有任何点，
// 不该与任何东西重叠，但代入后者会得到 5 < 10 && 0 < 5 → true。
//
// 写成 max/min 之后判据与定义直接对应（「两区间交集非空」），
// 不留这种要靠举反例才想起来的边角。
func (s Span) Overlaps(o Span) bool {
	return max(s.Start, o.Start) < min(s.End, o.End)
}

// Len 返回区间长度（rune 数）。
func (s Span) Len() int { return s.End - s.Start }
