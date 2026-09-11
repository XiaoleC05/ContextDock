// Package types 定义 ContextDock 的核心数据结构。
//
// 这个包刻意保持"纯数据"：只有结构体和方法，没有任何 IO、没有数据库依赖、
// 没有网络调用。因此它可以被所有其他包安全地引用，也最容易做单元测试。
//
// 零值约定（全包统一，务必遵守）：
//
//	字段              零值含义                    判断方法
//	Document.ID       0 = 尚未落库                IsPersisted()
//	Document.CreatedAt 零值 = 交给数据库 DEFAULT now()  IsPersisted()
//	Chunk.ID          0 = 尚未落库                IsPersisted()
//	Chunk.Embedding   nil = 尚未嵌入               IsEmbedded()
//	Chunk.DocumentID  0 = **非法值**               Validate() 会报错
//	Chunk.Ordinal     0 = **合法值**（第一段）      不可用作哨兵！
//	SearchResult.*Rank 0 = 该通道**未召回**        RRFScore() 内部已处理
//
// 判断零值请统一走上面的方法，不要在各处直接写 `!= 0` 比较——
// 收口之后规则才不会漂移。
package types

import (
	"fmt"
	"strings"
	"time"
)

// Document 是一篇完整的原始文档。
//
// 它保存的是"切分之前"的原文，而不是切分后的片段。原因是：
// 以后如果调整切分策略（比如把 chunk 从 400 字改成 600 字），
// 只要原文还在，就能重新切一遍；如果只存了片段，就只能重新导入。
type Document struct {
	// ID 是数据库自增主键。零值 0 表示"这条还没存进数据库"，用 IsPersisted() 判断。
	//
	// 这是 Go 里很常见的一个约定：用零值表达"尚未赋值"。
	// 好处是不需要额外的 *int64 或者单独的 NewDocument 类型。
	ID int64 `json:"id"`

	// Title 是文档标题，通常取自文件名或 Markdown 的一级标题。
	Title string `json:"title"`

	// Source 是文档来源，例如文件路径或 "inline"（用户直接传文本）。
	Source string `json:"source"`

	// Content 是文档原文，尚未切分。
	Content string `json:"content"`

	// Metadata 存放附加信息，例如文件扩展名、标题面包屑等。
	//
	// ⚠️ 和 Chunk.Metadata 一样是裸 map，零值为 nil，直接写入会 panic。
	// 请使用 SetMetadata()。
	//
	// 用 map[string]string 而不是 map[string]any 是刻意的：
	// 值类型统一成字符串，才能直接落进 PostgreSQL 的 JSONB 列，
	// 不用为每种类型写序列化分支。
	Metadata map[string]string `json:"metadata,omitempty"`

	// CreatedAt 是导入时间。零值表示由数据库的 DEFAULT now() 生成。
	//
	// ⚠️ 这里**刻意没有加 omitempty**，原因是加了也没用：
	// encoding/json 的 isEmptyValue 只处理 Array/Map/Slice/String/Bool/
	// 数字/Interface/Pointer，**struct 永远不算空**，所以
	// `time.Time` 加 omitempty 照样会输出 "0001-01-01T00:00:00Z"。
	//
	// 由此产生两条必须遵守的契约：
	//   1. INSERT 语句不要包含 created_at 列（让数据库生成），
	//      写漏一次就会把公元 1 年写进库；
	//   2. 输出给 MCP 客户端的 Document 必须来自数据库读回，
	//      不要直接序列化一个尚未落库的 Document。
	CreatedAt time.Time `json:"created_at"`
}

// SetMetadata 安全地写入一个元数据键值对，自动处理 nil map。
func (d *Document) SetMetadata(key, value string) {
	if d.Metadata == nil {
		d.Metadata = make(map[string]string, 2)
	}
	d.Metadata[key] = value
}

// IsPersisted 判断这篇文档是否已经落库（即已经拿到数据库自增 ID）。
//
// 持久化层应当只调用这个方法，而不是各处直接比较 `d.ID != 0`。
// 收口之后，零值规则才不会在某个角落被写反。
func (d Document) IsPersisted() bool { return d.ID != 0 }

// Validate 校验文档是否处于可导入的状态。
func (d Document) Validate() error {
	if strings.TrimSpace(d.Title) == "" {
		return ErrDocumentEmptyTitle
	}
	if strings.TrimSpace(d.Content) == "" {
		return fmt.Errorf("types: document %q 的 Content 为空", d.Title)
	}
	return nil
}

// String 实现 fmt.Stringer，避免日志把整篇原文打出来。
func (d Document) String() string {
	return fmt.Sprintf("Document{id:%d title:%q source:%q content:%d字节}",
		d.ID, d.Title, d.Source, len(d.Content))
}
