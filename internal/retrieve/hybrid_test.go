package retrieve

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/XiaoleC05/ContextDock/internal/types"
)

// stubLexical 是可注入延迟与错误的假关键词检索器。
type stubLexical struct {
	results []types.SearchResult
	err     error
	delay   time.Duration
	calls   int
	mu      sync.Mutex
}

func (s *stubLexical) Search(_ string, _ int) ([]types.SearchResult, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	return s.results, s.err
}

// stubVector 同上，用于向量路。
type stubVector struct {
	results []types.SearchResult
	err     error
	delay   time.Duration
	calls   int
	mu      sync.Mutex
}

func (s *stubVector) Search(_ context.Context, _ []float32, _ int) ([]types.SearchResult, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	return s.results, s.err
}

func lexResults(chunks ...types.Chunk) []types.SearchResult {
	rs := make([]types.SearchResult, len(chunks))
	for i, c := range chunks {
		rs[i] = types.SearchResult{Chunk: c, LexicalScore: float64(len(chunks) - i)}
	}
	return rs
}

func vecResults(chunks ...types.Chunk) []types.SearchResult {
	rs := make([]types.SearchResult, len(chunks))
	for i, c := range chunks {
		rs[i] = types.SearchResult{Chunk: c, VectorScore: 1 - float64(i)*0.1}
	}
	return rs
}

func TestHybridBothPathsSucceed(t *testing.T) {
	a, b := mkChunk(1, "两路都命中"), mkChunk(2, "只有一路")

	h := NewHybrid(
		&stubLexical{results: lexResults(b, a)},
		&stubVector{results: vecResults(a)},
	)

	got, err := h.Search(context.Background(), "查询", vec(1), 10)
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应返回 2 条，实际 %d", len(got))
	}
	if got[0].Chunk.ID != 1 {
		t.Errorf("两路都命中的 1 号应排第一，实际 %v", resultIDs(got))
	}
}

// TestHybridDegradesWhenOnePathFails 是 #14 的核心验收点。
//
// 单路失败**不应**让整个查询失败——否则一次限流或者一个维度 bug
// 就会让 Agent 完全查不到东西。
func TestHybridDegradesWhenOnePathFails(t *testing.T) {
	t.Run("向量路失败", func(t *testing.T) {
		h := NewHybrid(
			&stubLexical{results: lexResults(mkChunk(1, "关键词命中"))},
			&stubVector{err: errors.New("模拟向量检索故障")},
		)

		got, err := h.Search(context.Background(), "查询", vec(1), 10)
		if err != nil {
			t.Fatalf("一路失败不应让整个查询失败，实际 %v", err)
		}
		if len(got) != 1 || got[0].Chunk.ID != 1 {
			t.Errorf("应降级为只返回关键词路的结果，实际 %v", resultIDs(got))
		}
		if got[0].LexicalRank != 1 {
			t.Errorf("降级结果仍应带名次，实际 LexicalRank=%d", got[0].LexicalRank)
		}
	})

	t.Run("关键词路失败", func(t *testing.T) {
		h := NewHybrid(
			&stubLexical{err: errors.New("模拟关键词检索故障")},
			&stubVector{results: vecResults(mkChunk(1, "向量命中"))},
		)

		got, err := h.Search(context.Background(), "查询", vec(1), 10)
		if err != nil {
			t.Fatalf("一路失败不应让整个查询失败，实际 %v", err)
		}
		if len(got) != 1 || got[0].VectorRank != 1 {
			t.Errorf("应降级为只返回向量路的结果，实际 %v", got)
		}
	})
}

func TestHybridFailsWhenBothPathsFail(t *testing.T) {
	h := NewHybrid(
		&stubLexical{err: errors.New("关键词故障")},
		&stubVector{err: errors.New("向量故障")},
	)

	_, err := h.Search(context.Background(), "查询", vec(1), 10)
	if !errors.Is(err, ErrBothRetrieversFailed) {
		t.Errorf("两路都失败应返回 ErrBothRetrieversFailed，实际 %v", err)
	}
}

// TestHybridErrorHandler 验证降级不是静默的。
//
// 一路挂掉照样返回结果，如果没有观测口，线上就只会表现为
// "检索质量莫名下降"而没有任何信号。
func TestHybridErrorHandler(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []types.Retriever
	)
	wantErr := errors.New("模拟故障")

	h := NewHybrid(
		&stubLexical{results: lexResults(mkChunk(1, "命中"))},
		&stubVector{err: wantErr},
	).WithErrorHandler(func(r types.Retriever, err error) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, r)
		if !errors.Is(err, wantErr) {
			t.Errorf("回调收到的错误不对: %v", err)
		}
	})

	if _, err := h.Search(context.Background(), "查询", vec(1), 10); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != types.RetrieverVector {
		t.Errorf("应回调一次且通道为 vector，实际 %v", seen)
	}
}

// TestHybridTimeout 验证超时能按预期降级。
func TestHybridTimeout(t *testing.T) {
	t.Run("一路慢但另一路已就绪", func(t *testing.T) {
		h := NewHybrid(
			&stubLexical{results: lexResults(mkChunk(1, "快路"))},
			&stubVector{results: vecResults(mkChunk(2, "慢路")), delay: 5 * time.Second},
		).WithTimeout(80 * time.Millisecond)

		start := time.Now()
		got, err := h.Search(context.Background(), "查询", vec(1), 10)
		elapsed := time.Since(start)

		if err != nil {
			t.Fatalf("有一路已完成时不应报错，实际 %v", err)
		}
		if elapsed > 2*time.Second {
			t.Errorf("应当在超时后立即返回，实际耗时 %v", elapsed)
		}
		if len(got) == 0 {
			t.Error("应当返回已完成那一路的结果")
		}
	})

	t.Run("两路都慢", func(t *testing.T) {
		h := NewHybrid(
			&stubLexical{results: lexResults(mkChunk(1, "a")), delay: 5 * time.Second},
			&stubVector{results: vecResults(mkChunk(2, "b")), delay: 5 * time.Second},
		).WithTimeout(60 * time.Millisecond)

		start := time.Now()
		_, err := h.Search(context.Background(), "查询", vec(1), 10)
		elapsed := time.Since(start)

		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("两路都没完成时应返回超时错误，实际 %v", err)
		}
		if elapsed > 2*time.Second {
			t.Errorf("应在超时后立即返回，实际 %v", elapsed)
		}
	})
}

// TestHybridRespectsParentContext 验证外层 ctx 取消能传递下去。
func TestHybridRespectsParentContext(t *testing.T) {
	h := NewHybrid(
		&stubLexical{results: lexResults(mkChunk(1, "a")), delay: 5 * time.Second},
		&stubVector{results: vecResults(mkChunk(2, "b")), delay: 5 * time.Second},
	)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := h.Search(ctx, "查询", vec(1), 10); err == nil {
		t.Error("外层 ctx 取消时应当返回错误")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("应在 ctx 取消后立即返回，实际 %v", elapsed)
	}
}

// TestHybridCandidateMultiplier 验证每路会多取候选。
//
// 只取 topK 的话，两路各自的第一名可能都不在对方的候选里，
// 融合后的结果会非常单薄。
func TestHybridCandidateMultiplier(t *testing.T) {
	var gotTopK int
	var mu sync.Mutex

	lex := &topKRecorder{lexical: true, mu: &mu, topK: &gotTopK}
	h := NewHybrid(lex, &stubVector{}).WithCandidateMultiplier(3)

	if _, err := h.Search(context.Background(), "查询", vec(1), 5); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotTopK != 15 {
		t.Errorf("topK=5、倍数=3 时每路应取 15 条候选，实际取了 %d 条", gotTopK)
	}
}

type topKRecorder struct {
	lexical bool
	mu      *sync.Mutex
	topK    *int
}

func (s *topKRecorder) Search(_ string, topK int) ([]types.SearchResult, error) {
	s.mu.Lock()
	*s.topK = topK
	s.mu.Unlock()
	return nil, nil
}

// TestHybridDefaultTopK 验证 topK<=0 时有合理默认值。
func TestHybridDefaultTopK(t *testing.T) {
	var gotTopK int
	var mu sync.Mutex
	h := NewHybrid(&topKRecorder{mu: &mu, topK: &gotTopK}, &stubVector{})

	if _, err := h.Search(context.Background(), "查询", vec(1), 0); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotTopK <= 0 {
		t.Errorf("topK<=0 时应使用默认值，实际传给检索器的是 %d", gotTopK)
	}
}

// TestHybridIsRaceFree 需要 go test -race 才有意义。
func TestHybridIsRaceFree(t *testing.T) {
	h := NewHybrid(
		&stubLexical{results: lexResults(mkChunk(1, "a"), mkChunk(2, "b"))},
		&stubVector{results: vecResults(mkChunk(2, "b"), mkChunk(3, "c"))},
	)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.Search(context.Background(), "查询", vec(1), 10); err != nil {
				t.Errorf("并发调用失败: %v", err)
			}
		}()
	}
	wg.Wait()
}
