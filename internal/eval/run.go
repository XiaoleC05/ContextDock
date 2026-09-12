package eval

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/XiaoleC05/ContextDock/internal/config"
	"github.com/XiaoleC05/ContextDock/internal/embed"
	"github.com/XiaoleC05/ContextDock/internal/retrieve"
	"github.com/XiaoleC05/ContextDock/internal/service"
	"github.com/XiaoleC05/ContextDock/internal/store"
	"github.com/XiaoleC05/ContextDock/internal/tokenize"
	"github.com/XiaoleC05/ContextDock/internal/types"
)

// Channel 是评测对比的三条路径。
type Channel string

const (
	// ChannelLexical 只用关键词通道（BM25）。
	ChannelLexical Channel = "lexical"

	// ChannelVector 只用向量通道（余弦相似度）。
	ChannelVector Channel = "vector"

	// ChannelFused 是 RRF 融合。
	ChannelFused Channel = "fused"
)

// AllChannels 是全部通道，顺序即报告里的展示顺序。
//
// 固定顺序是必须的：报告要逐次比对（可复现性的前提），
// 而 map 遍历顺序随机，会让同一份数据两次输出的行序不同。
var AllChannels = []Channel{ChannelLexical, ChannelVector, ChannelFused}

// Options 是一次评测运行的参数。
//
// 全部字段都能从命令行覆盖，因为参数扫描（#43/#44）靠的就是
// 「同一份评测集 + 不同参数 → 可比的数字」。
type Options struct {
	// TopK 是每条查询保留的结果条数，也就是 recall@k 的 k。
	TopK int `json:"topk"`

	// RRFK 是 RRF 的平滑常数。
	RRFK int `json:"rrf_k"`

	// Mult 是融合前每路的候选放大倍数。
	Mult int `json:"mult"`

	// MaxRunes / Overlap 是切分参数。
	MaxRunes int `json:"max_runes"`
	Overlap  int `json:"overlap"`

	// NDCGK 是 NDCG 的截断位置。
	NDCGK int `json:"ndcg_k"`

	// TokenizeScheme 是 CJK 分词方案（#45 的对比实验用）。
	// 空值表示用默认（bigram）。
	TokenizeScheme tokenize.Scheme `json:"tokenize_scheme,omitempty"`

	// ContextNeighbors 是上下文扩展的相邻片段数（#49）。
	// > 0 时报告里会多出一列 ExpandedRecall，用来量化答案完整性的提升。
	ContextNeighbors int `json:"context_neighbors,omitempty"`

	// KeepDetail 为真时在报告里保留逐条查询的明细。
	KeepDetail bool `json:"-"`
}

// describe 把检索结果转成报告用的位置描述。
func describe(results []types.SearchResult, expects []Expect) []RetrievedItem {
	out := make([]RetrievedItem, 0, len(results))
	for _, r := range results {
		relevant := false
		for _, e := range expects {
			if covers(r, e) {
				relevant = true
				break
			}
		}
		out = append(out, RetrievedItem{
			Source:   r.Chunk.Metadata[types.MetadataKeySource],
			Start:    r.Chunk.StartOffset,
			End:      r.Chunk.EndOffset,
			Score:    r.Score,
			LexRank:  r.LexicalRank,
			VecRank:  r.VectorRank,
			Relevant: relevant,
		})
	}
	return out
}

// Normalize 补全零值。
func (o Options) Normalize() Options {
	if o.TopK <= 0 {
		o.TopK = config.DefaultTopK
	}
	if o.RRFK <= 0 {
		o.RRFK = types.RRFK
	}
	if o.Mult <= 0 {
		// 用**生产默认值**而不是 1。
		//
		// 基线必须跑在与线上一致的参数上，否则后面所有"改进"都是在
		// 和一个不存在的配置比。倍数=1 意味着融合前每路只取 topK 条，
		// 两路各自的第一名可能都不在对方的候选里，融合先天吃亏——
		// 那样测出来的不是"融合好不好"，而是"候选不够时融合好不好"。
		o.Mult = retrieve.DefaultCandidateMultiplier
	}
	if o.MaxRunes <= 0 {
		o.MaxRunes = config.DefaultChunkMaxRunes
	}
	// ⚠️ 判据是**负数**不是 <= 0。
	//
	// 与 chunk.Config 同一套语义：0 是合法的重叠值（完全不重叠）。
	// 写成 <= 0 的话，`-overlap 0` 会被静默换成 60——
	// 参数扫描里 Overlap=0 那一列就永远是假的，而报告上看不出来。
	if o.Overlap < 0 {
		o.Overlap = config.DefaultChunkOverlap
	}
	if o.NDCGK <= 0 {
		o.NDCGK = NDCGK
	}
	if !o.TokenizeScheme.Valid() {
		o.TokenizeScheme = tokenize.SchemeBigram
	}
	return o
}

// Latency 是单次检索的耗时分布（毫秒）。
//
// 用 P50/P95 而不是平均值：检索耗时是长尾分布，平均值会被少数慢查询
// 拉高，既不代表"典型体验"也不代表"最差体验"。
//
// ⚠️ 它包含**查询嵌入**的耗时。暖缓存时那是一次磁盘读（亚毫秒），
// 于是这个数主要反映检索本身；冷缓存时会被网络往返淹没。
// 改候选倍数这类参数只会影响检索部分，所以比较**必须**在暖缓存下做——
// 报告里的缓存命中数就是用来判断这一点的。
type Latency struct {
	P50Ms float64 `json:"p50_ms"`
	P95Ms float64 `json:"p95_ms"`
	MaxMs float64 `json:"max_ms"`
}

// CorpusStat 是一份语料在本次参数下的规模。
type CorpusStat struct {
	Source string `json:"source"`
	Chunks int    `json:"chunks"`
}

// QueryDetail 是一条查询的明细，供失败分析（#41）使用。
type QueryDetail struct {
	ID    string `json:"id"`
	Query string `json:"query"`
	Group Group  `json:"group"`

	// RequireLexical 表示这条查询必须由词法通道命中才算通过。
	// 报告据此单独拉出「精确 token」那组做词法 / 向量对照。
	RequireLexical bool `json:"require_lexical,omitempty"`

	// ByChannel 是三条路径各自的得分。
	ByChannel map[Channel]QueryScore `json:"by_channel"`

	// Missed 是**融合路径下**没被召回的期望下标。
	//
	// 只记融合路径的：融合是最终交付给用户的东西，
	// 它没召回的条目才是真正需要归因的失败。
	// 单路各自的失败在 #40 的证伪实验里单独看。
	Missed []int `json:"missed,omitempty"`

	// Retrieved 是三条路径各自实际返回了什么，按通道分。
	//
	// 指标只告诉你「差多少」，这个字段才告诉你「差在哪」：
	// 是没召回到，还是召回了但被挤出了 top-k。
	// #40 查「融合为什么变差」和 #41 做失败归因都要靠它。
	Retrieved map[Channel][]RetrievedItem `json:"retrieved,omitempty"`
}

// RetrievedItem 是一条实际返回的片段在原文里的位置。
//
// 只记位置和分数，不记正文：正文在语料里，按 (source, start, end)
// 随时能取回来，而报告里塞进 196 个片段会大到没法看。
type RetrievedItem struct {
	Source string `json:"source"`
	Start  int    `json:"start"`
	End    int    `json:"end"`

	// Score 是本次排序用的分（单路是原始分，融合是 RRF 分）。
	Score float64 `json:"score"`

	// LexRank / VecRank 是这条在各通道里的名次，0 表示未被该通道召回。
	LexRank int `json:"lex_rank,omitempty"`
	VecRank int `json:"vec_rank,omitempty"`

	// Relevant 表示它命中了任一期望。
	Relevant bool `json:"relevant,omitempty"`
}

// Report 是一次评测的完整结果。
type Report struct {
	Options Options `json:"options"`

	// Corpus 是本次导入的语料规模。
	Corpus []CorpusStat `json:"corpus"`

	// TotalChunks 是本次导入产生的片段总数。
	TotalChunks int `json:"total_chunks"`

	// TextBytes / VectorBytes 是索引体积。切分扫描要靠它们识别
	// 「切得越碎 recall 越高、但索引爆炸」这类病态参数。
	TextBytes   int64 `json:"text_bytes"`
	VectorBytes int64 `json:"vector_bytes"`

	// ImportMs 是导入并建好内存索引的墙钟耗时（毫秒）。
	//
	// ⚠️ 它包含**嵌入耗时**。嵌入缓存命中时这部分接近 0，
	// 于是这个数主要反映切分与建索引的成本；
	// 冷缓存时它会被网络往返淹没，不能拿来比较切分参数。
	// 报告里同时给出缓存命中数，就是为了让读的人能判断这一点。
	ImportMs int64 `json:"import_ms"`

	// Origins 是语料清单，记进报告以满足可复现性要求（#58）。
	Origins []CorpusFile `json:"origins"`

	// Embedder 是嵌入缓存/上游的命中统计，零值表示没启用缓存。
	Embed embed.Stats `json:"embed_cache"`

	// Groups 按 AllGroups 顺序排列。
	Groups []GroupReport `json:"groups"`

	// Latency 是单次检索的耗时分布。
	Latency Latency `json:"latency"`

	// Overall 是全部查询的汇总。
	Overall map[Channel]Aggregate `json:"overall"`

	// ExpandedRecall 是「结果**连同它们的相邻片段**」的召回率，按通道分。
	//
	// 它与 Overall[ch].Recall 的差，就是上下文扩展带来的答案完整性提升（#49）。
	// Options.ContextNeighbors == 0 时为空。
	//
	// 只报召回率不报 NDCG/MRR：相邻片段没有名次，给它们编一个名次
	// 只会让指标变成数字游戏。
	ExpandedRecall map[Channel]float64 `json:"expanded_recall,omitempty"`

	// Detail 是逐条明细，Options.KeepDetail 为真时才有内容。
	Detail []QueryDetail `json:"detail,omitempty"`
}

// GroupReport 是一个子集在三路上的成绩。
type GroupReport struct {
	Group     Group                 `json:"group"`
	Title     string                `json:"title"`
	N         int                   `json:"n"`
	ByChannel map[Channel]Aggregate `json:"by_channel"`
}

// Run 用给定参数跑一次完整评测。
//
// # 为什么用内存存储，而不是 pgvector
//
// 两个理由，第二个才是关键：
//
//  1. 语料要反复重灌。参数扫描是几十种配置 × 近两百个片段，
//     每次都写库、建索引，既慢又会把库搅脏。
//
//  2. **检索路径目前是暴力扫描**（见 README「已知限制」）：
//     向量检索在内存里对全部向量算一遍余弦相似度，查库只是启动时
//     把数据读进来。也就是说，换存储**不改变任何检索结果**。
//
// 第 2 条意味着这里测出来的数字就是线上会得到的数字。等到检索路径
// 真的用上 pgvector 的 HNSW 索引那天，这个前提就不成立了——
// 那时评测必须改成对着真实数据库跑，否则会漏掉近似索引带来的召回损失。
//
// 副产品是评测**不依赖 Docker**，可以进 CI（#57）。
func Run(ctx context.Context, suite *Suite, emb embed.Embedder, opt Options, logf func(string, ...any)) (*Report, error) {
	opt = opt.Normalize()
	if logf == nil {
		logf = func(string, ...any) {}
	}

	cfg := &config.Config{
		ChunkMaxRunes:  opt.MaxRunes,
		ChunkOverlap:   opt.Overlap,
		TopK:           opt.TopK,
		SearchTimeout:  60 * time.Second,
		UseMemoryStore: true,
		PoolMaxConns:   8,
		EmbeddingDim:   types.EmbeddingDim,
		TokenizeScheme: opt.TokenizeScheme,
	}
	svc, err := service.New(cfg, emb, store.NewMemory())
	if err != nil {
		return nil, fmt.Errorf("eval: 组装服务失败: %w", err)
	}
	defer func() { _ = svc.Close() }()

	// ---- 导入语料 ----
	importStart := time.Now()
	sources := suite.Sources()
	stats := make([]CorpusStat, 0, len(sources))
	for _, src := range sources {
		// ⚠️ 必须用 suite 里的字符串，不要重新读磁盘文件。
		// 引文定位出的 rune 区间是按这份**已归一化换行**的文本算的，
		// 换成 CRLF 的原始文件会让区间整体错位——而代码不报错，
		// 只是命中率莫名偏低，排查会从检索算法查起，方向全错。
		text, ok := suite.Content(src)
		if !ok {
			return nil, fmt.Errorf("eval: 语料 %s 没有内容", src)
		}

		logf("导入 %s（%d 字符）…", src, len([]rune(text)))
		res, err := svc.Import(ctx, &types.Document{
			Title:   src,
			Source:  src,
			Content: text,
		})
		if err != nil {
			// 嵌入失败必须**直接失败**，不能降级继续。
			// 向量通道少了一部分片段，后面所有关于向量和融合的数字
			// 都是错的——而报告照样会打印出来，看起来一切正常。
			return nil, fmt.Errorf("eval: 导入 %s 失败: %w", src, err)
		}
		stats = append(stats, CorpusStat{Source: src, Chunks: res.ChunkCount})
	}

	// 把 service 里的索引规模报出来，确认导入确实落到了索引上。
	total, embedded := svc.Stats()
	idx := svc.IndexStats()
	importMs := time.Since(importStart).Milliseconds()
	logf("索引就绪：%d 个片段，其中 %d 个带向量，耗时 %dms", total, embedded, importMs)
	if embedded < total {
		return nil, fmt.Errorf("eval: %d/%d 个片段没有向量，向量通道不完整，"+
			"此时任何关于向量或融合的数字都不可信", total-embedded, total)
	}

	// ---- 跑查询 ----
	rep := &Report{
		Options:     opt,
		Corpus:      stats,
		Origins:     suite.Corpus.Files,
		TotalChunks: idx.Chunks,
		TextBytes:   idx.TextBytes,
		VectorBytes: idx.VectorBytes,
		ImportMs:    importMs,
		Overall:     make(map[Channel]Aggregate, len(AllChannels)),
	}
	// 嵌入统计在全部查询跑完之后再读——中途读会漏掉后半段。
	// 用接口断言而不是把 Cache 类型写进签名：评测器只依赖 embed.Embedder，
	// 有没有缓存是调用方的事。
	var statsOf func() embed.Stats
	if c, ok := emb.(interface{ Stats() embed.Stats }); ok {
		statsOf = c.Stats
	}

	// 上下文扩展后的召回累计（#49）。相邻片段没有名次，
	// 所以只累加召回率，不碰 MRR / NDCG。
	expandedSum := make(map[Channel]float64, len(AllChannels))

	// 每次检索的耗时，跑完查询后算分位数。
	// 预分配到查询总数，避免在计时循环里触发扩容——
	// 扩容本身会体现在被测量的那段代码旁边，虽然不在里面，但没必要冒这个险。
	latencies := make([]float64, 0, len(suite.Queries()))

	for _, set := range suite.Sets {
		gr := GroupReport{
			Group:     set.Group,
			Title:     set.Title,
			N:         len(set.Queries),
			ByChannel: make(map[Channel]Aggregate, len(AllChannels)),
		}
		for i := range set.Queries {
			q := &set.Queries[i]

			searchStart := time.Now()
			runs, err := svc.SearchChannels(ctx, q.Query, opt.TopK, opt.RRFK, opt.Mult)
			latencies = append(latencies, float64(time.Since(searchStart).Microseconds())/1000)
			if err != nil {
				return nil, fmt.Errorf("eval: 查询 %s 失败: %w", q.ID, err)
			}
			if runs.Degraded != "" {
				return nil, fmt.Errorf("eval: 查询 %s 发生降级（%s）——"+
					"降级会让向量与融合的数字失去意义，不能继续", q.ID, runs.Degraded)
			}

			detail := QueryDetail{ID: q.ID, Query: q.Query, Group: set.Group,
				RequireLexical: q.RequireLexical,
				ByChannel:      make(map[Channel]QueryScore, len(AllChannels))}

			for _, ch := range AllChannels {
				var results []types.SearchResult
				switch ch {
				case ChannelLexical:
					results = runs.Lexical
				case ChannelVector:
					results = runs.Vector
				case ChannelFused:
					results = runs.Fused
				}
				sc := Score(q.Expect, results, opt.NDCGK)
				detail.ByChannel[ch] = sc

				if opt.ContextNeighbors > 0 {
					// 把每条结果的相邻片段也放进候选集，重新算一次召回。
					// 这一步回答的是「Agent 拿到的东西够不够完整」，
					// 而不是「检索排得好不好」——所以只看召回。
					expandedSum[ch] += Score(q.Expect,
						expandWithNeighbors(svc, results, opt.ContextNeighbors),
						opt.NDCGK).Recall
				}
				if opt.KeepDetail {
					if detail.Retrieved == nil {
						detail.Retrieved = make(map[Channel][]RetrievedItem, len(AllChannels))
					}
					detail.Retrieved[ch] = describe(results, q.Expect)
				}

				agg := gr.ByChannel[ch]
				agg.Add(sc)
				gr.ByChannel[ch] = agg

				oa := rep.Overall[ch]
				oa.Add(sc)
				rep.Overall[ch] = oa
			}

			// 融合路径没命中的期望，供 #41 逐条归因。
			fused := detail.ByChannel[ChannelFused]
			hit := make(map[int]bool, len(fused.Matched))
			for _, m := range fused.Matched {
				hit[m] = true
			}
			for j := range q.Expect {
				if !hit[j] {
					detail.Missed = append(detail.Missed, j)
				}
			}

			if opt.KeepDetail {
				rep.Detail = append(rep.Detail, detail)
			}
		}
		rep.Groups = append(rep.Groups, gr)
	}

	if opt.ContextNeighbors > 0 && len(rep.Overall) > 0 {
		rep.ExpandedRecall = make(map[Channel]float64, len(AllChannels))
		for _, ch := range AllChannels {
			rep.ExpandedRecall[ch] = expandedSum[ch] / float64(rep.Overall[ch].Queries)
		}
	}

	if statsOf != nil {
		rep.Embed = statsOf()
	}
	rep.Latency = percentiles(latencies)

	// ⚠️ 必须先取出来再放回去：map 的元素不可取址，
	// 而 Mean() 是指针接收者，直接写 rep.Overall[ch].Mean() 编译不过。
	for ch := range rep.Overall {
		v := rep.Overall[ch]
		rep.Overall[ch] = v.Mean()
	}
	for i := range rep.Groups {
		for ch := range rep.Groups[i].ByChannel {
			v := rep.Groups[i].ByChannel[ch]
			rep.Groups[i].ByChannel[ch] = v.Mean()
		}
	}
	return rep, nil
}

// percentiles 计算耗时分布。
//
// 用最近秩（nearest-rank）而不是插值：插值出来的"P95"可能不是任何一次
// 真实检索的耗时，而看这个数的人想知道的恰恰是"最慢的那几次有多慢"。
// 样本量只有几十条时，插值的意义也不大。
func percentiles(ms []float64) Latency {
	if len(ms) == 0 {
		return Latency{}
	}
	// 复制一份再排序：调用方传进来的切片不该被这个函数改掉。
	s := append([]float64(nil), ms...)
	sort.Float64s(s)
	at := func(p float64) float64 {
		i := int(math.Ceil(p/100*float64(len(s)))) - 1
		return s[max(0, min(i, len(s)-1))]
	}
	return Latency{P50Ms: at(50), P95Ms: at(95), MaxMs: s[len(s)-1]}
}

// expandWithNeighbors 把每条结果的相邻片段并进候选集（#49）。
//
// # 为什么相邻片段不参与排序
//
// 它们不是"检索到的"，是"顺带带出来的"。给它们编一个名次会让 MRR / NDCG
// 变成数字游戏——把邻居排前面就能刷高 MRR，而那不反映任何检索能力。
// 所以这个函数产出的集合**只用来看召回**。
//
// # 为什么去重
//
// 相邻片段会互相重叠（一个片段可能同时是两条结果的"前一段"），
// 重复计入不会改变召回（命中与否是布尔量），但会让集合白白变大。
func expandWithNeighbors(svc *service.Service, results []types.SearchResult, n int) []types.SearchResult {
	out := make([]types.SearchResult, 0, len(results)*(2*n+1))
	seen := make(map[string]struct{}, cap(out))

	add := func(c types.Chunk) {
		k := c.StableKey()
		if _, dup := seen[k]; dup {
			return
		}
		seen[k] = struct{}{}
		out = append(out, types.SearchResult{Chunk: c})
	}

	for _, r := range results {
		add(r.Chunk)
		prev, next := svc.Neighbors(r.Chunk, n, n)
		for _, c := range prev {
			add(c)
		}
		for _, c := range next {
			add(c)
		}
	}
	return out
}
