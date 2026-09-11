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
