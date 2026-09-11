# ContextDock

> 基于 Go 的混合检索 MCP 服务，为本地 Agent 提供向量 + BM25 混合召回

ContextDock 是一个本机运行的 MCP Server。它把文档切分成片段、生成向量、存入 PostgreSQL，
并在 Agent 提问时用「关键词检索 + 向量检索 + RRF 融合」找出最相关的片段返回。

**核心职责：为 Agent 提供准确、快速、可靠的文档检索结果。**

## 状态

🚧 开发中（第一版开发周期：一周）

## 技术栈

| 层 | 选型 |
| --- | --- |
| 语言 | Go |
| 通信 | MCP stdio |
| 存储 | PostgreSQL + pgvector |
| 数据库驱动 | pgx |
| Embedding | 硅基流动 Qwen3-Embedding |
| 关键词检索 | BM25 |
| 混合排序 | RRF |

## 文档

- [ ] 架构图
- [ ] 快速开始
- [ ] MCP 工具说明
- [ ] 性能测试报告
