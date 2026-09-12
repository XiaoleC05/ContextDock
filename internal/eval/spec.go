// Package eval 定义检索评测集的格式，并提供加载与校验。
//
// 评测集是这个里程碑的「基准尺」——尺子本身错了，后面所有数据都不可信。
// 所以本包的第一职责不是「能读进来」，而是「读进来时把错误拦住」：
// 每条错误都要指到具体是哪一条 query，而不是笼统地说「格式错误」。
//
// 评测集**不是**单元测试。它测的是检索质量这类没有二值对错的东西，
// 样本量小、标注靠人、结论带置信区间——这些局限写在 docs/EVAL.md 里。
package eval

// FormatVersion 是评测集与语料清单的格式版本。
//
// 加载时严格比对：格式一旦不兼容，宁可报错退出，也不要「尽力解析」。
// 评测数据被静默读错，比读不进来危险得多——前者会产出一份看起来正常、
// 实际不可比的数字，而数字一旦写进文档就没人再回头查了。
const FormatVersion = 1

// Group 是评测子集。
//
// 分子集统计是刻意的：这两类查询考验的是**不同的通道**，
// 混在一起平均会把这个差别抹平。
//
//   - GroupToken 测词法通道（精确 token 必须逐字命中）
//   - GroupSynonym 测向量通道（查询与原文不共享任何 bigram）
//
// 一组的失败可以被另一组的成功掩盖，所以报告里必须分开看。
type Group string

const (
	// GroupZH 是中文提问。
	GroupZH Group = "zh"

	// GroupEN 是英文提问，含「纯英文问、命中中文答案」这种跨语言情况。
	GroupEN Group = "en"

	// GroupToken 是精确 token / 错误码。
	//
	// 背景：向量检索在精确 token 上会犯典型错误——`Error 5001` 和 `Error 5002`
	// 语义上极近，向量通道会把错的递给你。这组查询**就是用来暴露这个问题的**：
	// 如果它们靠向量也能全中，说明向量通道没有在区分近似 token，那本身是个信号。
	GroupToken Group = "token"

	// GroupNoRel 是**语料里根本没有答案**的查询。
	//
	// 它的用途和别的组完全不同：别的组量「找得准不准」，
	// 它量「**找不到的时候，分数长什么样**」——这是相关性阈值（#53）
	// 唯一的实验依据。
	//
	// 所以这一组的 `expect` 允许为空，而且通常就是空的：
	// 它期望的结果是**没有结果**，而不是某一条。
	// 返回的每一条都按定义不相关，正好构成"纯噪声"的分数分布。
	GroupNoRel Group = "norel"

	// GroupSynonym 是同义改写——与目标原文**不共享任何字面 token**。
	//
	// 这是词法通道的瞎区：问「怎么让程序跑得快」，文档里写的是「性能优化」，
	// 一个字没对上。这组用来量化「向量通道到底补了多少」。
	GroupSynonym Group = "synonym"
)

// AllGroups 是全部合法子集，**顺序即报告里的展示顺序**。
//
// 刻意用切片而不是 map：报告要按固定顺序输出才能逐次比对，
// 而 map 的遍历顺序是随机的——那会让「同一输入得到同一份输出」这条
// 可复现性要求（#58）直接失效。
var AllGroups = []Group{GroupZH, GroupEN, GroupToken, GroupSynonym, GroupNoRel}

// Valid 判断是不是已知子集。
func (g Group) Valid() bool {
	for _, x := range AllGroups {
		if g == x {
			return true
		}
	}
	return false
}

// Kind 是提问方式。
//
// 分类的意义不在于好看：**间接提问和多跳的失败原因完全不同**。
// 间接提问失败通常意味着向量通道没跟上；多跳失败多半是切分把答案拆散了。
// #41 做失败归因时，这个字段就是第一刀。
type Kind string

const (
	// KindDirect 是直接提问：用文档里的词问文档里的事。
	KindDirect Kind = "direct"

	// KindIndirect 是间接提问：换个概念问同一件事。
	// 它是真正考验向量通道的那一类。
	KindIndirect Kind = "indirect"

	// KindMultiHop 是多跳：答案散在两处或更多地方，一条片段答不全。
	KindMultiHop Kind = "multi-hop"
)

// Valid 判断是不是已知提问方式。
func (k Kind) Valid() bool {
	switch k {
	case KindDirect, KindIndirect, KindMultiHop:
		return true
	}
	return false
}

// Grade 是相关性分级。
//
// 为什么要分级而不是二值：真实标注里大量条目是「沾边但不完全回答」，
// 强行二选一会让标注者在每条上都纠结，反而降低一致性。
// 先如实记下来，怎么算分是 #37 标注规范的事。
type Grade int

const (
	// GradePartial 表示部分相关：主题对得上，但没有直接回答问题。
	GradePartial Grade = 1

	// GradeFull 是完全回答。
	GradeFull Grade = 2
)

// gradeUnset 是「这个字段没写」的哨兵值，不是合法分级。
const gradeUnset Grade = 0

// ⚠️ 合法分级从 1 开始，不是从 0 —— 这是刻意的。
//
// Go 里 int 的零值是 0，而「没写这个字段」和「显式设成 0」读进来之后
// **无法区分**。把 0 留给「没写」（由加载时补成 GradeFull），
// 就绕开了这个问题；分级从 0 开始的话，补默认值的逻辑根本写不出来。
//
// 另一种解法是 *Grade 指针，但那会让每个标注点都要多写一层解引用，
// 而这里丢掉的区分能力（0 到底是不是合法分级）本来也没有意义。

// Expect 是一条「期望命中」。
//
// # 为什么锚定在原文引文上，而不是片段 ID 或片段指纹
//
// 相关性是「原文的哪一段回答了这个问题」——这是一个**与切分无关**的客观事实。
// 而片段是会变的：MaxRunes 参数一调、切分逻辑一改，片段边界就全变了。
// 任何绑在片段上的标识（数据库 ID、内容指纹）在新参数下都指不到原来那段文字，
// 参数扫描会得到一堆假的 0 分——而且**不报错**，只是数字变得毫无意义。
//
// 引文的另一个好处是可验证：加载时必须在原文里**唯一定位**得到，
// 定位不到就是标注错了或语料漂移了，两种情况都该立刻报出来。
type Expect struct {
	// Source 是语料清单（corpus.json）里的标识，**不是文件路径**。
	// 用标识而不是路径，是为了让评测集与语料存放位置解耦。
	Source string `json:"source"`

	// Quote 是原文里的一段连续文本，必须能唯一定位到。
	//
	// 引文要足够长到唯一（几个字通常不够），但也不必抄一整段——
	// 定位到的是「这段文字所在的区域」，不是这段文字本身。
	Quote string `json:"quote"`

	// Occurrence 是引文第几次出现，1-based，缺省 1。
	//
	// 存在的理由是引文可能不唯一（「安装」这类词在文档里会出现多次）。
	// **不唯一时必须显式指定，不允许「默认取第一个」**：那会让标注者
	// 以为自己标的是 A 处、实际测的是 B 处，而结果看起来完全正常。
	Occurrence int `json:"occurrence,omitempty"`

	// Grade 是相关性分级，缺省 GradeFull。取值只能是 GradePartial / GradeFull。
	Grade Grade `json:"grade,omitempty"`

	// Start 和 End 是定位结果：引文在原文中的 rune 区间，左闭右开。
	//
	// ⚠️ 由 Resolve 填充，**不来自 JSON**。加载阶段一定调过 Resolve，
	// 所以校验通过之后这两个值必然有效。
	//
	// 单位是 rune 不是 byte，与 types.Chunk 的 StartOffset / EndOffset 一致——
	// 命中判定要跨这两者做区间比较，单位不一致会静默算错。
	Start int `json:"-"`
	End   int `json:"-"`
}

// Query 是一条评测查询。
type Query struct {
	// ID 是稳定的人工标识（如 "zh-003"），用于在报告和失败分析里指代这一条。
	ID string `json:"id"`

	// Query 是喂给检索的原始查询串。
	Query string `json:"query"`

	// Kind 是提问方式，可省略。省略时不参与分类统计，但 query 仍然计入总分。
	Kind Kind `json:"kind,omitempty"`

	// Expect 是期望命中。
	//
	// 除了 `norel` 组，其余组**至少一条**：为空会让这条 query 在任何指标里
	// 都恒为「没召回」，从而静默拉低整体分数——所以校验时按错误拦掉。
	//
	// `norel` 组按定义就是空的：它期望的是"没有结果"。
	Expect []Expect `json:"expect"`

	// RequireLexical 表示这条查询**必须由词法通道命中才算通过**。
	//
	// 精确 token 组（#35）用它：`Error 5001` 和 `Error 5002` 语义上极近，
	// 向量通道会把错的递给你。所以「靠向量蒙对」不算这条通过——
	// 那恰恰说明向量没有在区分近似 token，是需要单独指出来的信号。
	//
	// 注意它不是「过滤条件」：评测**照样**统计这条的向量召回，
	// 只是报告里会把它标成「仅向量命中」，而不是计入通过。
	RequireLexical bool `json:"require_lexical,omitempty"`

	// Note 是标注理由，写给复核用。
	//
	// #37 要求「隔一段时间重标一遍比对一致性」，而重标时最难恢复的是
	// **当时为什么判它相关**。没有 Note，复核只能凭印象，等于重标一遍没差别。
	Note string `json:"note,omitempty"`

	// File 是这条 query 来自哪个文件，由加载器填充，用于报错定位。
	File string `json:"-"`
}

// QuerySet 是一个评测子集文件。
type QuerySet struct {
	// Version 必须等于 FormatVersion。
	Version int `json:"version"`

	// Group 必须与文件名一致（zh.json → zh），否则会出现
	// 「文件叫 zh.json 但统计进 synonym 组」这种只在报告里才看得出的错位。
	Group Group `json:"group"`

	// Title 是展示用名称。
	Title string `json:"title"`

	// Queries 是子集内的全部查询。
	Queries []Query `json:"queries"`

	// SHA256 是**文件内容**的指纹，由加载器填充，用于可复现性（#58）。
	//
	// 记它而不只记查询条数：条数相同、内容不同的两份评测集，
	// 跑出来的数字差别可能很大——而报告上只写着"51 条"，看不出区别。
	SHA256 string `json:"sha256,omitempty"`
}
