package types

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// EmbeddingDim 是向量维度，全项目唯一真值来源。
//
// 为什么必须做成常量：这个数字会出现在三个地方——
// Embedder 的返回校验、向量检索的维度检查、pgvector 的建表语句（vector(1024)）。
// 三处各写一个字面量，任何一处写错（768/1536）都不会在编译期暴露，
// 只会在插入数据库时报一条指向插入、而非指向源头的错误。
//
// 1024 是硅基流动 Pro/BAAI/bge-m3 的原生输出维度，不做截断。
// 不能改成 4096：pgvector 的 HNSW 索引对 vector 类型上限 2000 维，
// 4096 维连 halfvec（上限 4000）都建不了索引。
const EmbeddingDim = 1024

// Metadata 里约定使用的键。
const (
	// MetadataKeyHeading 存放标题面包屑，例如 "安装指南 > 快速开始"。
	MetadataKeyHeading = "heading"
)

// ErrNilChunk 之类的哨兵错误，供调用方用 errors.Is 判断。
var (
	ErrChunkNoDocument    = errors.New("types: chunk 的 DocumentID 为 0")
	ErrChunkBadOrdinal    = errors.New("types: chunk 的 Ordinal 为负数")
	ErrChunkEmptyContent  = errors.New("types: chunk 的 Content 为空")
	ErrChunkBadEmbedding  = errors.New("types: chunk 的 Embedding 维度不等于 EmbeddingDim")
	ErrDocumentEmptyTitle = errors.New("types: document 的 Title 为空")
)

// Chunk 是文档切分后的一个片段，也是检索的最小单位。
//
// "检索的最小单位"这句话很重要：向量检索返回的不是整篇文档，而是片段。
// 所以切分策略的好坏，直接决定检索质量——切得太碎会丢上下文，
// 切得太大会让无关内容污染相似度分数。
type Chunk struct {
	// ID 是数据库自增主键，零值 0 表示尚未持久化。用 IsPersisted() 判断。
	ID int64 `json:"id"`

	// DocumentID 指向所属的 Document.ID。**零值是非法值**，必须大于 0。
	DocumentID int64 `json:"document_id"`

	// Ordinal 是这段在文档中的序号，从 0 开始。
	//
	// ⚠️ 注意零值语义：0 是**合法值**（绝大多数文档的第一段就是 0），
	// 它不能像 ID 那样被当作"未设置"的哨兵。
	// 如果确实需要表达"未计算"，请另加字段，不要复用 0。
	//
	// 保留序号的用途：检索时如果发现某段很相关，可以顺手把它的
	// 前后各一段也取出来返回（这叫"上下文扩展"），提高答案的完整性。
	Ordinal int `json:"ordinal"`

	// StartOffset 和 EndOffset 是这段在**原文中的位置**，左闭右开。
	//
	// ⚠️ 单位是 **rune（字符）而不是 byte**。
	// Go 的 string 索引是字节，一个汉字占 3 字节——用字节偏移配合 s[a:b]
	// 会切出乱码，甚至切碎多字节字符。要还原原文必须用：
	//
	//	runes := []rune(doc.Content)
	//	text := string(runes[c.StartOffset:c.EndOffset])
	//
	// 为什么要保留偏移：
	//   1. 校验切分没有丢内容（相邻片段应当首尾相接）
	//   2. 上下文扩展时判断两段是否重叠
	//   3. 将来改切分参数后，能把新旧片段对齐
	//
	// 注意：片段之间有 overlap，所以相邻片段的偏移区间是**重叠**的，
	// 不是相接的。覆盖性校验应该用「区间并集 == [0, 原文长度)」。
	StartOffset int `json:"start_offset"`
	EndOffset   int `json:"end_offset"`

	// Content 是片段正文。它应当等于 []rune(原文)[StartOffset:EndOffset]。
	Content string `json:"content"`

	// Embedding 是 Content 的向量表示，维度固定为 EmbeddingDim（1024）。
	// 尚未嵌入时为零值 nil —— 用 IsEmbedded() 区分"还没算"和"算错了"。
	//
	// 为什么是 []float32 而不是 []float64？
	//   1. pgvector 存的就是 float32，1024 维占 4KB；用 float64 会翻倍到 8KB。
	//   2. pgvector-go 的 NewVector() 接收 []float32。
	// 代价是：硅基流动 API 返回的是 float64，需要在调用边界转换一次。
	// 这个转换是值得的——只在入库那一瞬间发生一次，而内存和存储省一半。
	//
	// 为什么打上 json:"-"？
	//   MCP 工具返回搜索结果时，如果 Embedding 被序列化，
	//   10 条结果 × 1024 维会产生约 100KB 的纯数字噪声，
	//   而 Agent 拿到这些数字毫无用处——它要的只是 Content。
	//
	// ⚠️ json:"-" 是**单向**的：任何"序列化成 JSON 再读回"的路径
	//   （本地缓存、调试快照）都会静默丢掉向量，读回来是 nil，
	//   与"还没嵌入"无法区分，可能导致重复调用 Embedding API 产生费用。
	//   结论：向量只能来自 pgvector，不能走 JSON 往返。
	Embedding []float32 `json:"-"`

	// Metadata 存放片段级附加信息，最主要是标题面包屑（键 MetadataKeyHeading）。
	//
	// ⚠️ 这是裸 map，零值是 nil。**直接写入会 panic**：
	//     c := Chunk{Content: "..."}
	//     c.Metadata["heading"] = "安装"   // panic: assignment to entry in nil map
	// 请改用 SetMetadata()，它会自动初始化。
	//
	// 用 map[string]string 而不是 map[string]any 是刻意的：
	// 值类型统一成字符串，才能直接落进 PostgreSQL 的 JSONB 列。
	Metadata map[string]string `json:"metadata,omitempty"`
}

// SetMetadata 安全地写入一个元数据键值对，自动处理 nil map。
//
// 用指针接收者，因为它会修改 Metadata 字段本身。
func (c *Chunk) SetMetadata(key, value string) {
	if c.Metadata == nil {
		c.Metadata = make(map[string]string, 2)
	}
	c.Metadata[key] = value
}

// IndexText 返回**真正拿去做分词建索引和算 embedding 的文本**。
//
// 这是本包最重要的一个方法。背景：片段本身可能只写着"运行 go build"，
// 单独看毫无上下文。把标题面包屑拼进去之后，用户搜"安装"也能命中它。
//
// 为什么必须共用这一个方法，而不是让各处自己拼：
//   - BM25 的分词、文档长度、倒排索引，都建立在索引文本上
//   - 向量的语义空间，也建立在同一个文本上
//
// 如果 Embedder 和 BM25 各自拼一次（一个读 Metadata["heading"] 用 ">" 拼，
// 另一个忘了拼或分隔符不同），**两路检索搜的就不是同一个文本**，
// 召回结果不可比，RRF 融合质量下降，而且极难排查。
//
// 硬约束：凡是"拿去 embed 或建索引"的代码路径，
// 都不允许直接读 c.Content，必须经过 IndexText()。
func (c Chunk) IndexText() string {
	heading := c.Metadata[MetadataKeyHeading]
	if heading == "" {
		return c.Content
	}
	return heading + "\n" + c.Content
}

// StableKey 返回跨检索通道识别"同一条结果"的稳定键。
//
// 为什么需要它：RRF 融合要按某个键把两路结果对应起来。
// 最自然的想法是用 Chunk.ID，但**落库之前所有 chunk 的 ID 都是 0**，
// 于是两个完全不同的片段会被当成同一条合并，RRF 名次整体错乱——
// 而且不会报任何错，只是结果变差，排查时会先怀疑分词和模型。
//
// 规则：已持久化用 ID，未持久化退化为 (DocumentID, Ordinal)。
// 加上前缀是为了避免两种形式意外撞键。
//
// 硬约束：M4 的 RRF 只允许用这个键做映射与去重。
func (c Chunk) StableKey() string {
	if c.ID != 0 {
		return "id:" + strconv.FormatInt(c.ID, 10)
	}
	return fmt.Sprintf("doc:%d#%d", c.DocumentID, c.Ordinal)
}

// String 实现 fmt.Stringer，防止日志把 1024 个浮点数倒出来。
//
// 背景：json:"-" 只挡住 encoding/json，**挡不住 fmt 的 %v / %+v**。
// 下个阶段接真实 Embedder 后，调试时几乎必然会 log.Printf("%+v", chunk)，
// 单条就是约 8KB 纯数字，几十条就把日志淹掉，反过来拖慢调试。
func (c Chunk) String() string {
	return fmt.Sprintf("Chunk{id:%d doc:%d ordinal:%d content:%q embedding:%d维}",
		c.ID, c.DocumentID, c.Ordinal, truncateRunes(c.Content, 30), len(c.Embedding))
}

// IsPersisted 判断这个片段是否已经落库（即已经拿到数据库自增 ID）。
func (c Chunk) IsPersisted() bool { return c.ID != 0 }

// IsEmbedded 判断是否已经算过向量。
//
// 注意区分两件事：Embedding 为 nil 表示"还没算"，
// 长度既不是 0 也不是 EmbeddingDim 表示"算错了"（见 Validate）。
func (c Chunk) IsEmbedded() bool { return len(c.Embedding) > 0 }

// Validate 校验片段是否处于合法状态。
//
// 收敛校验规则的目的：切分、嵌入、落库三个边界都需要这些检查。
// 如果各写各的 if，规则会漂移——最典型的后果是空文档被切成 0 个 chunk
// 后静默入库，检索时它永远召不回，但不报错。
func (c Chunk) Validate() error {
	if c.DocumentID == 0 {
		return ErrChunkNoDocument
	}
	if c.Ordinal < 0 {
		return ErrChunkBadOrdinal
	}
	if c.StartOffset < 0 || c.EndOffset < c.StartOffset {
		return fmt.Errorf("types: 偏移非法 [%d, %d)", c.StartOffset, c.EndOffset)
	}
	if strings.TrimSpace(c.Content) == "" {
		return ErrChunkEmptyContent
	}
	// Embedding 为 nil 是允许的（尚未嵌入），非 nil 则必须维度正确。
	if len(c.Embedding) != 0 && len(c.Embedding) != EmbeddingDim {
		return fmt.Errorf("%w: 实际 %d 维", ErrChunkBadEmbedding, len(c.Embedding))
	}
	return nil
}

// truncateRunes 按**字符**（rune）截断，不是按字节。
//
// Go 的 len() 返回字节数，一个汉字占 3 字节——按字节截断会把汉字切成半个，
// 输出乱码。这是中文场景下最容易踩的语言级 bug。
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
