package service

import (
	"context"
	"testing"

	"github.com/XiaoleC05/ContextDock/internal/config"
	"github.com/XiaoleC05/ContextDock/internal/embed"
	"github.com/XiaoleC05/ContextDock/internal/store"
	"github.com/XiaoleC05/ContextDock/internal/types"
)

func neighborsSvc(t *testing.T, content string) *Service {
	t.Helper()
	cfg := &config.Config{
		ChunkMaxRunes: 40, ChunkOverlap: 5, TopK: 5, ContextNeighbors: 1,
		UseMemoryStore: true, EmbeddingDim: types.EmbeddingDim, PoolMaxConns: 4,
	}
	svc, err := New(cfg, embed.NewFake(), store.NewMemory())
	if err != nil {
		t.Fatalf("组装服务失败: %v", err)
	}
	if _, err := svc.Import(context.Background(), &types.Document{
		Title: "t", Source: "t.md", Content: content,
	}); err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	return svc
}

func TestNeighborsReturnsSurroundingChunks(t *testing.T) {
	svc := neighborsSvc(t, strings40())
	chunks := svc.mustAll(t)
	if len(chunks) < 3 {
		t.Fatalf("用例设计有误，应切出多段，实际 %d", len(chunks))
	}

	mid := chunks[1]
	prev, next := svc.Neighbors(mid, 1, 1)
	if len(prev) != 1 || prev[0].Ordinal != chunks[0].Ordinal {
		t.Errorf("前一段应为 ordinal=%d，实际 %v", chunks[0].Ordinal, ordinals(prev))
	}
	if len(next) != 1 || next[0].Ordinal != chunks[2].Ordinal {
		t.Errorf("后一段应为 ordinal=%d，实际 %v", chunks[2].Ordinal, ordinals(next))
	}
}

func TestNeighborsAtDocumentEdges(t *testing.T) {
	// 首段没有"前一段"、末段没有"后一段"。这时返回空切片而不是报错，
	// 也不该把别的文档的片段拿来充数。
	svc := neighborsSvc(t, strings40())
	chunks := svc.mustAll(t)

	prev, _ := svc.Neighbors(chunks[0], 1, 1)
	if len(prev) != 0 {
		t.Errorf("首段不该有前一段，实际 %v", ordinals(prev))
	}
	_, next := svc.Neighbors(chunks[len(chunks)-1], 1, 1)
	if len(next) != 0 {
		t.Errorf("末段不该有后一段，实际 %v", ordinals(next))
	}
}

func TestNeighborsZeroMeansOff(t *testing.T) {
	svc := neighborsSvc(t, strings40())
	chunks := svc.mustAll(t)
	prev, next := svc.Neighbors(chunks[1], 0, 0)
	if len(prev) != 0 || len(next) != 0 {
		t.Error("before/after 都是 0 时不该返回任何邻居")
	}
}

func TestNeighborsUnknownChunkReturnsNothing(t *testing.T) {
	// 找不到自己时**返回空而不是猜一个位置**：猜错会把别的片段的正文
	// 当成"上下文"贴上去，而 Agent 无从分辨——那比没有上下文糟糕得多。
	svc := neighborsSvc(t, strings40())
	ghost := types.Chunk{DocumentID: 1, Ordinal: 9999}
	prev, next := svc.Neighbors(ghost, 1, 1)
	if len(prev) != 0 || len(next) != 0 {
		t.Error("不存在的片段不该返回任何邻居")
	}
}

func TestNeighborsMultipleBothSides(t *testing.T) {
	svc := neighborsSvc(t, strings40())
	chunks := svc.mustAll(t)
	if len(chunks) < 5 {
		t.Fatalf("需要至少 5 段，实际 %d", len(chunks))
	}
	prev, next := svc.Neighbors(chunks[2], 2, 2)
	if len(prev) != 2 || len(next) != 2 {
		t.Fatalf("前后各 2 段，实际 prev=%d next=%d", len(prev), len(next))
	}
	// 前一段序列应当是"离命中由远及近"，这样调用方取最后一个是最近的那段。
	if prev[0].Ordinal != 0 || prev[1].Ordinal != 1 {
		t.Errorf("前一段顺序应为由远及近 [0 1]，实际 %v", ordinals(prev))
	}
	if next[0].Ordinal != 3 || next[1].Ordinal != 4 {
		t.Errorf("后一段顺序应为由近及远 [3 4]，实际 %v", ordinals(next))
	}
}

// ---- 辅助 ----

func strings40() string {
	return stringsRepeat("这一段用来把文档切成多段，每段长度接近上限。", 6)
}

func stringsRepeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

func ordinals(cs []types.Chunk) []int {
	out := make([]int, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Ordinal)
	}
	return out
}

func (s *Service) mustAll(t *testing.T) []types.Chunk {
	t.Helper()
	all, err := s.store.AllChunks(context.Background())
	if err != nil {
		t.Fatalf("读取片段失败: %v", err)
	}
	return all
}
