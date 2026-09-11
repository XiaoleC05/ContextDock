package embed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/XiaoleC05/ContextDock/internal/types"
)

// fakeVec 造一个指定长度的向量。
func fakeVec(dim int, seed float64) []float64 {
	v := make([]float64, dim)
	for i := range v {
		v[i] = seed + float64(i)*1e-6
	}
	return v
}

// okBody 造一个正常的成功响应。
func okBody(n, dim int) string {
	items := make([]string, n)
	for i := 0; i < n; i++ {
		items[i] = fmt.Sprintf(`{"index":%d,"embedding":%s}`, i, mustJSON(fakeVec(dim, float64(i))))
	}
	return fmt.Sprintf(
		`{"model":"Pro/BAAI/bge-m3","data":[%s],`+
			`"usage":{"prompt_tokens":10,"completion_tokens":0,"total_tokens":10}}`,
		strings.Join(items, ","))
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// newTestServer 起一个假服务器。handler 收到的是原始请求体。
func newTestServer(t *testing.T, handler func(w http.ResponseWriter, body []byte, call int)) (*httptest.Server, *SiliconFlow, *int) {
	t.Helper()
	var (
		mu    sync.Mutex
		calls int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()

		if r.URL.Path != "/embeddings" {
			t.Errorf("请求路径应为 /embeddings，实际 %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization 头不正确: %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		handler(w, body, n)
	}))
	t.Cleanup(srv.Close)

	e, err := NewSiliconFlow("test-key",
		WithBaseURL(srv.URL),
		WithBackoffBase(time.Millisecond), // 让重试测试瞬间完成
	)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	return srv, e, &calls
}

func TestSiliconFlowRequiresAPIKey(t *testing.T) {
	if _, err := NewSiliconFlow(""); !errors.Is(err, ErrNoAPIKey) {
		t.Errorf("空 API Key 应返回 ErrNoAPIKey，实际 %v", err)
	}
}

// TestRequestHasNoDimensionsField 守护 DESIGN §2 的硬约束。
//
// 硅基流动的 dimensions 参数仅对 Qwen3 系列生效，对 bge-m3 传会**返回 400**。
// 从 Qwen3 迁移过来时，1024 这个数字碰巧不用改，很容易忘了删这个参数——
// 而数字对得上会让人以为代码没问题。
func TestRequestHasNoDimensionsField(t *testing.T) {
	_, e, _ := newTestServer(t, func(w http.ResponseWriter, body []byte, _ int) {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Errorf("请求体不是合法 JSON: %v", err)
		}
		if _, ok := raw["dimensions"]; ok {
			t.Error("请求体里出现了 dimensions 字段 —— 对 bge-m3 传它会返回 400")
		}
		// 同时也检查一下原始文本，防止字段名被写成别的形式
		if strings.Contains(string(body), "dimension") {
			t.Errorf("请求体文本里出现了 dimension: %s", body)
		}
		for _, k := range []string{"model", "input", "encoding_format"} {
			if _, ok := raw[k]; !ok {
				t.Errorf("请求体缺少必需字段 %q", k)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBody(1, types.EmbeddingDim)))
	})

	if _, err := e.Embed(context.Background(), []string{"测试"}); err != nil {
		t.Fatalf("Embed 失败: %v", err)
	}
}

// TestBatchAutoSplitsAt32 守护「单次 input 最多 32 条」这个硬上限。
//
// 超一条是**整个请求**返回 400，不是截断。所以必须客户端分批。
func TestBatchAutoSplitsAt32(t *testing.T) {
	var mu sync.Mutex
	var sizes []int

	_, e, _ := newTestServer(t, func(w http.ResponseWriter, body []byte, _ int) {
		var req sfRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("解析请求失败: %v", err)
		}
		mu.Lock()
		sizes = append(sizes, len(req.Input))
		mu.Unlock()

		if len(req.Input) > MaxBatchSize {
			t.Errorf("单次请求 %d 条，超过上限 %d —— 整个请求会 400", len(req.Input), MaxBatchSize)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBody(len(req.Input), types.EmbeddingDim)))
	})

	// 100 条 → 期望 32 + 32 + 32 + 4
	texts := make([]string, 100)
	for i := range texts {
		texts[i] = fmt.Sprintf("第 %d 条文本", i)
	}
	vecs, err := e.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("Embed 失败: %v", err)
	}
	if len(vecs) != 100 {
		t.Fatalf("应返回 100 个向量，实际 %d", len(vecs))
	}

	mu.Lock()
	defer mu.Unlock()
	want := []int{32, 32, 32, 4}
	if len(sizes) != len(want) {
		t.Fatalf("应分 %d 批，实际 %d 批: %v", len(want), len(sizes), sizes)
	}
	for i := range want {
		if sizes[i] != want[i] {
			t.Errorf("第 %d 批应为 %d 条，实际 %d 条（全部批次 %v）", i, want[i], sizes[i], sizes)
		}
	}
}

// TestRetriesOn429 验证限流会重试并最终成功。
func TestRetriesOn429(t *testing.T) {
	_, e, calls := newTestServer(t, func(w http.ResponseWriter, _ []byte, call int) {
		if call <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"Request was rejected due to rate limiting. Details:TPM limit reached."}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBody(1, types.EmbeddingDim)))
	})

	vecs, err := e.Embed(context.Background(), []string{"测试"})
	if err != nil {
		t.Fatalf("应当在重试后成功，实际 %v", err)
	}
	if len(vecs) != 1 {
		t.Fatalf("应返回 1 个向量，实际 %d", len(vecs))
	}
	if *calls != 3 {
		t.Errorf("应当请求 3 次（2 次失败 + 1 次成功），实际 %d 次", *calls)
	}
}

// TestRetriesOn503 验证模型过载（503）也重试。
func TestRetriesOn503(t *testing.T) {
	_, e, calls := newTestServer(t, func(w http.ResponseWriter, _ []byte, call int) {
		if call == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"code":50505,"message":"Model service overloaded. Please try again later."}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBody(1, types.EmbeddingDim)))
	})

	if _, err := e.Embed(context.Background(), []string{"测试"}); err != nil {
		t.Fatalf("应当重试成功，实际 %v", err)
	}
	if *calls != 2 {
		t.Errorf("应当请求 2 次，实际 %d 次", *calls)
	}
}

// TestDoesNotRetryOn400 验证参数错误不重试——重试只是浪费时间和配额。
func TestDoesNotRetryOn400(t *testing.T) {
	_, e, calls := newTestServer(t, func(w http.ResponseWriter, _ []byte, _ int) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":20012,"message":"input batch size 33 > maximum allowed batch size 32"}`))
	})

	_, err := e.Embed(context.Background(), []string{"测试"})
	if err == nil {
		t.Fatal("应当返回错误")
	}
	if *calls != 1 {
		t.Errorf("400 不应重试，应当只请求 1 次，实际 %d 次", *calls)
	}
	var se *sfError
	if !errors.As(err, &se) {
		t.Errorf("错误应当能解析成 sfError，实际 %T: %v", err, err)
	} else if se.Code != 20012 {
		t.Errorf("错误码应为 20012，实际 %d", se.Code)
	}
}

// TestDoesNotRetryOn401 验证鉴权失败不重试。
func TestDoesNotRetryOn401(t *testing.T) {
	_, e, calls := newTestServer(t, func(w http.ResponseWriter, _ []byte, _ int) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":30014,"message":"Token is invalid."}`))
	})

	if _, err := e.Embed(context.Background(), []string{"测试"}); err == nil {
		t.Fatal("应当返回错误")
	}
	if *calls != 1 {
		t.Errorf("401 不应重试，实际请求 %d 次", *calls)
	}
}

// TestGivesUpAfterMaxRetries 验证重试次数用尽后返回错误。
func TestGivesUpAfterMaxRetries(t *testing.T) {
	_, e, calls := newTestServer(t, func(w http.ResponseWriter, _ []byte, _ int) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"rate limited"}`))
	})

	if _, err := e.Embed(context.Background(), []string{"测试"}); err == nil {
		t.Fatal("重试耗尽后应当返回错误")
	}
	// 首次 + maxRetries(4) 次重试 = 5 次
	if *calls != 5 {
		t.Errorf("应当请求 5 次（1 首次 + 4 重试），实际 %d 次", *calls)
	}
}

// TestRejectsWrongDimension 验证维度不符会报错，而不是静默接受。
//
// 维度错如果放过去，会一路走到 pgvector 插入时才失败，
// 而那时的错误指向插入语句、不指向源头。
func TestRejectsWrongDimension(t *testing.T) {
	_, e, _ := newTestServer(t, func(w http.ResponseWriter, _ []byte, _ int) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBody(1, 768))) // 故意错
	})

	_, err := e.Embed(context.Background(), []string{"测试"})
	if !errors.Is(err, ErrDimMismatch) {
		t.Fatalf("应返回 ErrDimMismatch，实际 %v", err)
	}
	// 错误信息里必须能看到实际维度，否则排查无从下手
	if !strings.Contains(err.Error(), "768") {
		t.Errorf("错误信息应包含实际维度 768: %v", err)
	}
}

// TestToleratesUnknownFields 验证多出来的非标准字段不会导致解析失败。
//
// 硅基流动的 usage 里多一个 completion_tokens，这不是 OpenAI 标准字段。
// 如果代码里用了 DisallowUnknownFields，这里就会报错。
func TestToleratesUnknownFields(t *testing.T) {
	_, e, _ := newTestServer(t, func(w http.ResponseWriter, _ []byte, _ int) {
		w.Header().Set("Content-Type", "application/json")
		body := fmt.Sprintf(
			`{"model":"Pro/BAAI/bge-m3","data":[{"index":0,"embedding":%s,"extra_field":"x"}],`+
				`"usage":{"prompt_tokens":5,"completion_tokens":0,"total_tokens":5,"brand_new_field":1}}`,
			mustJSON(fakeVec(types.EmbeddingDim, 1)))
		_, _ = w.Write([]byte(body))
	})

	if _, err := e.Embed(context.Background(), []string{"测试"}); err != nil {
		t.Fatalf("多出的非标准字段不应导致失败: %v", err)
	}
}

// TestRespectsContextDuringRetry 验证重试等待期间取消 ctx 能立刻返回。
func TestRespectsContextDuringRetry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"rate limited"}`))
	}))
	defer srv.Close()

	// 退避设长一点，确保取消发生在等待期间
	e, err := NewSiliconFlow("k", WithBaseURL(srv.URL), WithBackoffBase(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := e.Embed(ctx, []string{"测试"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("应返回 DeadlineExceeded，实际 %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("取消后应立即返回，实际耗时 %v", elapsed)
	}
}

// TestRejectsEmptyInputBeforeRequest 验证空输入在本地就被拦下，不发请求。
func TestRejectsEmptyInputBeforeRequest(t *testing.T) {
	_, e, calls := newTestServer(t, func(w http.ResponseWriter, _ []byte, _ int) {
		t.Error("不该发出请求")
		w.WriteHeader(http.StatusOK)
	})

	ctx := context.Background()
	if _, err := e.Embed(ctx, nil); !errors.Is(err, ErrEmptyInput) {
		t.Errorf("空列表应返回 ErrEmptyInput，实际 %v", err)
	}
	if _, err := e.Embed(ctx, []string{"ok", "  "}); !errors.Is(err, ErrEmptyText) {
		t.Errorf("含空文本应返回 ErrEmptyText，实际 %v", err)
	}
	if *calls != 0 {
		t.Errorf("不该发出任何请求，实际 %d 次", *calls)
	}
}

// TestSortsByIndex 验证服务端乱序返回时能按 index 还原正确顺序。
func TestSortsByIndex(t *testing.T) {
	_, e, _ := newTestServer(t, func(w http.ResponseWriter, _ []byte, _ int) {
		// 故意倒序返回：index 2, 1, 0，各自的向量首元素用来标识身份
		w.Header().Set("Content-Type", "application/json")
		body := fmt.Sprintf(
			`{"model":"m","data":[`+
				`{"index":2,"embedding":%s},`+
				`{"index":1,"embedding":%s},`+
				`{"index":0,"embedding":%s}]}`,
			mustJSON(fakeVec(types.EmbeddingDim, 200)),
			mustJSON(fakeVec(types.EmbeddingDim, 100)),
			mustJSON(fakeVec(types.EmbeddingDim, 0)))
		_, _ = w.Write([]byte(body))
	})

	vecs, err := e.Embed(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []float32{0, 100, 200} {
		if vecs[i][0] != want {
			t.Errorf("第 %d 个向量首元素应为 %v，实际 %v（排序没生效）", i, want, vecs[i][0])
		}
	}
}
