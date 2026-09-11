package retrieve

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/XiaoleC05/ContextDock/internal/embed"
	"github.com/XiaoleC05/ContextDock/internal/types"
)

// 基准测试的数据集规模。
//
// 固定规模是刻意的：benchmark 的数字只有在可复现的前提下才能被比较。
// M7 做性能优化时就是拿这里的数字做前后对比。
const (
	benchDocCount = 1000
	benchTopK     = 10
)

// benchChunks 造一份固定的测试语料。
//
// 用确定性的方式生成，保证每次运行的输入完全一致。
func benchChunks(n int) []types.Chunk {
	topics := []string{
		"PostgreSQL 数据库的连接池配置与调优",
		"向量检索的余弦相似度计算方式",
		"BM25 关键词检索的词频饱和与长度归一化",
		"Go 语言的 goroutine 调度与 channel 通信",
		"pgvector 的 HNSW 索引构建与查询",
		"MCP 协议的 stdio 传输与工具注册",
		"文档切分策略对检索召回率的影响",
		"RRF 融合算法与加权求和的对比",
	}
	chunks := make([]types.Chunk, n)
	for i := range chunks {
		chunks[i] = types.Chunk{
			ID:         int64(i + 1),
			DocumentID: int64(i/10 + 1),
			Ordinal:    i % 10,
			Content: fmt.Sprintf("%s（第 %d 段）%s",
				topics[i%len(topics)], i, topics[(i*7)%len(topics)]),
		}
	}
	return chunks
}

// benchChunksWithVectors 造一份带向量的语料。
func benchChunksWithVectors(b *testing.B, n int) []types.Chunk {
	b.Helper()
	chunks := benchChunks(n)

	f := embed.NewFake()
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.IndexText()
	}
	vecs, err := f.Embed(context.Background(), texts)
	if err != nil {
		b.Fatalf("生成向量失败: %v", err)
	}
	for i := range chunks {
		chunks[i].Embedding = vecs[i]
	}
	return chunks
}

func BenchmarkBM25Index(b *testing.B) {
	chunks := benchChunks(benchDocCount)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m := NewBM25()
		m.Index(chunks)
	}
}

func BenchmarkBM25Search(b *testing.B) {
	m := NewBM25()
	m.Index(benchChunks(benchDocCount))

	queries := []string{
		"向量检索",
		"BM25 关键词",
		"PostgreSQL 连接池",
		"goroutine 调度与 channel",
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.Search(queries[i%len(queries)], benchTopK)
	}
}

func BenchmarkVectorIndex(b *testing.B) {
	chunks := benchChunksWithVectors(b, benchDocCount)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		idx := NewVectorIndex()
		if err := idx.Index(chunks); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkVectorSearch(b *testing.B) {
	chunks := benchChunksWithVectors(b, benchDocCount)
	idx := NewVectorIndex()
	if err := idx.Index(chunks); err != nil {
		b.Fatal(err)
	}
	query := chunks[0].Embedding

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := idx.Search(query, benchTopK); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFuseRRF(b *testing.B) {
	chunks := benchChunks(benchDocCount)
	lex := lexRun(chunks[:50]...)
	vec := vecRun(chunks[25:75]...)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = FuseRRF(types.RRFK, benchTopK, lex, vec)
	}
}

func BenchmarkHybridSearch(b *testing.B) {
	chunks := benchChunksWithVectors(b, benchDocCount)

	bm := NewBM25()
	bm.Index(chunks)
	vi := NewVectorIndex()
	if err := vi.Index(chunks); err != nil {
		b.Fatal(err)
	}

	h := NewHybrid(BM25Searcher{bm}, vi)
	query := "向量检索与 BM25 的混合排序"
	queryVec := chunks[0].Embedding

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := h.Search(context.Background(), query, queryVec, benchTopK); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkTokenize 单独测分词——它是 BM25 建索引里最重的一环。
func BenchmarkTokenize(b *testing.B) {
	m := NewBM25()
	chunks := benchChunks(100)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Index(chunks)
	}
}

// 让编译器不要把测试里构造的东西优化掉。
var (
	_ = math.Abs
	_ = time.Now
)
