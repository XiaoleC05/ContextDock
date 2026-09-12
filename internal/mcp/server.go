// Package mcp 把 Service 暴露成两个 MCP 工具。
//
// 这一层是**薄适配器**：只做参数校验、调用 Service、把结果转成 DTO。
// 所有业务逻辑（切分、嵌入、检索、降级）都在 service 包里——
// 这样换传输方式（stdio 换 HTTP）时业务逻辑一行都不用改。
package mcp

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/XiaoleC05/ContextDock/internal/service"
	"github.com/XiaoleC05/ContextDock/internal/types"
)

// 支持的文件扩展名。第一版只做纯文本。
var supportedExts = map[string]bool{
	".txt":      true,
	".md":       true,
	".markdown": true,
}

// ---------------------------------------------------------------------------
// import_document
// ---------------------------------------------------------------------------

// ImportInput 是 import_document 的入参。
//
// schema 由 SDK 反射推导，两条规则要记住：
//   - **没有 omitempty 的字段是必填**，有 omitempty 的是可选
//   - jsonschema tag 里只能放描述文本，写成 "required" 或 "enum=..." 会生成失败
type ImportInput struct {
	// 用一个字段同时接文本和路径会让 schema 说不清楚，
	// 所以拆成两个可选字段，在 handler 里校验"必须有且只有一个"。
	Content  string `json:"content,omitempty" jsonschema:"文档正文。与 file_path 二选一"`
	FilePath string `json:"file_path,omitempty" jsonschema:"文件路径（仅支持 .txt / .md）。与 content 二选一"`
	Title    string `json:"title,omitempty" jsonschema:"文档标题。省略时从文件名推导"`
	Source   string `json:"source,omitempty" jsonschema:"文档来源标签，用于后续区分出处"`
}

// ImportOutput 是 import_document 的返回。
type ImportOutput struct {
	DocumentID  int64  `json:"document_id" jsonschema:"数据库中的文档 ID"`
	ChunkCount  int    `json:"chunk_count" jsonschema:"切分出的片段数"`
	EmbeddedNum int    `json:"embedded_count" jsonschema:"成功生成向量的片段数"`
	Warning     string `json:"warning,omitempty" jsonschema:"非致命问题的说明，例如部分片段未生成向量"`
}

// HandleImport 实现 import_document。
func HandleImport(svc *service.Service) sdkmcp.ToolHandlerFor[ImportInput, ImportOutput] {
	return func(ctx context.Context, _ *sdkmcp.CallToolRequest, in ImportInput) (
		*sdkmcp.CallToolResult, ImportOutput, error) {

		doc, err := buildDocument(in)
		if err != nil {
			return nil, ImportOutput{}, err
		}

		// ⚠️ 日志走 log（stderr）。stdout 是 MCP 的 JSON-RPC 通道，
		// 往那里写一个字节都会破坏协议，而且客户端只会报一个
		// 看不懂的解析错误。
		log.Printf("import_document: title=%q source=%q", doc.Title, doc.Source)

		res, err := svc.Import(ctx, doc)
		out := ImportOutput{}
		if res != nil {
			out.DocumentID = res.DocumentID
			out.ChunkCount = res.ChunkCount
			out.EmbeddedNum = res.EmbeddedNum
		}
		if err != nil {
			if res == nil {
				// 文档都没落库，是真的失败
				return nil, ImportOutput{}, fmt.Errorf("导入失败: %w", err)
			}
			// 文档已落库但后续步骤有问题——告诉调用方，但不算失败。
			// 重试只需要重算向量，不需要重新导入。
			out.Warning = fmt.Sprintf("文档已保存，但部分步骤未完成：%v", err)
		}
		log.Printf("import_document 完成: doc=%d chunks=%d embedded=%d",
			out.DocumentID, out.ChunkCount, out.EmbeddedNum)
		return nil, out, nil
	}
}

// buildDocument 把入参转成 Document，并做校验。
func buildDocument(in ImportInput) (*types.Document, error) {
	hasContent := strings.TrimSpace(in.Content) != ""
	hasPath := strings.TrimSpace(in.FilePath) != ""

	switch {
	case !hasContent && !hasPath:
		return nil, fmt.Errorf("必须提供 content 或 file_path 之一")
	case hasContent && hasPath:
		return nil, fmt.Errorf("content 和 file_path 只能提供一个")
	}

	doc := &types.Document{Source: in.Source}

	if hasPath {
		path := strings.TrimSpace(in.FilePath)
		if err := checkExtension(path); err != nil {
			return nil, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("读取文件失败: %w", err)
		}
		doc.Content = string(data)
		doc.Source = firstNonEmpty(in.Source, path)
		// 标题省略时从文件名推导
		doc.Title = firstNonEmpty(in.Title, strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	} else {
		doc.Content = in.Content
		doc.Title = firstNonEmpty(in.Title, deriveTitleFromContent(in.Content))
		doc.Source = firstNonEmpty(in.Source, "inline")
	}

	// 用类型层统一的校验，不在这一层重复写规则
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	return doc, nil
}

// checkExtension 明确拒绝不支持的格式。
//
// 静默失败（比如把 PDF 当文本读进来）比明确报错糟糕得多——
// 用户会得到一篇乱码文档，而且不知道哪里出了问题。
func checkExtension(path string) error {
	ext := strings.ToLower(filepath.Ext(path))
	if !supportedExts[ext] {
		return fmt.Errorf("不支持的文件格式 %q（第一版只支持 .txt 和 .md）", ext)
	}
	return nil
}

// deriveTitleFromContent 从正文第一行推导标题。
func deriveTitleFromContent(content string) string {
	first, _, _ := strings.Cut(content, "\n")
	first = strings.TrimSpace(strings.TrimLeft(first, "#"))
	if first == "" {
		return "未命名文档"
	}
	// 标题不该太长
	if r := []rune(first); len(r) > 60 {
		first = string(r[:60]) + "…"
	}
	return first
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// search_knowledge_base
// ---------------------------------------------------------------------------

// SearchInput 是 search_knowledge_base 的入参。
type SearchInput struct {
	Query string `json:"query" jsonschema:"要检索的问题或关键词"`
	TopK  int    `json:"top_k,omitempty" jsonschema:"返回多少条结果，默认 10"`
}

// ResultItem 是返回给 Agent 的一条结果。
//
// ⚠️ 这里**刻意不从 types.SearchResult 直接序列化**，而是单独定义 DTO。
//
// 原因：SearchResult 里内嵌了完整的 Chunk，而 Chunk 有 Embedding 字段。
// 虽然它打了 json:"-"，但那是"靠一个标签挡住"，一旦有人改动标签，
// 10 条结果 × 1024 维 = 约 100KB 的纯数字就会灌进 Agent 的上下文。
// 用独立 DTO 是**从类型上**保证不会有向量——不需要依赖任何标签。
type ResultItem struct {
	Content string  `json:"content" jsonschema:"片段正文"`
	Score   float64 `json:"score" jsonschema:"融合后的排序分（RRF 分）"`
	Source  string  `json:"source,omitempty" jsonschema:"所属文档的来源"`
	Ordinal int     `json:"ordinal" jsonschema:"片段在原文中的序号"`
	Heading string  `json:"heading,omitempty" jsonschema:"片段所属的标题层级（面包屑）"`
	// LexicalScore / VectorScore 是**各通道的原始分**。
	//
	// # 为什么要把原始分暴露给 Agent
	//
	// 因为分数判断相关性这件事，工具这边**做不到**（#52 实测）：
	//
	//   - RRF 分**完全不可分**：库里根本没有答案时，返回结果的最高分
	//     与真正命中时完全相同（都是 0.0328）
	//   - 原始余弦分部分可分，但任何可用阈值都会误杀 20%~50% 的有答案查询
	//     （阈值 0.58 时召回 100%、精确率只有 0.38）
	//
	// 所以工具**不做自动过滤**，而是把原始分交出去。
	// Agent 能看到内容的细节，而一个固定阈值只能看到一个数。
	//
	// ⚠️ 代价也要说清：这等于把判断责任推给了 Agent。
	// 如果它不加判断地照单全收，结果就是"永远返回 top-K"——也就是现状。
	LexicalScore float64 `json:"lexical_score" jsonschema:"关键词通道的原始 BM25 分（0 = 未被该通道召回）"`
	VectorScore  float64 `json:"vector_score" jsonschema:"向量通道的原始余弦相似度（0 = 未被该通道召回）"`

	// 命中的检索通道，便于解释"为什么这条排前面"
	MatchedBy []string `json:"matched_by,omitempty" jsonschema:"该结果被哪些检索通道命中（lexical / vector）"`

	// ContextBefore / ContextAfter 是**前后相邻片段的一小段摘要**。
	//
	// 命中片段常常只写着「运行 go build」，单独看不知道在讲什么。
	// 带上前一段的尾部、后一段的头部就够定位语境了。
	//
	// ⚠️ 是截断过的，不是完整相邻片段——见 contextSnippetRunes 的说明。
	ContextBefore string `json:"context_before,omitempty" jsonschema:"命中片段前一段的末尾（已截断），用于理解上下文"`
	ContextAfter  string `json:"context_after,omitempty" jsonschema:"命中片段后一段的开头（已截断），用于理解上下文"`
}

// scoreNote 是结果非空时随附的说明。
//
// 它**只陈述一件事**：分数不能用来判断相关性，判断得靠读内容。
//
// 保持简短是有意的：它每次检索都会跟着结果发出去，
// 而 Agent 的上下文窗口是稀缺资源（同 contextSnippetRunes 那条考虑）。
const scoreNote = "结果按融合名次排序，但 **score 不能用来判断相关性**" +
	"（实测：库里没有答案时的最高分与真正命中时完全相同）。" +
	"请阅读 content 自行判断是否回答了问题；vector_score 可作参考。"

// contextSnippetRunes 是上下文摘要的截断长度（rune）。
//
// # 为什么截断而不是整段带出
//
// Agent 的上下文窗口是稀缺资源。10 条结果各带两整段（每段可达 400 字）
// 会让输出膨胀近三倍，而那些内容**大多是重复的**——相邻片段本来就重叠 60 字。
//
// # 为什么前一段取尾、后一段取头
//
// 因为那才是紧挨着命中片段的一头。取前一段的头部等于给 Agent 看一段
// 它根本接不上的话，比不给还糟。
const contextSnippetRunes = 120

// SearchOutput 是 search_knowledge_base 的返回。
type SearchOutput struct {
	Query    string       `json:"query" jsonschema:"原始查询"`
	Count    int          `json:"count" jsonschema:"返回的结果条数"`
	Results  []ResultItem `json:"results" jsonschema:"按相关度排序的文档片段"`
	Degraded string       `json:"degraded,omitempty" jsonschema:"检索发生降级时的原因说明"`
	Hint     string       `json:"hint,omitempty" jsonschema:"没有结果时给出的下一步建议"`
}

// HandleSearch 实现 search_knowledge_base。
func HandleSearch(svc *service.Service) sdkmcp.ToolHandlerFor[SearchInput, SearchOutput] {
	return func(ctx context.Context, _ *sdkmcp.CallToolRequest, in SearchInput) (
		*sdkmcp.CallToolResult, SearchOutput, error) {

		out := SearchOutput{Query: in.Query, Results: []ResultItem{}}

		// 上下文扩展：每条结果带出前后各 N 段摘要。0 表示关掉。
		neighbors := svc.ContextNeighbors()

		query := strings.TrimSpace(in.Query)
		if query == "" {
			return nil, out, fmt.Errorf("query 不能为空")
		}

		log.Printf("search_knowledge_base: query=%q topK=%d", query, in.TopK)

		res, err := svc.Search(ctx, query, in.TopK)
		if err != nil {
			return nil, SearchOutput{}, fmt.Errorf("检索失败: %w", err)
		}

		out.Degraded = res.Degraded
		for _, r := range res.Results {
			prev, next := svc.Neighbors(r.Chunk, neighbors, neighbors)
			out.Results = append(out.Results, toResultItem(r, prev, next))
		}
		out.Count = len(out.Results)

		// hint 有两种，对应两种**完全不同**的处境。
		//
		// # 为什么"库非空但问题不相关"这种不能靠判断分数来解决
		//
		// #22 验收时发现：hint 只在索引为空时触发，库非空但问题完全不相关时
		// 不给任何信号。当时以为补一个"不相关提示"就行，实测发现**做不到**：
		// 试了四类候选信号，全部与真正命中的查询重叠——
		//
		//   最高余弦 / 顶部陡峭度 / 两路都召回的条数 / 融合 1~5 名分差
		//
		// 最直接的一条：库里没有答案时返回的最高 RRF 分，
		// 与真正命中时的最高分**完全相同**（都是 0.0328）。
		// 详见 BENCHMARKS.md 的 #52 与 #54 两节。
		//
		// 所以这里**不断言"这些结果不相关"**——那必然是猜的，
		// 而假阳性（把真命中误判成不相关）比漏报更糟：
		// Agent 会因此放弃一份本来能回答用户的结果。
		//
		// 改成明确说出「工具判断不了，请你来判断」。它不是相关性判断，
		// 但它是 Agent 真正缺的那条信息——它能看到 content，而工具只有分数。
		if out.Count == 0 {
			// 空结果时给一句可操作的建议，而不是只回一个空数组。
			// Agent 拿到空数组通常会说"没找到"，用户不知道下一步该干什么。
			out.Hint = "知识库中没有匹配的内容。可以先用 import_document 导入相关文档。"
		} else {
			out.Hint = scoreNote
		}

		log.Printf("检索完成: 命中 %d 条%s", out.Count,
			func() string {
				if out.Degraded != "" {
					return "（已降级：" + out.Degraded + "）"
				}
				return ""
			}())
		return nil, out, nil
	}
}

// toResultItem 把内部结果转成对外的 DTO。
//
// prev / next 是上下文扩展带出来的相邻片段，由 svc.Neighbors 提供；
// 传空表示不做扩展（配置里关掉了，或片段本来就在文档首尾）。
func toResultItem(r types.SearchResult, prev, next []types.Chunk) ResultItem {
	item := ResultItem{
		Content: r.Chunk.Content,
		Score:   r.Score,
		// 原始分：给 Agent 判断"这条到底相不相关"用，见字段注释。
		LexicalScore: r.LexicalScore,
		VectorScore:  r.VectorScore,
		Ordinal:      r.Chunk.Ordinal,
		Heading:      r.Chunk.Metadata[types.MetadataKeyHeading],
		// Source 来自**片段元数据**，不是 Document ——
		// 检索路径里根本没有 Document，它由 ingest 在导入时下沉进来。
		// 这个字段曾经声明了却从不赋值（DTO 声明 ≠ 有人填），
		// 结果是 Agent 拿不到任何溯源信息。见 TestSearchReturnsSource。
		Source: r.Chunk.Metadata[types.MetadataKeySource],
	}
	for _, m := range r.MatchedBy() {
		item.MatchedBy = append(item.MatchedBy, string(m))
	}

	// 只取**离命中最近的那一段**：取多段会让输出线性膨胀，
	// 而语境信息基本集中在紧挨着的那一段里。
	if n := len(prev); n > 0 {
		item.ContextBefore = tailRunes(prev[n-1].Content, contextSnippetRunes)
	}
	if len(next) > 0 {
		item.ContextAfter = headRunes(next[0].Content, contextSnippetRunes)
	}
	return item
}

// headRunes / tailRunes 按 **rune** 截断，不是按字节。
//
// 按字节截会把汉字切成半个、输出乱码——这是中文场景下最容易踩的语言级
// 问题，而它不会报错，只是结果看起来像乱码。
func headRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func tailRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n:])
}
