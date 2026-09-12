package retrieve

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/XiaoleC05/ContextDock/internal/embed"
	"github.com/XiaoleC05/ContextDock/internal/types"
)

// vec 造一个 1024 维向量，把给定值放在开头，其余补 0。
func vec(vals ...float32) []float32 {
	v := make([]float32, types.EmbeddingDim)
	copy(v, vals)
	return v
}

// vecAt 造一个 1024 维向量，只在指定下标放一个值。
func vecAt(idx int, val float32) []float32 {
	v := make([]float32, types.EmbeddingDim)
	v[idx] = val
	return v
}

// mkEmbedded 造一个带向量的片段。
func mkEmbedded(id int64, content string, v []float32) types.Chunk {
	return types.Chunk{ID: id, DocumentID: 1, Content: content, Embedding: v}
}

// TestCosineSimilarityBasics 用几何上已知答案的向量验证余弦相似度。
func TestCosineSimilarityBasics(t *testing.T) {
	idx := NewVectorIndex()
	if err := idx.Index([]types.Chunk{
		mkEmbedded(1, "同向", vec(1)),      // 与查询同向
		mkEmbedded(2, "正交", vecAt(1, 1)), // 与查询正交
		mkEmbedded(3, "反向", vec(-1)),     // 与查询反向
	}); err != nil {
		t.Fatal(err)
	}

	// 不截断，拿到全部三条的分数
	got, err := idx.Search(context.Background(), vec(1), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("应返回 3 条，实际 %d", len(got))
	}

	scores := map[int64]float64{}
	for _, r := range got {
		scores[r.Chunk.ID] = r.VectorScore
	}

	tests := []struct {
		id   int64
		want float64
		desc string
	}{
		{1, 1, "同向应为 1"},
		{2, 0, "正交应为 0"},
		{3, -1, "反向应为 -1"},
	}
	for _, tt := range tests {
		if math.Abs(scores[tt.id]-tt.want) > 1e-5 {
			t.Errorf("%s: 期望 %.6f，实际 %.6f", tt.desc, tt.want, scores[tt.id])
		}
	}

	// 排序：同向 > 正交 > 反向
	if got[0].Chunk.ID != 1 || got[1].Chunk.ID != 2 || got[2].Chunk.ID != 3 {
		t.Errorf("排序错误，期望 [1 2 3]，实际 %v", resultIDs(got))
	}
}

// TestCosineRange 验证所有相似度都落在 [-1, 1] 内。
//
// 超出这个范围通常意味着归一化算错了（比如忘了除模长）。
func TestCosineRange(t *testing.T) {
	idx := NewVectorIndex()
	var chunks []types.Chunk
	for i := 0; i < 20; i++ {
		v := make([]float32, types.EmbeddingDim)
		for j := range v {
			v[j] = float32(math.Sin(float64(i*13 + j)))
		}
		chunks = append(chunks, mkEmbedded(int64(i+1), "内容", v))
	}
	if err := idx.Index(chunks); err != nil {
		t.Fatal(err)
	}

	got, err := idx.Search(context.Background(), vec(1, 2, 3), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range got {
		if r.VectorScore < -1.00001 || r.VectorScore > 1.00001 {
			t.Errorf("相似度 %v 超出 [-1,1] 范围", r.VectorScore)
		}
	}
}

// TestVectorRankAndScoreFields 验证返回结果填对了字段。
func TestVectorRankAndScoreFields(t *testing.T) {
	idx := NewVectorIndex()
	if err := idx.Index([]types.Chunk{
		mkEmbedded(1, "a", vec(1)),
		mkEmbedded(2, "b", vec(0.5)),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := idx.Search(context.Background(), vec(1), 0)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range got {
		if r.VectorRank != i+1 {
			t.Errorf("第 %d 条的 VectorRank 应为 %d，实际 %d", i, i+1, r.VectorRank)
		}
		if r.LexicalRank != 0 {
			t.Errorf("向量检索不应设置 LexicalRank，实际 %d", r.LexicalRank)
		}
		if r.Score != r.VectorScore {
			t.Errorf("单路检索时 Score 应等于 VectorScore: %v vs %v", r.Score, r.VectorScore)
		}
	}
}

func TestVectorRejectsBadDimension(t *testing.T) {
	idx := NewVectorIndex()

	// 文档向量维度错误
	err := idx.Index([]types.Chunk{
		mkEmbedded(1, "正常", vec(1)),
		{ID: 2, DocumentID: 1, Content: "错误维度", Embedding: make([]float32, 768)},
	})
	if !errors.Is(err, ErrVectorDim) {
		t.Fatalf("文档向量维度错误应返回 ErrVectorDim，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "768") {
		t.Errorf("错误信息应包含实际维度 768，方便排查: %v", err)
	}

	// 没有向量的片段也不能混进来
	err = idx.Index([]types.Chunk{
		{ID: 1, DocumentID: 1, Content: "没有向量"},
	})
	if !errors.Is(err, ErrVectorDim) {
		t.Errorf("未嵌入的片段应报错，实际 %v", err)
	}

	// 查询向量维度错误
	if err := idx.Index([]types.Chunk{mkEmbedded(1, "正常", vec(1))}); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Search(context.Background(), make([]float32, 100), 10); !errors.Is(err, ErrVectorDim) {
		t.Errorf("查询向量维度错误应返回 ErrVectorDim，实际 %v", err)
	}
}

// TestVectorRejectsZeroQuery 验证零向量查询会明确报错。
//
// 零向量的余弦相似度在数学上没有定义（分母为 0）。如果不拦，
// 会算出 NaN，而 NaN 参与排序的结果是未定义的——可能是任意顺序。
func TestVectorRejectsZeroQuery(t *testing.T) {
	idx := NewVectorIndex()
	if err := idx.Index([]types.Chunk{mkEmbedded(1, "内容", vec(1))}); err != nil {
		t.Fatal(err)
	}

	_, err := idx.Search(context.Background(), make([]float32, types.EmbeddingDim), 10)
	if !errors.Is(err, ErrZeroVector) {
		t.Errorf("零向量查询应返回 ErrZeroVector，实际 %v", err)
	}
}

// TestVectorSkipsZeroNormDocuments 验证零模长的文档被跳过而不是产生 NaN。
func TestVectorSkipsZeroNormDocuments(t *testing.T) {
	idx := NewVectorIndex()
	if err := idx.Index([]types.Chunk{
		mkEmbedded(1, "正常", vec(1)),
		mkEmbedded(2, "全零向量", make([]float32, types.EmbeddingDim)),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := idx.Search(context.Background(), vec(1), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Chunk.ID != 1 {
		t.Fatalf("全零向量应当被跳过，实际结果 %v", resultIDs(got))
	}
	for _, r := range got {
		if math.IsNaN(r.VectorScore) {
			t.Error("分数出现了 NaN")
		}
	}
}

func TestVectorEdgeCases(t *testing.T) {
	t.Run("空索引", func(t *testing.T) {
		idx := NewVectorIndex()
		got, err := idx.Search(context.Background(), vec(1), 10)
		if err != nil {
			t.Fatalf("空索引不应报错: %v", err)
		}
		if len(got) != 0 || got == nil {
			t.Errorf("空索引应返回空的非 nil 切片，实际 %v", got)
		}
	})
	t.Run("topK 截断", func(t *testing.T) {
		idx := NewVectorIndex()
		var chunks []types.Chunk
		for i := int64(1); i <= 5; i++ {
			chunks = append(chunks, mkEmbedded(i, "内容", vec(1, float32(i))))
		}
		if err := idx.Index(chunks); err != nil {
			t.Fatal(err)
		}
		got, err := idx.Search(context.Background(), vec(1, 2), 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Errorf("topK=2 应返回 2 条，实际 %d", len(got))
		}
	})
}

// TestVectorStableTieBreak 验证同分时顺序稳定。
func TestVectorStableTieBreak(t *testing.T) {
	idx := NewVectorIndex()
	var chunks []types.Chunk
	for i := int64(1); i <= 8; i++ {
		chunks = append(chunks, mkEmbedded(i, "内容", vec(1)))
	}
	if err := idx.Index(chunks); err != nil {
		t.Fatal(err)
	}

	first := resultIDs(mustSearch(t, idx, vec(1), 0))
	for i := 0; i < 20; i++ {
		got := resultIDs(mustSearch(t, idx, vec(1), 0))
		for j := range first {
			if got[j] != first[j] {
				t.Fatalf("第 %d 次查询顺序不稳定:\n  首次 %v\n  本次 %v", i, first, got)
			}
		}
	}
}

func mustSearch(t *testing.T, idx *VectorIndex, q []float32, topK int) []types.SearchResult {
	t.Helper()
	got, err := idx.Search(context.Background(), q, topK)
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	return got
}

// TestVectorSemanticRetrieval 用 FakeEmbedder 验证端到端的语义检索效果。
//
// 这是把 embed 和 retrieve 两个包接起来测，验证「意思相近的文本确实排前面」。
func TestVectorSemanticRetrieval(t *testing.T) {
	texts := []string{
		"PostgreSQL 是一款关系型数据库",
		"pgvector 是 PostgreSQL 的向量扩展",
		"今天天气不错，适合出门散步",
	}
	embs := mustEmbed(t, texts)

	idx := NewVectorIndex()
	var chunks []types.Chunk
	for i, txt := range texts {
		chunks = append(chunks, mkEmbedded(int64(i+1), txt, embs[i]))
	}
	if err := idx.Index(chunks); err != nil {
		t.Fatal(err)
	}

	q := mustEmbed(t, []string{"PostgreSQL 数据库"})[0]
	got := mustSearch(t, idx, q, 3)

	if len(got) != 3 {
		t.Fatalf("应返回 3 条，实际 %v", resultIDs(got))
	}
	// 与查询共享词最多的是 1 号（PostgreSQL + 数据库），应当排第一。
	if got[0].Chunk.ID != 1 {
		t.Errorf("与查询最相似的是 1 号，实际顺序 %v", resultIDs(got))
	}
	// 完全无关的天气那条应当排最后。
	if got[len(got)-1].Chunk.ID != 3 {
		t.Errorf("与查询无关的天气那条应当排最后，实际顺序 %v", resultIDs(got))
	}
}

// mustEmbed 用 FakeEmbedder 生成向量（不访问网络）。
func mustEmbed(t *testing.T, texts []string) [][]float32 {
	t.Helper()
	got, err := embed.NewFake().Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("生成向量失败: %v", err)
	}
	return got
}
