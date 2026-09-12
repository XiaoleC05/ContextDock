package retrieve

import (
	"testing"

	"github.com/XiaoleC05/ContextDock/internal/types"
)

// mk 造一条结果：来自 doc 文档、序号 ordinal、原文区间 [start,end)、正文取自 text。
func mk(doc int64, ordinal, start, end int, text string, score float64) types.SearchResult {
	c := types.Chunk{
		DocumentID: doc, Ordinal: ordinal,
		StartOffset: start, EndOffset: end,
		Content: string([]rune(text)[start:end]),
	}
	c.SetMetadata(types.MetadataKeySource, "doc.md")
	return types.SearchResult{Chunk: c, Score: score}
}

const mergeText = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func TestMergeAdjacentJoinsConsecutiveOrdinals(t *testing.T) {
	// 序号 0 与 1 相邻（片段 [0,20) 与 [15,35)，重叠 5 个字符）。
	in := []types.SearchResult{
		mk(1, 0, 0, 20, mergeText, 0.9),
		mk(1, 1, 15, 35, mergeText, 0.8),
	}
	out := MergeAdjacent(in)

	if len(out) != 1 {
		t.Fatalf("相邻的两条应当合并成一条，实际 %d 条", len(out))
	}
	if out[0].Chunk.StartOffset != 0 || out[0].Chunk.EndOffset != 35 {
		t.Errorf("合并后区间应为并集 [0,35)，实际 [%d,%d)",
			out[0].Chunk.StartOffset, out[0].Chunk.EndOffset)
	}
	// ⚠️ 正文必须等于原文的 [start,end)，重叠部分**不能重复出现**。
	// 这条不变量是上下文扩展和「重新切分对齐」共同的基石。
	want := string([]rune(mergeText)[0:35])
	if out[0].Chunk.Content != want {
		t.Errorf("合并后正文与区间不匹配：\n  期望 %q\n  实际 %q", want, out[0].Chunk.Content)
	}
}

func TestMergeAdjacentKeepsBestScoreNotSum(t *testing.T) {
	// 合并后取**最高**分，不做累加。
	//
	// 累加会让"命中了一堆相邻片段"比"命中了一条高度相关的片段"得分更高——
	// 而前者只说明切分把一段话切碎了，不是什么好事。
	in := []types.SearchResult{
		mk(1, 0, 0, 20, mergeText, 0.9),
		mk(1, 1, 15, 35, mergeText, 0.8),
	}
	if got := MergeAdjacent(in)[0].Score; got != 0.9 {
		t.Errorf("合并后应保留最高分 0.9，实际 %v", got)
	}
}

func TestMergeAdjacentLeavesGapsAlone(t *testing.T) {
	// 序号不连续（中间那段没被命中）不能合并：它们之间的内容是断的，
	// 合起来会造出一个"包含没被命中的内容"的片段。
	in := []types.SearchResult{
		mk(1, 0, 0, 20, mergeText, 0.9),
		mk(1, 2, 30, 50, mergeText, 0.8),
	}
	if out := MergeAdjacent(in); len(out) != 2 {
		t.Errorf("序号不连续不该合并，实际合并成 %d 条", len(out))
	}
}

func TestMergeAdjacentDoesNotCrossDocuments(t *testing.T) {
	// 不同文档的片段即使序号"看起来挨着"也绝不能合并——
	// 合起来会造出一段横跨两份文档的、原文里根本不存在的正文。
	in := []types.SearchResult{
		mk(1, 5, 0, 20, mergeText, 0.9),
		mk(2, 6, 15, 35, mergeText, 0.8),
	}
	if out := MergeAdjacent(in); len(out) != 2 {
		t.Errorf("跨文档不该合并，实际合并成 %d 条", len(out))
	}
}

func TestMergeAdjacentPreservesOrder(t *testing.T) {
	// 合并只删冗余、不动顺序。这条保证了「原本排在前面的代表仍在前面」，
	// 也就保证了原 top-k 名里的内容不会被合并挤出去。
	in := []types.SearchResult{
		mk(1, 0, 0, 20, mergeText, 0.9),
		mk(2, 0, 0, 20, mergeText, 0.85),
		mk(1, 1, 15, 35, mergeText, 0.8),
	}
	out := MergeAdjacent(in)
	if len(out) != 2 {
		t.Fatalf("应合并成 2 条，实际 %d", len(out))
	}
	if out[0].Chunk.DocumentID != 1 || out[1].Chunk.DocumentID != 2 {
		t.Errorf("顺序应保持 [doc1 doc2]，实际 [doc%d doc%d]",
			out[0].Chunk.DocumentID, out[1].Chunk.DocumentID)
	}
}

func TestMergeAdjacentNeverLosesCoverage(t *testing.T) {
	// 这是合并最重要的一条性质：**合并不会丢掉任何命中**。
	//
	// 合并后的区间是各段的并集，任何原本落在其中某一段里的期望仍落在并集里。
	// 换句话说「合并导致漏召回」在结构上不可能发生。
	//
	// 之所以要专门测它：这是个**改了也看不出来**的地方——
	// 真丢了命中的话，指标只是低一点，没人会想到是合并干的。
	var in []types.SearchResult
	for i := 0; i < 8; i++ {
		in = append(in, mk(1, i, i*30, i*30+40, mergeText+mergeText+mergeText+mergeText, 1-float64(i)/100))
	}
	out := MergeAdjacent(in)
	if len(out) != 1 {
		t.Fatalf("全部相邻，应合并成 1 条，实际 %d", len(out))
	}
	// 原覆盖的区间并集
	lo, hi := in[0].Chunk.StartOffset, in[0].Chunk.EndOffset
	for _, r := range in {
		if r.Chunk.StartOffset < lo {
			lo = r.Chunk.StartOffset
		}
		if r.Chunk.EndOffset > hi {
			hi = r.Chunk.EndOffset
		}
	}
	if out[0].Chunk.StartOffset != lo || out[0].Chunk.EndOffset != hi {
		t.Errorf("合并后区间应为并集 [%d,%d)，实际 [%d,%d)",
			lo, hi, out[0].Chunk.StartOffset, out[0].Chunk.EndOffset)
	}
}

func TestMergeAdjacentSingleResultUnchanged(t *testing.T) {
	in := []types.SearchResult{mk(1, 0, 0, 20, mergeText, 0.9)}
	out := MergeAdjacent(in)
	if len(out) != 1 || out[0].Chunk.Content != in[0].Chunk.Content {
		t.Error("只有一条时应当原样返回")
	}
}

func TestMergeAdjacentEmpty(t *testing.T) {
	if out := MergeAdjacent(nil); len(out) != 0 {
		t.Errorf("空输入应返回空，实际 %d 条", len(out))
	}
}

func TestMergeAdjacentRepresentativeIsBestRanked(t *testing.T) {
	// 组里的代表必须是**名次最好**的那条，不是序号最小的那条。
	//
	// 两条相邻片段在结果里谁排前面，取决于各自的分——而合并后的那一条
	// 要占前者的位置。取错了会让合并后的结果整体后移，把本可以露出
	// 的别的文档又挤回去（那正是合并想解决的问题）。
	//
	// 这里刻意让「序号大的那条排得靠前」：
	//   index 0 → doc1 / ordinal 1
	//   index 1 → doc2 / ordinal 0    （无关文档，用来隔开）
	//   index 2 → doc1 / ordinal 0
	in := []types.SearchResult{
		mk(1, 1, 15, 35, mergeText, 0.8),
		mk(2, 0, 0, 20, mergeText, 0.7),
		mk(1, 0, 0, 20, mergeText, 0.6),
	}
	out := MergeAdjacent(in)

	if len(out) != 2 {
		t.Fatalf("应合并成 2 条，实际 %d", len(out))
	}
	if out[0].Chunk.DocumentID != 1 {
		t.Errorf("合并后的 doc1 应占名次最好的那位（index 0），实际第 1 条是 doc%d",
			out[0].Chunk.DocumentID)
	}
	if out[0].Score != 0.8 {
		t.Errorf("应保留组里最好的分 0.8，实际 %v", out[0].Score)
	}
}
