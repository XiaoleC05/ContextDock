package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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

// closeErrStore 包一层 Store，只覆盖 Close，用来验证关停路径的错误传播。
//
// 内嵌接口而不是实现全部方法：Store 有十来个方法，
// 为了测其中一个就把其余都写成 panic 桩，只会让这个测试本身变成负担。
type closeErrStore struct {
	store.Store
	err error
}

func (s closeErrStore) Close() error { return s.err }

// TestServiceClosePropagatesStoreError 验证关停失败不会被吞掉。
//
// main.go 是在 defer 里调 Service.Close() 的，而且**只在出错时打一行日志**。
// 也就是说：如果这里把错误吞掉，进程退出码仍然是 0，
// 「关停时连接池没关干净」就变成一个没有任何信号的谜。
func TestServiceClosePropagatesStoreError(t *testing.T) {
	wantErr := errors.New("模拟关停失败")
	st := closeErrStore{Store: store.NewMemory(), err: wantErr}

	svc, err := New(testConfig(), embed.NewFake(), st)
	if err != nil {
		t.Fatalf("创建服务失败: %v", err)
	}

	if err := svc.Close(); !errors.Is(err, wantErr) {
		t.Errorf("Service.Close() 应把存储的错误原样传出，实际 %v", err)
	}
}

// TestServiceCloseOnMemoryStore 验证内存存储的关停是安全且可重复的。
//
// 它没有资源要释放，本身是 `return nil`；但这条链路值得测，有两个原因：
//
//  1. 它是 main.go 里那条 defer 的**唯一终点**，没有测试就只能靠读代码确认
//  2. 重复关闭**真的会发生**：main.go 既 defer 了 st.Close()，
//     又 defer 了 svc.Close()，而后者内部又调一次 st.Close()。
//     单进程里同一个 store 会被关两次。
func TestServiceCloseOnMemoryStore(t *testing.T) {
	svc, _ := newTestService(t, nil)

	if err := svc.Close(); err != nil {
		t.Errorf("内存存储关停不应报错，实际 %v", err)
	}
	if err := svc.Close(); err != nil {
		t.Errorf("重复关停不应报错，实际 %v", err)
	}
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
// ⚠️ 它守护的**不是**「锁盖住了整个检索过程」——那条路已经废弃，
// 见 Service.idxMu 上的不变量说明。现在保证安全的是「重建换指针、
// 不改写已发布对象」，这个测试验证的是并发重建期间检索依然自洽。
//
// ⚠️ 它**覆盖不到 hybrid 的超时分支**：默认 5s 超时下，内存检索
// 永远不会超时，所以 `<-ctx.Done()` 那条路走不到。那条路由
// TestSearchTimeoutDoesNotRaceWithRebuild 专门覆盖。
//
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

// TestRebuildSwapsIndexesInsteadOfMutating 钉住「已发布对象永不改写」这条不变量。
//
// 这是 #74 那类数据竞争的**根因防线**，而且是确定性的——
// 不依赖 -race 的时序运气。
//
// 机制：只要重建是「造新实例、换指针」，那么任何还在使用旧索引的
// goroutine（比如 hybrid 超时后被落下的那个）读到的就永远是一份
// 完整、一致的数据，锁有没有盖住它都不再重要。
//
// 反过来，如果哪天有人把 Rebuild 改回对已有实例调 Index()，
// 这个测试会立刻红。
func TestRebuildSwapsIndexesInsteadOfMutating(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t, nil)

	// ⚠️ 两份文档必须有不同的 Source。
	//
	// 去重键是按**来源**算的（types.DedupKey）：来源为空时退回内容指纹，
	// 而两份内容一模一样 → 相同的指纹 → 第二份会把第一份**替换**掉，
	// 索引根本不会变大。这个测试第一次就是这么写错的。
	if _, err := svc.Import(ctx, &types.Document{
		Title: "第一份", Source: "doc-a", Content: longText(8),
	}); err != nil {
		t.Fatalf("导入失败: %v", err)
	}

	oldBM25, oldVec := svc.snapshot()
	oldBM25Len := oldBM25.Len()
	oldVecLen := oldVec.Len()
	if oldBM25Len == 0 || oldVecLen == 0 {
		t.Fatal("前置条件不成立：导入之后索引应当非空")
	}

	// 再导入一份**不同来源**的文档，触发一次增量式的 Rebuild。
	if _, err := svc.Import(ctx, &types.Document{
		Title: "第二份", Source: "doc-b", Content: longText(8),
	}); err != nil {
		t.Fatalf("导入失败: %v", err)
	}

	newBM25, newVec := svc.snapshot()
	if newBM25.Len() <= oldBM25Len {
		t.Fatalf("重建后索引应当变大：BM25 %d -> %d", oldBM25Len, newBM25.Len())
	}
	if newVec.Len() <= oldVecLen {
		t.Fatalf("重建后向量索引应当变大：%d -> %d", oldVecLen, newVec.Len())
	}

	// ⚠️ 关键断言：旧对象必须**原封不动**。
	//
	// 原地改写的实现会让 oldBM25Len 跟着变大——因为那个指针指向的
	// 就是被改写的那一个对象。
	if got := oldBM25.Len(); got != oldBM25Len {
		t.Errorf("旧 BM25 被原地改写了：%d -> %d。"+
			"Rebuild 必须造新实例再换指针，不能对已发布的实例调 Index()", oldBM25Len, got)
	}
	if got := oldVec.Len(); got != oldVecLen {
		t.Errorf("旧 VectorIndex 被原地改写了：%d -> %d", oldVecLen, got)
	}
}

// TestSearchTimeoutDoesNotRaceWithRebuild 专打 hybrid 的**超时分支**。
//
// 为什么需要它：`hybrid.go` 的 `<-ctx.Done()` 分支会直接 return，
// **不排空另一个仍在读索引的 goroutine**。那个被落下的 goroutine
// 与并发的 Rebuild 之间就是 #74 描述的竞争。默认 5s 超时下这条分支
// 在内存检索里永远走不到，所以既有测试覆盖不到。
//
// 做法是把 SearchTimeout 压到 1ns，让每次混合检索都走超时分支。
//
// ⚠️ 诚实说明：**竞争类测试只能降低漏检概率，不能证明没有竞争**。
// 它真正的裁判是 CI 上的 `-race`。本机没有 gcc 跑不了 -race。
// 真正的防线是 TestRebuildSwapsIndexesInsteadOfMutating 那条确定性断言。
//
// ⚠️ 超时后 Service.Search 会返回错误，那是**预期行为**（拿不到足够
// 结果时宁可报错也不返回半份），所以这里只忽略错误、不做断言。
func TestSearchTimeoutDoesNotRaceWithRebuild(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t, nil)
	svc.cfg.SearchTimeout = time.Nanosecond

	// 语料大一点，让每次检索持续得久一些，扩大与 Rebuild 的重叠窗口。
	if _, err := svc.Import(ctx, &types.Document{Title: "d", Content: longText(40)}); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			_ = svc.Rebuild(ctx)
		}
	}()

	for i := 0; i < 40; i++ {
		// 两条路都要走：Search 走融合，SearchChannels 走单路 + 融合。
		_, _ = svc.Search(ctx, "测试内容", 5)
		_, _ = svc.SearchChannels(ctx, "测试内容", 5, 0, 0)
	}
	<-done
}
