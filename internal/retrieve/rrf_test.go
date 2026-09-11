package retrieve

import (
	"math"
	"testing"

	"github.com/XiaoleC05/ContextDock/internal/types"
)

// lexRun / vecRun 造一路结果（下标即名次）。
func lexRun(chunks ...types.Chunk) Run {
	rs := make([]types.SearchResult, len(chunks))
	for i, c := range chunks {
		rs[i] = types.SearchResult{Chunk: c, Score: float64(len(chunks) - i), LexicalScore: float64(len(chunks) - i)}
	}
	return Run{Retriever: types.RetrieverLexical, Results: rs}
}

func vecRun(chunks ...types.Chunk) Run {
	rs := make([]types.SearchResult, len(chunks))
	for i, c := range chunks {
		rs[i] = types.SearchResult{Chunk: c, Score: 1 - float64(i)*0.1, VectorScore: 1 - float64(i)*0.1}
	}
	return Run{Retriever: types.RetrieverVector, Results: rs}
}

func TestFuseRRFEmpty(t *testing.T) {
	got := FuseRRF(types.RRFK, 10)
	if len(got) != 0 || got == nil {
		t.Errorf("无输入应返回空的非 nil 切片，实际 %v", got)
	}

	got = FuseRRF(types.RRFK, 10, Run{Retriever: types.RetrieverLexical})
	if len(got) != 0 {
		t.Errorf("空结果应返回空，实际 %d 条", len(got))
	}
}

func TestFuseRRFSinglePath(t *testing.T) {
	got := FuseRRF(types.RRFK, 10,
		lexRun(mkChunk(1, "a"), mkChunk(2, "b")))

	if len(got) != 2 {
		t.Fatalf("应返回 2 条，实际 %d", len(got))
	}
	if got[0].Chunk.ID != 1 {
		t.Errorf("第 1 名应排前，实际 %v", resultIDs(got))
	}
	if got[0].LexicalRank != 1 || got[1].LexicalRank != 2 {
		t.Errorf("名次应为 1,2，实际 %d,%d", got[0].LexicalRank, got[1].LexicalRank)
	}
	if got[0].VectorRank != 0 {
		t.Errorf("向量路未参与，VectorRank 应为 0，实际 %d", got[0].VectorRank)
	}

	// 单路时 RRF 分应当等于 1/(k+rank)
	want := 1.0 / float64(types.RRFK+1)
	if math.Abs(got[0].Score-want) > 1e-12 {
		t.Errorf("单路第 1 名的 RRF 分应为 %.10f，实际 %.10f", want, got[0].Score)
	}
}

// TestFuseRRFBothPathsBoost 验证两路都命中的结果排名更高。
//
// 这是 RRF 的核心价值：两路都认为相关的，比只有一路认为相关的更可信。
func TestFuseRRFBothPathsBoost(t *testing.T) {
	a := mkChunk(1, "两路都命中")
	b := mkChunk(2, "只有关键词命中")
	c := mkChunk(3, "只有向量命中")

	got := FuseRRF(types.RRFK, 10,
		lexRun(b, a), // 关键词：b 第 1，a 第 2
		vecRun(c, a), // 向量：c 第 1，a 第 2
	)

	if len(got) != 3 {
		t.Fatalf("应返回 3 条，实际 %d", len(got))
	}
	if got[0].Chunk.ID != 1 {
		t.Errorf("两路都命中的 1 号应当排第一，实际顺序 %v", resultIDs(got))
	}
	if !got[0].MatchedByBoth() {
		t.Error("1 号应当 MatchedByBoth()")
	}
	if got[0].LexicalRank != 2 || got[0].VectorRank != 2 {
		t.Errorf("1 号在两路都应是第 2 名，实际 lexical=%d vector=%d",
			got[0].LexicalRank, got[0].VectorRank)
	}
}

// TestFuseRRFUsesStableKeyNotID 守护 DESIGN §7。
//
// 落库之前所有 chunk 的 ID 都是 0。如果融合时用 ID 做去重键，
// 两个**完全不同**的片段会被合并成一条，RRF 名次整体错乱——
// 而且不报任何错，只是结果变差。
func TestFuseRRFUsesStableKeyNotID(t *testing.T) {
	// 两个片段：ID 都是 0（未落库），靠 DocumentID + Ordinal 区分
	a := types.Chunk{DocumentID: 1, Ordinal: 0, Content: "第一段"}
	b := types.Chunk{DocumentID: 1, Ordinal: 1, Content: "第二段"}

	if a.StableKey() == b.StableKey() {
		t.Fatal("用例设计有误：两个片段的 StableKey 不应相同")
	}

	got := FuseRRF(types.RRFK, 10,
		lexRun(a),
		vecRun(b),
	)

	if len(got) != 2 {
		t.Fatalf("两个不同的片段应当各占一条，实际合并成了 %d 条。"+
			"若为 1 条，说明去重用了 Chunk.ID（落库前全为 0）而不是 StableKey", len(got))
	}
}

// TestFuseRRFNoZeroRankContribution 守护 0 号哨兵。
//
// LexicalRank=0 表示"该通道未召回"。如果把它当成第 0 名代入公式，
// 1/(60+0)=0.016667 会比真正的第一名 1/(60+1)=0.016393 还大，
// 未召回的反倒拿最高分。
func TestFuseRRFNoZeroRankContribution(t *testing.T) {
	only := mkChunk(1, "只被向量召回")

	got := FuseRRF(types.RRFK, 10, vecRun(only))
	if len(got) != 1 {
		t.Fatalf("应返回 1 条，实际 %d", len(got))
	}

	want := 1.0 / float64(types.RRFK+1) // 只有向量那一路的贡献
	if math.Abs(got[0].Score-want) > 1e-12 {
		t.Errorf("未召回的通道不应贡献分数：\n  期望 %.10f（仅向量路）\n  实际 %.10f\n"+
			"  若实际约等于 %.10f，说明 0 被当成了第 0 名",
			want, got[0].Score, want+1.0/float64(types.RRFK))
	}
}

func TestFuseRRFTopK(t *testing.T) {
	chunks := make([]types.Chunk, 10)
	for i := range chunks {
		chunks[i] = mkChunk(int64(i+1), "内容")
	}
	got := FuseRRF(types.RRFK, 3, lexRun(chunks...))
	if len(got) != 3 {
		t.Errorf("topK=3 应返回 3 条，实际 %d", len(got))
	}

	got = FuseRRF(types.RRFK, 0, lexRun(chunks...))
	if len(got) != 10 {
		t.Errorf("topK=0 应不截断，实际 %d 条", len(got))
	}
}

func TestFuseRRFStableOrder(t *testing.T) {
	var chunks []types.Chunk
	for i := int64(1); i <= 8; i++ {
		chunks = append(chunks, mkChunk(i, "相同内容"))
	}

	first := resultIDs(FuseRRF(types.RRFK, 0, lexRun(chunks...)))
	for i := 0; i < 20; i++ {
		got := resultIDs(FuseRRF(types.RRFK, 0, lexRun(chunks...)))
		for j := range first {
			if got[j] != first[j] {
				t.Fatalf("第 %d 次融合顺序不稳定:\n  首次 %v\n  本次 %v", i, first, got)
			}
		}
	}
}

func TestFuseRRFDefaultK(t *testing.T) {
	a := lexRun(mkChunk(1, "a"))
	if FuseRRF(0, 10, a)[0].Score != FuseRRF(types.RRFK, 10, a)[0].Score {
		t.Error("k<=0 时应回落到默认值 RRFK")
	}
}

// TestFuseRRFRanksAreRecorded 验证各路名次被正确记录。
func TestFuseRRFRanksAreRecorded(t *testing.T) {
	a, b, c := mkChunk(1, "a"), mkChunk(2, "b"), mkChunk(3, "c")

	got := FuseRRF(types.RRFK, 10,
		lexRun(a, b, c),
		vecRun(c, b, a),
	)

	ranks := map[int64][2]int{}
	for _, r := range got {
		ranks[r.Chunk.ID] = [2]int{r.LexicalRank, r.VectorRank}
	}

	want := map[int64][2]int{
		1: {1, 3},
		2: {2, 2},
		3: {3, 1},
	}
	for id, w := range want {
		if ranks[id] != w {
			t.Errorf("%d 号名次应为 %v，实际 %v", id, w, ranks[id])
		}
	}

	// ⚠️ 反直觉的一点：**"两路都是第 2 名"并不比"一路第 1、一路第 3"更优**。
	//
	//	1/(k+1) + 1/(k+3) = 0.0163934 + 0.0158730 = 0.0322664   ← a 和 c
	//	1/(k+2) + 1/(k+2) = 0.0161290 × 2          = 0.0322581   ← b
	//
	// 因为 1/(k+x) 对 x 是凸函数，两端的和大于中间的两倍。
	// 差距很小但确实存在——所以 a 和 c 并列第一，b 排第三。
	//
	// a 和 c 同分，靠 StableKey 决出先后：a 是 "id:1"，c 是 "id:3"。
	if got[0].Chunk.ID != 1 || got[1].Chunk.ID != 3 || got[2].Chunk.ID != 2 {
		t.Errorf("期望 [1 3 2]（1 和 3 并列第一、2 排第三），实际 %v", resultIDs(got))
	}
	if math.Abs(got[0].Score-got[1].Score) > 1e-12 {
		t.Errorf("1 和 3 应当同分，实际 %.10f vs %.10f", got[0].Score, got[1].Score)
	}
	if !(got[1].Score > got[2].Score) {
		t.Errorf("并列第一的两个都应当高于 2 号: %.10f vs %.10f", got[1].Score, got[2].Score)
	}
}
