package retrieve

import (
	"math"

	"github.com/XiaoleC05/ContextDock/internal/tokenize"
	"github.com/XiaoleC05/ContextDock/internal/types"
)

// BM25 的默认参数。
//
// 1.2 和 0.75 是 Elasticsearch / Lucene 的默认值，bleve 源码里也是这两个数
// （注释写着 "as per elastic search's implementation"）。
const (
	DefaultK1 = 1.2
	DefaultB  = 0.75
)

// BM25 是内存版的 BM25 关键词检索。
//
// 它**不是并发安全的**：Index 会重建内部状态，应当在一个 goroutine 里建好，
// 之后并发调用 Search。Search 本身只读，可以并发。
type BM25 struct {
	tk    *tokenize.Tokenizer
	k1, b float64

	docs  []types.Chunk
	tfs   []map[string]int // 每篇文档的词频
	df    map[string]int   // 每个词出现在多少篇文档里
	avgdl float64          // 平均文档长度（token 数）

	// docLens[i] 是第 i 篇文档的 token 数，即 BM25 里的 |D|。
	//
	// 在索引时算好存下来，而不是每次查询时遍历词频 map 去数。
	// 后者是查询热路径上的 O(词数) 操作，白白浪费。
	docLens []int

	// normCache[i] = k1 * (1 - b + b * docLens[i] / avgdl)
	//
	// 这一项只和文档有关、和查询无关，所以在索引时算一次。
	// 放在查询循环里的话，每个文档对每个查询词项都要重算一次除法。
	normCache []float64
}

// NewBM25 创建一个 BM25 检索器。
func NewBM25() *BM25 {
	return &BM25{
		tk: tokenize.New(),
		k1: DefaultK1,
		b:  DefaultB,
	}
}

// WithParams 覆盖 k1 和 b，用于做参数对比实验。
//
// k1 控制词频饱和：越大，词频高的文档越占优，趋近线性。
// b 控制长度归一化：0 表示不归一化，1 表示完全归一化。
func (m *BM25) WithParams(k1, b float64) *BM25 {
	m.k1, m.b = k1, b
	return m
}

// Index 重建索引，丢弃之前的状态。
//
// ⚠️ 建索引用的文本必须走 Chunk.IndexText()，**不能直接读 Content**。
// IndexText 会把标题面包屑拼进去。如果这里用 Content、而 Embedder 用 IndexText，
// 两路检索搜的就不是同一个文本，RRF 融合质量下降且极难排查。
// 详见 docs/DESIGN.md §8。
func (m *BM25) Index(chunks []types.Chunk) {
	m.docs = make([]types.Chunk, len(chunks))
	copy(m.docs, chunks)

	m.tfs = make([]map[string]int, len(chunks))
	m.df = make(map[string]int, len(chunks)*8)
	m.docLens = make([]int, len(chunks))
	m.normCache = make([]float64, len(chunks))

	var totalTokens int
	for i, c := range chunks {
		tokens := m.tk.Tokenize(c.IndexText())
		tf := make(map[string]int, len(tokens))
		for _, t := range tokens {
			tf[t]++
		}
		m.tfs[i] = tf
		m.docLens[i] = len(tokens)
		totalTokens += len(tokens)

		// df 统计的是「出现在多少篇文档里」，不是总词频，
		// 所以同一篇文档里重复出现的词只算一次。
		for t := range tf {
			m.df[t]++
		}
	}

	if len(chunks) > 0 {
		m.avgdl = float64(totalTokens) / float64(len(chunks))
	} else {
		m.avgdl = 0
	}

	// 第二遍：avgdl 要等全部文档统计完才知道，所以归一化因子只能在这里算。
	for i, dl := range m.docLens {
		if m.avgdl > 0 {
			m.normCache[i] = m.k1 * (1 - m.b + m.b*float64(dl)/m.avgdl)
		} else {
			m.normCache[i] = m.k1
		}
	}
}

// Len 返回已索引的文档数。
func (m *BM25) Len() int { return len(m.docs) }

// Search 返回与 query 最相关的前 topK 条（topK <= 0 表示不截断）。
//
// 返回的 SearchResult 里填好了 LexicalRank（1-based）。
func (m *BM25) Search(query string, topK int) []types.SearchResult {
	if len(m.docs) == 0 {
		return []types.SearchResult{}
	}

	qTokens := m.tk.Tokenize(query)
	if len(qTokens) == 0 {
		return []types.SearchResult{}
	}

	// 查询词去重：BM25 是对查询中的词项**集合**求和，
	// 同一个词在查询里出现多次不应重复计分。
	//
	// ⚠️ 去重必须在这里一次做完，不能把 seen 放进下面的文档循环里——
	// 那样第一篇文档会把所有查询词标记成"已见过"，从第二篇起全部跳过，
	// 结果是只有第一篇文档被正确打分。**而且不报错**，只是排序明显不对。
	uniq := make([]string, 0, len(qTokens))
	seen := make(map[string]bool, len(qTokens))
	for _, q := range qTokens {
		if !seen[q] {
			seen[q] = true
			uniq = append(uniq, q)
		}
	}

	n := float64(len(m.docs))
	items := make([]scored, 0, len(m.docs))
	for i, doc := range m.docs {
		tf := m.tfs[i]
		// 长度归一化因子在索引时就算好了，这里直接取。
		// 之前是每次查询都遍历词频 map 数 token 数、再算一遍除法。
		norm := m.normCache[i]

		var score float64
		for _, q := range uniq {
			f := float64(tf[q])
			if f == 0 {
				// 词频为 0 直接跳过。
				//
				// 这是**纯性能优化**，不是正确性保障：
				// 分子的 f*(k1+1) 本来就是 0，不跳过算出来也是 0。
				// 跳过只是省掉一次 IDF 的 log 和一次除法。
				//
				// 注意：不要因为「未登录词 IDF 很大」就以为这里能防住什么——
				// df 为 0 时 IDF 确实大，但乘上 tf=0 就归零了。
				continue
			}
			score += m.idf(q, n) * (f * (m.k1 + 1)) / (f + norm)
		}

		if score > 0 {
			items = append(items, scored{chunk: doc, score: score})
		}
	}

	items = sortAndTrim(items, topK)

	out := make([]types.SearchResult, len(items))
	for i, it := range items {
		out[i] = types.SearchResult{
			Chunk:        it.chunk,
			Score:        it.score,
			LexicalScore: it.score,
			LexicalRank:  i + 1, // 1-based
		}
	}
	return out
}

// idf 是 Lucene 变体的 IDF。
//
//	IDF(t) = ln(1 + (N - n + 0.5) / (n + 0.5))
//
// ⚠️ 必须用这个变体，不能用 Robertson 原始版 ln((N-n+0.5)/(n+0.5))。
// 原始版在 n > N/2（词出现在一半以上文档里）时结果为**负数**，
// 会让总分变成负的，排序直接错乱。Lucene、Elasticsearch、bleve 用的都是加了 1 的版本。
func (m *BM25) idf(term string, n float64) float64 {
	df := float64(m.df[term])
	return math.Log(1 + (n-df+0.5)/(df+0.5))
}

// 文档长度 |D| 用 **token 数**而不是字符数或字节数：
// BM25 的长度归一化建立在词数上。中文走 bigram，
// 所以「检索系统」算 3 个 token 而不是 4 个字符。
//
// 这个值在 Index 时一次性算好存进 docLens —— 之前是在每次查询里
// 遍历词频 map 现数，属于查询热路径上的浪费。
