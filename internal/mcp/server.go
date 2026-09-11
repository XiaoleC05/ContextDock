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
	// 命中的检索通道，便于解释"为什么这条排前面"
	MatchedBy []string `json:"matched_by,omitempty" jsonschema:"该结果被哪些检索通道命中（lexical / vector）"`
}

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
			out.Results = append(out.Results, toResultItem(r))
		}
		out.Count = len(out.Results)

		if out.Count == 0 {
			// 空结果时给一句可操作的建议，而不是只回一个空数组。
			// Agent 拿到空数组通常会说"没找到"，用户不知道下一步该干什么。
			out.Hint = "知识库中没有匹配的内容。可以先用 import_document 导入相关文档。"
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
func toResultItem(r types.SearchResult) ResultItem {
	item := ResultItem{
		Content: r.Chunk.Content,
		Score:   r.Score,
		Ordinal: r.Chunk.Ordinal,
		Heading: r.Chunk.Metadata[types.MetadataKeyHeading],
	}
	for _, m := range r.MatchedBy() {
		item.MatchedBy = append(item.MatchedBy, string(m))
	}
	return item
}
