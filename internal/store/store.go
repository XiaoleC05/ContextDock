// Package store 定义文档与片段的持久化抽象，并提供两套实现。
//
// 两套实现并存是刻意的：
//   - memory：纯内存，用于单元测试和 benchmark——写检索逻辑时不该被迫先起数据库
//   - postgres：pgvector 持久化，用于真实运行
//
// 两套实现必须行为一致，否则测试通过不代表线上可用。
package store

import (
	"context"
	"errors"

	"github.com/XiaoleC05/ContextDock/internal/types"
)

var (
	// ErrDocumentNotFound 表示按 ID 找不到文档。
	ErrDocumentNotFound = errors.New("store: 文档不存在")

	// ErrEmptyDocument 表示要保存的文档没有内容。
	ErrEmptyDocument = errors.New("store: 文档内容为空")

	// ErrNoChunks 表示要保存的文档没有产出任何片段。
	//
	// 单独定义这个错误是有意的：空文档被切成 0 个片段后如果静默入库，
	// 检索时它永远召不回，而且不报错——属于最难排查的一类问题。
	ErrNoChunks = errors.New("store: 文档没有产出任何片段")

	// ErrVectorDim 表示向量的维度与 types.EmbeddingDim 不符。
	//
	// 维度不对如果放过去，会一路走到数据库插入时才报错，
	// 而那时的错误信息指向 SQL 语句、不指向源头。
	ErrVectorDim = errors.New("store: 向量维度与 EmbeddingDim 不符")
)

// Store 是持久化抽象。
//
// 所有方法都接收 ctx，且实现必须是**并发安全**的：
// MCP server 会并发处理多个请求。
type Store interface {
	// SaveDocument 保存一篇文档及其全部片段。
	//
	// 必须是**事务性**的：要么文档和片段一起成功，要么一起失败。
	// 否则一次中途失败会留下"有片段但没有文档"的孤儿数据。
	//
	// 成功后回填 doc.ID，并返回带上了 ID 的片段列表。
	SaveDocument(ctx context.Context, doc *types.Document, chunks []types.Chunk) ([]types.Chunk, error)

	// AllChunks 按 (document_id, ordinal) 顺序读出**全部**片段（含向量）。
	//
	// 这是启动时重建内存索引的数据来源——见 docs/DESIGN.md 里关于
	// "重启后数据恢复"的说明。BM25 是内存索引，每次启动都要从库里重建。
	AllChunks(ctx context.Context) ([]types.Chunk, error)

	// ChunksByDocument 读出一篇文档的全部片段（按 ordinal 排序）。
	// 用于上下文扩展：找到某段后，顺手把它前后的段也取出来。
	ChunksByDocument(ctx context.Context, documentID int64) ([]types.Chunk, error)

	// Documents 列出全部文档。
	Documents(ctx context.Context) ([]types.Document, error)

	// DocumentByDedupKey 按去重键查找文档。找不到返回 ErrDocumentNotFound。
	//
	// 存在的原因见 types.Document.DedupKey()：重复导入要认出"这是同一份东西"，
	// 而查一次比"先插再查有没有重复"可靠得多——后者在并发下拦不住。
	DocumentByDedupKey(ctx context.Context, key string) (*types.Document, error)

	// DeleteDocument 删除文档及其全部片段。
	DeleteDocument(ctx context.Context, documentID int64) error

	// SaveEmbeddings 为已落库的片段回填向量。
	//
	// 拆成单独的方法是因为流程上向量是后算的：
	// 先落库拿到 ID，再调 Embedding API。这样中途失败时文档不会丢，
	// 重试只需要重算向量。
	SaveEmbeddings(ctx context.Context, chunks []types.Chunk) error

	// Close 释放资源（连接池等）。
	Close() error
}

// EmbeddingSearcher 是存储**可选**的向量检索能力。
//
// 实现了它的存储可以直接在库内做近邻检索，不必把全部向量读进内存。
// 目前只有 *Postgres 实现。
//
// # 为什么是独立接口，而不是并进 Store
//
// 并进 Store 会逼着 *Memory 也实现一份，而那意味着它要在
// SaveDocument / SaveEmbeddings / DeleteDocument 里同步维护第二份
// 派生状态——一处「必须保持同步、错了不报错、只是结果变差」的状态。
// 内存路径已经有 retrieve.VectorIndex 承担同样的职责（Service 在
// 没有 EmbeddingSearcher 时就用它），没必要再来一份。
//
// 因此内存路径的暴力扫描是**对照组基线**，不是临时方案。
//
// # 语义契约（两套实现必须一致）
//
//   - 返回**至多** topK 条，按余弦相似度**降序**
//   - ⚠️ **不保证穷尽**：HNSW 是近似索引，返回条数可能少于 topK。
//     调用方**不得**假设 len(results) == topK
//   - 每条结果必须填好：
//   - Score 与 VectorScore —— 都是**原始余弦相似度**，不是距离
//   - VectorRank —— 1-based
//   - Chunk 的 ID / DocumentID / Ordinal / 偏移 / Content / Metadata。
//     ⚠️ Metadata 尤其不能漏：评测的命中判定读 Metadata["source"]，
//     漏了会让**所有 recall 静默归零**，而报告照样打印
//   - 空结果返回**非 nil 的空切片**
//   - topK <= 0 返回错误。数据库表达不了「不限量」，而静默只返回
//     ef_search 条会得到一个看起来正常、实则可疑偏小的候选池
type EmbeddingSearcher interface {
	SearchByEmbedding(ctx context.Context, query []float32, topK int) ([]types.SearchResult, error)
}
