package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/XiaoleC05/ContextDock/internal/types"
)

func chunkWith(content string, ordinal int) types.Chunk {
	c := types.Chunk{DocumentID: 1, Ordinal: ordinal, Content: content}
	c.SetMetadata(types.MetadataKeySource, "doc.md")
	return c
}

func TestToResultItemContextTakesNearestEnds(t *testing.T) {
	// ⚠️ 方向是关键：前一段取**尾部**、后一段取**头部**。
	//
	// 因为那才是紧挨着命中片段的一头。取前一段的头部等于给 Agent 看一段
	// 它根本接不上的话，比不给还糟。
	hit := chunkWith("命中片段正文", 1)
	prev := chunkWith("前段开头"+strings.Repeat("前", 200)+"前段结尾", 0)
	next := chunkWith("后段开头"+strings.Repeat("后", 200)+"后段结尾", 2)

	item := toResultItem(
		types.SearchResult{Chunk: hit},
		[]types.Chunk{prev},
		[]types.Chunk{next},
	)

	if !strings.HasSuffix(item.ContextBefore, "前段结尾") {
		t.Errorf("context_before 应当取前一段的尾部，实际 %q", item.ContextBefore)
	}
	if !strings.HasPrefix(item.ContextAfter, "后段开头") {
		t.Errorf("context_after 应当取后一段的头部，实际 %q", item.ContextAfter)
	}
	if strings.Contains(item.ContextBefore, "前段开头") {
		t.Error("context_before 里不该出现前一段的头部")
	}
	if strings.Contains(item.ContextAfter, "后段结尾") {
		t.Error("context_after 里不该出现后一段的尾部")
	}
}

func TestContextSnippetIsBounded(t *testing.T) {
	// 输出体积必须有上限：Agent 的上下文窗口是稀缺资源。
	long := strings.Repeat("字", 1000)
	item := toResultItem(
		types.SearchResult{Chunk: chunkWith("命中", 1)},
		[]types.Chunk{chunkWith(long, 0)},
		[]types.Chunk{chunkWith(long, 2)},
	)
	for _, s := range []string{item.ContextBefore, item.ContextAfter} {
		// +1 是省略号
		if n := len([]rune(s)); n > contextSnippetRunes+1 {
			t.Errorf("上下文摘要长度 %d 超过上限 %d", n, contextSnippetRunes)
		}
	}
}

func TestContextSnippetTruncatesByRuneNotByte(t *testing.T) {
	// 按字节截会把汉字切成半个、输出乱码——中文场景下最容易踩的语言级问题，
	// 而且它不报错，只是结果看起来像乱码。
	//
	// ⚠️ 前缀那个 ASCII 字符是**故意的**：截断长度 120 恰好是 3 的倍数，
	// 纯汉字串按字节截正好落在字符边界上，切不出乱码。
	// 加上 1 个 ASCII 字符之后边界错开 1 字节，才能真正验出按字节截的 bug。
	// （第一版没加，变异测试直接报了 MISSED。）
	long := "x" + strings.Repeat("汉", 500)
	item := toResultItem(
		types.SearchResult{Chunk: chunkWith("命中", 1)},
		nil,
		[]types.Chunk{chunkWith(long, 2)},
	)
	for _, r := range item.ContextAfter {
		if r == '�' {
			t.Fatal("截断把汉字切碎了（出现替换字符）")
		}
	}
}

func TestNoNeighborsMeansNoContextFields(t *testing.T) {
	item := toResultItem(types.SearchResult{Chunk: chunkWith("命中", 1)}, nil, nil)
	if item.ContextBefore != "" || item.ContextAfter != "" {
		t.Errorf("没有邻居时不该有上下文字段：%q / %q",
			item.ContextBefore, item.ContextAfter)
	}
	// 序列化时也该整个消失（omitempty），而不是留一个空串。
	raw := mustJSON(t, item)
	if strings.Contains(raw, "context_before") || strings.Contains(raw, "context_after") {
		t.Errorf("没有上下文时不该出现这两个键：%s", raw)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	return string(b)
}
