package mcp

import (
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/XiaoleC05/ContextDock/internal/service"
)

// 工具名。集中定义，测试和文档都引用这里，避免各处写字符串字面量。
const (
	ToolImport = "import_document"
	ToolSearch = "search_knowledge_base"
)

// NewServer 注册全部工具并返回一个 MCP server。
//
// 工具描述是写给**模型**看的——它决定 Agent 什么时候会调用这个工具。
// 所以要写清楚"什么时候该用"，而不只是"这个工具做什么"。
func NewServer(svc *service.Service, version string) *sdkmcp.Server {
	srv := sdkmcp.NewServer(&sdkmcp.Implementation{
		Name:        "contextdock",
		Title:       "ContextDock",
		Version:     version,
		Description: "本地文档混合检索服务：关键词 + 向量检索，RRF 融合",
	}, nil)

	sdkmcp.AddTool(srv, &sdkmcp.Tool{
		Name: ToolImport,
		Description: "把文档导入本地知识库，之后可以用 search_knowledge_base 检索。" +
			"支持直接传入文本内容，或 .txt / .md 文件的路径。" +
			"当用户提供了一份资料并希望在后续对话中能查到时，调用这个工具。",
	}, HandleImport(svc))

	sdkmcp.AddTool(srv, &sdkmcp.Tool{
		Name: ToolSearch,
		Description: "在已导入的本地知识库中检索相关文档片段。" +
			"当用户的问题需要依据之前导入的资料来回答时，调用这个工具。" +
			"返回的是原始文档片段，需要你根据这些片段组织回答。",
	}, HandleSearch(svc))

	return srv
}
