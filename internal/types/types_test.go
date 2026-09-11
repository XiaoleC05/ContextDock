package types

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestChunkEmbeddingIsNotSerialized 守护 chunk.go 上那个 json:"-" 标签。
//
// 这是一个"防回归测试"：它测试的不是功能，而是一条设计约束。
// 如果以后有人（包括未来的你自己）觉得 Embedding 应该被序列化而删掉了
// json:"-"，这个测试会立刻失败，而不是等到某天 MCP 返回 100KB 垃圾数据
// 才发现。
func TestChunkEmbeddingIsNotSerialized(t *testing.T) {
	c := Chunk{
		ID:         1,
		DocumentID: 10,
		Ordinal:    0,
		Content:    "ContextDock 是一个混合检索服务",
		Embedding:  []float32{0.1, 0.2, 0.3},
	}

	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}

	// 断言 Embedding 字段名没有出现在 JSON 里。
	if strings.Contains(string(raw), "embedding") {
		t.Errorf("Embedding 不应被序列化，但输出里出现了:\n%s", raw)
	}

	// 反序列化回来，Embedding 应该是空的。
	var back Chunk
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if len(back.Embedding) != 0 {
		t.Errorf("反序列化后 Embedding 应为空，实际长度 %d", len(back.Embedding))
	}

	// 但其他字段必须完整保留，否则就是"忽略过头"了。
	if back.Content != c.Content {
		t.Errorf("Content 丢失: 期望 %q，实际 %q", c.Content, back.Content)
	}
	if back.DocumentID != c.DocumentID {
		t.Errorf("DocumentID 丢失: 期望 %d，实际 %d", c.DocumentID, back.DocumentID)
	}
}

// TestDocumentMetadataOmittedWhenEmpty 验证空 Metadata 不会产生 "metadata": null。
//
// omitempty 的作用是让空值字段整个消失，而不是留下一个 null。
// 对 Agent 来说，多余的 null 字段是噪声，也浪费 token。
func TestDocumentMetadataOmittedWhenEmpty(t *testing.T) {
	d := Document{ID: 1, Title: "测试", Content: "正文"}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if bytes.Contains(raw, []byte("metadata")) {
		t.Errorf("空 Metadata 应被 omitempty 省略，实际输出:\n%s", raw)
	}
}

// TestSearchResultMatchedBy 用表驱动的方式覆盖 MatchedBy 的全部分支。
//
// 表驱动（table-driven）是 Go 测试的惯用写法：把输入和期望值放进一个
// 切片里循环跑。好处是加用例只要加一行，不用复制粘贴整个测试函数。
func TestSearchResultMatchedBy(t *testing.T) {
	tests := []struct {
		name        string
		lexicalRank int
		vectorRank  int
		want        []Retriever
		wantBoth    bool
	}{
		{
			name:        "两路都召回",
			lexicalRank: 1,
			vectorRank:  3,
			want:        []Retriever{RetrieverLexical, RetrieverVector},
			wantBoth:    true,
		},
		{
			name:        "只有关键词召回",
			lexicalRank: 2,
			vectorRank:  0,
			want:        []Retriever{RetrieverLexical},
			wantBoth:    false,
		},
		{
			name:        "只有向量召回",
			lexicalRank: 0,
			vectorRank:  5,
			want:        []Retriever{RetrieverVector},
			wantBoth:    false,
		},
		{
			name:        "两路都没召回",
			lexicalRank: 0,
			vectorRank:  0,
			want:        []Retriever{},
			wantBoth:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := SearchResult{LexicalRank: tt.lexicalRank, VectorRank: tt.vectorRank}

			got := r.MatchedBy()
			if len(got) != len(tt.want) {
				t.Fatalf("MatchedBy() 长度: 期望 %v，实际 %v", tt.want, got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("MatchedBy()[%d]: 期望 %q，实际 %q", i, tt.want[i], got[i])
				}
			}

			if r.MatchedByBoth() != tt.wantBoth {
				t.Errorf("MatchedByBoth(): 期望 %v，实际 %v", tt.wantBoth, r.MatchedByBoth())
			}
		})
	}
}

// TestRetrieverJSONValue 确认 Retriever 序列化成可读字符串而非数字。
//
// 这是选择"自定义字符串类型"而不是 int 常量的直接收益：
// 日志和调试输出里看到的是 "lexical"，一眼就懂。
func TestRetrieverJSONValue(t *testing.T) {
	raw, err := json.Marshal(RetrieverLexical)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if string(raw) != `"lexical"` {
		t.Errorf("期望 %q，实际 %q", `"lexical"`, raw)
	}
}
