// Package retrieve 实现两路检索：BM25 关键词检索和向量语义检索。
//
// 两路都以 types.SearchResult 返回，各自的通道名次写在 LexicalRank / VectorRank 里，
// 供上层的 RRF 融合使用（见 rrf.go）。
//
// 本包的所有实现都是**内存版**，不依赖数据库。这样写测试和跑 benchmark
// 都不需要先启动 PostgreSQL。
package retrieve

import (
	"sort"

	"github.com/XiaoleC05/ContextDock/internal/types"
)

// scored 是排序的中间结果。
type scored struct {
	chunk types.Chunk
	score float64
}

// sortAndTrim 按分数降序排序并截取前 topK 条。
//
// 两个细节值得注意：
//
//  1. **同分时按 StableKey 字典序**。没有这个 tie-break 的话，同分文档的
//     相对顺序取决于排序算法的内部实现，两次查询可能给出不同顺序——
//     演示时会看到结果莫名其妙地跳来跳去。
//
//  2. 目前是**全排序**（O(n log n)）。对于 top-K 只需要一个大小为 K 的堆
//     （O(n log K)）。这里先用简单实现把功能做对，性能优化留到 M7，
//     那时会有真实的 benchmark 数字来决定值不值得改。
//     算法本身不会变，只是选择方式不同。
//
// topK <= 0 表示不截断。
func sortAndTrim(items []scored, topK int) []scored {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].score != items[j].score {
			return items[i].score > items[j].score
		}
		return items[i].chunk.StableKey() < items[j].chunk.StableKey()
	})
	if topK > 0 && len(items) > topK {
		items = items[:topK]
	}
	return items
}
