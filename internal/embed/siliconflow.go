package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sort"
	"time"

	"github.com/XiaoleC05/ContextDock/internal/types"
)

// 硅基流动的接口常量。
const (
	// DefaultBaseURL 是国内站的 OpenAI 兼容入口。
	DefaultBaseURL = "https://api.siliconflow.cn/v1"

	// DefaultModel 是 bge-m3 的 Pro 版本。
	//
	// 选它的理由：原生输出 1024 维（不需要 Matryoshka 截断，无信息损失），
	// 且 1024 落在 pgvector 的 HNSW 索引上限（2000 维）之内。
	// 详见 docs/DESIGN.md §1。
	DefaultModel = "Pro/BAAI/bge-m3"

	// MaxBatchSize 是单次请求 input 数组的**硬上限**。
	//
	// ⚠️ 超一条是**整个请求**返回 400，不是截断：
	//     input batch size 33 > maximum allowed batch size 32
	// 所以必须在客户端分批。
	MaxBatchSize = 32

	// DefaultTimeout 是单次 HTTP 请求的超时。
	// 取 90s：单条最长 8192 token，长文本 embedding 耗时较长。
	DefaultTimeout = 90 * time.Second

	// defaultBackoffBase 是指数退避的基准时长。
	defaultBackoffBase = 500 * time.Millisecond

	// maxBackoff 是退避的上限，防止重试次数多时等待过久。
	maxBackoff = 30 * time.Second
)

// SiliconFlow 是调用硅基流动 Embedding API 的真实实现。
//
// 它是并发安全的：所有字段在构造后只读。
type SiliconFlow struct {
	apiKey      string
	baseURL     string
	model       string
	client      *http.Client
	maxRetries  int
	backoffBase time.Duration
}

// Option 用于配置 SiliconFlow。
type Option func(*SiliconFlow)

// WithBaseURL 覆盖接口地址（测试用 httptest.Server 时需要）。
func WithBaseURL(u string) Option { return func(s *SiliconFlow) { s.baseURL = u } }

// WithModel 覆盖模型 ID。
func WithModel(m string) Option { return func(s *SiliconFlow) { s.model = m } }

// WithHTTPClient 覆盖 HTTP 客户端。
func WithHTTPClient(c *http.Client) Option { return func(s *SiliconFlow) { s.client = c } }

// WithMaxRetries 覆盖最大重试次数（不含首次尝试）。
func WithMaxRetries(n int) Option { return func(s *SiliconFlow) { s.maxRetries = n } }

// WithBackoffBase 覆盖退避基准时长。测试里设成 1ms 可以让重试测试瞬间完成。
func WithBackoffBase(d time.Duration) Option { return func(s *SiliconFlow) { s.backoffBase = d } }

// NewSiliconFlow 构造一个真实 Embedder。
//
// apiKey 为空时直接返回错误，而不是等到第一次检索才 401——
// 启动期失败比运行期失败便宜得多。
func NewSiliconFlow(apiKey string, opts ...Option) (*SiliconFlow, error) {
	if apiKey == "" {
		return nil, ErrNoAPIKey
	}
	s := &SiliconFlow{
		apiKey:      apiKey,
		baseURL:     DefaultBaseURL,
		model:       DefaultModel,
		client:      &http.Client{Timeout: DefaultTimeout},
		maxRetries:  4,
		backoffBase: defaultBackoffBase,
	}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// Embed 实现 Embedder。它会自动按 MaxBatchSize 分批。
//
// 分批是**顺序**执行的，不是并发。原因是硅基流动的限流按账户+模型计算，
// 并发请求会更容易撞上 TPM 上限，而且撞上之后重试的等待会互相叠加。
// 第一版以稳定为先；真要提速，应该在更高层做带限流的并发。
func (s *SiliconFlow) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if err := checkTexts(texts); err != nil {
		return nil, err
	}

	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += MaxBatchSize {
		end := start + MaxBatchSize
		if end > len(texts) {
			end = len(texts)
		}
		vecs, err := s.embedBatch(ctx, texts[start:end])
		if err != nil {
			return nil, fmt.Errorf("embed: 第 %d~%d 条失败: %w", start+1, end, err)
		}
		out = append(out, vecs...)
	}
	return out, nil
}

// sfRequest 是请求体。
//
// ⚠️ 这里**刻意没有 Dimensions 字段**。
// 硅基流动的 dimensions 参数「仅 Qwen/Qwen3 系列支持」，对 bge-m3 传会返回 400。
// bge-m3 原生就是 1024 维，不需要也不应该指定。详见 docs/DESIGN.md §2。
type sfRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	EncodingFormat string   `json:"encoding_format"`
}

// sfResponse 是响应体。
//
// 注意 usage 里多一个 completion_tokens（非 OpenAI 标准字段）。
// 因此**绝不能开 DisallowUnknownFields**——那会因为这个多出来的字段直接报错。
type sfResponse struct {
	Model string `json:"model"`
	Data  []struct {
		Index     int       `json:"index"`
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// sfError 是硅基流动的错误体。
type sfError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data"`
}

func (e *sfError) Error() string {
	return fmt.Sprintf("siliconflow 错误 %d: %s", e.Code, e.Message)
}

// embedBatch 发一批（不超过 MaxBatchSize 条），带重试。
func (s *SiliconFlow) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) > MaxBatchSize {
		return nil, fmt.Errorf("embed: 批次 %d 条超过上限 %d", len(texts), MaxBatchSize)
	}

	body, err := json.Marshal(sfRequest{
		Model:          s.model,
		Input:          texts,
		EncodingFormat: "float",
	})
	if err != nil {
		return nil, fmt.Errorf("embed: 构造请求失败: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= s.maxRetries; attempt++ {
		if attempt > 0 {
			if err := s.sleepBackoff(ctx, attempt); err != nil {
				return nil, err
			}
		}

		vecs, retryable, err := s.doRequest(ctx, body)
		if err == nil {
			return vecs, nil
		}
		lastErr = err

		if !retryable {
			return nil, err // 4xx 之类的错误，重试没有意义
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("embed: 重试 %d 次后仍失败: %w", s.maxRetries, lastErr)
}

// doRequest 发一次请求。
//
// 第二个返回值表示「这个错误是否值得重试」。
func (s *SiliconFlow) doRequest(ctx context.Context, body []byte) ([][]float32, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, false, fmt.Errorf("embed: 构造请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		// 网络层错误（超时、连接重置）通常是瞬时的，值得重试。
		return nil, true, fmt.Errorf("embed: 请求失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, true, fmt.Errorf("embed: 读取响应失败: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
		// 不用 json.Decoder + DisallowUnknownFields，理由见 sfResponse 的注释。
		var out sfResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, false, fmt.Errorf("embed: 解析响应失败: %w", err)
		}
		return convertVectors(out)

	case http.StatusTooManyRequests, // 429：限流（TPM/RPM）
		http.StatusServiceUnavailable, // 503：code 50505 模型过载
		http.StatusGatewayTimeout:     // 504：上游超时
		var se sfError
		_ = json.Unmarshal(raw, &se)
		if se.Message == "" {
			se.Message = string(raw)
		}
		return nil, true, &se

	default:
		// 400 参数错误 / 401 鉴权失败 / 403 常见于未实名认证 —— 重试都没用。
		var se sfError
		if json.Unmarshal(raw, &se) == nil && se.Message != "" {
			return nil, false, &se
		}
		return nil, false, fmt.Errorf("embed: HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
}

// convertVectors 把响应里的 []float64 转成 []float32 并校验维度。
func convertVectors(out sfResponse) ([][]float32, bool, error) {
	if len(out.Data) == 0 {
		return nil, false, fmt.Errorf("embed: 响应里没有任何向量")
	}

	// 服务端返回的顺序不保证与请求一致，按 index 排序后再取。
	sort.Slice(out.Data, func(i, j int) bool { return out.Data[i].Index < out.Data[j].Index })

	vecs := make([][]float32, len(out.Data))
	for i, d := range out.Data {
		if len(d.Embedding) != types.EmbeddingDim {
			// 这条错误信息必须带上实际维度，否则排查时无从下手。
			return nil, false, fmt.Errorf("%w: 期望 %d，实际 %d",
				ErrDimMismatch, types.EmbeddingDim, len(d.Embedding))
		}
		v := make([]float32, types.EmbeddingDim)
		for j, f := range d.Embedding {
			v[j] = float32(f)
		}
		vecs[i] = v
	}
	return vecs, false, nil
}

// sleepBackoff 指数退避 + 抖动，并且尊重 ctx 取消。
func (s *SiliconFlow) sleepBackoff(ctx context.Context, attempt int) error {
	base := s.backoffBase << uint(attempt-1)
	if base > maxBackoff || base <= 0 {
		base = maxBackoff
	}
	jitter := time.Duration(rand.Int63n(int64(base/2) + 1))

	t := time.NewTimer(base + jitter)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
