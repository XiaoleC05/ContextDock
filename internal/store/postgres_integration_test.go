//go:build integration

// 集成测试：需要一个真实的 PostgreSQL + pgvector 实例。
//
// ⚠️ **必须显式指定 CONTEXTDOCK_TEST_DSN，否则整个包的测试会被跳过。**
//
// 这里刻意**没有默认值**：每个测试都会 `TRUNCATE document CASCADE`，
// 而回落到开发库（deploy/docker-compose.yml 起的那个）意味着
// 「照着文档跑一次测试就把开发库清空了」，跑的人还不会意识到。
//
// 用一个**专用库**：
//
//	docker compose -f deploy/docker-compose.yml up -d
//	docker exec contextdock-pg psql -U postgres -c "CREATE DATABASE contextdock_test"
//	for f in migrations/*.sql; do
//	    docker exec -i contextdock-pg psql -v ON_ERROR_STOP=1 -U postgres -d contextdock_test < "$f"
//	done
//	CONTEXTDOCK_TEST_DSN="postgres://postgres:postgres@localhost:5432/contextdock_test?sslmode=disable" \
//	    go test -v -tags=integration ./internal/store/
package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"

	"github.com/XiaoleC05/ContextDock/internal/types"
)

// testDSN 返回集成测试要连的库。**刻意没有默认值**。
//
// ⚠️ 这里曾经回落到 `postgres://…/contextdock`——也就是
// deploy/docker-compose.yml 起的那个库、.env 里配的那个库。
// 而每个集成测试都会 `TRUNCATE document CASCADE`：
// **照着 README 跑一次测试就把开发库清空了**，而跑的人不会意识到。
//
// 清库本身没错（集成测试必须从干净状态开始），错的是"默认清哪个库"。
// 所以现在缺 DSN 时直接**跳过**，把"要跑集成测试"变成一个显式动作。
// CI 里本来就设了 CONTEXTDOCK_TEST_DSN，不受影响。
func testDSN(t *testing.T) string {
	t.Helper()
	v := os.Getenv("CONTEXTDOCK_TEST_DSN")
	if v == "" {
		t.Skip("未设置 CONTEXTDOCK_TEST_DSN，跳过集成测试。\n" +
			"这里刻意不回落到开发库——集成测试会 TRUNCATE，而开发库里可能有真实数据。\n" +
			"要跑就显式指定一个**专用库**，例如：\n" +
			"  CONTEXTDOCK_TEST_DSN=\"postgres://postgres:postgres@localhost:5432/contextdock_test?sslmode=disable\" \\\n" +
			"    go test -tags=integration ./internal/store/")
	}
	return v
}

// newTestStore 连上数据库并清空表，保证每个测试从干净状态开始。
//
// 清空是必须的：集成测试如果依赖残留数据，就会"单跑通过、连跑失败"，
// 而且失败原因极难定位。TRUNCATE ... RESTART IDENTITY CASCADE
// 同时重置自增序列，让 ID 从 1 开始，断言才能写死具体数字。
func newTestStore(t *testing.T) *Postgres {
	t.Helper()
	ctx := context.Background()

	p, err := NewPostgres(ctx, testDSN(t), 4, 0)
	if err != nil {
		t.Fatalf("连接数据库失败（容器起来了吗？）: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	if _, err := p.pool.Exec(ctx, `TRUNCATE document RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("清空表失败: %v", err)
	}
	return p
}

// TestIntegrationExtensionVersion 验证 pgvector 版本满足安全要求。
func TestIntegrationExtensionVersion(t *testing.T) {
	p := newTestStore(t)

	var version string
	if err := p.pool.QueryRow(context.Background(),
		`SELECT extversion FROM pg_extension WHERE extname = 'vector'`).Scan(&version); err != nil {
		t.Fatalf("未安装 vector 扩展: %v", err)
	}
	t.Logf("pgvector 版本: %s", version)

	// 0.8.6 修复了 CVE-2026-3172（并行 HNSW 建索引堆溢出，CVSS 8.1）。
	// 用字符串比较粗糙，但版本号是 "0.8.6" 这种定长格式，够用。
	if version < "0.8.6" {
		t.Errorf("pgvector 版本 %s 低于 0.8.6，存在 CVE-2026-3172 风险", version)
	}
}

// TestIntegrationRoundTrip 是核心验收：写入 → 读出，字段一个不少。
func TestIntegrationRoundTrip(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)

	doc := &types.Document{
		Title:   "测试文档",
		Source:  "inline",
		Content: "这是一篇用于集成测试的文档。",
	}
	doc.SetMetadata("ext", ".md")

	chunks := []types.Chunk{
		{Ordinal: 0, StartOffset: 0, EndOffset: 5, Content: "第一段内容"},
		{Ordinal: 1, StartOffset: 5, EndOffset: 10, Content: "第二段内容"},
	}
	chunks[0].SetMetadata(types.MetadataKeyHeading, "安装指南")

	saved, err := p.SaveDocument(ctx, doc, chunks)
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if doc.ID == 0 || len(saved) != 2 {
		t.Fatalf("应当回填 ID: doc.ID=%d 片段数=%d", doc.ID, len(saved))
	}

	// ---- 读文档 ----
	docs, err := p.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 {
		t.Fatalf("应读出 1 篇文档，实际 %d", len(docs))
	}
	got := docs[0]
	if got.Title != doc.Title || got.Source != doc.Source || got.Content != doc.Content {
		t.Errorf("文档字段不一致:\n  期望 %+v\n  实际 %+v", *doc, got)
	}
	if got.Metadata["ext"] != ".md" {
		t.Errorf("文档元数据丢失: %v", got.Metadata)
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at 应当由数据库生成，不应为零值")
	}

	// ---- 读片段 ----
	all, err := p.AllChunks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("应读出 2 个片段，实际 %d", len(all))
	}
	if all[0].Ordinal != 0 || all[1].Ordinal != 1 {
		t.Errorf("片段顺序不对: %d, %d", all[0].Ordinal, all[1].Ordinal)
	}
	if all[0].StartOffset != 0 || all[0].EndOffset != 5 {
		t.Errorf("偏移丢失: [%d,%d)", all[0].StartOffset, all[0].EndOffset)
	}
	if all[0].Metadata[types.MetadataKeyHeading] != "安装指南" {
		t.Errorf("片段元数据丢失: %v", all[0].Metadata)
	}
	if all[0].Embedding != nil {
		t.Errorf("尚未回填向量时应当是 nil，实际 %d 维", len(all[0].Embedding))
	}
}

// TestIntegrationVectorRoundTrip 验证向量的写入与读出。
//
// 这是最容易出错的一环：pgx 需要注册 pgvector 类型，
// 不注册的话读出来是 nil、写进去会报一堆指向不明的错误。
func TestIntegrationVectorRoundTrip(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)

	doc := &types.Document{Title: "向量测试", Content: "内容"}
	saved, err := p.SaveDocument(ctx, doc, []types.Chunk{
		{Ordinal: 0, Content: "第一段"},
		{Ordinal: 1, Content: "第二段"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 回填两个不同的向量
	for i := range saved {
		saved[i].Embedding = mkVec(float32(i) * 0.5)
	}
	if err := p.SaveEmbeddings(ctx, saved); err != nil {
		t.Fatalf("回填向量失败: %v", err)
	}

	all, err := p.AllChunks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range all {
		if len(c.Embedding) != types.EmbeddingDim {
			t.Fatalf("第 %d 个片段应有 %d 维向量，实际 %d 维",
				i, types.EmbeddingDim, len(c.Embedding))
		}
		// 逐元素比对，确认往返没有精度或顺序问题
		want := mkVec(float32(i) * 0.5)
		for j := range want {
			if c.Embedding[j] != want[j] {
				t.Fatalf("第 %d 个片段第 %d 维不一致: 期望 %v，实际 %v",
					i, j, want[j], c.Embedding[j])
			}
		}
	}
}

// TestIntegrationDimensionCheck 验证维度不对时在应用层就被拦住。
//
// 不拦的话会走到数据库层报错，而那里的错误信息指向 SQL 语句、
// 不指向"你少传了或者多传了 288 个维度"这个真正的原因。
func TestIntegrationDimensionCheck(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)

	doc := &types.Document{Title: "维度测试", Content: "内容"}
	saved, err := p.SaveDocument(ctx, doc, []types.Chunk{{Ordinal: 0, Content: "片段"}})
	if err != nil {
		t.Fatal(err)
	}

	saved[0].Embedding = make([]float32, 768) // 错的维度
	err = p.SaveEmbeddings(ctx, saved)
	if !errors.Is(err, ErrVectorDim) {
		t.Errorf("应返回 ErrVectorDim，实际 %v", err)
	}
}

// TestIntegrationDeleteCascade 验证删文档会级联删片段。
//
// 依赖 init 脚本里的 ON DELETE CASCADE。如果外键没建对，
// 这里会留下孤儿片段——它们会被检索召回到，但拼不出任何上下文。
func TestIntegrationDeleteCascade(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)

	doc := &types.Document{Title: "待删", Content: "内容"}
	if _, err := p.SaveDocument(ctx, doc, mkChunks(3, "片段")); err != nil {
		t.Fatal(err)
	}

	if err := p.DeleteDocument(ctx, doc.ID); err != nil {
		t.Fatalf("删除失败: %v", err)
	}

	var count int
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM chunk`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("删文档后片段应被级联删除，实际还剩 %d 行", count)
	}

	if err := p.DeleteDocument(ctx, doc.ID); !errors.Is(err, ErrDocumentNotFound) {
		t.Errorf("删第二次应返回 ErrDocumentNotFound，实际 %v", err)
	}
}

// TestIntegrationTransactionIsAtomic 验证事务性。
//
// 中途失败时不能留下半截数据。
func TestIntegrationTransactionIsAtomic(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)

	chunks := mkChunks(3, "正常内容")
	chunks[2].Content = "" // 第三个非法，Validate 会拦下

	doc := &types.Document{Title: "原子性测试", Content: "内容"}
	if _, err := p.SaveDocument(ctx, doc, chunks); err == nil {
		t.Fatal("应当报错")
	}

	var docs, cs int
	_ = p.pool.QueryRow(ctx, `SELECT count(*) FROM document`).Scan(&docs)
	_ = p.pool.QueryRow(ctx, `SELECT count(*) FROM chunk`).Scan(&cs)
	if docs != 0 || cs != 0 {
		t.Errorf("事务应当回滚，实际留下 %d 篇文档、%d 个片段", docs, cs)
	}
}

// TestIntegrationRebuildIndexAfterRestart 是 #29 的核心验收点。
//
// 模拟"服务重启"：丢弃全部内存状态，只从数据库重建索引，
// 然后确认还能检索到之前导入的内容。
//
// 不测这条的话，M6 能跑通一次，但**重启后检索会静默只剩向量一路**，
// RRF 质量下降而且不报错。
func TestIntegrationRebuildIndexAfterRestart(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)

	// ---- 第一次"运行"：导入并回填向量 ----
	doc := &types.Document{Title: "重启测试", Content: "内容"}
	saved, err := p.SaveDocument(ctx, doc, []types.Chunk{
		{Ordinal: 0, Content: "PostgreSQL 连接池配置"},
		{Ordinal: 1, Content: "向量检索的余弦相似度"},
		{Ordinal: 2, Content: "完全无关的第三段内容"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := range saved {
		saved[i].Embedding = mkVec(float32(i + 1))
	}
	if err := p.SaveEmbeddings(ctx, saved); err != nil {
		t.Fatal(err)
	}

	// ---- "重启"：关掉连接池，重新连一个 ----
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	p2, err := NewPostgres(ctx, testDSN(t), 4, 0)
	if err != nil {
		t.Fatalf("重连失败: %v", err)
	}
	t.Cleanup(func() { _ = p2.Close() })

	// ---- 从库里重建 ----
	rebuilt, err := p2.AllChunks(ctx)
	if err != nil {
		t.Fatalf("重建失败: %v", err)
	}
	if len(rebuilt) != 3 {
		t.Fatalf("重启后应读出 3 个片段，实际 %d —— 数据丢了", len(rebuilt))
	}
	for i, c := range rebuilt {
		if len(c.Embedding) != types.EmbeddingDim {
			t.Errorf("第 %d 个片段的向量没读回来，实际 %d 维 —— "+
				"重启后向量路会静默失效", i, len(c.Embedding))
		}
		if c.Content == "" {
			t.Errorf("第 %d 个片段的内容丢失", i)
		}
	}
	if rebuilt[0].DocumentID != doc.ID {
		t.Errorf("文档 ID 应当保持一致: 期望 %d，实际 %d", doc.ID, rebuilt[0].DocumentID)
	}
}

// TestIntegrationContextCancel 验证 ctx 取消能传到数据库层。
func TestIntegrationContextCancel(t *testing.T) {
	p := newTestStore(t)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()
	time.Sleep(2 * time.Millisecond) // 确保已经超时

	_, err := p.AllChunks(ctx)
	if err == nil {
		t.Error("已超时的 ctx 应当返回错误")
	}
}

// TestIntegrationHNSWIndexExists 验证向量索引真的建出来了。
//
// 用 EXPLAIN 确认查询走的是索引扫描而不是全表扫描。
// 索引没建成的后果是静默的性能退化——结果还是对的，只是慢。
func TestIntegrationHNSWIndexExists(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)

	var idxdef string
	err := p.pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes
		 WHERE tablename = 'chunk' AND indexname LIKE '%hnsw%'`).Scan(&idxdef)
	if err != nil {
		t.Fatalf("找不到 HNSW 索引（建表 SQL 没生效？）: %v", err)
	}
	t.Logf("索引定义: %s", idxdef)

	// 插入足够的数据让优化器愿意用索引
	doc := &types.Document{Title: "索引测试", Content: "内容"}
	saved, err := p.SaveDocument(ctx, doc, mkChunks(50, "用于索引测试的片段内容"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range saved {
		saved[i].Embedding = mkVec(float32(i))
	}
	if err := p.SaveEmbeddings(ctx, saved); err != nil {
		t.Fatal(err)
	}

	// 余弦距离查询能跑通（说明 vector_cosine_ops 索引可用）
	var n int
	q := fmt.Sprintf("[%s]", vecLiteral(mkVec(1)))
	err = p.pool.QueryRow(ctx,
		`SELECT count(*) FROM (
		    SELECT id FROM chunk WHERE embedding IS NOT NULL
		    ORDER BY embedding <=> $1::vector LIMIT 5
		 ) t`, q).Scan(&n)
	if err != nil {
		t.Fatalf("余弦距离查询失败: %v", err)
	}
	if n != 5 {
		t.Errorf("应当返回 5 条，实际 %d", n)
	}
}

// vecLiteral 把向量转成 pgvector 的文本字面量。
func vecLiteral(v []float32) string {
	s := ""
	for i, f := range v {
		if i > 0 {
			s += ","
		}
		s += fmt.Sprintf("%g", f)
	}
	return s
}

// TestIntegrationDocumentByDedupKey 验证按去重键查文档这条 SQL 路径（#55）。
//
// 为什么内存版测过了还要测这里：两套实现必须**行为一致**，
// 而它们的差异恰恰在 SQL 上——列名写错、WHERE 条件写反、
// 忘了把新列加进 SELECT，这些内存版一个都测不出来。
func TestIntegrationDocumentByDedupKey(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()

	doc := &types.Document{
		Title: "手册", Source: "docs/deploy.md", Content: "内容",
		ContentHash: "abc123", DedupKey: "src:docs/deploy.md",
	}
	chunks := []types.Chunk{{Ordinal: 0, StartOffset: 0, EndOffset: 2, Content: "内容"}}
	if _, err := p.SaveDocument(ctx, doc, chunks); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	got, err := p.DocumentByDedupKey(ctx, "src:docs/deploy.md")
	if err != nil {
		t.Fatalf("按去重键查找失败: %v", err)
	}
	if got.ID != doc.ID || got.Title != "手册" {
		t.Errorf("查回的文档不对：%+v", got)
	}
	// 新加的列必须真的被读回来——只写不读的话，
	// 上层拿到的 DedupKey 永远是空串，而去重依赖它。
	if got.ContentHash != "abc123" || got.DedupKey != "src:docs/deploy.md" {
		t.Errorf("content_hash / dedup_key 没有读回来：%+v", got)
	}

	if _, err := p.DocumentByDedupKey(ctx, "src:不存在.md"); !errors.Is(err, ErrDocumentNotFound) {
		t.Errorf("查不到时应返回 ErrDocumentNotFound，实际 %v", err)
	}
}

// TestIntegrationDedupKeyUnique 验证唯一索引真的挡得住重复。
//
// 应用层的"先查再插"在并发下拦不住——两个事务都会查不到、然后都插入。
// 唯一的保证只能来自数据库。
func TestIntegrationDedupKeyUnique(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()

	save := func(title string) error {
		_, err := p.SaveDocument(ctx,
			&types.Document{
				Title: title, Source: "docs/a.md", Content: "内容",
				DedupKey: "src:docs/a.md",
			},
			[]types.Chunk{{Ordinal: 0, StartOffset: 0, EndOffset: 2, Content: "内容"}})
		return err
	}

	if err := save("第一份"); err != nil {
		t.Fatalf("首次保存失败: %v", err)
	}
	if err := save("第二份"); err == nil {
		t.Error("同一个去重键插入两次应当被唯一索引拦住")
	}
}

// TestIntegrationEmptyDedupKeyNotConstrained 验证空去重键不受唯一约束。
//
// 去重是 ingest 层的职责，store 是更下面的一层——有人会直接调
// SaveDocument 传一个没算过去重键的文档。那些文档不该互相冲突。
func TestIntegrationEmptyDedupKeyNotConstrained(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		_, err := p.SaveDocument(ctx,
			&types.Document{Title: fmt.Sprintf("无键 %d", i), Content: "内容"},
			[]types.Chunk{{Ordinal: 0, StartOffset: 0, EndOffset: 2, Content: "内容"}})
		if err != nil {
			t.Fatalf("第 %d 篇（无去重键）保存失败: %v", i+1, err)
		}
	}
}

// ---------------------------------------------------------------------------
// #68 向量检索下推到 HNSW
// ---------------------------------------------------------------------------

// planQueryVec 造一个确定性的 1024 维向量。
func planQueryVec(seed float32) []float32 {
	v := make([]float32, types.EmbeddingDim)
	for i := range v {
		v[i] = seed + float32(i)*1e-6
	}
	return v
}

// seedChunks 插入 n 个带向量的片段，返回文档 ID。
//
// 走裸 SQL 而不是 SaveDocument/SaveEmbeddings：这个测试关心的是
// **执行计划**，让插入路径参与进来只会增加噪音。
func seedChunks(t *testing.T, p *Postgres, n int) int64 {
	t.Helper()
	ctx := context.Background()

	var docID int64
	if err := p.pool.QueryRow(ctx,
		`INSERT INTO document (title, source, content)
		 VALUES ('计划测试', 'plan-test', '正文') RETURNING id`,
	).Scan(&docID); err != nil {
		t.Fatalf("插入文档失败: %v", err)
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("开启事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for i := 0; i < n; i++ {
		if _, err := tx.Exec(ctx,
			`INSERT INTO chunk
			   (document_id, ordinal, start_offset, end_offset, content, metadata, embedding)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			docID, i, i*10, i*10+10,
			fmt.Sprintf("片段内容 %d：这是一段用于测试近邻检索的中文文本。", i),
			[]byte(`{"source":"plan-test"}`),
			pgvector.NewVector(planQueryVec(float32(i)*0.001)),
		); err != nil {
			t.Fatalf("插入第 %d 个片段失败: %v", i, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	return docID
}

// explainANN 跑一次 EXPLAIN（默认设置）并返回计划文本。
func explainANN(t *testing.T, p *Postgres, q []float32, topK int) string {
	t.Helper()
	return explainWith(t, p, q, topK, false)
}

// explainANNNoSeqScan 在**禁用顺序扫描**的前提下取执行计划。
//
// 这是断言「索引可用」的正确姿势，理由见
// TestIntegrationANNCanUseHNSWIndex 的说明。
func explainANNNoSeqScan(t *testing.T, p *Postgres, q []float32, topK int) string {
	t.Helper()
	return explainWith(t, p, q, topK, true)
}

// explainWith 跑 EXPLAIN ANALYZE。
//
// noSeqScan 为真时在同一个事务里 `SET LOCAL enable_seqscan = off`——
// 必须是同一个连接，所以走事务而不是直接 pool.Query。
func explainWith(t *testing.T, p *Postgres, q []float32, topK int, noSeqScan bool) string {
	t.Helper()
	ctx := context.Background()

	if !noSeqScan {
		rows, err := p.pool.Query(ctx, `EXPLAIN (ANALYZE, BUFFERS) `+annQuery,
			pgvector.NewVector(q), topK)
		if err != nil {
			t.Fatalf("EXPLAIN 失败: %v", err)
		}
		defer rows.Close()
		return scanPlan(t, rows)
	}

	// ⚠️ 必须是**同一个连接**：enable_seqscan 是会话级 GUC。
	// 直接 pool.Query 会从池里随机取一条连接，SET 未必落在它上面。
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("开启事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatalf("设置 enable_seqscan 失败: %v", err)
	}
	rows, err := tx.Query(ctx, `EXPLAIN (ANALYZE, BUFFERS) `+annQuery,
		pgvector.NewVector(q), topK)
	if err != nil {
		t.Fatalf("EXPLAIN 失败: %v", err)
	}
	defer rows.Close()
	return scanPlan(t, rows)
}

// scanPlan 把 EXPLAIN 的结果行拼成一个字符串。
//
// ⚠️ 计划文本里会**原样带出整个查询向量**（1024 个浮点数），
// 直接打进日志或失败信息里是几千字符的噪声。这里截断掉。
func scanPlan(t *testing.T, rows pgx.Rows) string {
	t.Helper()
	var sb strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("扫描计划失败: %v", err)
		}
		sb.WriteString(compactPlanLine(line))
		sb.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("读取计划失败: %v", err)
	}
	return sb.String()
}

// compactPlanLine 把计划行里的向量字面量换成占位符。
//
// 不这么做的话，一条失败信息能有两万字符，实际没人在看。
func compactPlanLine(line string) string {
	const marker = "'["
	i := strings.Index(line, marker)
	if i < 0 {
		return line
	}
	j := strings.Index(line[i:], "]'::vector")
	if j < 0 {
		return line
	}
	return line[:i] + "'[…]'::vector" + line[i+j+len("]'::vector"):]
}

// TestIntegrationANNCanUseHNSWIndex 是 #68 的核心验收。
//
// **"SQL 写得对"不等于"索引能用上"。** 距离算子写错（`<->` 是 L2、
// `<#>` 是内积）不会报任何错，只会静默退化成全表扫描——结果一模一样，
// 索引白建。所以这里断言的是**执行计划**，不是结果。
//
// # 为什么断言"能用"而不是"用了"
//
// 实测过规划器的选择，结论是**非单调**的，表越大不代表越会用索引：
//
//	片段数     默认计划
//	  1000     顺序扫描
//	  5000     ✅ HNSW 索引
//	 20000     顺序扫描
//
// 所以「在某个 N 上断言必须走索引」是个会随 pgvector 版本、
// 统计信息、行宽漂移的脆弱断言。这里改成在
// `SET LOCAL enable_seqscan = off` 下取计划：**如果算子与算子类
// 不匹配，即便禁掉顺序扫描它也用不上索引**——这才是会被写错、
// 且必须被抓到的东西。
//
// 两个断言缺一不可：
//   - 计划里出现 chunk_embedding_hnsw_idx
//   - 计划里**没有 Sort 节点**：说明索引直接提供了 ORDER BY 的顺序。
//     只断言前者的话，"扫索引 + 排序"也能通过——那正是算子不匹配的样子
//
// 默认计划只记录不断言（`t.Logf`），因为它是规划器的自由，不是契约。
func TestIntegrationANNCanUseHNSWIndex(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)

	const n = 5000
	seedChunks(t, p, n)

	// ⚠️ 规划器需要统计信息才会考虑索引。不 ANALYZE 的话，刚插入的表
	// 在它眼里"很小"，会直接选顺序扫描——测试会以一个和代码无关的
	// 原因失败（或者更糟：因为恰好选对了而通过）。
	if _, err := p.pool.Exec(ctx, `ANALYZE chunk`); err != nil {
		t.Fatalf("ANALYZE 失败: %v", err)
	}

	// 记录但不评判：这是规划器在当前规模下的选择，不是接口契约。
	t.Logf("默认计划（仅记录）：\n%s", explainANN(t, p, planQueryVec(0.5), 10))

	plan := explainANNNoSeqScan(t, p, planQueryVec(0.5), 10)

	if !strings.Contains(plan, "chunk_embedding_hnsw_idx") {
		t.Errorf("禁用顺序扫描后仍然没有用上 chunk_embedding_hnsw_idx——"+
			"说明这个查询在结构上就与索引不匹配。检查 ORDER BY 的算子"+
			"是否与索引的 vector_cosine_ops 一致\n%s", plan)
	}
	if strings.Contains(plan, "Sort") {
		t.Errorf("计划里有 Sort 节点，说明索引**没有**提供 ORDER BY 需要的顺序——"+
			"正是距离算子与算子类不匹配的症状（索引被扫了个遍却没帮上忙）\n%s", plan)
	}
}

// TestIntegrationSearchByEmbeddingContract 验证库内近邻检索满足接口契约。
//
// 契约见 store.EmbeddingSearcher 的说明。这里逐条钉住，因为其中几条
// 一旦破了**不会报错，只会让下游静默算错**：
//
//   - Metadata 必须带上：评测的命中判定读 Metadata["source"]，
//     漏了会让所有 recall 归零，而报告照常打印
//   - Score 与 VectorScore 必须都是**余弦相似度**（不是距离）：
//     填反了排序不变、融合结果照常正确，只有暴露给 Agent 的
//     vector_score 是错的，而没有一个测试抓得到（RRF 不看分数）
//   - VectorRank 必须 1-based：MatchedBy() 依赖它推导 matched_by
func TestIntegrationSearchByEmbeddingContract(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)

	const n = 200
	seedChunks(t, p, n)
	if _, err := p.pool.Exec(ctx, `ANALYZE chunk`); err != nil {
		t.Fatalf("ANALYZE 失败: %v", err)
	}

	const topK = 10
	got, err := p.SearchByEmbedding(ctx, planQueryVec(0.1), topK)
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("应当有结果")
	}
	if len(got) > topK {
		t.Errorf("返回 %d 条，超过 topK=%d", len(got), topK)
	}

	for i, r := range got {
		if r.VectorRank != i+1 {
			t.Errorf("第 %d 条的 VectorRank = %d，应当是 1-based 的 %d",
				i, r.VectorRank, i+1)
		}
		if r.Score != r.VectorScore {
			t.Errorf("第 %d 条的 Score(%v) 与 VectorScore(%v) 不一致——"+
				"单路结果里这两个都该是原始余弦分", i, r.Score, r.VectorScore)
		}
		// 余弦相似度落在 [-1, 1]。超出这个范围说明填的是距离而不是相似度。
		if r.Score < -1.0001 || r.Score > 1.0001 {
			t.Errorf("第 %d 条的分数是 %v，超出余弦相似度的取值范围。"+
				"`<=>` 返回的是**距离**，必须填 1-distance", i, r.Score)
		}
		if src := r.Chunk.Metadata[types.MetadataKeySource]; src != "plan-test" {
			t.Errorf("第 %d 条缺少 source 元数据（得到 %q）。"+
				"评测的命中判定依赖它，漏了会让所有 recall 静默归零", i, src)
		}
		if r.Chunk.ID == 0 || r.Chunk.DocumentID == 0 {
			t.Errorf("第 %d 条缺少身份字段：ID=%d DocumentID=%d",
				i, r.Chunk.ID, r.Chunk.DocumentID)
		}
		// ⚠️ 不该把 embedding 带出来：检索下游没有任何地方读它，
		// 带上等于把想省的内存又搬回来。
		if len(r.Chunk.Embedding) != 0 {
			t.Errorf("第 %d 条带回了向量（%d 维）——annQuery 不该 select embedding",
				i, len(r.Chunk.Embedding))
		}
	}

	// 降序
	for i := 1; i < len(got); i++ {
		if got[i].Score > got[i-1].Score {
			t.Errorf("结果没有按相似度降序：第 %d 条 %v > 第 %d 条 %v",
				i, got[i].Score, i-1, got[i-1].Score)
		}
	}
}

// TestIntegrationSearchByEmbeddingRejectsBadInput 验证入参校验。
func TestIntegrationSearchByEmbeddingRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)

	if _, err := p.SearchByEmbedding(ctx, make([]float32, 100), 10); !errors.Is(err, ErrVectorDim) {
		t.Errorf("维度不对应当返回 ErrVectorDim，实际 %v", err)
	}
	if _, err := p.SearchByEmbedding(ctx, planQueryVec(0), 0); err == nil {
		t.Error("topK<=0 应当报错（数据库表达不了「不限量」）")
	}
	if _, err := p.SearchByEmbedding(ctx, planQueryVec(0), -1); err == nil {
		t.Error("topK<0 应当报错")
	}

	// 空库应当返回非 nil 的空切片，而不是 nil
	got, err := p.SearchByEmbedding(ctx, planQueryVec(0), 10)
	if err != nil {
		t.Fatalf("空库检索不该报错: %v", err)
	}
	if got == nil {
		t.Error("空结果应当是空切片而不是 nil——nil 会让序列化出 null")
	}
	if len(got) != 0 {
		t.Errorf("空库应当返回 0 条，实际 %d", len(got))
	}
}
