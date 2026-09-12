package service

import (
	"sort"

	"github.com/XiaoleC05/ContextDock/internal/types"
)

// Neighbors 返回某个片段在同一文档内的前后相邻片段。
//
// before / after 各自表示取几段，<= 0 表示不取那一侧。
//
// # 为什么要它
//
// 命中片段可能只写着「运行 go build」，单独看不知道在讲什么；
// 带上前一段才知道上下文。Chunk 上的 Ordinal 和偏移量早就是为这件事
// 留的，只是一直没实现（见 types.Chunk.Ordinal 的注释）。
//
// # 为什么按 Ordinal 而不是按偏移量找
//
// 偏移量能回答「哪一段在原文里紧挨着我」，但它要求所有片段都参与比较；
// Ordinal 是切分时按顺序编好的，查一下就知道邻居是谁。
// 代价是它只在**同一次切分**内有效——而这正是使用场景。
func (s *Service) Neighbors(c types.Chunk, before, after int) ([]types.Chunk, []types.Chunk) {
	if before <= 0 && after <= 0 {
		return nil, nil
	}

	s.idxMu.RLock()
	defer s.idxMu.RUnlock()

	doc := s.byDoc[c.DocumentID]
	if len(doc) == 0 {
		return nil, nil
	}

	// 二分定位自己。
	//
	// 找不到自己时**返回空而不是猜一个位置**：猜错会把别的片段的正文
	// 当成"上下文"贴上去，而 Agent 无从分辨——那比没有上下文糟糕得多。
	i := sort.Search(len(doc), func(i int) bool { return doc[i].Ordinal >= c.Ordinal })
	if i >= len(doc) || doc[i].Ordinal != c.Ordinal {
		return nil, nil
	}

	var prev, next []types.Chunk
	for k := 1; k <= before && i-k >= 0; k++ {
		prev = append(prev, doc[i-k])
	}
	for k := 1; k <= after && i+k < len(doc); k++ {
		next = append(next, doc[i+k])
	}
	// 统一成"离命中片段由远及近"的顺序，调用方不必再关心取了几段。
	reverse(prev)
	return prev, next
}

// reverse 就地反转切片。
func reverse(cs []types.Chunk) {
	for i, j := 0, len(cs)-1; i < j; i, j = i+1, j-1 {
		cs[i], cs[j] = cs[j], cs[i]
	}
}

// ContextNeighbors 返回配置里设定的相邻片段数，0 表示不做上下文扩展。
//
// 单独开一个 getter 而不是让调用方去读 cfg：cfg 是私有的，
// 而且这个值将来可能由别处（比如按查询动态决定）提供。
func (s *Service) ContextNeighbors() int { return s.cfg.ContextNeighbors }
