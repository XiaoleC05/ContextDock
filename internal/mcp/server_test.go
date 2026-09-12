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
	// ⚠️ 这条断言**改过**。原来它断言「有结果时 hint 必须为空」，
	// 而 #54 之后有结果时也会带一条说明——但**内容完全不同**：
	// 空结果说「去导入文档」，有结果说「分数判断不了相关性，请读内容」。
	//
	// 所以要断的是**语义**（不该出现"没找到"那套），而不是"字段为空"——
	// 后者会把一条正确的说明误判成回归。
	if strings.Contains(out.Hint, "没有匹配的内容") {
		t.Errorf("有结果时不该给出「没找到」的提示: %q", out.Hint)
	}
}

// TestSearchReturnsSource 验证溯源字段真的被填上了。
//
// 这条用例的存在本身就是个教训。`ResultItem.Source` 曾经声明了、
// 还带着 jsonschema 描述（Agent 在 tools/list 里看得见），
// 但 `toResultItem` **从不给它赋值**；而 Source 挂在 Document 上、
// 检索路径里走的只有 Chunk —— 于是它**永远是空的**。
//
// 为什么之前的测试全绿也没发现：DTO 里有这个字段、schema 里有这个条目，
// 但没有任何用例断言过它的**值**。声明 ≠ 赋值 ≠ 有测试。
func TestSearchReturnsSource(t *testing.T) {
	svc := newTestService(t)

	callImport(t, svc, ImportInput{
		Title:   "部署手册",
		Source:  "docs/deploy.md",
		Content: "灰度发布的第一步是先切百分之五的流量，观察错误率再决定要不要继续放量。",
	})

	out := callSearch(t, svc, SearchInput{Query: "灰度发布怎么开始", TopK: 5})
	if out.Count == 0 {
		t.Fatal("应当能搜到刚导入的内容")
	}
	if got := out.Results[0].Source; got != "docs/deploy.md" {
		t.Errorf("结果应当带上文档来源：期望 %q，实际 %q", "docs/deploy.md", got)
	}

	// 光断言结构体字段还不够。这个字段带 omitempty，
	// 空值会被**静默**从 JSON 里抹掉——那正是它当初的失效形态：
	// 结构体里明明有字段，Agent 收到的东西里却没有。
	// 所以这里断言的是序列化之后的字节。
	raw, err := json.Marshal(out.Results[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"source":"docs/deploy.md"`) {
		t.Errorf("source 必须出现在序列化结果里，实际: %s", raw)
	}
}

// TestSearchSourceDefaultsToInline 验证省略 source 时的兜底值能一路透传到结果。
//
// 兜底在 MCP 层做（firstNonEmpty(in.Source, "inline")），
// 这条用例同时覆盖了"导入时兜底"和"检索时透传"是接上的——
// 任何一端断了，这里都会红。
func TestSearchSourceDefaultsToInline(t *testing.T) {
	svc := newTestService(t)
	callImport(t, svc, ImportInput{
		Title:   "随手记",
		Content: "向量检索是暴力扫描，千级片段下耗时在毫秒级。",
	})

	out := callSearch(t, svc, SearchInput{Query: "暴力扫描的耗时", TopK: 5})
	if out.Count == 0 {
		t.Fatal("应当有结果")
	}
	if got := out.Results[0].Source; got != "inline" {
		t.Errorf("省略 source 时应兜底为 %q，实际 %q", "inline", got)
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

// TestSearchGivesHintWhenEmpty 验证**索引为空**时给出可操作建议。
//
// 只回一个空数组的话，Agent 通常只会说"没找到"，
// 用户不知道下一步该干什么。
//
// ⚠️ 这条测试**改过**。原来它先导入一份无关文档、再搜一个无关问题，
// 期望"检索器召回不到"——于是它永远 SKIP：库里非空时检索**总会返回 top-K 条**
// （见 README「没有相关性阈值」）。一条长期 SKIP 的测试等于没有测试。
//
// 现在直接测"索引为空"这个能确定构造出来的前提。
func TestSearchGivesHintWhenEmpty(t *testing.T) {
	svc := newTestService(t) // 不导入任何东西 → 索引为空

	out := callSearch(t, svc, SearchInput{Query: "随便问点什么"})
	if out.Count != 0 {
		t.Fatalf("索引为空时不该有结果，实际 %d 条", out.Count)
	}
	if out.Hint == "" {
		t.Fatal("空结果时应当给出下一步建议")
	}
	if !strings.Contains(out.Hint, "import_document") {
		t.Errorf("建议里应当提到导入工具: %q", out.Hint)
	}
}

// TestSearchExplainsScoreLimitation 验证**有结果**时说明分数判断不了相关性。
//
// 这是 #54 的交付。原问题：库非空但问题完全不相关时，检索照样返回 top-K 条，
// 而且不带任何信号。补"不相关提示"被实测否掉了——四类候选信号
// （最高余弦 / 顶部陡峭度 / 两路一致条数 / 融合分差）**全部与真正命中的重叠**，
// 没有任何阈值能不误判。
//
// 所以交付的不是相关性判断，而是**把"工具判断不了"明说**：
// Agent 能看到 content，工具只有分数——它缺的正是这条信息。
func TestSearchExplainsScoreLimitation(t *testing.T) {
	svc := newTestService(t)
	callImport(t, svc, ImportInput{
		Title:   "无关文档",
		Content: "今天天气不错，适合出门散步。公园里的花开得正好。",
	})

	// 一个库里根本没有答案的问题——检索照样会返回 top-K 条。
	out := callSearch(t, svc, SearchInput{Query: "量子纠缠的数学基础"})
	if out.Count == 0 {
		t.Fatal("库非空时检索总会返回结果，本用例的前提是「有结果但可能不相关」")
	}
	if out.Hint == "" {
		t.Fatal("有结果时也应当说明分数的局限——否则 Agent 没有任何信号")
	}
	if !strings.Contains(out.Hint, "score") {
		t.Errorf("说明里应当点出 score 这个字段名，实际: %q", out.Hint)
	}
	if !strings.Contains(out.Hint, "content") {
		t.Errorf("说明里应当告诉 Agent 去看 content，实际: %q", out.Hint)
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
	// 白名单只收**标量**。往这里加字段时问一句「它会不会是 1024 个浮点数」——
	// 那正是这个测试要挡的东西。
	want := map[string]bool{
		"content": true, "score": true, "ordinal": true,
		// 原始分（#53）：两个标量，用来让 Agent 自己判断相关性。
		// RRF 分完全不可分，原始分部分可分——但都不该由工具替 Agent 过滤。
		"lexical_score": true, "vector_score": true,
	}
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
