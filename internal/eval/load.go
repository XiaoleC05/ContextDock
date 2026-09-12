package eval

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/XiaoleC05/ContextDock/internal/chunk"
	"github.com/XiaoleC05/ContextDock/internal/tokenize"
	"github.com/XiaoleC05/ContextDock/internal/types"
)

// eval/ 下的固定路径。集中在这里，避免各处拼字符串拼错。
const (
	// Dir 是评测数据的根目录，相对仓库根。
	Dir = "eval"

	// CorpusManifest 是语料清单文件名。
	CorpusManifest = "corpus.json"

	// QueriesDir 是评测集文件所在目录。
	QueriesDir = "queries"
)

// maxProblems 是一次校验最多报告多少条问题。
//
// 上限的意义是让输出**可读**：手工编辑评测集时，一个复制粘贴错误
// 可能瞬间造出上百条问题，把真正的原因淹掉。截断时明确写出还剩多少条，
// 不做无声丢弃。
const maxProblems = 30

// Problems 汇总一次校验里发现的全部问题。
//
// 刻意**一次报告所有问题**而不是遇到第一个就返回：
// 这个命令的用途是手工编辑评测集时的即时反馈，
// 报一条改一条再跑一次，会把标注这种本就枯燥的活拖得更长。
type Problems struct {
	items []string
	total int
}

// Addf 追加一条问题。
func (p *Problems) Addf(format string, args ...any) {
	p.total++
	if len(p.items) < maxProblems {
		p.items = append(p.items, fmt.Sprintf(format, args...))
	}
}

// Empty 表示没有问题。
func (p *Problems) Empty() bool { return p.total == 0 }

// Err 把问题汇总成 error；没有问题返回 nil。
func (p *Problems) Err() error {
	if p.total == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "评测集校验失败，共 %d 个问题：", p.total)
	for i, s := range p.items {
		fmt.Fprintf(&b, "\n  %d) %s", i+1, s)
	}
	if rest := p.total - len(p.items); rest > 0 {
		fmt.Fprintf(&b, "\n  …… 还有 %d 条未列出（先修上面的）", rest)
	}
	return errors.New(b.String())
}

// Suite 是加载并校验完成的全部评测输入。
type Suite struct {
	// Corpus 是语料清单。
	Corpus *Corpus

	// Sets 是全部评测集，**按 AllGroups 的顺序**排列。
	Sets []QuerySet

	// contents 是 source → 语料正文（已归一化换行）。
	contents map[string]string
}

// Content 返回某个 source 的语料正文。
//
// ⚠️ 导入端必须用这个字符串，不要自己再读一次磁盘文件——见 Corpus.Contents 的说明。
func (s *Suite) Content(source string) (string, bool) {
	c, ok := s.contents[source]
	return c, ok
}

// Sources 按清单顺序返回全部 source。
func (s *Suite) Sources() []string {
	out := make([]string, 0, len(s.Corpus.Files))
	for _, f := range s.Corpus.Files {
		out = append(out, f.Source)
	}
	return out
}

// Queries 按 Sets 的顺序返回全部 query，供评测器遍历。
func (s *Suite) Queries() []Query {
	var out []Query
	for _, set := range s.Sets {
		out = append(out, set.Queries...)
	}
	return out
}

// Load 从仓库根加载 eval/ 下的语料清单与全部评测集，并做完整校验。
//
// 校验与加载是同一个动作，而不是两个可选步骤：一份**没校验过的**评测集
// 不该存在于内存里。分开的话，早晚会有人只调 Load 不调 Validate，
// 然后得到一份 silently 有错的基准尺。
//
// 任何一处不合法都返回 error，且 error 里带上具体是哪一条 query。
func Load(root string) (*Suite, error) {
	base := filepath.Join(root, filepath.FromSlash(Dir))

	corpus, err := LoadCorpus(filepath.Join(base, CorpusManifest))
	if err != nil {
		return nil, err
	}
	contents, err := corpus.Contents(root)
	if err != nil {
		return nil, err
	}

	sets, err := loadQuerySets(filepath.Join(base, QueriesDir), corpus, contents)
	if err != nil {
		return nil, err
	}

	return &Suite{Corpus: corpus, Sets: sets, contents: contents}, nil
}

// loadQuerySets 读取评测集目录下全部 *.json 并校验。
//
// 这个函数**同时**做三件必须一起做的事，顺序不能换：
//  1. 读文件、解析（语法错误在这里拦住）
//  2. 逐条校验字段、定位引文（语义错误在这里拦住）
//  3. 跨文件查重（重复 query 只有看全所有文件才能发现）
func loadQuerySets(dir string, corpus *Corpus, contents map[string]string) ([]QuerySet, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("eval: 读取评测集目录 %s 失败: %w", dir, err)
	}

	var (
		problems = &Problems{}
		byGroup  = make(map[Group]QuerySet, len(AllGroups))
		// 跨文件查重：query 串 → 第一次出现在哪
		seenQuery = make(map[string]string)
		seenID    = make(map[string]string)
	)

	// 每份语料按默认参数切一遍，供同义改写组做字面重叠检查。
	// 切分是纯内存计算，5 份文档 200 个片段，成本可以忽略。
	chunked := chunkAll(contents, corpusSources(corpus))

	// 排序遍历：ReadDir 本身就按文件名排序，这里显式排一次是为了
	// 让「报错的顺序」也确定 —— 否则同一份错误数据在不同机器上
	// 可能报出不同的第一条问题，对比输出时会以为改了东西。
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	found := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		found++
		path := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("eval: 读取 %s 失败: %w", path, err)
		}

		var set QuerySet
		if err := unmarshalStrict(raw, e.Name(), &set); err != nil {
			// 语法错误没法继续，直接返回 —— 这份文件连结构都不完整，
			// 再往下校验只会报出一堆由它派生的假问题。
			return nil, err
		}

		where := e.Name()
		if set.Version != FormatVersion {
			problems.Addf("%s: version=%d，本程序只认 %d", where, set.Version, FormatVersion)
		}
		if !set.Group.Valid() {
			problems.Addf("%s: group=%q 不是合法子集（合法值：%s）",
				where, set.Group, joinGroups())
		} else if want := string(set.Group) + ".json"; e.Name() != want {
			// 文件名与 group 不一致会造成「统计进哪个组」与「文件叫什么」
			// 两个真相，而这种错位只有对着报告逐条核对才看得出来。
			problems.Addf("%s: group=%q 但文件名应为 %s", where, set.Group, want)
		}
		if strings.TrimSpace(set.Title) == "" {
			problems.Addf("%s: 缺少 title", where)
		}
		if len(set.Queries) == 0 {
			problems.Addf("%s: 没有任何 query", where)
		}
		if prev, dup := byGroup[set.Group]; dup {
			problems.Addf("%s: group %q 已经出现在另一个文件里，两组会被后加载的覆盖",
				where, set.Group)
			_ = prev
		}

		for i := range set.Queries {
			q := &set.Queries[i]
			q.File = e.Name()
			validateQuery(q, i+1, set.Group, corpus, contents, chunked, seenQuery, seenID, problems)
		}
		byGroup[set.Group] = set
	}

	if found == 0 {
		return nil, fmt.Errorf("eval: 目录 %s 里没有任何 .json 评测集", dir)
	}
	if err := problems.Err(); err != nil {
		return nil, err
	}

	// 按 AllGroups 的固定顺序输出，报告才能逐次比对。
	sets := make([]QuerySet, 0, len(byGroup))
	for _, g := range AllGroups {
		if s, ok := byGroup[g]; ok {
			sets = append(sets, s)
		}
	}
	return sets, nil
}

// validateQuery 校验一条 query 并定位它的全部引文。
func validateQuery(q *Query, seq int, group Group, corpus *Corpus, contents map[string]string,
	chunked map[string][]types.Chunk,
	seenQuery, seenID map[string]string, problems *Problems) {

	where := fmt.Sprintf("%s 第 %d 条", q.File, seq)
	if strings.TrimSpace(q.ID) == "" {
		problems.Addf("%s: 缺少 id", where)
	} else {
		where = fmt.Sprintf("%s（id=%s）", q.File, q.ID)
		if prev, dup := seenID[q.ID]; dup {
			problems.Addf("%s: id 与 %s 重复", where, prev)
		}
		seenID[q.ID] = q.File
	}

	if strings.TrimSpace(q.Query) == "" {
		problems.Addf("%s: query 为空", where)
	} else {
		// 查重用归一化后的串：只差首尾空格的两个 query 实际是同一条，
		// 它们会让整体指标里这一条被算两遍。
		key := strings.ToLower(strings.TrimSpace(q.Query))
		if prev, dup := seenQuery[key]; dup {
			problems.Addf("%s: query %q 与 %s 里的重复", where, q.Query, prev)
		}
		seenQuery[key] = fmt.Sprintf("%s（id=%s）", q.File, q.ID)
	}

	if q.Kind != "" && !q.Kind.Valid() {
		problems.Addf("%s: kind=%q 不是合法值（direct / indirect / multi-hop）", where, q.Kind)
	}

	// norel 组按定义就没有期望命中——它期望的是"没有结果"。
	// 别的组为空则一定是漏标了：那条 query 在任何指标里都会恒为未召回，
	// 悄悄拉低整体分数而没人知道。
	if len(q.Expect) == 0 {
		if group != GroupNoRel {
			problems.Addf("%s: 没有任何期望命中（这条 query 在任何指标里都会恒为未召回）", where)
		}
		return
	}

	synonymChecked := false
	for j := range q.Expect {
		e := &q.Expect[j]
		tag := fmt.Sprintf("%s 的第 %d 个期望命中", where, j+1)

		text, ok := contents[e.Source]
		if e.Source == "" {
			problems.Addf("%s: 缺少 source", tag)
			continue
		}
		if !ok {
			problems.Addf("%s: source=%q 不在语料清单里（可用：%s）",
				tag, e.Source, strings.Join(corpusSources(corpus), ", "))
			continue
		}

		if e.Grade != gradeUnset && e.Grade != GradePartial && e.Grade != GradeFull {
			problems.Addf("%s: grade=%d 不是合法分级（1=部分相关，2=完全回答，省略=2）",
				tag, e.Grade)
			continue
		}
		if e.Grade == gradeUnset {
			e.Grade = GradeFull
		}

		start, end, err := LocateQuote(text, e.Quote, e.Occurrence)
		if err != nil {
			// 报错时把引文头几个字带上 —— 一份评测集里有几十条引文，
			// 只说「第 3 个期望命中的引文找不到」还得回头翻文件数一遍。
			problems.Addf("%s: %v（source=%s，引文以 %q 开头）",
				tag, err, e.Source, head(e.Quote, 20))
			continue
		}
		e.Start, e.End = start, end

		// 同义改写组的自动化检查（#36 的验收要求）。
		//
		// 这组的**存在意义**就是「查询和目标原文一个字都对不上，只能靠语义召回」。
		// 一旦共享了字面 token，词法通道就能撞上它，这组立刻退化成
		// 「又一条普通查询」——而它名义上还在给「向量通道的贡献」背书。
		// 那会让 #40 的结论直接出错，且从报告上看不出任何异常。
		//
		// 所以宁可在校验期拦死，也不留一条悄悄失真的评测数据。
		if group == GroupSynonym && !synonymChecked {
			synonymChecked = true
			if idx, shared := overlappedChunkSharingTokens(
				chunked[e.Source], q.Query, Span{start, end}); len(shared) > 0 {
				problems.Addf("%s: 同义改写组不该与目标片段共享任何词，"+
					"但与 %s 第 %d 个片段共有了 %v。共享字面 token 会让词法通道也能命中，"+
					"这组就失去「只测向量通道」的意义", where, e.Source, idx, shared)
			}
		}
	}
}

// tokenizer 是无状态且并发安全的，包级复用一份即可。
var sharedTokenizer = tokenize.New()

// sharedTokens 返回两段文本共有的 token，去重后按字典序排列。
//
// 用项目自己的分词器而不是简单的子串匹配：BM25 索引的正是这里切出来的 token，
// 用别的口径去查会得到「检查说没重叠、检索却撞上了」这种自相矛盾的结论。
func sharedTokens(a, b string) []string {
	if a == "" || b == "" {
		return nil
	}
	inB := make(map[string]struct{}, 64)
	for _, t := range sharedTokenizer.Tokenize(b) {
		inB[t] = struct{}{}
	}
	seen := make(map[string]struct{})
	var out []string
	for _, t := range sharedTokenizer.Tokenize(a) {
		if _, ok := inB[t]; !ok {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// overlappedChunkSharingTokens 找出与引文区间重叠、且与查询共享 token 的片段。
//
// 返回片段序号（1-based）与共有的 token。
//
// 为什么拿**真实片段**比，而不是在引文前后取固定窗口：
// 要被词法通道召回的是片段，而 BM25 索引的是片段的 IndexText()
// （含标题面包屑）。固定窗口只是它的近似，会给出「检查说没重叠、
// 检索却撞上了」这种自相矛盾的结论——而检查的全部价值就在于可信。
//
// ⚠️ 片段按**切分器默认参数**切出。切分参数变了，这条检查的判据也会变；
// 它是一道**标注期的质量闸门**，不是运行期的保证。
// 运行期的真凭实据来自评测报告的按通道拆分（那里直接看词法通道召回了什么）。
func overlappedChunkSharingTokens(chunks []types.Chunk, query string, span Span) (int, []string) {
	for i, c := range chunks {
		if !span.Overlaps(Span{c.StartOffset, c.EndOffset}) {
			continue
		}
		if shared := sharedTokens(query, c.IndexText()); len(shared) > 0 {
			return i + 1, shared
		}
	}
	return 0, nil
}

// chunkAll 用切分器**默认参数**把每份语料切一遍，供同义改写组做重叠检查。
//
// 切不动（空文档等）的语料返回空片段列表，由校验的其它分支去报错，
// 不在这里拦——这里只是给重叠检查提供素材。
func chunkAll(contents map[string]string, sources []string) map[string][]types.Chunk {
	chunker, err := chunk.New(chunk.DefaultConfig())
	if err != nil {
		return nil
	}
	out := make(map[string][]types.Chunk, len(sources))
	for _, src := range sources {
		chunks, err := chunker.Split(1, contents[src])
		if err != nil {
			continue
		}
		out[src] = chunks
	}
	return out
}

// corpusSources 返回清单里全部 source，用于「可用的有哪些」这类提示。
func corpusSources(c *Corpus) []string {
	out := make([]string, 0, len(c.Files))
	for _, f := range c.Files {
		out = append(out, f.Source)
	}
	return out
}

func joinGroups() string {
	out := make([]string, 0, len(AllGroups))
	for _, g := range AllGroups {
		out = append(out, string(g))
	}
	return strings.Join(out, " / ")
}

// head 按 rune 截断，避免把多字节字符切成半个。
func head(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// unmarshalStrict 解析 JSON，并给出**带行号**的语法错误。
//
// encoding/json 的语法错误只给字节偏移，而手工编辑评测集时
// 「第 12345 字节附近有问题」几乎帮不上忙——得先知道那是第几行。
func unmarshalStrict(raw []byte, name string, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	// 拒绝未知字段：评测集是**手写**的，而 "quotes" 写成 "quote"、
	// "expect" 写成 "expects" 这类拼写错误，标准解析器会静默忽略该字段，
	// 于是这条期望命中凭空消失、query 变成「没有期望命中」——
	// 或者更糟：整个 expect 数组为空却仍然通过了某些检查。
	// 宁可因为它不认识某个字段而报错。
	dec.DisallowUnknownFields()

	if err := dec.Decode(v); err != nil {
		var syn *json.SyntaxError
		if errors.As(err, &syn) {
			return fmt.Errorf("eval: %s 第 %d 行附近 JSON 语法错误: %v",
				name, lineOf(raw, syn.Offset), err)
		}
		var typ *json.UnmarshalTypeError
		if errors.As(err, &typ) {
			return fmt.Errorf("eval: %s 第 %d 行附近类型不对: 字段 %s 需要 %s，实际是 %s",
				name, lineOf(raw, typ.Offset), typ.Field, typ.Type, typ.Value)
		}
		return fmt.Errorf("eval: 解析 %s 失败: %w", name, err)
	}
	// 多余的第二个 JSON 文档：Decode 只读第一个，不检查的话
	// 文件后半截会被静默丢弃。
	if dec.More() {
		return fmt.Errorf("eval: %s 里有多个 JSON 文档，只允许一个", name)
	}
	return nil
}

// lineOf 把字节偏移换算成 1-based 行号。
func lineOf(raw []byte, offset int64) int {
	if offset < 0 || offset > int64(len(raw)) {
		return 1
	}
	return 1 + bytes.Count(raw[:offset], []byte{'\n'})
}
