// Package types 定义 ContextDock 的核心数据结构。
//
// 这个包刻意保持"纯数据"：只有结构体和方法，没有任何 IO、没有数据库依赖、
// 没有网络调用。因此它可以被所有其他包安全地引用，也最容易做单元测试。
package types

import "time"

// Document 是一篇完整的原始文档。
//
// 它保存的是"切分之前"的原文，而不是切分后的片段。原因是：
// 以后如果调整切分策略（比如把 chunk 从 400 字改成 600 字），
// 只要原文还在，就能重新切一遍；如果只存了片段，就只能重新导入。
type Document struct {
	// ID 是数据库自增主键。零值 0 表示"这条还没存进数据库"。
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
	// 用 map[string]string 而不是 map[string]any 是刻意的：
	// 值类型统一成字符串，才能直接落进 PostgreSQL 的 JSONB 列，
	// 不用为每种类型写序列化分支。
	Metadata map[string]string `json:"metadata,omitempty"`

	// CreatedAt 是导入时间。零值表示由数据库的 DEFAULT now() 生成。
	CreatedAt time.Time `json:"created_at"`
}
