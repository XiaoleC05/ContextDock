// Package embed 把文本转成向量。
//
// 抽象出一层接口的目的：让切分、检索、导入流程都能在**不访问网络**的情况下
// 被测试。真实实现调用硅基流动 API，测试用 FakeEmbedder 替代。
package embed

import (
	"context"
	"errors"
)

var (
	// ErrEmptyInput 表示调用方传了空的文本列表。
	ErrEmptyInput = errors.New("embed: 文本列表为空")

	// ErrEmptyText 表示列表里有空字符串。硅基流动会返回 400，
	// 但那是运行期错误；在本地提前拦下来能给出更清楚的信息。
	ErrEmptyText = errors.New("embed: 文本列表中含有空字符串")

	// ErrDimMismatch 表示返回的向量维度与 types.EmbeddingDim 不符。
	//
	// 单独定义这个错误是有意的：维度不匹配如果放过去，会一路走到
	// pgvector 插入时才报错，而那时的错误信息指向插入语句、不指向源头。
	ErrDimMismatch = errors.New("embed: 返回的向量维度与 EmbeddingDim 不符")

	// ErrNoAPIKey 表示没有配置 API Key。
	ErrNoAPIKey = errors.New("embed: 未配置 API Key")
)

// Embedder 把一组文本转成一组向量。
//
// 契约：
//   - 返回的切片长度必须等于 texts 的长度，且**顺序一一对应**
//   - 每个向量的维度必须是 types.EmbeddingDim（1024）
//   - 必须尊重 ctx：ctx 取消时尽快返回
//   - 同一个 Embedder 实例会被并发调用，实现必须是并发安全的
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}
