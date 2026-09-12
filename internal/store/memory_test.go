package store

import (
	"context"
	"errors"
	"testing"

	"github.com/XiaoleC05/ContextDock/internal/types"
)

func mkDoc(title, content string) *types.Document {
	return &types.Document{Title: title, Source: "test", Content: content}
}

func mkChunks(n int, content string) []types.Chunk {
	out := make([]types.Chunk, n)
	for i := range out {
		out[i] = types.Chunk{Ordinal: i, Content: content}
	}
	return out
}

// mkVec 造一个合法的 1024 维向量。
func mkVec(seed float32) []float32 {
	v := make([]float32, types.EmbeddingDim)
	for i := range v {
		v[i] = seed + float32(i)*1e-6
	}
	return v
}

func TestMemorySaveAndRead(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()

	doc := mkDoc("标题", "正文")
	saved, err := m.SaveDocument(ctx, doc, mkChunks(3, "片段内容"))
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	if doc.ID == 0 {
		t.Error("应当回填文档 ID")
	}
	if len(saved) != 3 {
		t.Fatalf("应返回 3 个片段，实际 %d", len(saved))
	}
	for i, c := range saved {
		if c.ID == 0 {
			t.Errorf("第 %d 个片段应当回填 ID", i)
		}
		if c.DocumentID != doc.ID {
			t.Errorf("第 %d 个片段的 DocumentID 应为 %d，实际 %d", i, doc.ID, c.DocumentID)
		}
	}

	all, err := m.AllChunks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("应读出 3 个片段，实际 %d", len(all))
	}

	docs, err := m.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Title != "标题" {
		t.Errorf("文档读取不正确: %+v", docs)
	}
}

// TestMemoryAllChunksOrdered 验证读出顺序稳定。
//
// map 的遍历顺序在 Go 里是随机的。不排序的话，每次启动重建出来的
// 索引虽然内容一样，但 benchmark 和调试时的表现会不稳定。
func TestMemoryAllChunksOrdered(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()

	for i := 0; i < 5; i++ {
		if _, err := m.SaveDocument(ctx, mkDoc("d", "c"), mkChunks(3, "内容")); err != nil {
			t.Fatal(err)
		}
	}

	first, _ := m.AllChunks(ctx)
	if len(first) != 15 {
		t.Fatalf("应有 15 个片段，实际 %d", len(first))
	}

	for round := 0; round < 10; round++ {
		got, _ := m.AllChunks(ctx)
		for i := range first {
			if got[i].ID != first[i].ID {
				t.Fatalf("第 %d 次读出顺序不稳定:\n  首次 %v\n  本次 %v",
					round, ids(first), ids(got))
			}
		}
	}
}

func ids(cs []types.Chunk) []int64 {
	out := make([]int64, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	return out
}

func TestMemoryChunksByDocument(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()

	d1 := mkDoc("一", "内容一")
	if _, err := m.SaveDocument(ctx, d1, mkChunks(2, "a")); err != nil {
		t.Fatal(err)
	}
	d2 := mkDoc("二", "内容二")
	if _, err := m.SaveDocument(ctx, d2, mkChunks(3, "b")); err != nil {
		t.Fatal(err)
	}

	got, err := m.ChunksByDocument(ctx, d2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Errorf("应返回 3 个片段，实际 %d", len(got))
	}
	for _, c := range got {
		if c.DocumentID != d2.ID {
			t.Errorf("不该混入别的文档的片段: %d", c.DocumentID)
		}
	}

	if _, err := m.ChunksByDocument(ctx, 99999); !errors.Is(err, ErrDocumentNotFound) {
		t.Errorf("不存在的文档应返回 ErrDocumentNotFound，实际 %v", err)
	}
}

func TestMemoryDeleteCascades(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()

	doc := mkDoc("待删", "内容")
	if _, err := m.SaveDocument(ctx, doc, mkChunks(3, "x")); err != nil {
		t.Fatal(err)
	}

	if err := m.DeleteDocument(ctx, doc.ID); err != nil {
		t.Fatal(err)
	}

	all, _ := m.AllChunks(ctx)
	if len(all) != 0 {
		t.Errorf("删文档应当级联删掉片段，实际还剩 %d 个", len(all))
	}
	if m.Len() != 0 {
		t.Errorf("文档数应为 0，实际 %d", m.Len())
	}

	if err := m.DeleteDocument(ctx, doc.ID); !errors.Is(err, ErrDocumentNotFound) {
		t.Errorf("删第二次应返回 ErrDocumentNotFound，实际 %v", err)
	}
}

func TestMemorySaveEmbeddings(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()

	doc := mkDoc("d", "c")
	saved, err := m.SaveDocument(ctx, doc, mkChunks(2, "内容"))
	if err != nil {
		t.Fatal(err)
	}

	for i := range saved {
		saved[i].Embedding = mkVec(float32(i))
	}
	if err := m.SaveEmbeddings(ctx, saved); err != nil {
		t.Fatalf("回填向量失败: %v", err)
	}

	all, _ := m.AllChunks(ctx)
	for i, c := range all {
		if len(c.Embedding) != types.EmbeddingDim {
			t.Errorf("第 %d 个片段的向量应当已回填，实际 %d 维", i, len(c.Embedding))
		}
	}
}

func TestMemorySaveValidation(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()

	t.Run("空文档指针", func(t *testing.T) {
		if _, err := m.SaveDocument(ctx, nil, mkChunks(1, "a")); !errors.Is(err, ErrEmptyDocument) {
			t.Errorf("应返回 ErrEmptyDocument，实际 %v", err)
		}
	})
	t.Run("文档校验不过", func(t *testing.T) {
		bad := &types.Document{Title: "", Content: "正文"}
		if _, err := m.SaveDocument(ctx, bad, mkChunks(1, "a")); err == nil {
			t.Error("空标题应当报错")
		}
	})
	t.Run("没有片段", func(t *testing.T) {
		if _, err := m.SaveDocument(ctx, mkDoc("t", "c"), nil); !errors.Is(err, ErrNoChunks) {
			t.Errorf("应返回 ErrNoChunks，实际 %v", err)
		}
	})
	t.Run("片段校验不过", func(t *testing.T) {
		bad := []types.Chunk{{Ordinal: 0, Content: ""}} // 空内容
		if _, err := m.SaveDocument(ctx, mkDoc("t", "c"), bad); err == nil {
			t.Error("空内容片段应当报错")
		}
	})
}

// TestMemorySaveIsAtomicOnValidationFailure 验证校验失败时不会留下半截数据。
//
// 如果先插入文档、再逐个校验片段，第 3 个片段非法时前两个已经在库里了，
// 而文档也已经存在——留下"有文档但片段不全"的脏数据。
func TestMemorySaveIsAtomicOnValidationFailure(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()

	chunks := mkChunks(3, "正常内容")
	chunks[2].Content = "" // 第三个非法

	if _, err := m.SaveDocument(ctx, mkDoc("t", "c"), chunks); err == nil {
		t.Fatal("应当报错")
	}

	if m.Len() != 0 {
		t.Errorf("校验失败时不应留下任何文档，实际留下 %d 篇", m.Len())
	}
	all, _ := m.AllChunks(ctx)
	if len(all) != 0 {
		t.Errorf("校验失败时不应留下任何片段，实际留下 %d 个", len(all))
	}
}

func TestMemoryConcurrencySafe(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()

	done := make(chan error, 40)
	for i := 0; i < 20; i++ {
		go func() {
			_, err := m.SaveDocument(ctx, mkDoc("并发", "内容"), mkChunks(2, "x"))
			done <- err
		}()
		go func() {
			_, err := m.AllChunks(ctx)
			done <- err
		}()
	}
	for i := 0; i < 40; i++ {
		if err := <-done; err != nil {
			t.Errorf("并发调用失败: %v", err)
		}
	}
	if m.Len() != 20 {
		t.Errorf("应保存 20 篇文档，实际 %d", m.Len())
	}
}

func TestMemoryRespectsContext(t *testing.T) {
	m := NewMemory()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := m.SaveDocument(ctx, mkDoc("t", "c"), mkChunks(1, "a")); !errors.Is(err, context.Canceled) {
		t.Errorf("已取消的 ctx 应返回 context.Canceled，实际 %v", err)
	}
	if _, err := m.AllChunks(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("已取消的 ctx 应返回 context.Canceled，实际 %v", err)
	}
}

// 编译期确认两个实现都满足同一个接口。
//
// 放在测试里而不是实现文件里：Postgres 在带 integration 标签时才编译，
// 无标签构建下这个断言会指向不存在的类型。放到测试文件里两个都能覆盖。
var (
	_ Store = (*Memory)(nil)
	_ Store = (*Postgres)(nil)
)

// TestMemorySaveDocumentReturnsCopy 钉住 SaveDocument 的所有权约定。
//
// 返回值归调用方，store 内部**不再引用**它。见 SaveDocument 的注释。
//
// 为什么必须有这条：调用方（ingest 回填向量）会**不持锁**地就地改写
// 返回值的元素。如果返回值与 map 里那份是同一个切片，这个无锁的写
// 就会和 AllChunks 在读锁下的读构成数据竞争——读锁保护不了不持锁的写方。
func TestMemorySaveDocumentReturnsCopy(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()

	saved, err := m.SaveDocument(ctx, mkDoc("标题", "正文"), mkChunks(2, "原始内容"))
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if len(saved) == 0 {
		t.Fatal("前置条件不成立：应当返回片段")
	}

	// 调用方就地改写——这正是 ingest 回填向量时做的事。
	saved[0].Content = "被调用方改掉了"
	saved[0].Embedding = mkVec(1)

	got, err := m.ChunksByDocument(ctx, saved[0].DocumentID)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if got[0].Content != "原始内容" {
		t.Errorf("调用方改写返回值影响到了 store 内部：content = %q。"+
			"SaveDocument 必须返回副本", got[0].Content)
	}
	if len(got[0].Embedding) != 0 {
		t.Errorf("调用方写返回值里的 Embedding 影响到了 store 内部（%d 维）。"+
			"向量只能通过 SaveEmbeddings 落库", len(got[0].Embedding))
	}
}

// TestMemoryEmbeddingFlowsThroughSaveEmbeddings 确认副本化之后向量仍然落得进去。
//
// 这是副本化最需要确认的一点：改动之前，store 内部那份数据是通过
// **切片别名**顺带拿到向量的（ingest 写 saved[i].Embedding 的同时就写进去了），
// 副本化切断了这条隐式通路。必须确认 SaveEmbeddings 这条**显式**的路依旧有效，
// 否则导入的文档会静默地全部没有向量——检索只剩关键词一路。
func TestMemoryEmbeddingFlowsThroughSaveEmbeddings(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()

	saved, err := m.SaveDocument(ctx, mkDoc("标题", "正文"), mkChunks(2, "内容"))
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	// 模拟 ingest：拿着返回值回填向量，然后显式调用 SaveEmbeddings。
	for i := range saved {
		saved[i].Embedding = mkVec(float32(i))
	}
	if err := m.SaveEmbeddings(ctx, saved); err != nil {
		t.Fatalf("回填向量失败: %v", err)
	}

	got, err := m.ChunksByDocument(ctx, saved[0].DocumentID)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	for i := range got {
		if len(got[i].Embedding) != types.EmbeddingDim {
			t.Errorf("第 %d 个片段没有向量（%d 维）——"+
				"副本化不能切断 SaveEmbeddings 这条显式通路", i, len(got[i].Embedding))
		}
	}
}

// TestMemoryConcurrentSaveAndAllChunks 覆盖「导入回填」与「全量读取」并发。
//
// 这就是 #75 描述的场景：ingest 在回填向量时**不持锁**，而 Rebuild
// 会走 AllChunks（持读锁）。
//
// ⚠️ 与所有竞争类测试一样：它只能降低漏检概率，不能证明没有竞争。
// 真正的裁判是 CI 上的 `-race`（本机没有 gcc 跑不了）。
// 确定性的防线是上面那条 TestMemorySaveDocumentReturnsCopy。
func TestMemoryConcurrentSaveAndAllChunks(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()

	const rounds = 50
	errCh := make(chan error, 1)

	go func() {
		defer close(errCh)
		for i := 0; i < rounds; i++ {
			saved, err := m.SaveDocument(ctx, mkDoc("标题", "正文"), mkChunks(4, "内容"))
			if err != nil {
				errCh <- err
				return
			}
			// 模拟 ingest 回填向量：**不持锁**地改返回值。
			for j := range saved {
				saved[j].Embedding = mkVec(float32(j))
			}
			if err := m.SaveEmbeddings(ctx, saved); err != nil {
				errCh <- err
				return
			}
		}
	}()

	for i := 0; i < rounds; i++ {
		if _, err := m.AllChunks(ctx); err != nil {
			t.Fatalf("AllChunks 失败: %v", err)
		}
	}

	if err := <-errCh; err != nil {
		t.Errorf("并发写入失败: %v", err)
	}
}
