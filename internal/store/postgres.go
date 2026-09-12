package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	pgxvec "github.com/pgvector/pgvector-go/pgx"

	"github.com/XiaoleC05/ContextDock/internal/types"
)

// 连接池的默认参数。
const (
	DefaultMaxConns        = 8
	DefaultMinConns        = 1
	DefaultMaxConnLifetime = time.Hour
	DefaultMaxConnIdleTime = 30 * time.Minute
	DefaultHealthCheck     = time.Minute
)

// DefaultEfSearch 是 HNSW 搜索宽度的兜底值，与 config.DefaultHNSWEfSearch 一致。
//
// 两边各留一份是因为 store 不该依赖 config（它只需要一个整数）。
// 传 0 或负数时用这个兜底，避免"配置忘了传"变成静默的召回下降。
const DefaultEfSearch = 100

// Postgres 是 pgvector 实现。
type Postgres struct {
	pool *pgxpool.Pool

	// efSearch 是 HNSW 的搜索宽度下界（pgvector 的 hnsw.ef_search）。
	//
	// 每次检索实际用的是 max(efSearch, 候选数)：候选数不够宽时 HNSW
	// 会把候选池截断，召回悄悄下降，而结果看起来完全正常。
	efSearch int
}

// 编译期断言。
var (
	_ Store             = (*Postgres)(nil)
	_ EmbeddingSearcher = (*Postgres)(nil)
)

// NewPostgres 建立连接池并做一次连通性检查。
//
// 连接池参数是**显式收窄**的：pgx 默认 MaxConns = max(4, runtime.NumCPU())，
// 在 28 线程的机器上会开到 28 条连接。单机 MCP server 用不到那么多，
// 而且会和容器里的 max_connections 叠加。
//
// AfterConnect 里注册 pgvector 类型是**必须**的：
// 不注册的话，pgx 不认识 vector 类型，读出来是 nil、写进去会报一堆
// 指向不明（甚至误导）的错误。
func NewPostgres(ctx context.Context, dsn string, maxConns int32, efSearch int) (*Postgres, error) {
	if dsn == "" {
		return nil, errors.New("store: 数据库连接串为空")
	}
	if maxConns <= 0 {
		maxConns = DefaultMaxConns
	}
	if efSearch <= 0 {
		efSearch = DefaultEfSearch
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: 解析连接串失败: %w", err)
	}
	cfg.MaxConns = maxConns
	cfg.MinConns = DefaultMinConns
	cfg.MaxConnLifetime = DefaultMaxConnLifetime
	cfg.MaxConnIdleTime = DefaultMaxConnIdleTime
	cfg.HealthCheckPeriod = DefaultHealthCheck
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return pgxvec.RegisterTypes(ctx, conn)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: 建立连接池失败: %w", err)
	}

	// 立刻 Ping 一次：连接串写错、数据库没起、扩展没装这些问题
	// 都应该在**启动期**暴露，而不是等第一次查询。
	// RegisterTypes 在扩展不存在时会报 "vector type not found in the database"，
	// 这条信息比运行期的一堆扫描错误有用得多。
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: 连接数据库失败（检查 DSN、容器是否启动、"+
			"以及是否执行过 CREATE EXTENSION vector）: %w", err)
	}

	return &Postgres{pool: pool, efSearch: efSearch}, nil
}

// Close 实现 Store。
func (p *Postgres) Close() error {
	p.pool.Close()
	return nil
}

// Pool 暴露底层连接池，供集成测试做清理等操作。
func (p *Postgres) Pool() *pgxpool.Pool { return p.pool }

// SaveDocument 实现 Store。
//
// 整个操作在一个事务里：文档和它的全部片段要么一起成功、要么一起失败。
// 中途失败会留下"有片段但没有文档"的孤儿数据——外键能挡住这种，
// 但分段提交还会留下"文档存在但片段只有一半"的情况，那也一样糟。
func (p *Postgres) SaveDocument(ctx context.Context, doc *types.Document, chunks []types.Chunk) ([]types.Chunk, error) {
	if doc == nil {
		return nil, ErrEmptyDocument
	}
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	if len(chunks) == 0 {
		return nil, ErrNoChunks
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: 开启事务失败: %w", err)
	}
	// 提交之后 Rollback 是 no-op，所以这里无条件 defer 是安全的，
	// 而且能保证任何一条 return 路径都不会漏掉回滚。
	defer func() { _ = tx.Rollback(ctx) }()

	meta, err := marshalMetadata(doc.Metadata)
	if err != nil {
		return nil, err
	}

	err = tx.QueryRow(ctx,
		`INSERT INTO document (title, source, content, metadata, content_hash, dedup_key)
		 VALUES ($1, $2, $3, $4::jsonb, $5, $6)
		 RETURNING id`,
		doc.Title, doc.Source, doc.Content, meta, doc.ContentHash, doc.DedupKey,
	).Scan(&doc.ID)
	if err != nil {
		return nil, fmt.Errorf("store: 插入文档失败: %w", err)
	}

	out := make([]types.Chunk, len(chunks))
	for i, c := range chunks {
		c.DocumentID = doc.ID
		if err := c.Validate(); err != nil {
			return nil, err
		}
		cmeta, err := marshalMetadata(c.Metadata)
		if err != nil {
			return nil, err
		}

		err = tx.QueryRow(ctx,
			`INSERT INTO chunk
			   (document_id, ordinal, start_offset, end_offset, content, metadata, embedding)
			 VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7)
			 RETURNING id`,
			c.DocumentID, c.Ordinal, c.StartOffset, c.EndOffset, c.Content, cmeta,
			toVector(c.Embedding),
		).Scan(&c.ID)
		if err != nil {
			return nil, fmt.Errorf("store: 插入第 %d 个片段失败: %w", i, err)
		}
		out[i] = c
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: 提交事务失败: %w", err)
	}
	return out, nil
}

// SaveEmbeddings 实现 Store。
//
// 用一个事务 + 多条 UPDATE 完成。本来可以用 pgx.Batch 提高吞吐，
// 但 Batch 里的错误定位更麻烦；导入量不大时，可读性更重要。
func (p *Postgres) SaveEmbeddings(ctx context.Context, chunks []types.Chunk) error {
	if len(chunks) == 0 {
		return nil
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for i, c := range chunks {
		if c.ID == 0 {
			return fmt.Errorf("store: 第 %d 个片段没有 ID，无法回填向量", i)
		}
		if len(c.Embedding) != types.EmbeddingDim {
			return fmt.Errorf("%w: 第 %d 个片段有 %d 维，期望 %d 维",
				ErrVectorDim, i, len(c.Embedding), types.EmbeddingDim)
		}
		tag, err := tx.Exec(ctx,
			`UPDATE chunk SET embedding = $1 WHERE id = $2`,
			pgvector.NewVector(c.Embedding), c.ID)
		if err != nil {
			return fmt.Errorf("store: 回填第 %d 个片段的向量失败: %w", i, err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: 片段 id=%d", ErrDocumentNotFound, c.ID)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: 提交事务失败: %w", err)
	}
	return nil
}

// AllChunks 实现 Store。
//
// ⚠️ 这是启动时重建内存索引的数据来源，会把**全表**读进内存。
// 数据量大时要改成流式处理（pgx 的 Rows 本身就是流式的，
// 但调用方要一次性建索引，所以最终还是会全在内存里）。
func (p *Postgres) AllChunks(ctx context.Context) ([]types.Chunk, error) {
	rows, err := p.pool.Query(ctx, chunkSelect+` ORDER BY document_id, ordinal, id`)
	if err != nil {
		return nil, fmt.Errorf("store: 查询片段失败: %w", err)
	}
	defer rows.Close()
	return collectChunks(rows)
}

// ChunksByDocument 实现 Store。
func (p *Postgres) ChunksByDocument(ctx context.Context, documentID int64) ([]types.Chunk, error) {
	rows, err := p.pool.Query(ctx,
		chunkSelect+` WHERE document_id = $1 ORDER BY ordinal, id`, documentID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询片段失败: %w", err)
	}
	defer rows.Close()

	out, err := collectChunks(rows)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrDocumentNotFound
	}
	return out, nil
}

// DocumentByDedupKey 实现 Store。
//
// 返回 ErrDocumentNotFound 而不是 (nil, nil)：调用方必须能区分
// 「查过了、确实没有」和「查失败但没报错」——前者要新增，后者要中止。
func (p *Postgres) DocumentByDedupKey(ctx context.Context, key string) (*types.Document, error) {
	if key == "" {
		return nil, ErrDocumentNotFound
	}
	rows, err := p.pool.Query(ctx,
		`SELECT id, title, source, content, metadata, content_hash, dedup_key, created_at
		   FROM document WHERE dedup_key = $1`, key)
	if err != nil {
		return nil, fmt.Errorf("store: 按去重键查文档失败: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("store: 按去重键查文档失败: %w", err)
		}
		return nil, ErrDocumentNotFound
	}
	var (
		d       types.Document
		metaRaw []byte
	)
	if err := rows.Scan(&d.ID, &d.Title, &d.Source, &d.Content, &metaRaw,
		&d.ContentHash, &d.DedupKey, &d.CreatedAt); err != nil {
		return nil, fmt.Errorf("store: 扫描文档失败: %w", err)
	}
	if err := unmarshalMetadata(metaRaw, &d.Metadata); err != nil {
		return nil, err
	}
	return &d, nil
}

// Documents 实现 Store。
func (p *Postgres) Documents(ctx context.Context) ([]types.Document, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, title, source, content, metadata, content_hash, dedup_key, created_at
		 FROM document ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: 查询文档失败: %w", err)
	}
	defer rows.Close()

	var out []types.Document
	for rows.Next() {
		var (
			d       types.Document
			metaRaw []byte
		)
		if err := rows.Scan(&d.ID, &d.Title, &d.Source, &d.Content, &metaRaw,
			&d.ContentHash, &d.DedupKey, &d.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: 扫描文档失败: %w", err)
		}
		if err := unmarshalMetadata(metaRaw, &d.Metadata); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历文档失败: %w", err)
	}
	if out == nil {
		return []types.Document{}, nil
	}
	return out, nil
}

// DeleteDocument 实现 Store。
//
// 片段由外键的 ON DELETE CASCADE 一起删掉，不需要手动删。
func (p *Postgres) DeleteDocument(ctx context.Context, documentID int64) error {
	tag, err := p.pool.Exec(ctx, `DELETE FROM document WHERE id = $1`, documentID)
	if err != nil {
		return fmt.Errorf("store: 删除文档失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: id=%d", ErrDocumentNotFound, documentID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 内部辅助
// ---------------------------------------------------------------------------

// chunkSelect 是片段查询的公共列。
//
// ⚠️ embedding 被转成 **text** 而不是直接 select vector 列。
// 原因是 pgvector-go 的 pgx codec 只接受 *pgvector.Vector 作为扫描目标，
// 而 NULL（尚未算向量的片段）需要额外处理。转成文本之后，
// 用 *string 接住 NULL、用 Vector.Parse 解析，逻辑直白且不会踩到 codec 的边界。
const chunkSelect = `
SELECT id, document_id, ordinal, start_offset, end_offset, content,
       metadata, embedding::text
FROM chunk`

func collectChunks(rows pgx.Rows) ([]types.Chunk, error) {
	var out []types.Chunk
	for rows.Next() {
		var (
			c       types.Chunk
			metaRaw []byte
			embText *string
		)
		if err := rows.Scan(&c.ID, &c.DocumentID, &c.Ordinal,
			&c.StartOffset, &c.EndOffset, &c.Content, &metaRaw, &embText); err != nil {
			return nil, fmt.Errorf("store: 扫描片段失败: %w", err)
		}
		if err := unmarshalMetadata(metaRaw, &c.Metadata); err != nil {
			return nil, err
		}
		if embText != nil {
			var v pgvector.Vector
			if err := v.Parse(*embText); err != nil {
				return nil, fmt.Errorf("store: 解析片段 id=%d 的向量失败: %w", c.ID, err)
			}
			c.Embedding = v.Slice()
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历片段失败: %w", err)
	}
	if out == nil {
		return []types.Chunk{}, nil
	}
	return out, nil
}

// annQuery 是近邻检索的 SQL。
//
// ⚠️ 三个与索引强耦合的点，改动前先读 docs/PITFALLS.md 的 pgvector 一节：
//
//  1. 距离算子必须是 `<=>`（余弦距离），与建索引时的 vector_cosine_ops
//     **逐字匹配**。写成 `<->`（L2）或 `<#>`（内积）**不会报任何错**，
//     只会静默退化成全表扫描——索引白建，而结果看起来完全一样。
//  2. `WHERE embedding IS NOT NULL` 是必须的：还没算过向量的片段
//     （导入中途失败会留下这种）距离是 NULL，扫进 float64 会报错，
//     而错误信息指向扫描、不指向源头。
//  3. **不 select embedding 本身**。它是全表最贵的一列（1024 维转 text
//     有十几 KB），而检索下游没有任何地方读 Chunk.Embedding。带上它
//     等于把这个 issue 想省的内存和带宽原样搬回来。
const annQuery = `
SELECT id, document_id, ordinal, start_offset, end_offset, content, metadata,
       embedding <=> $1 AS distance
FROM chunk
WHERE embedding IS NOT NULL
ORDER BY embedding <=> $1
LIMIT $2`

// SearchByEmbedding 实现 EmbeddingSearcher：把近邻检索下推到数据库。
//
// 与内存版（retrieve.VectorIndex）的差异只在**精确性**：HNSW 是近似
// 索引，可能返回少于 topK 条、也可能漏掉真正的最近邻。契约见
// EmbeddingSearcher 的说明。
//
// 注意这里**没有**分页或 OFFSET：HNSW 在带 OFFSET 时退化成
// "取前 N+offset 条再丢掉前 offset 条"，越翻越慢。
func (p *Postgres) SearchByEmbedding(ctx context.Context, query []float32, topK int) ([]types.SearchResult, error) {
	if len(query) != types.EmbeddingDim {
		return nil, fmt.Errorf("%w: 查询向量 %d 维，期望 %d 维",
			ErrVectorDim, len(query), types.EmbeddingDim)
	}
	if topK <= 0 {
		return nil, fmt.Errorf("store: SearchByEmbedding 的 topK 必须为正数，实际 %d", topK)
	}

	// ⚠️ ef_search 必须 >= 本次要取的候选数。小了会把候选池截断，
	// 召回悄悄下降，而返回的结果条数看起来完全正常。
	ef := p.efSearch
	if ef < topK {
		ef = topK
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// ⚠️ 这里必须把整数**拼进 SQL**：GUC 的值不支持参数占位符
	// （`SET LOCAL hnsw.ef_search = $1` 是语法错误）。拼进去的是
	// 程序自己算出来的整数，不是外部输入。
	//
	// 用 SET LOCAL 而不是 SET：连接池会复用连接，`SET` 会留在连接上
	// 影响后续查询（docs/PITFALLS.md 记了这条）。LOCAL 要求有事务，
	// 所以上面开了 tx——这也是本次检索多两次往返的原因。
	if _, err := tx.Exec(ctx, fmt.Sprintf(`SET LOCAL hnsw.ef_search = %d`, ef)); err != nil {
		return nil, fmt.Errorf("store: 设置 hnsw.ef_search=%d 失败: %w", ef, err)
	}

	rows, err := tx.Query(ctx, annQuery, pgvector.NewVector(query), topK)
	if err != nil {
		return nil, fmt.Errorf("store: 向量检索失败: %w", err)
	}

	out, err := collectNeighbors(rows)
	// 必须先关掉 rows 再提交：pgx 要求连接上的结果集读完才能复用。
	rows.Close()
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: 提交事务失败: %w", err)
	}
	return out, nil
}

// collectNeighbors 把 annQuery 的结果集扫成 SearchResult。
//
// 单独开一个函数是为了让 rows 的生命周期在 SearchByEmbedding 里
// 看得清楚——它必须在 Commit 之前关闭。
func collectNeighbors(rows pgx.Rows) ([]types.SearchResult, error) {
	out := make([]types.SearchResult, 0, 16)
	rank := 0
	for rows.Next() {
		var (
			c        types.Chunk
			metaRaw  []byte
			distance float64
		)
		if err := rows.Scan(&c.ID, &c.DocumentID, &c.Ordinal,
			&c.StartOffset, &c.EndOffset, &c.Content, &metaRaw, &distance); err != nil {
			return nil, fmt.Errorf("store: 扫描检索结果失败: %w", err)
		}
		if err := unmarshalMetadata(metaRaw, &c.Metadata); err != nil {
			return nil, err
		}

		// ⚠️ `<=>` 是**余弦距离**，相似度 = 1 - 距离。
		//
		// 填反了**排序不会变**（SQL 里已经排好了），所以融合结果照常正确；
		// 但暴露给 Agent 的 vector_score 就成了错的——而 README 里
		// 「阈值判不了相关性」那条结论正建立在这个字段上。
		// 现有测试一个都抓不到，因为 RRF 根本不看分数。
		sim := 1 - distance

		rank++
		out = append(out, types.SearchResult{
			Chunk:       c,
			Score:       sim,
			VectorScore: sim,
			VectorRank:  rank, // 1-based，与内存版一致
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历检索结果失败: %w", err)
	}
	return out, nil
}

// toVector 把 []float32 转成可以传给 pgx 的值。
//
// 空切片返回 nil（写进数据库就是 NULL），因为 pgvector 的 codec
// 只认 pgvector.Vector 值类型，传 nil 是让 pgx 走它自己的 NULL 路径。
func toVector(v []float32) any {
	if len(v) == 0 {
		return nil
	}
	return pgvector.NewVector(v)
}

// marshalMetadata 把元数据转成 JSON 文本，空 map 返回 nil（写进库是 NULL）。
func marshalMetadata(m map[string]string) (any, error) {
	if len(m) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("store: 序列化元数据失败: %w", err)
	}
	return string(b), nil
}

// unmarshalMetadata 把 JSONB 列解析回 map。NULL 或空值保持 nil。
func unmarshalMetadata(raw []byte, dst *map[string]string) error {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("store: 解析元数据失败: %w", err)
	}
	if len(m) > 0 {
		*dst = m
	}
	return nil
}
