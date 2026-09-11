package types

// Chunk 是文档切分后的一个片段，也是检索的最小单位。
//
// "检索的最小单位"这句话很重要：向量检索返回的不是整篇文档，而是片段。
// 所以切分策略的好坏，直接决定检索质量——切得太碎会丢上下文，
// 切得太大会让无关内容污染相似度分数。
type Chunk struct {
	// ID 是数据库自增主键，零值 0 表示尚未持久化。
	ID int64 `json:"id"`

	// DocumentID 指向所属的 Document.ID。
	DocumentID int64 `json:"document_id"`

	// Ordinal 是这段在文档中的序号，从 0 开始。
	//
	// 保留序号的用途：检索时如果发现某段很相关，可以顺手把它的
	// 前后各一段也取出来返回（这叫"上下文扩展"），提高答案的完整性。
	Ordinal int `json:"ordinal"`

	// Content 是片段正文。
	Content string `json:"content"`

	// Embedding 是 Content 的向量表示，维度固定为 1024（bge-m3）。
	//
	// 为什么是 []float32 而不是 []float64？
	//   1. pgvector 存的就是 float32，1024 维占 4KB；用 float64 会翻倍到 8KB。
	//   2. pgvector-go 的 NewVector() 接收 []float32。
	// 代价是：硅基流动 API 返回的是 float64，需要在调用边界转换一次。
	// 这个转换是值得的——只在入库那一瞬间发生一次，而内存和存储省一半。
	//
	// 为什么打上 json:"-"？
	//   这是本项目一个容易忽略的坑。MCP 工具返回搜索结果时，如果
	//   Embedding 被序列化，10 条结果 × 1024 维会产生约 100KB 的纯数字噪声，
	//   而 Agent 拿到这些数字毫无用处——它要的只是 Content。
	//   json:"-" 让这个字段永远不参与序列化，从根上避免这个问题。
	Embedding []float32 `json:"-"`

	// Metadata 存放片段级附加信息。
	//
	// 目前最主要的用途是标题面包屑，例如 {"heading": "安装指南 > 快速开始"}。
	// 把它拼进被索引的正文是一种廉价的"上下文增强"：
	// 片段本身可能只写着"运行 go build"，加上面包屑后，
	// 用户搜"安装"也能命中它。
	Metadata map[string]string `json:"metadata,omitempty"`
}
