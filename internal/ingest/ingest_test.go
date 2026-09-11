package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/XiaoleC05/ContextDock/internal/chunk"
	"github.com/XiaoleC05/ContextDock/internal/embed"
	"github.com/XiaoleC05/ContextDock/internal/store"
	"github.com/XiaoleC05/ContextDock/internal/types"
)

func newTestIngester(t *testing.T, st store.Store, emb embed.Embedder) *Ingester {
	t.Helper()
	c, err := chunk.New(chunk.Config{MaxRunes: 40, OverlapRunes: 8})
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		st = store.NewMemory()
	}
	if emb == nil {
		emb = embed.NewFake()
	}
	return New(c, emb, st)
}

func longContent(sentences int) string {
	return strings.Repeat("这是一段用来测试导入流程的中文句子。", sentences)
}

// TestIngestHappyPath 走通全流程：切分 → 落库 → 嵌入 → 回填。
func TestIngestHappyPath(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	g := newTestIngester(t, st, nil)

	doc := &types.Document{Title: "测试文档", Source: "inline", Content: longContent(20)}
	res, err := g.Ingest(ctx, doc)
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}

	if res.DocumentID == 0 {
		t.Error("应当回填文档 ID")
	}
	if res.ChunkCount < 2 {
		t.Fatalf("长文档应当切成多段，实际 %d 段", res.ChunkCount)
	}
	if res.EmbeddedNum != res.ChunkCount {
		t.Errorf("所有片段都应当生成向量: %d/%d", res.EmbeddedNum, res.ChunkCount)
	}

	// 库里应当有内容，且向量已回填
	all, err := st.AllChunks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != res.ChunkCount {
		t.Errorf("库里应有 %d 个片段，实际 %d", res.ChunkCount, len(all))
	}
	for i, c := range all {
		if len(c.Embedding) != types.EmbeddingDim {
			t.Errorf("第 %d 个片段的向量没回填，实际 %d 维", i, len(c.Embedding))
		}
	}
}

// TestIngestUsesIndexText 守护 DESIGN §8。
//
// 送给 Embedder 的必须是 IndexText()（正文 + 标题面包屑），不是裸 Content。
// 否则 BM25 和向量搜的就不是同一个文本，RRF 融合质量下降且极难排查。
func TestIngestUsesIndexText(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	var mu sync.Mutex
	var seen []string
	rec := &recordingEmbedder{
		inner:  embed.NewFake(),
		onCall: func(texts []string) { mu.Lock(); seen = append(seen, texts...); mu.Unlock() },
	}

	c, _ := chunk.New(chunk.DefaultConfig())
	g := New(c, rec, st)

	doc := &types.Document{
		Title:   "文档",
		Content: "# 安装指南\n\n运行 go build 即可。\n",
	}
	if _, err := g.Ingest(ctx, doc); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("Embedder 没被调用")
	}
	joined := strings.Join(seen, "\n")
	if !strings.Contains(joined, "安装指南") {
		t.Errorf("送进 Embedder 的文本里没有标题面包屑——"+
			"说明用的是 Content 而不是 IndexText:\n%s", joined)
	}
}

// TestIngestBatchesAt32EndToEnd 验证分批保证。
//
// 用一个"超过 32 条就返回 400"的假服务器模拟硅基流动的硬上限。
// 如果分批在任何一层被漏掉，这里就会失败。
//
// 注意：分批实现在 embed 包里（API 客户端知道自己的限制），
// ingest 只管把全部文本交出去。这个测试验证的是**整条链路**上的保证。
// TestIngestSinksDocumentSourceIntoChunks 验证文档来源被下沉到每个片段的元数据。
//
// 为什么要下沉：检索返回的是**片段**，SearchResult 内嵌的是 Chunk，
// 而 Source 是 Document 上的字段——检索路径里根本没有 Document。
// 不在这条链路里存一份，MCP 的输出就永远填不出 source。
//
// ⚠️ 这份测试必须留在 ingest 包里，不能只靠 mcp 包的同名用例。
// 变异测试是**按包**跑的（go test ./<被变异的包>/...），
// 只在 mcp 里测的话，ingest 这段逻辑被改坏了也没人发现——
// 这不是假设，加这条用例之前，"不下沉来源"这个变异就是 MISSED。
func TestIngestSinksDocumentSourceIntoChunks(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	g := newTestIngester(t, st, nil)

	doc := &types.Document{Title: "部署手册", Source: "docs/deploy.md", Content: longContent(10)}
	if _, err := g.Ingest(ctx, doc); err != nil {
		t.Fatalf("导入失败: %v", err)
	}

	chunks, err := st.AllChunks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) == 0 {
		t.Fatal("应当切出片段")
	}
	for _, c := range chunks {
		if got := c.Metadata[types.MetadataKeySource]; got != "docs/deploy.md" {
			t.Errorf("片段 %d 的来源元数据不对：期望 %q，实际 %q",
				c.Ordinal, "docs/deploy.md", got)
		}
	}
}

// TestIngestSkipsEmptySource 验证来源为空时**不写入**该元数据键。
//
// 写一个空字符串进去，MCP 层虽然会被 omitempty 挡住，
// 但库里会留下一堆 `"source": ""`，而且和"这份文档本来就没有来源"
// 没法区分——空值和缺失混在一起，将来想做迁移就分不清了。
func TestIngestSkipsEmptySource(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	g := newTestIngester(t, st, nil)

	doc := &types.Document{Title: "无来源文档", Content: longContent(5)}
	if _, err := g.Ingest(ctx, doc); err != nil {
		t.Fatalf("导入失败: %v", err)
	}

	chunks, err := st.AllChunks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range chunks {
		if v, ok := c.Metadata[types.MetadataKeySource]; ok {
			t.Errorf("来源为空时不该写入该键，片段 %d 里却是 %q", c.Ordinal, v)
		}
	}
}

func TestIngestBatchesAt32EndToEnd(t *testing.T) {
	ctx := context.Background()

	var mu sync.Mutex
	var batchSizes []int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		mu.Lock()
		batchSizes = append(batchSizes, len(req.Input))
		mu.Unlock()

		if len(req.Input) > 32 {
			// 真实 API 的行为：整个请求 400
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, `{"code":20012,"message":"input batch size %d > maximum allowed batch size 32"}`, len(req.Input))
			return
		}

		items := make([]string, len(req.Input))
		for i := range items {
			vec := make([]float64, types.EmbeddingDim)
			vec[0] = 1
			b, _ := json.Marshal(vec)
			items[i] = fmt.Sprintf(`{"index":%d,"embedding":%s}`, i, b)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"model":"m","data":[%s]}`, strings.Join(items, ","))
	}))
	defer srv.Close()

	emb, err := embed.NewSiliconFlow("k", embed.WithBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}

	// 切出远多于 32 段
	c, _ := chunk.New(chunk.Config{MaxRunes: 20, OverlapRunes: 4})
	g := New(c, emb, store.NewMemory())

	doc := &types.Document{Title: "大文档", Content: longContent(300)}
	res, err := g.Ingest(ctx, doc)
	if err != nil {
		t.Fatalf("导入失败（可能是分批没生效，整个请求被 400 了）: %v", err)
	}
	if res.ChunkCount <= 32 {
		t.Fatalf("用例设计有误：应当切出超过 32 段，实际 %d 段", res.ChunkCount)
	}

	mu.Lock()
	defer mu.Unlock()
	var sum int
	for _, n := range batchSizes {
		sum += n
		if n > 32 {
			t.Errorf("有一批 %d 条，超过上限 32", n)
		}
	}
	if sum != res.ChunkCount {
		t.Errorf("送到 Embedder 的文本总数应为 %d，实际 %d", res.ChunkCount, sum)
	}
	if len(batchSizes) < 2 {
		t.Errorf("%d 个片段应当分成多批，实际只有 %d 批", res.ChunkCount, len(batchSizes))
	}
	t.Logf("切出 %d 段，分成 %d 批：%v", res.ChunkCount, len(batchSizes), batchSizes)
}

// TestIngestKeepsDocumentWhenEmbedFails 是这条流水线最重要的设计决策。
//
// 向量是网络调用，可能限流、可能超时。如果"先生成向量再落库"，
// 一次 429 就让整篇文档白导。现在是先落库再回填，
// 所以**即使向量全失败，文档内容也不会丢**。
func TestIngestKeepsDocumentWhenEmbedFails(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	fake := embed.NewFake()
	fake.SetError(errors.New("模拟限流"))
	g := newTestIngester(t, st, fake)

	doc := &types.Document{Title: "文档", Content: longContent(10)}
	res, err := g.Ingest(ctx, doc)

	if err == nil {
		t.Fatal("嵌入失败时应当返回错误，让调用方知道需要重试")
	}
	if res == nil || res.DocumentID == 0 {
		t.Fatal("即使嵌入失败，也应当返回已落库的文档 ID")
	}

	// 关键断言：文档和片段确实在库里
	all, err := st.AllChunks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 {
		t.Error("嵌入失败不应导致文档丢失——重试时只需重算向量")
	}
	for i, c := range all {
		if c.Embedding != nil {
			t.Errorf("第 %d 个片段不该有向量", i)
		}
	}
}

func TestIngestRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	g := newTestIngester(t, nil, nil)

	t.Run("nil 文档", func(t *testing.T) {
		if _, err := g.Ingest(ctx, nil); !errors.Is(err, ErrNilDocument) {
			t.Errorf("应返回 ErrNilDocument，实际 %v", err)
		}
	})
	t.Run("空内容", func(t *testing.T) {
		doc := &types.Document{Title: "标题", Content: "   "}
		if _, err := g.Ingest(ctx, doc); err == nil {
			t.Error("空内容应当报错")
		}
	})
	t.Run("空标题", func(t *testing.T) {
		doc := &types.Document{Title: "", Content: "正文"}
		if _, err := g.Ingest(ctx, doc); err == nil {
			t.Error("空标题应当报错")
		}
	})
	t.Run("只有标题没有正文", func(t *testing.T) {
		doc := &types.Document{Title: "标题", Content: "# 标题\n"}
		_, err := g.Ingest(ctx, doc)
		if !errors.Is(err, store.ErrNoChunks) && !errors.Is(err, chunk.ErrEmptyDocument) {
			t.Errorf("只有标题时应当报「没有片段」类错误，实际 %v", err)
		}
	})
}

// TestIngestRejectsVectorCountMismatch 守护片段与向量的对应关系。
//
// 数量不一致意味着向量会和片段错位——第 N 个片段的向量是别的段的内容。
// 检索结果会变得莫名其妙，而且**不报错**。
func TestIngestRejectsVectorCountMismatch(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	// 故意少返回一个向量
	g := newTestIngester(t, st, &shortEmbedder{inner: embed.NewFake()})

	doc := &types.Document{Title: "文档", Content: longContent(20)}
	_, err := g.Ingest(ctx, doc)
	if !errors.Is(err, ErrChunkCountMismatch) {
		t.Errorf("向量数与片段数不一致应报错，实际 %v", err)
	}
}

// TestIngestRejectsWrongDimension 验证维度错误在落库前就被拦下。
func TestIngestRejectsWrongDimension(t *testing.T) {
	ctx := context.Background()
	g := newTestIngester(t, store.NewMemory(), &wrongDimEmbedder{})

	doc := &types.Document{Title: "文档", Content: longContent(10)}
	_, err := g.Ingest(ctx, doc)
	if !errors.Is(err, store.ErrVectorDim) {
		t.Errorf("维度错误应报错，实际 %v", err)
	}
}

func TestIngestRespectsContext(t *testing.T) {
	g := newTestIngester(t, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	doc := &types.Document{Title: "文档", Content: longContent(5)}
	if _, err := g.Ingest(ctx, doc); err == nil {
		t.Error("已取消的 ctx 应当报错")
	}
}

// ---------------------------------------------------------------------------
// 测试替身
// ---------------------------------------------------------------------------

// recordingEmbedder 记录每次收到的文本。
type recordingEmbedder struct {
	inner  embed.Embedder
	onCall func([]string)
}

func (r *recordingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	r.onCall(texts)
	return r.inner.Embed(ctx, texts)
}

// shortEmbedder 故意少返回一个向量。
type shortEmbedder struct{ inner embed.Embedder }

func (s *shortEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	vecs, err := s.inner.Embed(ctx, texts)
	if err != nil {
		return nil, err
	}
	if len(vecs) > 0 {
		return vecs[:len(vecs)-1], nil
	}
	return vecs, nil
}

// wrongDimEmbedder 返回错误维度的向量。
type wrongDimEmbedder struct{}

func (w *wrongDimEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = make([]float32, 768) // 错的维度
		out[i][0] = 1
	}
	return out, nil
}
