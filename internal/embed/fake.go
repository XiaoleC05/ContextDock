package embed

import (
	"context"
	"hash/fnv"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/XiaoleC05/ContextDock/internal/tokenize"
	"github.com/XiaoleC05/ContextDock/internal/types"
)

// FakeEmbedder 是不访问网络的测试替身。
//
// 它用「哈希技巧」（hashing trick）把文本变成向量：
//
//	把文本切成 token → 每个 token 哈希到 1024 维中的某一维 → 计数 → L2 归一化
//
// 这样做的关键收益是**语义相近的文本会得到相近的向量**（共享 token 越多越像），
// 所以向量检索和 RRF 融合的测试才有意义。如果只是随机哈希出向量，
// 那向量检索测试就退化成"随便返回几条"，测不出任何东西。
//
// 它同时满足确定性：同一段文本永远得到同一个向量。
type FakeEmbedder struct {
	tk *tokenize.Tokenizer

	mu    sync.Mutex
	err   error
	delay time.Duration
	calls []int // 每次调用传入的文本条数，供分批测试断言
}

// NewFake 创建测试替身。
func NewFake() *FakeEmbedder {
	return &FakeEmbedder{tk: tokenize.New()}
}

// SetError 让后续的 Embed 直接返回这个错误，用于测试失败路径。
// 传 nil 恢复正常。
func (f *FakeEmbedder) SetError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// SetDelay 让每次 Embed 至少耗时 d，用于测试超时与并发。
func (f *FakeEmbedder) SetDelay(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delay = d
}

// Calls 返回每次调用传入的文本条数，按调用顺序。
//
// 用途：断言批量分块是否正确。比如送 100 条文本进去，
// 期望看到 [32 32 32 4]，而不是一次 100 条。
func (f *FakeEmbedder) Calls() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]int, len(f.calls))
	copy(out, f.calls)
	return out
}

// Embed 实现 Embedder。
func (f *FakeEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	f.mu.Lock()
	err := f.err
	delay := f.delay
	f.calls = append(f.calls, len(texts))
	f.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	if err != nil {
		return nil, err
	}
	if err := checkTexts(texts); err != nil {
		return nil, err
	}

	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = f.vector(t)
	}
	return out, nil
}

// vector 把一段文本映射成 1024 维单位向量。
func (f *FakeEmbedder) vector(text string) []float32 {
	vec := make([]float32, types.EmbeddingDim)
	for _, tok := range f.tk.Tokenize(text) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(tok))
		vec[h.Sum32()%types.EmbeddingDim]++
	}

	// L2 归一化。归一化之后余弦相似度等于点积，
	// 正好模拟真实 embedding 的行为（真实模型输出的也基本是归一化的）。
	var sum float64
	for _, v := range vec {
		sum += float64(v) * float64(v)
	}
	if sum == 0 {
		// 空文本理论上已被 checkTexts 拦住；纯符号的文本会走到这里。
		// 返回一个固定的单位向量，保证后续计算不出现 NaN。
		vec[0] = 1
		return vec
	}
	norm := float32(math.Sqrt(sum))
	for i := range vec {
		vec[i] /= norm
	}
	return vec
}

// checkTexts 做本地校验，避免把明显非法的请求发给 API。
func checkTexts(texts []string) error {
	if len(texts) == 0 {
		return ErrEmptyInput
	}
	for i, t := range texts {
		if strings.TrimSpace(t) == "" {
			return ErrEmptyText
		}
		_ = i
	}
	return nil
}
