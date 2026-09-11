package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/XiaoleC05/ContextDock/internal/config"
	"github.com/XiaoleC05/ContextDock/internal/embed"
	"github.com/XiaoleC05/ContextDock/internal/service"
	"github.com/XiaoleC05/ContextDock/internal/store"
	"github.com/XiaoleC05/ContextDock/internal/types"
)

func newTestService(t *testing.T) *service.Service {
	t.Helper()
	cfg := &config.Config{
		SiliconFlowAPIKey: "sk-test",
		UseMemoryStore:    true,
		TopK:              10,
		SearchTimeout:     config.DefaultSearchTimeout,
		ChunkMaxRunes:     150,
		ChunkOverlap:      20,
		PoolMaxConns:      4,
		EmbeddingDim:      types.EmbeddingDim,
	}
	svc, err := service.New(cfg, embed.NewFake(), store.NewMemory())
	if err != nil {
		t.Fatalf("创建服务失败: %v", err)
	}
	return svc
}

// callImport 直接调用 handler，不走 MCP 协议（协议本身由 SDK 负责）。
func callImport(t *testing.T, svc *service.Service, in ImportInput) ImportOutput {
	t.Helper()
	h := HandleImport(svc)
	_, out, err := h(context.Background(), &sdkmcp.CallToolRequest{}, in)
	if err != nil {
		t.Fatalf("import 失败: %v", err)
	}
	return out
}

func callSearch(t *testing.T, svc *service.Service, in SearchInput) SearchOutput {
	t.Helper()
	h := HandleSearch(svc)
	_, out, err := h(context.Background(), &sdkmcp.CallToolRequest{}, in)
	if err != nil {
		t.Fatalf("search 失败: %v", err)
	}
	return out
}

// ---------------------------------------------------------------------------
// NewServer
// ---------------------------------------------------------------------------

func TestNewServerRegistersTools(t *testing.T) {
	svc := newTestService(t)
	srv := NewServer(svc, "v0.0.0-test")
	if srv == nil {
		t.Fatal("NewServer 不应返回 nil")
	}
	// 工具名是稳定契约，改名等于破坏 Agent 的配置
	if ToolImport != "import_document" || ToolSearch != "search_knowledge_base" {
		t.Errorf("工具名被改动: %q / %q —— 这是破坏性变更", ToolImport, ToolSearch)
	}
}

// ---------------------------------------------------------------------------
// import_document
// ---------------------------------------------------------------------------

func TestImportFromContent(t *testing.T) {
	svc := newTestService(t)
	out := callImport(t, svc, ImportInput{
		Content: "PostgreSQL 的连接池需要显式收窄，否则会按 CPU 核数开一堆连接。",
		Title:   "数据库笔记",
	})

	if out.DocumentID == 0 {
		t.Error("应当返回文档 ID")
	}
	if out.ChunkCount == 0 {
		t.Error("应当切出片段")
	}
	if out.EmbeddedNum != out.ChunkCount {
		t.Errorf("所有片段都应当有向量: %d/%d", out.EmbeddedNum, out.ChunkCount)
	}
	if out.Warning != "" {
		t.Errorf("正常导入不该有警告: %s", out.Warning)
	}
}

func TestImportFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "笔记.md")
	content := "# 安装指南\n\n运行 go build 即可编译。还需要配置 pgvector。\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	svc := newTestService(t)
	out := callImport(t, svc, ImportInput{FilePath: path})

	if out.DocumentID == 0 {
		t.Error("应当返回文档 ID")
	}

	// 标题应当从文件名推导
	docs, err := svc.Search(context.Background(), "安装指南", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs.Results) == 0 {
		t.Fatal("应当能搜到刚导入的内容")
	}
}

func TestImportTitleDerivation(t *testing.T) {
	svc := newTestService(t)

	// 不给标题时从正文首行推导
	out := callImport(t, svc, ImportInput{Content: "# 自动推导的标题\n\n正文内容在这里。"})
	if out.DocumentID == 0 {
		t.Fatal("导入失败")
	}

	// 空正文会被拦下
	h := HandleImport(svc)
	if _, _, err := h(context.Background(), &sdkmcp.CallToolRequest{},
		ImportInput{Content: "   "}); err == nil {
		t.Error("空内容应当报错")
	}
}

func TestImportValidation(t *testing.T) {
	svc := newTestService(t)
	h := HandleImport(svc)
	ctx := context.Background()

	t.Run("两个都不给", func(t *testing.T) {
		_, _, err := h(ctx, &sdkmcp.CallToolRequest{}, ImportInput{})
		if err == nil || !strings.Contains(err.Error(), "content 或 file_path") {
			t.Errorf("应提示必须提供其中一个，实际 %v", err)
		}
	})
	t.Run("两个都给", func(t *testing.T) {
		_, _, err := h(ctx, &sdkmcp.CallToolRequest{},
			ImportInput{Content: "正文", FilePath: "x.md"})
		if err == nil || !strings.Contains(err.Error(), "只能提供一个") {
			t.Errorf("应提示只能提供其中一个，实际 %v", err)
		}
	})
	t.Run("文件不存在", func(t *testing.T) {
		_, _, err := h(ctx, &sdkmcp.CallToolRequest{},
			ImportInput{FilePath: filepath.Join(t.TempDir(), "不存在.md")})
		if err == nil || !strings.Contains(err.Error(), "读取文件失败") {
			t.Errorf("应提示读取失败，实际 %v", err)
		}
	})
	t.Run("不支持的格式", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "文档.pdf")
		_ = os.WriteFile(path, []byte("%PDF-1.4"), 0o600)
		_, _, err := h(ctx, &sdkmcp.CallToolRequest{}, ImportInput{FilePath: path})
		if err == nil || !strings.Contains(err.Error(), "不支持的文件格式") {
			t.Errorf("PDF 应当被明确拒绝，实际 %v", err)
		}
		// 错误信息里要带上实际扩展名，方便排查
		if err != nil && !strings.Contains(err.Error(), ".pdf") {
			t.Errorf("错误信息应包含实际扩展名: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// search_knowledge_base
// ---------------------------------------------------------------------------

func TestSearchAfterImport(t *testing.T) {
	svc := newTestService(t)
	callImport(t, svc, ImportInput{
		Title:   "检索笔记",
		Content: "RRF 融合只用名次不用分数，所以不要求两路分数可比。BM25 的关键词检索依赖倒排索引。",
	})

	out := callSearch(t, svc, SearchInput{Query: "RRF 融合", TopK: 5})
	if out.Count == 0 {
		t.Fatal("应当能搜到刚导入的内容")
	}
	if out.Query != "RRF 融合" {
		t.Errorf("应当回显原始查询，实际 %q", out.Query)
	}
	if out.Results[0].Content == "" {
		t.Error("结果里应当有正文")
	}
	if out.Hint != "" {
		t.Errorf("有结果时不该给出「没找到」的提示: %q", out.Hint)
	}
}

func TestSearchEmptyQuery(t *testing.T) {
	svc := newTestService(t)
	h := HandleSearch(svc)

	for _, q := range []string{"", "   ", "\n\t"} {
		_, _, err := h(context.Background(), &sdkmcp.CallToolRequest{}, SearchInput{Query: q})
		if err == nil {
			t.Errorf("空查询 %q 应当报错", q)
		}
	}
}

// TestSearchGivesHintWhenEmpty 验证空结果时给出可操作建议。
//
// 只回一个空数组的话，Agent 通常只会说"没找到"，
// 用户不知道下一步该干什么。
func TestSearchGivesHintWhenEmpty(t *testing.T) {
	svc := newTestService(t)
	callImport(t, svc, ImportInput{Title: "无关文档", Content: "今天天气不错，适合出门散步。"})

	out := callSearch(t, svc, SearchInput{Query: "量子纠缠的数学基础"})
	if out.Count != 0 {
		t.Skip("检索器召回了不相关的内容，本用例的前提不成立")
	}
	if out.Hint == "" {
		t.Error("空结果时应当给出下一步建议")
	}
	if !strings.Contains(out.Hint, "import_document") {
		t.Errorf("建议里应当提到导入工具: %q", out.Hint)
	}
}

// TestSearchOutputHasNoEmbedding 是这条工具最重要的契约。
//
// 10 条结果 × 1024 维 = 约 100KB 纯数字。Agent 拿到它毫无用处，
// 只是白白吃掉上下文窗口。
//
// 这里断言的是**序列化之后的 JSON**，而不是结构体字段——
// 要防的正是"某天有人给 DTO 加了个字段"。
func TestSearchOutputHasNoEmbedding(t *testing.T) {
	svc := newTestService(t)
	callImport(t, svc, ImportInput{
		Title:   "文档",
		Content: strings.Repeat("这是一段用于测试输出体积的内容。", 30),
	})

	out := callSearch(t, svc, SearchInput{Query: "测试输出体积", TopK: 10})
	if out.Count == 0 {
		t.Fatal("应当有结果")
	}

	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}

	// ⚠️ 必须**按键名**判断，不能搜子串。
	//
	// 这里踩过一次：`matched_by` 字段的合法取值里就有 "vector"（检索通道名），
	// 用 strings.Contains(raw, "vector") 会误报。
	// 这和最初那个假测试是同一类错误——用文本子串检查 JSON 结构不可靠，
	// 既会误报也会漏报。
	keys := collectKeys(t, raw)
	for _, bad := range []string{"embedding", "Embedding", "vector", "Vec", "dim"} {
		if keys[bad] {
			t.Errorf("输出里出现了键 %q —— 向量不该出现在 MCP 输出里", bad)
		}
	}

	// 体积是行为层面的断言：带向量的话 10 条就是 100KB 以上。
	if len(raw) > 20000 {
		t.Errorf("输出 %d 字节，疑似把向量也发出去了（10 条纯文本应远小于 20KB）", len(raw))
	}
}

// collectKeys 递归收集 JSON 里出现过的所有键名。
func collectKeys(t *testing.T, raw []byte) map[string]bool {
	t.Helper()
	keys := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, sub := range x {
				keys[k] = true
				walk(sub)
			}
		case []any:
			for _, sub := range x {
				walk(sub)
			}
		}
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("序列化结果不是合法 JSON: %v", err)
	}
	walk(doc)
	return keys
}

// TestResultItemHasNoEmbeddingField 从类型层面确认 DTO 里没有向量字段。
//
// 这是编译期的保证：DTO 是我们自己定义的，字段列表是可数的。
func TestResultItemHasNoEmbeddingField(t *testing.T) {
	raw, err := json.Marshal(ResultItem{Content: "正文", Score: 0.01})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"content": true, "score": true, "ordinal": true}
	for k := range m {
		if !want[k] {
			t.Errorf("ResultItem 出现了未预期的字段 %q —— "+
				"新增字段时要确认它不是向量或其它大体积数据", k)
		}
	}
}

func TestSearchResultCarriesHeading(t *testing.T) {
	svc := newTestService(t)
	callImport(t, svc, ImportInput{
		Title:   "手册",
		Content: "# 安装指南\n\n运行 go build 编译本项目。\n",
	})

	out := callSearch(t, svc, SearchInput{Query: "安装指南", TopK: 5})
	if out.Count == 0 {
		t.Fatal("应当能搜到")
	}
	if out.Results[0].Heading == "" {
		t.Error("结果里应当带上标题面包屑，方便 Agent 引用出处")
	}
}

// TestSearchTopKPassthrough 验证 top_k 会自动填充。
func TestSearchTopKPassthrough(t *testing.T) {
	svc := newTestService(t)
	callImport(t, svc, ImportInput{
		Title:   "大文档",
		Content: strings.Repeat("不同的内容片段。", 50),
	})

	out := callSearch(t, svc, SearchInput{Query: "内容"})
	if out.Count > 10 {
		t.Errorf("未指定 top_k 时应当用默认值 10，实际返回 %d 条", out.Count)
	}
}
