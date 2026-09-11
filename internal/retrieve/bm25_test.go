package retrieve

import (
	"math"
	"strings"
	"testing"

	"github.com/XiaoleC05/ContextDock/internal/types"
)

// mkChunk 造一个用于检索测试的片段。
func mkChunk(id int64, content string) types.Chunk {
	return types.Chunk{ID: id, DocumentID: 1, Content: content}
}

// mkChunkWithHeading 造一个带标题面包屑的片段。
func mkChunkWithHeading(id int64, heading, content string) types.Chunk {
	c := types.Chunk{ID: id, DocumentID: 1, Content: content}
	c.SetMetadata(types.MetadataKeyHeading, heading)
	return c
}

// resultIDs 把结果里的 chunk ID 取出来，方便断言。
func resultIDs(rs []types.SearchResult) []int64 {
	out := make([]int64, len(rs))
	for i, r := range rs {
		out[i] = r.Chunk.ID
	}
	return out
}

func TestBM25BasicRetrieval(t *testing.T) {
	m := NewBM25()
	m.Index([]types.Chunk{
		mkChunk(1, "PostgreSQL 是一个关系型数据库"),
		mkChunk(2, "向量检索使用余弦相似度计算"),
		mkChunk(3, "BM25 是关键词检索算法"),
	})

	got := m.Search("向量检索", 10)
	if len(got) == 0 {
		t.Fatal("应当召回至少一条")
	}
	if got[0].Chunk.ID != 2 {
		t.Errorf("最相关的应当是 2 号，实际 %v", resultIDs(got))
	}
	if got[0].LexicalRank != 1 {
		t.Errorf("第一条的 LexicalRank 应为 1，实际 %d", got[0].LexicalRank)
	}
	if got[0].LexicalScore <= 0 {
		t.Errorf("分数应为正数，实际 %v", got[0].LexicalScore)
	}
	if got[0].Score != got[0].LexicalScore {
		t.Errorf("单路检索时 Score 应等于 LexicalScore: %v vs %v", got[0].Score, got[0].LexicalScore)
	}
}

// TestBM25IndexesHeadingBreadcrumb 守护 DESIGN §8：建索引必须走 IndexText()。
//
// 两段正文完全一样，只有标题面包屑不同。搜标题里的词，应当只有带那个
// 面包屑的片段被召回。如果建索引时直接读了 Content，两条都召不回。
func TestBM25IndexesHeadingBreadcrumb(t *testing.T) {
	m := NewBM25()
	m.Index([]types.Chunk{
		mkChunkWithHeading(1, "安装指南", "运行 go build"),
		mkChunkWithHeading(2, "部署说明", "运行 go build"),
	})

	got := m.Search("安装指南", 10)
	if len(got) != 1 {
		t.Fatalf("应当只召回带「安装指南」面包屑的那条，实际 %v", resultIDs(got))
	}
	if got[0].Chunk.ID != 1 {
		t.Errorf("应当召回 1 号，实际 %d", got[0].Chunk.ID)
	}
}

// TestBM25QueryTermDeduplication 守护一个真实踩过的 bug。
//
// 曾经把去重用的 seen map 建在了文档循环外面，导致第一篇文档把查询词全部
// 标记成"已见过"，从第二篇起直接跳过——结果是只有第一篇文档被打分。
// 这个 bug 不报错，只是排序明显不对。
func TestBM25QueryTermDeduplication(t *testing.T) {
	m := NewBM25()
	m.Index([]types.Chunk{
		mkChunk(1, "无关内容"),
		mkChunk(2, "关键词检索"),
		mkChunk(3, "另一个关键词检索文档"),
	})

	// 同一个词重复出现在查询里
	got := m.Search("关键词 关键词 关键词", 10)
	if len(got) != 2 {
		t.Fatalf("应召回 2 条（2 号和 3 号），实际 %v", resultIDs(got))
	}

	// 重复查询词不应让分数比只查一次更高
	once := m.Search("关键词", 10)
	base := map[int64]float64{}
	for _, r := range once {
		base[r.Chunk.ID] = r.LexicalScore
	}
	for _, r := range got {
		if math.Abs(r.LexicalScore-base[r.Chunk.ID]) > 1e-9 {
			t.Errorf("重复查询词改变了 %d 号的分数: %.6f vs %.6f",
				r.Chunk.ID, r.LexicalScore, base[r.Chunk.ID])
		}
	}
}

// TestBM25TFSaturation 验证词频饱和：出现 10 次不应是出现 3 次的 3 倍分。
func TestBM25TFSaturation(t *testing.T) {
	m := NewBM25()
	m.Index([]types.Chunk{
		mkChunk(1, "检索 检索 检索"),
		mkChunk(2, "检索 "+strings.Repeat("检索 ", 10)), // 出现 11 次
		mkChunk(3, "填充文档让 avgdl 合理一些，避免长度因素干扰"),
	})

	got := m.Search("检索", 10)
	scores := map[int64]float64{}
	for _, r := range got {
		scores[r.Chunk.ID] = r.LexicalScore
	}

	if scores[2] <= scores[1] {
		t.Fatalf("出现次数多的文档分数应当更高: 11 次=%.4f, 3 次=%.4f",
			scores[2], scores[1])
	}
	// 饱和意味着比值远小于词频比 11/3 ≈ 3.67
	if ratio := scores[2] / scores[1]; ratio > 3 {
		t.Errorf("词频饱和没生效：11 次是 3 次的 %.2f 倍，应当明显小于词频比 3.67",
			ratio)
	}
}

// TestBM25IDFDecreasesWithDocumentFrequency 验证词越常见，IDF 越低。
func TestBM25IDFDecreasesWithDocumentFrequency(t *testing.T) {
	m := NewBM25()
	// "常见" 出现在所有文档里，"罕见" 只出现在一篇里
	m.Index([]types.Chunk{
		mkChunk(1, "常见 罕见"),
		mkChunk(2, "常见"),
		mkChunk(3, "常见"),
		mkChunk(4, "常见"),
	})

	common := m.idf("常见", 4)
	rare := m.idf("罕见", 4)

	if !(rare > common) {
		t.Errorf("罕见词的 IDF 应当高于常见词: 罕见=%.4f 常见=%.4f", rare, common)
	}
	if common < 0 {
		t.Errorf("Lucene 变体的 IDF 应当恒为非负，实际 %.4f", common)
	}
}

// TestBM25IDFIsNeverNegative 守护 Lucene 变体这个选择。
//
// Robertson 原始版 ln((N-n+0.5)/(n+0.5)) 在词出现在一半以上文档时会变负，
// 让总分变成负数、排序错乱。这个测试用"词出现在全部文档里"的极端情况来区分两者。
func TestBM25IDFIsNeverNegative(t *testing.T) {
	m := NewBM25()
	var chunks []types.Chunk
	for i := int64(1); i <= 5; i++ {
		chunks = append(chunks, mkChunk(i, "无处不在的词"))
	}
	m.Index(chunks)

	if idf := m.idf("无处不在", 5); idf < 0 {
		t.Errorf("词出现在全部 5 篇文档里时 IDF 仍应非负，实际 %.6f。"+
			"负值说明用了 Robertson 原始版而非 Lucene 变体", idf)
	}
}

// TestBM25LengthNormalization 验证短文档不会被长文档压过。
//
// ⚠️ 用例设计的坑：两篇文档对"检索"的词频相同时，如果它们在**长度**上分不出
// 高下，就会同分，而同分会被 tie-break（按 StableKey 字典序）决定顺序——
// 那样即使长度归一化失效，测试也会因为 tie-break 恰好给出期望顺序而通过。
//
// 所以这里刻意让**长文档的 ID 更小**：如果长度归一化失效，两者同分，
// tie-break 会把长文档排在前面，测试才会失败。
func TestBM25LengthNormalization(t *testing.T) {
	m := NewBM25()
	m.Index([]types.Chunk{
		mkChunk(1, "检索 "+strings.Repeat("填充 ", 50)), // 长，ID 小
		mkChunk(2, "检索"), // 短，ID 大
	})

	got := m.Search("检索", 10)
	if len(got) < 2 {
		t.Fatalf("应召回 2 条，实际 %v", resultIDs(got))
	}
	if got[0].Chunk.ID != 2 {
		t.Errorf("短文档应当排在前（长度归一化生效）；"+
			"若长文档排前，说明长度归一化失效、两者同分后被 tie-break 决定了顺序。实际 %v",
			resultIDs(got))
	}
}

func TestBM25UnknownTermScoresNothing(t *testing.T) {
	m := NewBM25()
	m.Index([]types.Chunk{
		mkChunk(1, "PostgreSQL 数据库"),
		mkChunk(2, "向量检索"),
	})

	got := m.Search("完全不存在于文档里的词汇", 10)
	if len(got) != 0 {
		t.Errorf("未登录词不应召回任何文档，实际 %v", resultIDs(got))
	}
}

func TestBM25EdgeCases(t *testing.T) {
	t.Run("空索引", func(t *testing.T) {
		m := NewBM25()
		if got := m.Search("任意查询", 10); len(got) != 0 {
			t.Errorf("空索引应返回空结果，实际 %d 条", len(got))
		}
	})
	t.Run("空查询", func(t *testing.T) {
		m := NewBM25()
		m.Index([]types.Chunk{mkChunk(1, "内容")})
		if got := m.Search("", 10); len(got) != 0 {
			t.Errorf("空查询应返回空结果，实际 %d 条", len(got))
		}
	})
	t.Run("纯标点查询", func(t *testing.T) {
		m := NewBM25()
		m.Index([]types.Chunk{mkChunk(1, "内容")})
		if got := m.Search("，。！", 10); len(got) != 0 {
			t.Errorf("纯标点查询应返回空结果，实际 %d 条", len(got))
		}
	})
	t.Run("结果永不为 nil", func(t *testing.T) {
		m := NewBM25()
		if got := m.Search("x", 10); got == nil {
			t.Error("应返回空切片而不是 nil，调用方需要能直接 range")
		}
	})
}

func TestBM25TopK(t *testing.T) {
	m := NewBM25()
	var chunks []types.Chunk
	for i := int64(1); i <= 10; i++ {
		chunks = append(chunks, mkChunk(i, "共同词 内容"+strings.Repeat("填", int(i))))
	}
	m.Index(chunks)

	got := m.Search("共同词", 3)
	if len(got) != 3 {
		t.Errorf("topK=3 应返回 3 条，实际 %d 条", len(got))
	}

	got = m.Search("共同词", 0)
	if len(got) != 10 {
		t.Errorf("topK=0 应不截断（10 条），实际 %d 条", len(got))
	}
}

// TestBM25StableTieBreak 验证同分时顺序稳定。
//
// 不稳定的话，两次查询可能给出不同顺序——演示时会看到结果莫名其妙地跳。
func TestBM25StableTieBreak(t *testing.T) {
	m := NewBM25()
	var chunks []types.Chunk
	for i := int64(1); i <= 10; i++ {
		chunks = append(chunks, mkChunk(i, "完全相同的文本"))
	}
	m.Index(chunks)

	first := resultIDs(m.Search("完全相同的文本", 0))
	for i := 0; i < 20; i++ {
		got := resultIDs(m.Search("完全相同的文本", 0))
		for j := range first {
			if got[j] != first[j] {
				t.Fatalf("第 %d 次查询顺序不稳定:\n  首次 %v\n  本次 %v", i, first, got)
			}
		}
	}
}

func TestBM25RanksAreSequential(t *testing.T) {
	m := NewBM25()
	m.Index([]types.Chunk{
		mkChunk(1, "关键词 一"),
		mkChunk(2, "关键词 二"),
		mkChunk(3, "关键词 三"),
	})
	got := m.Search("关键词", 0)
	for i, r := range got {
		if r.LexicalRank != i+1 {
			t.Errorf("第 %d 条的名次应为 %d，实际 %d", i, i+1, r.LexicalRank)
		}
		if r.VectorRank != 0 {
			t.Errorf("BM25 不应设置 VectorRank，实际 %d", r.VectorRank)
		}
	}
}

func TestBM25Reindex(t *testing.T) {
	m := NewBM25()
	m.Index([]types.Chunk{mkChunk(1, "第一版内容")})
	if m.Len() != 1 {
		t.Fatalf("应有 1 条，实际 %d", m.Len())
	}

	m.Index([]types.Chunk{mkChunk(2, "第二版内容"), mkChunk(3, "更多内容")})
	if m.Len() != 2 {
		t.Fatalf("重建后应有 2 条，实际 %d", m.Len())
	}
	if got := m.Search("第一版", 10); len(got) != 0 {
		t.Errorf("重建后不应再召回旧内容，实际 %v", resultIDs(got))
	}
}
