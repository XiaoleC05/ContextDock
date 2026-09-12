package retrieve

import (
	"context"
	"testing"

	"github.com/XiaoleC05/ContextDock/internal/types"
)

// fixedLexical / fixedVector 是返回固定名单的假检索器，
// 并记录自己**被请求了多少条**。
type fixedSearch struct {
	n     int
	calls *[]int
	err   error
}

func (f fixedSearch) Search(_ string, topK int) ([]types.SearchResult, error) {
	if f.calls != nil {
		*f.calls = append(*f.calls, topK)
	}
	return makeResults(0, f.n), f.err
}

type fixedVec struct {
	n     int
	calls *[]int
}

func (f fixedVec) Search(_ []float32, topK int) ([]types.SearchResult, error) {
	if f.calls != nil {
		*f.calls = append(*f.calls, topK)
	}
	return makeResults(5, f.n), nil
}

// makeResults 造 n 条结果，ID 从 offset 开始。
//
// 两路给不同的 offset，是为了让融合后的并集比单路更大——
// 否则两路返回同一批片段，融合出来还是那几条，测不出"没截断"。
func makeResults(offset, n int) []types.SearchResult {
	out := make([]types.SearchResult, 0, n)
	for i := 0; i < n; i++ {
		c := types.Chunk{ID: int64(offset + i + 1), DocumentID: 1, Ordinal: offset + i, Content: "x"}
		out = append(out, types.SearchResult{Chunk: c, Score: float64(n - i)})
	}
	return out
}

func TestSearchAllDoesNotWidenChannelCandidates(t *testing.T) {
	// ⚠️ 这条守的是一个**实际踩过的坑**。
	//
	// 为了给「相邻片段合并」腾出截断空间，最直觉的做法是给 Search
	// 传一个更大的 topK。但那样会**同时**把每路取回的候选数
	// （n = topK × mult）放大，而 RRF 的排名依赖候选池大小——
	// 算出来的名次和原来的对不上，实测 recall 掉了 7 个百分点。
	//
	// SearchAll 的存在就是为了把这两件事拆开：候选深度不变，只是不截断。
	var lexCalls, vecCalls []int
	h := NewHybrid(
		lexSearcher{&lexCalls, 100},
		vecSearcher{&vecCalls, 100},
	).WithCandidateMultiplier(1)

	// 先跑一次普通 Search 当基准。
	if _, err := h.Search(context.Background(), "q", zeroVec(), 10); err != nil {
		t.Fatal(err)
	}
	wantDepth := append([]int(nil), lexCalls...)

	lexCalls, vecCalls = nil, nil
	all, err := h.SearchAll(context.Background(), "q", zeroVec(), 10)
	if err != nil {
		t.Fatal(err)
	}

	if len(lexCalls) != len(wantDepth) || lexCalls[0] != wantDepth[0] {
		t.Errorf("SearchAll 不该改变每路的候选深度：Search 取了 %v，SearchAll 取了 %v",
			wantDepth, lexCalls)
	}
	if len(all) <= 10 {
		t.Errorf("SearchAll 应当不截断，实际只返回 %d 条", len(all))
	}

	// 前缀必须与 Search 的结果逐条一致。
	trimmed, err := h.Search(context.Background(), "q", zeroVec(), 10)
	if err != nil {
		t.Fatal(err)
	}
	for i := range trimmed {
		if all[i].Chunk.StableKey() != trimmed[i].Chunk.StableKey() {
			t.Fatalf("第 %d 条与截断版不一致：%s vs %s",
				i, all[i].Chunk.StableKey(), trimmed[i].Chunk.StableKey())
		}
	}
}

// ---- 适配器：让简单函数满足接口 ----

type lexSearcher struct {
	calls *[]int
	n     int
}

func (s lexSearcher) Search(q string, topK int) ([]types.SearchResult, error) {
	return fixedSearch{n: min(s.n, topK), calls: s.calls}.Search(q, topK)
}

type vecSearcher struct {
	calls *[]int
	n     int
}

func (s vecSearcher) Search(v []float32, topK int) ([]types.SearchResult, error) {
	return fixedVec{n: min(s.n, topK), calls: s.calls}.Search(v, topK)
}

func zeroVec() []float32 { return make([]float32, types.EmbeddingDim) }
