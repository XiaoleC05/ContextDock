// Package chunk 把整篇文档切成检索用的片段。
//
// 切分质量**直接决定检索质量**：切得太碎会丢上下文，切得太大会让无关内容
// 污染相似度分数。所以这个包的输出格式、边界处理、覆盖性都有测试守着。
package chunk

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/XiaoleC05/ContextDock/internal/types"
)

// 默认切分参数。
//
// 400 字来自中文 RAG 的社区经验区间（200–500 字）；overlap 取 15%，
// 目的是防止答案正好被切断在两个片段的接缝处。
//
// ⚠️ 这些是经验值，不是硬指标。落地时应该用真实查询集测 Recall@k 来校准。
const (
	DefaultMaxRunes     = 400
	DefaultOverlapRunes = 60
)

var (
	// ErrEmptyDocument 表示文档没有任何可切分的内容。
	//
	// 之所以要报错而不是返回空片段列表：空文档切成 0 个片段后如果静默入库，
	// 检索时它永远召不回，而且不报错——属于最难排查的一类问题。
	ErrEmptyDocument = errors.New("chunk: 文档内容为空，没有可切分的文本")

	// ErrNoDocumentID 表示调用方没有传文档 ID。
	ErrNoDocumentID = errors.New("chunk: DocumentID 不能为 0")

	// ErrBadConfig 表示切分参数不合法。
	ErrBadConfig = errors.New("chunk: 切分参数不合法")
)

// Config 是切分参数。
type Config struct {
	// MaxRunes 是单个片段的最大字符数（rune 数，不是字节数）。
	MaxRunes int

	// OverlapRunes 是相邻片段的重叠字符数。
	OverlapRunes int
}

// DefaultConfig 返回推荐参数。
func DefaultConfig() Config {
	return Config{MaxRunes: DefaultMaxRunes, OverlapRunes: DefaultOverlapRunes}
}

// normalize 补全未设置的字段并做合法性检查。
//
// # ⚠️ 两个字段的「未设置」哨兵不一样，这是刻意的
//
// MaxRunes 用 0 表示未设置——0 个字符的片段本来就不合法，拿它当哨兵没有歧义。
//
// OverlapRunes **用负数**表示未设置。这里改过一次：
// 原来两个字段都用 0，结果是 `OverlapRunes: 0`（完全不重叠，一个完全合法
// 且有用的配置）会被静默改成 60，**用户根本没有办法关掉重叠**。
//
// 这条规则本项目已经写过一次，在 types.Chunk.Ordinal 的注释里：
//
//	> 0 是**合法值**……它不能像 ID 那样被当作"未设置"的哨兵。
//	> 如果确实需要表达"未计算"，请另加字段，不要复用 0。
//
// 代价是 `New(Config{})` 不再等价于 `DefaultConfig()`——空的 Config 会得到
// 不重叠的配置。想要默认值请显式写 `DefaultConfig()`。
// 「零值即默认」这个惯用法很好，但它不能以牺牲一类合法配置为代价。
func (c Config) normalize() (Config, error) {
	if c.MaxRunes == 0 {
		c.MaxRunes = DefaultMaxRunes
	}
	if c.OverlapRunes < 0 {
		c.OverlapRunes = DefaultOverlapRunes
	}
	if c.MaxRunes < 1 {
		return c, fmt.Errorf("%w: MaxRunes 必须为正数，实际 %d", ErrBadConfig, c.MaxRunes)
	}
	if c.OverlapRunes < 0 {
		return c, fmt.Errorf("%w: OverlapRunes 不能为负，实际 %d", ErrBadConfig, c.OverlapRunes)
	}
	// overlap 必须小于 chunk 大小，否则 pos 不前进，会死循环。
	if c.OverlapRunes >= c.MaxRunes {
		return c, fmt.Errorf("%w: OverlapRunes(%d) 必须小于 MaxRunes(%d)",
			ErrBadConfig, c.OverlapRunes, c.MaxRunes)
	}
	return c, nil
}

// Chunker 按配置切分文档。它是无状态的，可以并发使用。
type Chunker struct {
	cfg Config
}

// New 创建一个切分器。传零值 Config 会使用默认参数。
func New(cfg Config) (*Chunker, error) {
	norm, err := cfg.normalize()
	if err != nil {
		return nil, err
	}
	return &Chunker{cfg: norm}, nil
}

// Split 把文档内容切成片段。
//
// 处理顺序：
//  1. 按 Markdown 标题把文档分成若干节，每节记录标题面包屑
//  2. 每节的正文按 MaxRunes 切分，优先在句末标点处断开
//  3. 相邻片段重叠 OverlapRunes 个字符
//
// 每个返回的片段都满足不变量：Content == []rune(原文)[StartOffset:EndOffset]。
// 这条不变量是覆盖性校验和将来「重新切分对齐」的基础。
//
// ⚠️ Markdown 标题行本身**不会**出现在片段正文里——它被记进 Metadata，
// 由 types.Chunk.IndexText() 在拼接被索引文本时统一加回去。
// 这样标题不会在正文和面包屑里重复出现。
func (c *Chunker) Split(docID int64, content string) ([]types.Chunk, error) {
	if docID == 0 {
		return nil, ErrNoDocumentID
	}
	// 这里刻意不做「内容为空就提前返回」的检查：空内容、纯空白、以及
	// 只有标题行的情况，都会在下面自然产生 0 个片段，由 len(out)==0 统一兜底。
	// 多一条提前返回就多一条要维护、要测试的路径，而它并不能多拦住什么。

	runes := []rune(content)
	sections := splitSections(runes)

	out := make([]types.Chunk, 0, len(runes)/c.cfg.MaxRunes+1)
	ordinal := 0
	for _, sec := range sections {
		c.chunkSection(runes, sec, docID, &ordinal, &out)
	}

	if len(out) == 0 {
		// 内容全是标题行，没有正文可切。
		return nil, ErrEmptyDocument
	}
	return out, nil
}

// section 是文档里的一节：一段连续正文，以及它所属的标题面包屑。
type section struct {
	breadcrumb string // 例如 "安装指南 > 快速开始"；无标题时为 ""
	start, end int    // rune 下标，左闭右开
}

// splitSections 按 Markdown 标题把文档切成若干节。
//
// 标题行本身不属于任何一节（它只贡献面包屑），所以节与节之间在原文里
// 是**有间隙**的——间隙就是那几行标题。
func splitSections(runes []rune) []section {
	var (
		secs      []section
		titles    = make([]string, 7) // 下标即标题层级，支持 h1–h6
		current   string
		bodyStart = 0
	)

	flush := func(end int) {
		if end > bodyStart {
			secs = append(secs, section{breadcrumb: current, start: bodyStart, end: end})
		}
	}

	i := 0
	for i < len(runes) {
		j := i
		for j < len(runes) && runes[j] != '\n' {
			j++
		}

		if level, title, ok := parseHeading(runes[i:j]); ok && level < len(titles) {
			flush(i)
			titles[level] = title
			for k := level + 1; k < len(titles); k++ {
				titles[k] = "" // 更深层级的标题已失效
			}
			current = joinBreadcrumb(titles, level)
			bodyStart = j + 1
			if bodyStart > len(runes) {
				bodyStart = len(runes)
			}
		}
		i = j + 1
	}
	flush(len(runes))

	return secs
}

// parseHeading 判断一行是不是 Markdown 标题（# ~ ######，后跟空格）。
//
// 只认 ATX 形式（# 开头），不认 Setext 形式（下一行画 === 或 ---）。
// Setext 与水平分割线难以区分，第一版不处理。
func parseHeading(line []rune) (level int, title string, ok bool) {
	n := 0
	for n < len(line) && line[n] == '#' {
		n++
	}
	if n == 0 || n > 6 {
		return 0, "", false
	}
	// `#` 后面必须跟空格或直接结束，否则 #hashtag 会被误判成标题。
	if n < len(line) && line[n] != ' ' && line[n] != '\t' {
		return 0, "", false
	}
	title = strings.TrimSpace(string(line[n:]))
	if title == "" {
		return 0, "", false
	}
	return n, title, true
}

// joinBreadcrumb 把第 1..level 级标题拼成面包屑。
func joinBreadcrumb(titles []string, level int) string {
	parts := make([]string, 0, level)
	for i := 1; i <= level && i < len(titles); i++ {
		if titles[i] != "" {
			parts = append(parts, titles[i])
		}
	}
	return strings.Join(parts, " > ")
}

// chunkSection 把一节正文切成片段，追加到 out。
func (c *Chunker) chunkSection(runes []rune, sec section, docID int64, ordinal *int, out *[]types.Chunk) {
	pos := sec.start
	for pos < sec.end {
		// 跳过片段开头的空白，避免产出以换行开头的片段。
		for pos < sec.end && isSpace(runes[pos]) {
			pos++
		}
		if pos >= sec.end {
			return
		}

		end := pos + c.cfg.MaxRunes
		// ⚠️ hitEnd 必须在去掉尾部空白**之前**算出来。
		// 去空白会让 end 退到 sec.end 之前，如果之后再用 end >= sec.end 判断
		// 就会误以为还没切完，于是靠 `next = pos + 1` 兜底，一个字符一个字符地
		// 往前挪——短段落会切出一大堆单字片段，而且不报错。
		hitEnd := end >= sec.end
		if hitEnd {
			end = sec.end
		} else if brk := lastSentenceBreak(runes, pos, end); brk > pos {
			end = brk
		}
		// 去掉末尾空白，但不要退到起点之前。
		for end > pos+1 && isSpace(runes[end-1]) {
			end--
		}

		ch := types.Chunk{
			DocumentID:  docID,
			Ordinal:     *ordinal,
			StartOffset: pos,
			EndOffset:   end,
			Content:     string(runes[pos:end]),
		}
		if sec.breadcrumb != "" {
			// 必须走 SetMetadata —— 直接写裸 map 会 panic。
			ch.SetMetadata(types.MetadataKeyHeading, sec.breadcrumb)
		}
		*out = append(*out, ch)
		*ordinal++

		if hitEnd {
			return
		}
		// 回退 overlap 个字符作为下一段的起点。
		next := end - c.cfg.OverlapRunes
		if next <= pos {
			// 兜底：必须严格前进，否则死循环。
			next = pos + 1
		}
		pos = next
	}
}

// lastSentenceBreak 在 (from, limit] 范围内从后往前找句末标点，
// 返回标点之后的下标（即断点）。找不到返回 0。
//
// 优先在句子边界断开，是为了避免把一个完整的句子劈成两半——
// 那会让两个片段都变得语义不完整。
func lastSentenceBreak(runes []rune, from, limit int) int {
	for i := limit - 1; i > from; i-- {
		if isSentenceEnd(runes[i]) {
			return i + 1
		}
	}
	return 0
}

// isSentenceEnd 判断是不是句末标点，中英文都认。
func isSentenceEnd(r rune) bool {
	switch r {
	case '。', '！', '？', '；', '…', '\n':
		return true
	case '.', '!', '?', ';':
		return true
	}
	return false
}

// isSpace 判断是否空白字符。
//
// 用 unicode.IsSpace 而不是手写 ASCII 列表：中文全角空格 U+3000、
// 不换行空格 U+00A0 在中文文档里很常见，如果只认 ASCII，
// 一个只含全角空格的文档会切出一个"看似有内容"的片段，一路通过校验进库。
func isSpace(r rune) bool {
	return unicode.IsSpace(r)
}
