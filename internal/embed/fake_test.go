package embed

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/XiaoleC05/ContextDock/internal/types"
)

func TestFakeEmbedDimension(t *testing.T) {
	f := NewFake()
	vecs, err := f.Embed(context.Background(), []string{"测试文本", "another text"})
	if err != nil {
		t.Fatalf("Embed 失败: %v", err)
	}
	if len(vecs) != 2 {
		t.Fatalf("应返回 2 个向量，实际 %d", len(vecs))
	}
	for i, v := range vecs {
		if len(v) != types.EmbeddingDim {
			t.Errorf("第 %d 个向量维度应为 %d，实际 %d", i, types.EmbeddingDim, len(v))
		}
	}
}

// TestFakeEmbedIsDeterministic 是这套替身最重要的性质。
//
// 不确定的话，任何依赖向量结果的测试都会随机失败。
func TestFakeEmbedIsDeterministic(t *testing.T) {
	f := NewFake()
	const text = "ContextDock 混合检索"
	first, err := f.Embed(context.Background(), []string{text})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		got, err := f.Embed(context.Background(), []string{text})
		if err != nil {
			t.Fatal(err)
		}
		for j := range first[0] {
			if first[0][j] != got[0][j] {
				t.Fatalf("第 %d 次调用结果不同，第 %d 维: %v vs %v", i, j, first[0][j], got[0][j])
			}
		}
	}
}

// TestFakeEmbedSimilarTextsAreSimilar 验证替身能反映语义相似度。
//
// 如果替身只是随机哈希出向量，向量检索的测试就完全测不出东西。
// 这个测试保证「共享词多的文本，余弦相似度确实更高」。
func TestFakeEmbedSimilarTextsAreSimilar(t *testing.T) {
	f := NewFake()
	vecs, err := f.Embed(context.Background(), []string{
		"ContextDock 是一个混合检索服务", // 0
		"ContextDock 是混合检索服务",   // 1：与 0 高度相似
		"今天天气不错适合出门散步",          // 2：与 0 无关
	})
	if err != nil {
		t.Fatal(err)
	}

	simRelated := cosine(vecs[0], vecs[1])
	simUnrelated := cosine(vecs[0], vecs[2])

	if !(simRelated > simUnrelated) {
		t.Errorf("相关文本的相似度应高于无关文本:\n  相关 %.4f\n  无关 %.4f",
			simRelated, simUnrelated)
	}
}

func TestFakeEmbedReturnsUnitVectors(t *testing.T) {
	f := NewFake()
	vecs, err := f.Embed(context.Background(), []string{"归一化测试 normalized"})
	if err != nil {
		t.Fatal(err)
	}
	var sum float64
	for _, v := range vecs[0] {
		sum += float64(v) * float64(v)
	}
	if math.Abs(math.Sqrt(sum)-1.0) > 1e-5 {
		t.Errorf("向量应当已 L2 归一化，实际模长 %.6f", math.Sqrt(sum))
	}
}

func TestFakeEmbedRejectsBadInput(t *testing.T) {
	f := NewFake()
	ctx := context.Background()

	if _, err := f.Embed(ctx, nil); !errors.Is(err, ErrEmptyInput) {
		t.Errorf("空列表应返回 ErrEmptyInput，实际 %v", err)
	}
	if _, err := f.Embed(ctx, []string{}); !errors.Is(err, ErrEmptyInput) {
		t.Errorf("空切片应返回 ErrEmptyInput，实际 %v", err)
	}
	if _, err := f.Embed(ctx, []string{"正常", "   "}); !errors.Is(err, ErrEmptyText) {
		t.Errorf("含空文本应返回 ErrEmptyText，实际 %v", err)
	}
}

func TestFakeEmbedInjectedError(t *testing.T) {
	f := NewFake()
	want := errors.New("模拟网络故障")
	f.SetError(want)

	if _, err := f.Embed(context.Background(), []string{"文本"}); !errors.Is(err, want) {
		t.Errorf("应返回注入的错误，实际 %v", err)
	}

	f.SetError(nil)
	if _, err := f.Embed(context.Background(), []string{"文本"}); err != nil {
		t.Errorf("清除错误后应当成功，实际 %v", err)
	}
}

// TestFakeEmbedRecordsCalls 验证调用记录——分批测试靠它断言。
func TestFakeEmbedRecordsCalls(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	for _, n := range []int{3, 5, 1} {
		texts := make([]string, n)
		for i := range texts {
			texts[i] = "文本"
		}
		if _, err := f.Embed(ctx, texts); err != nil {
			t.Fatal(err)
		}
	}
	got := f.Calls()
	want := []int{3, 5, 1}
	if len(got) != len(want) {
		t.Fatalf("调用记录应为 %v，实际 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 次调用条数应为 %d，实际 %d", i, want[i], got[i])
		}
	}
}

// TestFakeEmbedRespectsContext 验证 ctx 取消会立刻返回。
func TestFakeEmbedRespectsContext(t *testing.T) {
	f := NewFake()
	f.SetDelay(time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := f.Embed(ctx, []string{"文本"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("应返回 DeadlineExceeded，实际 %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("ctx 取消后应立即返回，实际耗时 %v", elapsed)
	}
}

func TestFakeEmbedConcurrencySafe(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	done := make(chan error, 30)
	for i := 0; i < 30; i++ {
		go func(n int) {
			_, err := f.Embed(ctx, []string{"并发文本", "concurrent"})
			done <- err
		}(i)
	}
	for i := 0; i < 30; i++ {
		if err := <-done; err != nil {
			t.Errorf("并发调用失败: %v", err)
		}
	}
	if got := f.Calls(); len(got) != 30 {
		t.Errorf("应记录 30 次调用，实际 %d 次", len(got))
	}
}

// cosine 只在测试里用，用于验证替身的语义相似度是否合理。
func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
