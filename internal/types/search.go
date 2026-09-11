package types

// Retriever 标识一条检索通道（也就是"用什么方式找"）。
//
// 用自定义字符串类型而不是 int 常量，好处是打印日志和序列化成 JSON 时
// 直接就是可读的 "lexical" / "vector"，不用再写一层映射表。
type Retriever string

const (
	// RetrieverLexical 是关键词检索通道（BM25）。
	RetrieverLexical Retriever = "lexical"

	// RetrieverVector 是向量检索通道（余弦相似度）。
	RetrieverVector Retriever = "vector"
)

// SearchResult 是一条检索结果。
//
// 注意它的结构：Chunk 是内嵌的完整片段，而不是只有一个 ID。
// 这样 Agent 拿到结果就能直接读到正文，不需要再回查一次数据库。
type SearchResult struct {
	// Chunk 是命中的文档片段。它的 Embedding 字段不会出现在 JSON 里（见 chunk.go）。
	Chunk Chunk `json:"chunk"`

	// Score 是最终排序分。
	//
	// 它的含义取决于所处的阶段：
	//   - 单路检索时：就是该通道的原始分（BM25 分或余弦相似度）
	//   - RRF 融合后：是 RRF 分，通常落在 0.01 ~ 0.03 这个量级
	// 所以不要在代码里假定 Score 有固定的取值范围。
	Score float64 `json:"score"`

	// LexicalRank 和 VectorRank 是这条结果在各通道中的名次，1-based。
	// 值为 0 表示该通道没有召回这条结果。
	//
	// 为什么要把名次留在结果里？
	//   - 调试："这条为什么排第一" → 看它两路的名次就明白了
	//   - 评测："只看向量召回的结果准确率如何" → 按名次筛出子集
	//   - RRF 本身就是基于名次计算的，留着便于复核
	LexicalRank int `json:"lexical_rank"`

	VectorRank int `json:"vector_rank"`
}

// MatchedBy 返回召回这条结果的所有通道。
//
// 注意这是一个"算出来"的方法，而不是一个存储的字段。
// 新手容易顺手加一个 MatchedBy []Retriever 字段，但那是冗余状态：
// 它和 LexicalRank / VectorRank 表达的是同一件事，一旦两边不同步
// 就会产生难以排查的 bug。
//
// 能用已有字段算出来的东西，就不要存第二份。这是本项目的通用原则。
func (r SearchResult) MatchedBy() []Retriever {
	// 预分配容量 2，避免 append 过程中扩容。
	// 这是一个很小的优化，但在会被高频调用的函数里是好习惯。
	out := make([]Retriever, 0, 2)
	if r.LexicalRank > 0 {
		out = append(out, RetrieverLexical)
	}
	if r.VectorRank > 0 {
		out = append(out, RetrieverVector)
	}
	return out
}

// MatchedByBoth 表示这条结果同时被两路召回。
//
// 这在混合检索里是一个很强的相关信号：关键词和语义都认为它相关，
// 通常比只有单路召回的更可靠。第四天做 RRF 时会用到这个判断。
func (r SearchResult) MatchedByBoth() bool {
	return r.LexicalRank > 0 && r.VectorRank > 0
}
