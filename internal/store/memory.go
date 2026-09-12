package store

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/XiaoleC05/ContextDock/internal/types"
)

// Memory 是纯内存实现。
//
// 它存在的意义不是"生产可用"，而是让检索逻辑的测试和 benchmark
// 不必依赖数据库。所以它追求的是**行为与 Postgres 一致**，而不是性能。
type Memory struct {
	mu sync.RWMutex

	docs   map[int64]types.Document
	chunks map[int64][]types.Chunk // documentID -> chunks

	nextDocID   int64
	nextChunkID int64
}

// NewMemory 创建一个空的内存存储。
func NewMemory() *Memory {
	return &Memory{
		docs:   make(map[int64]types.Document),
		chunks: make(map[int64][]types.Chunk),
	}
}

// SaveDocument 实现 Store。
func (m *Memory) SaveDocument(ctx context.Context, doc *types.Document, chunks []types.Chunk) ([]types.Chunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, ErrEmptyDocument
	}
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	if len(chunks) == 0 {
		return nil, ErrNoChunks
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// 先分配文档 ID —— 片段的 DocumentID 是**必填**字段，
	// 校验时必须已经填好（它由存储层分配，调用方给不出来）。
	m.nextDocID++
	doc.ID = m.nextDocID

	// ⚠️ 全部校验通过之后才真正落盘。
	//
	// 边校验边写的话，第 N 个片段非法时前面那些和文档本身已经写进去了，
	// 留下"有文档但片段不全"的脏数据——而且不报错，只是检索时少几段。
	// Postgres 版有事务兜底，内存版没有，只能靠这个先后顺序保证原子性。
	out := make([]types.Chunk, len(chunks))
	for i, c := range chunks {
		c.DocumentID = doc.ID
		if err := c.Validate(); err != nil {
			return nil, fmt.Errorf("store: 第 %d 个片段校验失败: %w", i, err)
		}
		out[i] = c
	}

	// 到这里才动共享状态。
	for i := range out {
		m.nextChunkID++
		out[i].ID = m.nextChunkID
	}
	m.docs[doc.ID] = *doc
	m.chunks[doc.ID] = out

	// ⚠️ 返回**副本**，不是 out 本身。
	//
	// 调用方拿到之后会就地改写元素：ingest 回填向量时写
	// `saved[i].Embedding = vecs[i]`，而**那一步不持锁**。
	// 如果返回的是存在 map 里的那一个切片，这个无锁的写就会和
	// AllChunks 在读锁下的读构成数据竞争——读锁保护不了不持锁的写方。
	//
	// 副本化之后所有权是清楚的：交出去的归调用方，store 内部那一份
	// 只有 store 自己能碰，而 store 的每次访问都在锁下。
	//
	// 边界：这是**浅拷贝**。调用方替换元素（`saved[i] = ...`）不会
	// 影响 store；但透过 `Embedding` 那个切片去改**底层数组的元素**
	// 仍会共享。当前没有代码这么做，也不该这么做——要改向量请走
	// SaveEmbeddings，那条路是在锁内写的。
	return append([]types.Chunk(nil), out...), nil
}

// AllChunks 实现 Store。
func (m *Memory) AllChunks(ctx context.Context) ([]types.Chunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	// 先算总数，一次分配到位——避免 append 过程中的多次扩容。
	total := 0
	for _, cs := range m.chunks {
		total += len(cs)
	}
	out := make([]types.Chunk, 0, total)
	for _, cs := range m.chunks {
		out = append(out, cs...)
	}

	// 按 (document_id, ordinal) 排序，保证顺序稳定可复现。
	// 不排序的话，map 遍历顺序是随机的，重建出来的索引虽然内容一样，
	// 但 benchmark 和调试时的表现会不稳定。
	sortChunks(out)
	return out, nil
}

// ChunksByDocument 实现 Store。
func (m *Memory) ChunksByDocument(ctx context.Context, documentID int64) ([]types.Chunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	cs, ok := m.chunks[documentID]
	if !ok {
		return nil, ErrDocumentNotFound
	}
	out := make([]types.Chunk, len(cs))
	copy(out, cs)
	sortChunks(out)
	return out, nil
}

// DocumentByDedupKey 实现 Store。
func (m *Memory) DocumentByDedupKey(ctx context.Context, key string) (*types.Document, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if key == "" {
		return nil, ErrDocumentNotFound
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	// 线性扫描。内存版是给测试和小规模场景用的，
	// 而"文档数"在这里通常是几十条——为它维护一张索引表不划算。
	for _, d := range m.docs {
		if d.DedupKey == key {
			cp := d
			return &cp, nil
		}
	}
	return nil, ErrDocumentNotFound
}

// Documents 实现 Store。
func (m *Memory) Documents(ctx context.Context) ([]types.Document, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]types.Document, 0, len(m.docs))
	for _, d := range m.docs {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// DeleteDocument 实现 Store。
func (m *Memory) DeleteDocument(ctx context.Context, documentID int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.docs[documentID]; !ok {
		return ErrDocumentNotFound
	}
	delete(m.docs, documentID)
	// 片段跟着文档一起删——模拟 Postgres 的 ON DELETE CASCADE。
	delete(m.chunks, documentID)
	return nil
}

// SaveEmbeddings 实现 Store。
func (m *Memory) SaveEmbeddings(ctx context.Context, chunks []types.Chunk) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, c := range chunks {
		if c.ID == 0 {
			return ErrDocumentNotFound
		}
		cs, ok := m.chunks[c.DocumentID]
		if !ok {
			return ErrDocumentNotFound
		}
		found := false
		for i := range cs {
			if cs[i].ID == c.ID {
				cs[i].Embedding = c.Embedding
				found = true
				break
			}
		}
		if !found {
			return ErrDocumentNotFound
		}
	}
	return nil
}

// Close 实现 Store。内存实现没有资源要释放。
func (m *Memory) Close() error { return nil }

// Len 返回已保存的文档数，供测试断言。
func (m *Memory) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.docs)
}

// sortChunks 按 (document_id, ordinal, id) 排序。
//
// 三级排序键：文档 ID 相同时按序号，序号也相同时按落库 ID。
// 最后一级是为了让同序号（理论上不该出现）的顺序也稳定。
func sortChunks(cs []types.Chunk) {
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].DocumentID != cs[j].DocumentID {
			return cs[i].DocumentID < cs[j].DocumentID
		}
		if cs[i].Ordinal != cs[j].Ordinal {
			return cs[i].Ordinal < cs[j].Ordinal
		}
		return cs[i].ID < cs[j].ID
	})
}

// 编译期断言：Memory 必须满足 Store。
//
// 这行的价值：接口改动时，实现类不满足会**在编译期**报错，
// 而不是等到赋值给 Store 变量时才暴露。
var _ Store = (*Memory)(nil)
