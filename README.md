# ContextDock

> 基于 Go 的混合检索 MCP 服务，为本地 Agent 提供向量 + BM25 混合召回

[![CI](https://github.com/XiaoleC05/ContextDock/actions/workflows/ci.yml/badge.svg)](https://github.com/XiaoleC05/ContextDock/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![MCP](https://img.shields.io/badge/MCP-stdio-blueviolet)](https://modelcontextprotocol.io)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

ContextDock 是一个**本机运行**的 MCP Server。它把文档切分成片段、生成向量、存入 PostgreSQL，
并在 Agent 提问时用「关键词检索 + 向量检索 + RRF 融合」找出最相关的片段返回。

> 🚧 **开发中** · `v1.0.0` 进度 3/25 —— 见 [里程碑](https://github.com/XiaoleC05/ContextDock/milestone/9)

---

## 这是什么

### 解决什么问题

Agent 很聪明，但它**没读过你的文档**。你问它问题，它只能瞎编——因为资料不在它脑子里。

ContextDock 站在 Agent 和你的文档之间，干两件事：

- **存**：把文档剪成小纸条（切分），给每张纸条拍一张「语义指纹」（向量），存进仓库
- **取**：你提问时，同时用两种方式找纸条，合并排序后挑最相关的递回去

### 为什么要两种检索方式

因为各有各的瞎子区：

| 只用一种 | 会出的问题 |
| --- | --- |
| 只用关键词 | 你问「怎么让程序跑得快」，文档里写的是「性能优化」——一个字没对上，找不到 |
| 只用语义 | 你问「Error 5001 怎么解决」，语义检索觉得「Error 5002」也很像，把错的递给你 |

把两边结果**按名次**合并（而不是按分数加权），这一招叫 **RRF**，是 ContextDock 的核心。
为什么按名次而不是分数，见 [设计决策 #6](docs/DESIGN.md)。

### 不适合什么

第一版**刻意不做**这些，避免范围失控：

- LLM 对话生成、Agent 编排
- PDF / Word 解析、图片或图文混合内容
- 中文以外的语种（只处理中英混合）
- 前端页面、权限系统、多节点集群

---

## 关键特性

- **混合检索**：BM25 关键词 + 向量语义，RRF 融合（k=60）
- **中英混合分词**：按书写系统分流——CJK 走字符 bigram，拉丁字母按词切，零外部依赖
- **两套存储实现**：内存版用于测试和 benchmark，pgvector 版用于持久化——检索核心不依赖数据库
- **可测的检索核心**：`FakeEmbedder` 不访问网络，BM25 与向量检索都能在纯内存里跑测试
- **MCP stdio 接入**：两个工具 `import_document` / `search_knowledge_base`

---

## 快速开始

### 环境要求

| 组件 | 版本 | 说明 |
| --- | --- | --- |
| Go | ≥ 1.25 | 官方 MCP SDK 要求；本项目开发用 1.26.4 |
| Docker Desktop | 任意 | 需启用 Linux 容器模式，M5 之后才用得到 |
| C 编译器 | MinGW-w64 | 仅跑 `-race` 时需要，CI 上自带 |

### 构建与测试

```bash
git clone https://github.com/XiaoleC05/ContextDock.git
cd ContextDock

go build ./...
go vet ./...
go test ./...
```

**当前可运行的部分**：核心类型（`internal/types`），25 个测试用例，覆盖率 96.2%。

### 配置 API Key

```bash
cp .env.example .env      # 然后把你的 key 填进去
```

```ini
SILICONFLOW_API_KEY=sk-xxxxxxxx
```

> `.env` 已在 `.gitignore` 中。**不要在代码里硬编码 key。**

---

## 用法

> ⏳ M6 完成后可用。以下为目标形态。

### MCP 工具

| 工具 | 输入 | 输出 |
| --- | --- | --- |
| `import_document` | 文本内容 或 `.txt` / `.md` 文件路径、标题、元数据 | 导入的片段数、文档 ID |
| `search_knowledge_base` | 查询字符串、`top_k` | 最相关的文档片段（**不含向量**） |

### 在 Agent 中配置

Claude Desktop（Windows），编辑 `%APPDATA%\Claude\claude_desktop_config.json`：

```json
{
  "mcpServers": {
    "contextdock": {
      "command": "d:/05_Code/ContextDock/bin/contextdock.exe",
      "env": { "SILICONFLOW_API_KEY": "sk-xxx" }
    }
  }
}
```

改完**必须从系统托盘完全退出 Agent 再重启**——热重载无效。

> **Windows 注意**：`command` 直接指向编译好的 `.exe` 时**不需要** `cmd /c` 包装。
> 只有 `npx` / `uvx` 这类脚本才需要。

---

## 架构

```text
                    ┌─────────────────────────┐
   用户 ──────────► │  Agent (Claude/Cursor)  │
                    └───────────┬─────────────┘
                                │ MCP stdio
                                │ (JSON-RPC over stdin/stdout)
                    ┌───────────▼─────────────┐
                    │   ContextDock (Go)      │
                    │  ┌───────────────────┐  │
                    │  │ MCP 工具层         │  │
                    │  │ import_document   │  │
                    │  │ search_knowledge  │  │
                    │  └────────┬──────────┘  │
                    │  ┌────────▼──────────┐  │
                    │  │ 切分 chunk        │  │
                    │  │ 分词 tokenize     │  │
                    │  └────────┬──────────┘  │
                    │  ┌────────▼──────────┐  │
                    │  │ 检索层            │  │
                    │  │  BM25 ─┐          │  │
                    │  │  向量 ─┴► RRF 融合 │  │
                    │  └────────┬──────────┘  │
                    └───────────┼─────────────┘
                                │
              ┌─────────────────┴─────────────────┐
              ▼                                   ▼
     ┌─────────────────┐              ┌────────────────────┐
     │ SiliconFlow API │              │ PostgreSQL         │
     │ bge-m3 (Pro)    │              │ + pgvector         │
     │ d=1024          │              │ vector(1024) + HNSW│
     └─────────────────┘              └────────────────────┘
```

**读图要点**：左边是唯一的外部依赖（Embedding API），右边是自己的仓库。
中间三条横线（检索层）是这个项目真正要写的东西。

### 目录结构

```text
ContextDock/
├── cmd/contextdock/       # 程序入口，组装依赖
├── internal/
│   ├── types/             # ✅ 核心数据结构
│   ├── chunk/             # 文档切分
│   ├── tokenize/          # 中英分流分词器
│   ├── embed/             # Embedder 接口 + Fake + SiliconFlow
│   ├── retrieve/          # bm25 / vector / rrf
│   ├── store/             # 存储接口 + memory + postgres
│   └── mcp/               # MCP 工具注册
├── docs/                  # 设计与避坑文档
├── migrations/            # 建表 SQL
└── deploy/                # docker-compose
```

---

## 技术选型

| 层 | 选型 | 规格 |
| --- | --- | --- |
| 语言 | Go | 1.26 |
| 通信 | MCP stdio | 官方 `modelcontextprotocol/go-sdk` |
| 存储 | PostgreSQL + pgvector | pgvector **≥ 0.8.6** |
| 驱动 | pgx v5 + pgvector-go | |
| Embedding | 硅基流动 `Pro/BAAI/bge-m3` | **1024 维**，8K 上下文 |
| 关键词检索 | 自研内存版 BM25 | k1=1.2, b=0.75 |
| 分词 | 字符 bigram（CJK）+ 词级（拉丁） | 零依赖 |
| 混合排序 | RRF | k=60 |

### 几个关键取舍

| 选择 | 放弃了什么 | 为什么 |
| --- | --- | --- |
| **1024 维** | Qwen3 的更高 MTEB 分数 | pgvector 的 HNSW 索引对 `vector` 上限 **2000 维**，4096 维**连 halfvec 都建不了索引** |
| **bigram 分词** | 词级分词的语义精度 | `gojieba` 依赖 cgo 毁掉交叉编译，`sego` 五年无维护。bigram 零依赖且未登录词免疫 |
| **RRF 融合** | 加权求和的分数信息 | BM25 分与余弦相似度不在同一量级，加权求和需先归一化，而归一化没有标准答案 |
| **内存 + pgvector 双实现** | 一点抽象成本 | 否则写 BM25 和 benchmark 时被迫先启动数据库 |

完整的决策记录（含被否决的方案）见 **[docs/DESIGN.md](docs/DESIGN.md)**。

---

## 测试

```bash
go test ./...                              # 全部测试
go test -v ./...                           # 带用例名
go test -cover ./...                       # 覆盖率
CGO_ENABLED=1 go test -race ./...          # 竞态检测（需要 C 编译器）
go test -bench . -benchmem ./...           # 性能基准
```

**测试有效性用变异测试验证**——覆盖率高不代表测试有效。
本项目通过故意植入 bug 来确认测试会失败（7/7 被捕获）：

| 植入的 bug | 抓住它的测试 |
| --- | --- |
| 删掉 `Embedding` 的 `json:"-"` | `TestChunkJSONKeySetIsExact` |
| 删掉 RRF 的 `rank <= 0` 防护 | `TestRRFScoreIgnoresUnrecalledChannel` |
| `truncateRunes` 改成按字节截断 | `TestTruncateRunesHandlesMultiByte` |

> 这条来自一个真实教训：最初的断言用 `strings.Contains(raw, "embedding")`（小写），
> 而 Go 序列化出的是 `"Embedding"`（**大写 E**）——大小写不匹配导致断言**永不触发**，
> 删掉 `json:"-"` 测试照样绿。详见 [docs/PITFALLS.md](docs/PITFALLS.md)。

---

## 已知限制

- 只处理 **中英混合**的文本；其他语种（泰/老/高棉等无空格语言）需要词典分词，未实现
- 只支持 `.txt` / `.md` 和直接传入的文本，**不含 PDF / Word 解析**
- BM25 是**内存索引**，每次启动需从数据库重建，大文档库下启动会变慢
- 第一版**无并发写入保护**，不适合多进程同时导入
- 未做多租户隔离，单机单用户场景

---

## 文档

| 文档 | 内容 |
| --- | --- |
| [docs/DESIGN.md](docs/DESIGN.md) | 11 条关键设计决策，含被否决的替代方案 |
| [docs/PITFALLS.md](docs/PITFALLS.md) | 踩过的坑：环境 / Go 语言 / 外部 API / 数据库 |
| [Issues](https://github.com/XiaoleC05/ContextDock/issues) | 开发任务，一个 issue 一个可交付物 |

---

## License

[MIT](LICENSE) © 2026 XiaoleC05
