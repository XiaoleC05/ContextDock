package service

import (
	"context"
	"fmt"

	"github.com/XiaoleC05/ContextDock/internal/retrieve"
	"github.com/XiaoleC05/ContextDock/internal/types"
)

// ChannelRuns 是一次检索在三条路径上各自的完整结果。
//
// 三份结果**来自同一次查询向量**：如果每条路径各嵌一次查询，
// 它们比的就不是同一个东西了，而向量嵌入有微小抖动，
// 那点抖动在"融合比单路好多少"这种以百分点计的结论里不能忽略。
type ChannelRuns struct {
	Query string

	// Lexical 是只用关键词通道的结果（BM25 原始名次）。
	Lexical []types.SearchResult

	// Vector 是只用向量通道的结果（余弦相似度原始名次）。
	Vector []types.SearchResult

	// Fused 是 RRF 融合后的结果。
	Fused []types.SearchResult

	// Degraded 非空表示向量通道不可用，此时 Vector 为空、Fused 退化成词法结果。
	Degraded string
}

// SearchChannels 跑一次检索，把**两条单路**的结果和融合结果一起返回。
//
// 这是给评测与诊断用的入口，MCP 工具不走这条路——它只要融合后的结果。
//
// 为什么必须能拿到单路：整个项目的核心假设是「RRF 融合比任何单路都好」，
// 而验证这个假设唯一的方法就是**把单路拿出来做同样的测量**。
// 只暴露融合结果的话，这个假设永远无法被检验。
//
// rrfK / mult <= 0 时用默认值。
func (s *Service) SearchChannels(ctx context.Context, query string, topK, rrfK, mult int) (*ChannelRuns, error) {
	if topK <= 0 {
		topK = s.cfg.TopK
	}

	// 取快照，理由同 Service.Search：安全性来自「已发布对象永不改写」，
	// 不是来自把锁盖住整个检索过程。见 Service.idxMu 的说明。
	bm25, vecIdx := s.snapshot()

	out := &ChannelRuns{Query: query}

	bm25Empty := bm25.Len() == 0
	vecEmpty := vecIdx.Len() == 0
	if bm25Empty && vecEmpty {
		out.Degraded = "索引为空"
		return out, nil
	}

	// 生成查询向量：**刻意不持锁**，这是网络调用，可能几百毫秒。
	// 持读锁的话，并发的 Rebuild（要拿写锁）得等这么久，
	// 而 Rebuild 期间所有检索都会被卡住。
	var queryVec []float32
	if !vecEmpty {
		vecs, err := s.embedder.Embed(ctx, []string{query})
		switch {
		case err != nil:
			out.Degraded = fmt.Sprintf("嵌入失败: %v", err)
		case len(vecs) != 1 || len(vecs[0]) != types.EmbeddingDim:
			out.Degraded = "嵌入返回的维度不正确"
		default:
			queryVec = vecs[0]
		}
	} else {
		out.Degraded = "没有可用的向量索引"
	}

	// 检索全程**不持锁**，用上面那份快照。理由见 Service.idxMu 的说明。
	//
	// ⚠️ 单路向量检索（下面 out.Vector）和喂给 Hybrid 的向量检索
	// 必须是**同一个** searcher。这两处曾经各写各的，将来接入
	// 数据库后端时只改一处，就会出现「单路列测旧路径、融合列测新路径」
	// 的自相矛盾，而报告照样正常打印。
	//
	// 单路结果取 topK，不放大。
	//
	// 这里**故意**不取 topK*mult：单路基线要回答的是「如果只用这一路，
	// 用户会拿到什么」。放大候选再截断会给出比真实单路更好的结果，
	// 从而把融合的优势压低、得出偏保守的结论——那也是一种失真。
	out.Lexical = bm25.Search(query, topK)

	if queryVec == nil {
		if out.Degraded == "" {
			out.Degraded = "向量通道不可用"
		}
		out.Fused = out.Lexical
		return out, nil
	}

	var err error
	if out.Vector, err = vecIdx.Search(queryVec, topK); err != nil {
		return nil, fmt.Errorf("service: 向量检索失败: %w", err)
	}

	hybrid := retrieve.NewHybrid(
		retrieve.BM25Searcher{BM25: bm25},
		vecIdx,
	).WithTimeout(s.cfg.SearchTimeout)
	if rrfK > 0 {
		hybrid = hybrid.WithRRFK(rrfK)
	}
	if mult > 0 {
		hybrid = hybrid.WithCandidateMultiplier(mult)
	}

	// 开了相邻片段合并时，融合**不截断**：合并完再截断。
	//
	// ⚠️ 不能用「传一个更大的 topK」来代替——那会让每路取回的候选数
	// 跟着变大，而 RRF 的排名依赖候选池大小，算出来的名次和原来对不上。
	// 实测那样做会让 recall 掉 7 个百分点。见 Hybrid.SearchAll 的说明。
	if s.cfg.MergeAdjacent {
		fused, err := hybrid.SearchAll(ctx, query, queryVec, topK)
		if err != nil {
			return nil, fmt.Errorf("service: 混合检索失败: %w", err)
		}
		// 合并只删冗余、不动顺序，所以原 topK 名里属于各组的代表一条都不会丢；
		// 腾出来的位置由**同一份排名**里更深的名次填上。
		out.Fused = retrieve.MergeAdjacent(fused)
		if len(out.Fused) > topK {
			out.Fused = out.Fused[:topK]
		}
		return out, nil
	}

	if out.Fused, err = hybrid.Search(ctx, query, queryVec, topK); err != nil {
		return nil, fmt.Errorf("service: 混合检索失败: %w", err)
	}
	return out, nil
}
