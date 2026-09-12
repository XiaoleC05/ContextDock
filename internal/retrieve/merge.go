package retrieve

import (
	"sort"

	"github.com/XiaoleC05/ContextDock/internal/types"
)

// MergeAdjacent 把**同一文档里序号连续**的命中合并成一条。
//
// # 为什么需要它
//
// 一个片段被切在句子中间时，命中往往会连着命中它的邻居——
// 于是 top-10 里三四条都是同一处内容。它们高度冗余，
// 却占掉了本可以给其他文档的结果位。
//
// # 合并后按**最佳**名次计分
//
// 取这一组里最高的那个分，不做任何累加。累加会让"命中了一堆相邻片段"
// 比"命中了一条高度相关的片段"得分更高——而前者只是说明切分把一段话
// 切碎了，不是什么好事。
//
// # 合并不会丢掉命中
//
// 合并后的区间是各段的**并集**，任何原本落在其中某一段里的期望
// 仍然落在并集里。所以「合并导致漏召回」在结构上不可能发生。
// （这条有测试守着，因为它是个"改了也看不出来"的地方。）
//
// 返回结果保持原顺序（按分组里最好的那条的名次）。
func MergeAdjacent(results []types.SearchResult) []types.SearchResult {
	if len(results) <= 1 {
		return results
	}

	// 按文档分组，组内按 Ordinal 排序。
	groups := make(map[int64][]int, 8) // DocumentID → results 的下标
	for i, r := range results {
		groups[r.Chunk.DocumentID] = append(groups[r.Chunk.DocumentID], i)
	}

	// 每一组的"最佳成员"（名次最靠前 = results 里下标最小）代表这一组。
	//
	// 用下标当名次：results 进来时已经是排好序的，下标就是名次。
	byHead := make(map[int]mergedRun, len(results))
	consumed := make(map[int]bool, len(results))

	for _, idxs := range groups {
		sort.Slice(idxs, func(a, b int) bool {
			return results[idxs[a]].Chunk.Ordinal < results[idxs[b]].Chunk.Ordinal
		})

		for i := 0; i < len(idxs); {
			j := i + 1
			for j < len(idxs) &&
				results[idxs[j]].Chunk.Ordinal == results[idxs[j-1]].Chunk.Ordinal+1 {
				j++
			}
			run := idxs[i:j]

			// 组里名次最好的那条当代表。
			head := run[0]
			for _, k := range run {
				if k < head {
					head = k
				}
			}
			if len(run) > 1 {
				byHead[head] = mergeRun(results, run)
				for _, k := range run {
					if k != head {
						consumed[k] = true
					}
				}
			}
			i = j
		}
	}

	if len(byHead) == 0 {
		return results
	}

	out := make([]types.SearchResult, 0, len(results))
	for i, r := range results {
		if consumed[i] {
			continue
		}
		if m, ok := byHead[i]; ok {
			ch := m.head
			ch.StartOffset, ch.EndOffset = m.start, m.end
			ch.Content = m.content
			r.Chunk = ch
		}
		out = append(out, r)
	}
	return out
}

// mergedRun 是一组连续片段合并后的结果。
type mergedRun struct {
	// content 是拼接后的正文，等于原文的 [start, end)。
	content string
	start   int
	end     int
	// head 保留代表那一条的元数据（面包屑、来源），只换正文与区间。
	head types.Chunk
}

// mergeRun 把一组连续片段拼成一条：区间取并集，正文按顺序拼接并去掉重叠。
//
// 为什么正文要拼接而不是只保留第一条：片段的 Content 必须等于
// 原文 [StartOffset, EndOffset)——这条不变量是上下文扩展、覆盖性校验
// 和「重新切分对齐」共同的基石。只把区间撑大而不改正文会让它失效，
// 而失效的表现是"取回来的正文和偏移对不上"，很难查。
func mergeRun(results []types.SearchResult, run []int) mergedRun {
	// 按**原文位置**排序，而不是按名次——拼接必须沿原文顺序。
	ordered := append([]int(nil), run...)
	sort.Slice(ordered, func(a, b int) bool {
		return results[ordered[a]].Chunk.StartOffset < results[ordered[b]].Chunk.StartOffset
	})

	first := results[ordered[0]].Chunk
	content := first.Content
	prevEnd := first.EndOffset

	for _, k := range ordered[1:] {
		c := results[k].Chunk
		skip := prevEnd - c.StartOffset
		if skip < 0 {
			skip = 0
		}
		r := []rune(c.Content)
		if skip > len(r) {
			skip = len(r)
		}
		content += string(r[skip:])
		if c.EndOffset > prevEnd {
			prevEnd = c.EndOffset
		}
	}

	return mergedRun{
		content: content,
		start:   first.StartOffset,
		end:     prevEnd,
		head:    results[run[0]].Chunk,
	}
}
