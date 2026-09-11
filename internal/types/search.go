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

// RRFK 是 RRF 公式的平滑常数，默认 60。
//
// 出自 Cormack、Clarke、Büttcher 的 SIGIR 2009 论文
// 《Reciprocal Rank Fusion outperforms Condorcet and individual Rank Learning Methods》。
// Elasticsearch 的 rank_constant 默认值同样是 60。
//
// k 越大越"扁平化"（各名次贡献趋同），越小越"赢者通吃"。
// 实测 k 在 10~100 区间内对 NDCG@10 的影响不到半个点，所以不用调参。
const RRFK = 60

// SearchResult 是一条检索结果。
//
// 注意它的结构：Chunk 是内嵌的完整片段，而不是只有一个 ID。
// 这样 Agent 拿到结果就能直接读到正文，不需要再回查一次数据库。
type SearchResult struct {
	// Chunk 是命中的文档片段。它的 Embedding 字段不会出现在 JSON 里（见 chunk.go）。
	Chunk Chunk `json:"chunk"`

	// Score 是**本次排序实际使用的分**，含义取决于所处的阶段：
	//   - 单路检索时：等于该通道的原始分（LexicalScore 或 VectorScore）
	//   - RRF 融合后：等于 RRF 分，通常在 0.01 ~ 0.03 之间
	//
	// ⚠️ 这是一个**多刻度**字段，绝对不要跨阶段比较或做阈值过滤。
	// 典型事故：写 score >= 0.5 做阈值过滤，在向量阶段（余弦 -1~1）正常，
	// 切到 RRF 阶段（0.01~0.03）会把**所有结果静默过滤光**，
	// 表现为"混合检索突然没有结果"且不报错。
	//
	// 需要比较时，要么用同一阶段的 Score，要么用下面两个原始分。
	Score float64 `json:"score"`

	// LexicalScore / VectorScore 是各通道的**原始分**，0 表示该通道未召回。
	//
	// 拆出来的目的：让原始分永远可查、可比、可落库。
	// 调试时看它们知道"两路各自的判断有多强"，评测时能按通道分别算指标，
	// 而 Score 保持"本次排序用的分"这一个明确语义。
	LexicalScore float64 `json:"lexical_score"`
	VectorScore  float64 `json:"vector_score"`

	// LexicalRank 和 VectorRank 是这条结果在各通道中的名次，1-based。
	//
	// ⚠️ 值为 0 表示该通道**没有召回**这条结果，它**不是第 0 名**。
	// 绝对不要把 0 直接代入 RRF 公式：
	//     1/(60+0) = 0.016667  >  1/(60+1) = 0.016393
	// 未召回的片段会拿到比真正的第一名更高的分。
	// 请改用 RRFScore()，它在内部处理了这个哨兵值。
	//
	// 保留名次的用途：
	//   - 调试："这条为什么排第一" → 看它两路的名次就明白了
	//   - 评测："只看向量召回的结果准确率如何" → 按名次筛出子集
	LexicalRank int `json:"lexical_rank"`
	VectorRank  int `json:"vector_rank"`
}

// RRFScore 按 RRF 公式计算这条结果的融合分。
//
//	RRF(d) = Σ 1 / (k + rankᵢ(d))
//
// k <= 0 时使用默认值 RRFK。
//
// 这个方法存在的意义是**把哨兵值防护固化下来**：
// rank 为 0（未召回）时贡献 0，而不是 1/(k+0)。
// 直接在调用方写公式很容易漏掉这个判断，而且漏了不会报错，只是排序错。
func (r SearchResult) RRFScore(k int) float64 {
	if k <= 0 {
		k = RRFK
	}
	return rrfContribution(k, r.LexicalRank) + rrfContribution(k, r.VectorRank)
}

// rrfContribution 计算单条通道的 RRF 贡献。
//
// rank <= 0 表示该通道未召回这条结果，贡献为 0。
// 这是整个 RRF 实现里最容易写错的一行。
func rrfContribution(k, rank int) float64 {
	if rank <= 0 {
		return 0
	}
	return 1.0 / float64(k+rank)
}

// MatchedBy 返回召回这条结果的所有通道。
//
// 注意这是一个"算出来"的方法，而不是一个存储的字段。
// 新手容易顺手加一个 MatchedBy []Retriever 字段，但那是冗余状态：
// 它和 LexicalRank / VectorRank 表达的是同一件事，一旦两边不同步
// 就会产生难以排查的 bug。
//
// 能用已有字段算出来的东西，就不要存第二份。这是本项目的通用原则。
//
// 返回值保证非 nil（最坏是空切片），调用方可以直接 range 或 len()。
func (r SearchResult) MatchedBy() []Retriever {
	// 这里刻意用 var + append 而不是 make([]Retriever, 0, 2)：
	// 本方法多数情况下只被用来判断"有几路召回"，
	// append 到 nil 切片在无命中时不会分配，比预分配更省。
	// 真正的分配开销在切片本身，不在 append 会不会扩容。
	var out []Retriever
	if r.LexicalRank > 0 {
		out = append(out, RetrieverLexical)
	}
	if r.VectorRank > 0 {
		out = append(out, RetrieverVector)
	}
	if out == nil {
		return []Retriever{}
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
