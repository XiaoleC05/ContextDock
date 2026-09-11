package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/XiaoleC05/ContextDock/internal/config"
	"github.com/XiaoleC05/ContextDock/internal/embed"
	"github.com/XiaoleC05/ContextDock/internal/store"
	"github.com/XiaoleC05/ContextDock/internal/types"
)

func testConfig() *config.Config {
	return &config.Config{
		SiliconFlowAPIKey: "sk-test",
		UseMemoryStore:    true,
		TopK:              10,
		SearchTimeout:     config.DefaultSearchTimeout,
		ChunkMaxRunes:     120,
		ChunkOverlap:      20,
		PoolMaxConns:      4,
		EmbeddingDim:      types.EmbeddingDim,
	}
}

func newTestService(t *testing.T, emb embed.Embedder) (*Service, store.Store) {
	t.Helper()
	st := store.NewMemory()
	if emb == nil {
		emb = embed.NewFake()
	}
	svc, err := New(testConfig(), emb, st)
	if err != nil {
		t.Fatalf("创建服务失败: %v", err)
	}
	return svc, st
}

func longText(n int) string {
	return strings.Repeat("这是一段用于测试混合检索的中文内容。", n)
}

func TestServiceRebuildEmpty(t *testing.T) {
	svc, _ := newTestService(t, nil)

	if err := svc.Rebuild(context.Background()); err != nil {
		t.Fatalf("空库重建不应报错: %v", err)
	}
	chunks, embedded := svc.Stats()
	if chunks != 0 || embedded != 0 {
		t.Errorf("空库重建后统计应为 0，实际 %d/%d", chunks, embedded)
	}
}

// TestServiceSearchBeforeAnyImport 验证空索引时的行为。
func TestServiceSearchBeforeAnyImport(t *testing.T) {
	svc, _ := newTestService(t, nil)
	if err := svc.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}

	out, err := svc.Search(context.Background(), "任意查询", 5)
	if err != nil {
		t.Fatalf("空索引检索不应报错: %v", err)
	}
	if len(out.Results) != 0 {
		t.Errorf("空索引应返回 0 条，实际 %d", len(out.Results))
	}
	if out.Degraded == "" {
		t.Error("空索引时应当说明原因，而不是静默返回空结果")
	}
}

// TestServiceImportThenSearch 是端到端：导入之后能搜到。
func TestServiceImportThenSearch(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t, nil)

	doc := &types.Document{
		Title:   "数据库笔记",
		Content: "PostgreSQL 的连接池配置很重要。" + longText(5) + "向量检索使用余弦相似度。",
	}
	res, err := svc.Import(ctx, doc)
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if res.ChunkCount == 0 {
		t.Fatal("应当切出片段")
	}

	out, err := svc.Search(ctx, "PostgreSQL 连接池", 5)
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if len(out.Results) == 0 {
		t.Fatal("导入后应当能搜到内容")
	}
	if out.Degraded != "" {
		t.Logf("发生了降级（可能是正常的）: %s", out.Degraded)
	}
}

// TestServiceImportUpdatesIndex 验证导入后索引被刷新。
func TestServiceImportUpdatesIndex(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t, nil)

	before, _ := svc.Stats()

	doc := &types.Document{Title: "文档", Content: longText(10)}
	if _, err := svc.Import(ctx, doc); err != nil {
		t.Fatal(err)
	}

	after, embedded := svc.Stats()
	if after <= before {
		t.Errorf("导入后索引应当变大: %d -> %d", before, after)
	}
	if embedded != after {
		t.Errorf("所有片段都应当有向量: %d/%d", embedded, after)
	}
}

// TestServiceRebuildRestoresIndex 是 #29 的验收：模拟重启后重建。
func TestServiceRebuildRestoresIndex(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	emb := embed.NewFake()

	// ---- 第一次"运行" ----
	svc1, err := New(testConfig(), emb, st)
	if err != nil {
		t.Fatal(err)
	}
	doc := &types.Document{Title: "重启测试", Content: "pgvector 的 HNSW 索引有维度上限。" + longText(5)}
	if _, err := svc1.Import(ctx, doc); err != nil {
		t.Fatal(err)
	}

	// ---- "重启"：丢弃 Service，但存储还在（模拟数据库持久化）----
	svc2, err := New(testConfig(), emb, st)
	if err != nil {
		t.Fatal(err)
	}

	// 重建之前，索引是空的
	if chunks, _ := svc2.Stats(); chunks != 0 {
		t.Fatalf("新建的 Service 索引应当是空的，实际 %d", chunks)
	}

	// 重建之后应当恢复
	if err := svc2.Rebuild(ctx); err != nil {
		t.Fatalf("重建失败: %v", err)
	}
	chunks, embedded := svc2.Stats()
	if chunks == 0 {
		t.Fatal("重建后索引不该是空的 —— 不做重建的话，重启后检索会静默失效")
	}
	if embedded != chunks {
		t.Errorf("重建后应当所有片段都带向量: %d/%d", embedded, chunks)
	}

	// 而且真的能搜到
	out, err := svc2.Search(ctx, "HNSW 索引", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Results) == 0 {
		t.Error("重建后应当能检索到重启前导入的内容")
	}
}

// TestServiceSearchDegradesWhenEmbedFails 是降级的核心验收。
//
// 嵌入接口挂了（限流、超时、网络故障）不该让检索完全不可用——
// 关键词检索还能顶上。整个查询失败意味着"什么都查不到"，
// 那比"结果差一点"更糟。
func TestServiceSearchDegradesWhenEmbedFails(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	emb := embed.NewFake()

	svc, err := New(testConfig(), emb, st)
	if err != nil {
		t.Fatal(err)
	}

	// 先正常导入并建索引
	doc := &types.Document{Title: "文档", Content: "PostgreSQL 连接池配置指南。" + longText(5)}
	if _, err := svc.Import(ctx, doc); err != nil {
		t.Fatal(err)
	}

	// 然后让嵌入接口开始失败
	emb.SetError(errors.New("模拟 429 限流"))

	out, err := svc.Search(ctx, "PostgreSQL 连接池", 5)
	if err != nil {
		t.Fatalf("嵌入失败不应让整个检索失败，实际 %v", err)
	}
	if out.Degraded == "" {
		t.Error("应当明确标出发生了降级，而不是静默返回变差的结果")
	}
	if len(out.Results) == 0 {
		t.Error("降级后应当仍能通过关键词检索返回结果")
	}
	// 降级路径的结果只来自关键词通道
	for i, r := range out.Results {
		if r.VectorRank != 0 {
			t.Errorf("第 %d 条结果不该有向量名次（已降级）: VectorRank=%d", i, r.VectorRank)
		}
	}
}

// TestServiceSearchDegradesWhenNoVectors 验证向量索引为空时的降级。
//
// 场景：文档导入成功但嵌入全失败（向量没回填），
// 此时向量索引是空的，检索应当退化成纯关键词。
func TestServiceSearchDegradesWhenNoVectors(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	// 用一个会失败的 embedder 导入，结果是只有内容没有向量
	emb := embed.NewFake()
	emb.SetError(errors.New("模拟嵌入故障"))

	svc, err := New(testConfig(), emb, st)
	if err != nil {
		t.Fatal(err)
	}
	doc := &types.Document{Title: "文档", Content: "关键词检索不依赖向量。" + longText(5)}
	if _, err := svc.Import(ctx, doc); err == nil {
		t.Log("导入报错是预期的（嵌入失败）")
	}

	// 恢复嵌入能力，但库里已经有片段是没有向量的
	emb.SetError(nil)
	if err := svc.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}

	chunks, embedded := svc.Stats()
	if embedded != 0 {
		t.Fatalf("本次用例期望没有向量，实际 %d 个", embedded)
	}
	if chunks == 0 {
		t.Fatal("片段应当已落库")
	}

	out, err := svc.Search(ctx, "关键词检索", 5)
	if err != nil {
		t.Fatalf("检索不应报错: %v", err)
	}
	if out.Degraded == "" {
		t.Error("没有向量时应当说明已降级")
	}
	if len(out.Results) == 0 {
		t.Error("关键词检索应当仍能命中")
	}
}

func TestServiceSearchTopKDefault(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t, nil)

	if _, err := svc.Import(ctx, &types.Document{Title: "d", Content: longText(20)}); err != nil {
		t.Fatal(err)
	}

	out, err := svc.Search(ctx, "测试内容", 0)
	if err != nil {
		t.Fatal(err)
	}
	if out.TopK != testConfig().TopK {
		t.Errorf("topK<=0 时应使用配置里的默认值 %d，实际 %d", testConfig().TopK, out.TopK)
	}
}

// TestServiceConcurrentSearchAndRebuild 守护索引的并发保护。
//
// BM25 / VectorIndex 都不是并发安全的：Index 会重建内部状态。
// 检索时读、重建时写，不加锁的话会读到重建到一半的索引。
// 这个测试要用 -race 跑才有意义。
func TestServiceConcurrentSearchAndRebuild(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t, nil)

	if _, err := svc.Import(ctx, &types.Document{Title: "d", Content: longText(10)}); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 10; i++ {
			_ = svc.Rebuild(ctx)
		}
	}()
	for i := 0; i < 20; i++ {
		if _, err := svc.Search(ctx, "测试内容", 5); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("并发检索失败: %v", err)
		}
	}
	<-done
}
