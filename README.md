# ContextDock

> 基于 Go 的混合检索 MCP 服务，为本地 Agent 提供向量 + BM25 混合召回

ContextDock 是一个**本机运行**的 MCP Server。它把文档切分成片段、生成向量、存入 PostgreSQL，
并在 Agent 提问时用「关键词检索 + 向量检索 + RRF 融合」找出最相关的片段返回。

**核心职责：为 Agent 提供准确、快速、可靠的文档检索结果。**

---

## 目录

- [这是什么](#这是什么)
- [快速开始](#快速开始)
- [架构](#架构)
- [技术选型](#技术选型)
- [目录结构](#目录结构)
- [里程碑](#里程碑)
- [关键设计决策](#关键设计决策)
- [开发避坑清单](#开发避坑清单)

---

## 这是什么

想象你有一本很厚的资料册，和一个记性很好但**没读过这本册子**的助手。
你问它问题，它只能瞎编——因为册子不在它脑子里。

ContextDock 就是一个中间人，站在助手和资料册之间，干两件事：

- **存**：你把资料给它，它把资料剪成小纸条（**切分**），给每张纸条拍一张「语义指纹」（**向量**），存进仓库。
- **取**：你提问时，它同时用两种方式找纸条，然后把两边的结果合并排序，挑最相关的几张递给你。

### 为什么要两种检索方式

因为各有各的瞎子区：

| 只用一种 | 会出的问题 |
| --- | --- |
| 只用关键词 | 你问「怎么让程序跑得快」，资料里写的是「性能优化」——一个字没对上，找不到 |
| 只用语义 | 你问「Error 5001 怎么解决」，语义检索觉得「Error 5002」也很像，把错的递给你 |

把两边结果**按名次**合并（而不是按分数加权），这一招叫 **RRF**，是 ContextDock 的核心。

### 两条数据流

导入：

```text
用户提供文档 → Agent 调用 import_document → 读取文档
    → 切分成多个片段 → 调用 Embedding API 生成向量
    → 存入 PostgreSQL（片段 + 向量 + 元数据）
```

检索：

```text
用户提问 → Agent 调用 search_knowledge_base
    → 关键词检索（BM25） ┐
    → 向量检索（余弦）   ┴→ RRF 融合 → 返回最相关的片段
```

### 第一版的范围边界

**做**：直接传入的文本、`.txt`、`.md`、基础元数据、中英混合文档。

**不做**（刻意留白，避免范围失控）：

- LLM 对话生成、Agent 编排
- PDF / Word 解析、图片或图文混合内容
- 前端页面、复杂权限系统
- Elasticsearch、分布式任务队列、多节点集群

---

## 快速开始

### 环境要求

| 组件 | 版本 | 说明 |
| --- | --- | --- |
| Go | ≥ 1.25 | 官方 MCP SDK 要求；本项目开发用 1.26.4 |
| Docker Desktop | 任意 | 需启用 Linux 容器模式 |
| Docker Compose | v2+ | 本项目开发用 v5.4.0 |
| C 编译器 | MinGW-w64 | **仅跑 `-race` 时需要**，见[开发避坑清单](#开发避坑清单) |

### 编译与测试

```bash
go build ./...
go vet ./...
gofmt -l .            # 无输出 = 格式规范
go test ./...         # 跑全部测试
go test -v ./...      # 带用例名输出
go test -cover ./...  # 看覆盖率
```

### 启动数据库

> 第五天之前不需要，现在跑了也没有代码连它。

```bash
docker compose -f deploy/docker-compose.yml up -d
```

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
中间那三条横线（检索层）是这个项目真正要写的东西。

---

## 技术选型

| 层 | 选型 | 版本/规格 |
| --- | --- | --- |
| 语言 | Go | 1.26.4 |
| 通信 | MCP stdio | 官方 `modelcontextprotocol/go-sdk` |
| 存储 | PostgreSQL + pgvector | pgvector **≥ 0.8.6** |
| 数据库驱动 | pgx | v5 + `pgvector-go` |
| Embedding | 硅基流动 `Pro/BAAI/bge-m3` | **1024 维**，8K 上下文 |
| 关键词检索 | 自研内存版 BM25 | k1=1.2, b=0.75 |
| 分词 | 字符级 bigram（中文）+ 词级（英文） | 按书写系统分流 |
| 混合排序 | RRF | k=60 |

---

## 目录结构

```text
ContextDock/
├── cmd/
│   └── contextdock/
│       └── main.go            # 程序入口，组装依赖，启动 MCP server
├── internal/
│   ├── types/                 # ✅ 核心数据结构（已完成）
│   ├── chunk/                 # 文档切分
│   ├── tokenize/              # 中文 bigram + 英文词级分词器
│   ├── embed/                 # Embedder 接口 + FakeEmbedder + SiliconFlowEmbedder
│   ├── retrieve/              # bm25 / vector / rrf
│   ├── store/                 # 存储接口 + memory 实现 + postgres 实现
│   ├── mcp/                   # MCP 工具注册
│   └── config/                # 配置读取
├── migrations/                # 建表 SQL
├── deploy/
│   └── docker-compose.yml     # PostgreSQL + pgvector
├── go.mod
└── README.md
```

**为什么用 `internal/`**：Go 的约定，这个目录下的包外部**无法 import**。
单二进制项目用它是对的，能防止以后别人误引用内部实现。

**为什么没有 `pkg/`**：那是给「要被别人 import 的库」用的，本项目是应用，不是库。

**为什么 `store/` 有两套实现**：`memory` 和 `postgres` 并存是刻意的。
第三天写 BM25 和 benchmark 时，不该被迫先启动数据库。

---

## 里程碑

七个里程碑对应七天的开发周期。每个里程碑都有明确的交付物和验收标准，
**验收不通过不进入下一个**。

### M0 · 仓库初始化 ✅ 已完成

#### 目标

把空目录变成可以安全接收代码的仓库。

#### 任务清单

- [x] `git init` + 关联远程仓库
- [x] `.gitignore`（重点：`.env` 必须在第一个 commit 之前就位）
- [x] `.gitattributes`（统一换行符为 LF）
- [x] `README.md` 骨架

#### 验收标准

`git status` 干净，`git ls-files` 只有仓库级文件。

---

### M1 · 核心类型 ✅ 已完成

#### 目标

把架构决策变成**编译期约束**。这一阶段刻意不写任何业务逻辑。

#### 任务清单

- [x] `go mod init github.com/XiaoleC05/ContextDock`
- [x] `internal/types/document.go` —— `Document`
- [x] `internal/types/chunk.go` —— `Chunk`
- [x] `internal/types/search.go` —— `Retriever` / `SearchResult`
- [x] `internal/types/types_test.go` —— 4 个测试

#### 验收结果

| 检查 | 结果 |
| --- | --- |
| `go build ./...` | ✅ |
| `go vet ./...` | ✅ |
| `gofmt -l .` | ✅ |
| `go test ./...` | ✅ 4 passed |
| `go test -race ./...` | ⏸ 延后到 M4 前（需先装 C 编译器） |

#### 核心契约

```go
// 检索的最小单位
type Chunk struct {
    ID         int64
    DocumentID int64
    Ordinal    int      // 在文档中的序号，0-based
    Content    string
    Embedding  []float32 `json:"-"`  // 关键：永不序列化
    Metadata   map[string]string
}

// 一条检索结果
type SearchResult struct {
    Chunk       Chunk
    Score       float64    // 含义随阶段变化，见"关键设计决策"
    LexicalRank int        // 1-based，0 = 该通道未召回
    VectorRank  int
}
```

---

### M2 · 切分与 Embedding

#### 目标

文档能进去，向量能出来。这是导入流程的前半段。

**为什么先做切分**：它是纯函数、零依赖、最好写测试，而且**切分质量直接决定检索质量**。

#### 交付物

- [ ] `internal/chunk/chunker.go` —— 切分器
- [ ] `internal/chunk/chunker_test.go` —— 单测
- [ ] `internal/tokenize/tokenizer.go` —— 中英分流分词器
- [ ] `internal/tokenize/tokenizer_test.go` —— 单测
- [ ] `internal/embed/embedder.go` —— `Embedder` 接口
- [ ] `internal/embed/fake.go` —— `FakeEmbedder`（不访问网络）
- [ ] `internal/embed/siliconflow.go` —— `SiliconFlowEmbedder`（真实调用）

#### 接口契约

```go
type Embedder interface {
    Embed(ctx context.Context, texts []string) ([][]float32, error)
}
```

#### 切分参数

| 参数 | 值 | 理由 |
| --- | --- | --- |
| chunk size | **400 字** | 中文 RAG 的经验值区间是 200–500 字 |
| overlap | **10%–20%**（约 60 字） | 防止答案正好被切断在两个 chunk 的接缝处 |
| 长度计量 | `utf8.RuneCountInString` | ⚠️ `len()` 是**字节数**，一个汉字占 3 字节 |

#### 分词规则

按书写系统分流，而不是按语言：

```text
输入:  使用 pgvector 做向量检索
        └┬─┘ └───┬───┘ └────┬────┘
      CJK段    ASCII段    CJK段
输出: [使用]  [pgvector]  [做向, 向量, 量检, 检索]
```

- Han / 平假名 / 片假名 → 逐字 bigram
- 拉丁字母 / 数字 → 按空格和标点切词，转小写
- CJK 段长度为 1 时必须回吐 unigram（否则单字查询永远召回不到）

#### 验收标准

- [ ] `go test ./internal/chunk/ ./internal/tokenize/ ./internal/embed/` 全绿
- [ ] 切分测试覆盖：中英混合、超长段落、空文档、Markdown 标题
- [ ] `FakeEmbedder` 对同一输入**永远返回相同向量**（确定性，否则测试无法断言）
- [ ] `SiliconFlowEmbedder` 有单测（用 `httptest.Server` 造假服务器，不真调 API）

#### 测试方法

```bash
go test -v ./internal/chunk/
go test -v ./internal/tokenize/
go test -v ./internal/embed/
```

真实 API 连通性验证（需要 API Key，只跑一次）：

```bash
# 把 key 写进 .env（已在 .gitignore 中）
curl -X POST https://api.siliconflow.cn/v1/embeddings \
  -H "Authorization: Bearer $SILICONFLOW_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"Pro/BAAI/bge-m3","input":["测试文本"],"encoding_format":"float"}' \
  | jq '{model, dim: (.data[0].embedding|length)}'
# 期望 dim: 1024
```

---

### M3 · 内存检索

#### 目标

两路检索都能跑，且**不需要数据库**。

#### 交付物

- [ ] `internal/retrieve/bm25.go` —— 内存版 BM25
- [ ] `internal/retrieve/vector.go` —— 内存版向量检索 + 余弦相似度
- [ ] `internal/retrieve/*_test.go` —— 单测
- [ ] `internal/retrieve/bench_test.go` —— benchmark

#### BM25 公式

必须用 Lucene 变体：

```text
score(D, Q) = Σ IDF(qᵢ) · f(qᵢ,D)·(k1+1) / (f(qᵢ,D) + k1·(1 - b + b·|D|/avgdl))

IDF(qᵢ) = ln(1 + (N - n + 0.5) / (n + 0.5))
```

| 符号 | 含义 | 取值 |
| --- | --- | --- |
| k1 | 词频饱和系数 | **1.2** |
| b | 长度归一化强度 | **0.75** |
| N | 总文档数 | 全局统计量 |
| n | 含该词的文档数 | 全局统计量 |
| avgdl | 平均文档长度（**token 数**） | 全局算一次 |

> ⚠️ **IDF 必须用 Lucene 变体**（`ln(1 + ...)`）。
> Robertson 原始版 `ln((N-n+0.5)/(n+0.5))` 在 `n > N/2` 时会变**负数**，导致排序错乱。

#### 余弦相似度注意事项

- 入库前做 L2 归一化
- 预先算好每个向量的模长，避免每次查询重复计算
- 浮点数比较不要用 `==`

#### 验收标准

- [ ] BM25 测试：TF 饱和、IDF 随文档频率下降、长度归一化生效
- [ ] 余弦相似度测试：正交向量=0、同向=1、反向=-1
- [ ] 边界：空查询、空文档集、查询词不在任何文档中
- [ ] `go test -bench . -benchmem ./internal/retrieve/` 有输出

#### 测试方法

```bash
go test -v ./internal/retrieve/
go test -bench . -benchmem ./internal/retrieve/
```

---

### M4 · 混合排序与并发

#### 目标

两路结果合并，且并行执行。这是整个项目的技术核心。

#### 前置条件

- [ ] **装好 C 编译器**，`CGO_ENABLED=1 go test -race ./...` 能跑通

#### 交付物

- [ ] `internal/retrieve/rrf.go` —— RRF 融合
- [ ] `internal/retrieve/hybrid.go` —— 并行执行两路检索
- [ ] 超时与错误处理

#### RRF 公式

```text
RRF(d) = Σ 1 / (k + rankᵢ(d))

k = 60（论文推荐值，Elasticsearch 的 rank_constant 默认也是 60）
```

为什么选 RRF 而不是加权求和：BM25 分和余弦相似度**不在同一个数量级**——
BM25 可能是 0~20，余弦是 -1~1。加权求和必须先做归一化，而归一化本身
就是个没有标准答案的问题。RRF **只用名次，不用分数**，天然绕开这件事。

> 参考：RAGFlow 用的是 weighted_sum（默认 `"0.7,0.3"`），因为它需要分数归一化。
> 两种方案都能用，**能说清取舍**才是重点。

#### 并发要求

- [ ] 两路检索用 `errgroup` 或 `sync.WaitGroup` 并行
- [ ] 每路检索都接收 `ctx`，且**检查 `ctx.Done()`**
- [ ] 单路失败不应该让整个查询失败（降级：只返回另一路的结果）
- [ ] 结果合并时注意**并发写 map 会 panic**

#### 验收标准

- [ ] `CGO_ENABLED=1 go test -race ./...` **全绿**
- [ ] RRF 测试：单路命中、两路都命中、名次相同、空结果
- [ ] **tie-break 必须稳定**（同分时按 docID 排序），否则两次查询结果顺序会抖动
- [ ] 超时测试：注入慢检索，验证 deadline 生效
- [ ] 降级测试：一路返回 error，另一路仍能出结果

#### 测试方法

```bash
CGO_ENABLED=1 go test -race -v ./internal/retrieve/

# 重复跑，更容易暴露竞态
go test -run TestHybrid -race -count=10 ./internal/retrieve/
```

---

### M5 · PostgreSQL 持久化

#### 目标

数据落盘，重启不丢。

#### 交付物

- [ ] `deploy/docker-compose.yml` —— pgvector 容器
- [ ] `migrations/001_init.sql` —— 建表
- [ ] `internal/store/store.go` —— 存储接口
- [ ] `internal/store/memory.go` —— 内存实现
- [ ] `internal/store/postgres.go` —— pgvector 实现
- [ ] `internal/store/postgres_test.go` —— 集成测试

#### 建表 SQL 要点

```sql
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE chunk (
    id          bigserial PRIMARY KEY,
    document_id bigint NOT NULL REFERENCES document(id) ON DELETE CASCADE,
    ordinal     int    NOT NULL,
    content     text   NOT NULL,
    embedding   vector(1024),          -- 必须是 1024，不能是 4096
    metadata    jsonb,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX chunk_embedding_hnsw ON chunk
    USING hnsw (embedding vector_cosine_ops)
    WITH (m = 16, ef_construction = 64);
```

#### 必须先做对的三件事

1. **镜像 pin `>= 0.8.6`** —— 修复了 CVE-2026-3172（并行建索引堆溢出，CVSS 8.1）
2. **`CREATE EXTENSION` 放在 init 脚本里**，不要放 `AfterConnect`
   —— `docker-entrypoint-initdb.d/` 只在数据目录为空时执行一次；
   而 `AfterConnect` 会对每条新连接执行一次 DDL，且要求连接用户有 CREATE 权限
3. **批量导入时先插数据、后建索引** —— 已有 HNSW 索引时插入 100 万条要约 1 小时，
   无索引只要 45 秒

#### pgx 注意事项

- 用 `pgvector-go` 的 `pgxvec.RegisterTypes(ctx, conn)` 挂在 `AfterConnect` 上
- `CopyFrom` **强制 binary format**，忘记注册类型会报极具误导性的
  `vector cannot have more than 16000 dimensions`（维度其实完全正确）
- `CopyFrom` **不支持 `ON CONFLICT` 和 `RETURNING`**，需要 upsert 时要退回 batch INSERT
- `CopyFrom` 的表名参数是 `pgx.Identifier` 类型，不是 `string`
- 连接池在单机 MCP server 场景显式收窄到 `MaxConns = 8~16`
  （默认值是 `max(4, NumCPU)`，多核机器上会开到几十条）

#### 验收标准

- [ ] `docker compose up -d` 后 `SELECT extversion FROM pg_extension WHERE extname='vector'` 返回 ≥ 0.8.6
- [ ] **服务重启后数据仍在**（这是本阶段的核心验收点）
- [ ] 集成测试能在容器起停后重复运行
- [ ] 带元数据过滤 + 向量排序的 SQL 能正确返回

#### 测试方法

```bash
docker compose -f deploy/docker-compose.yml up -d
go test -v -tags=integration ./internal/store/

# 重启后重跑，验证数据没丢
docker compose -f deploy/docker-compose.yml restart
go test -v -tags=integration ./internal/store/
```

---

### M6 · MCP 接入

#### 目标

能被本机 Agent 调用。

#### 交付物

- [ ] `cmd/contextdock/main.go` —— 入口
- [ ] `internal/mcp/server.go` —— 工具注册
- [ ] `internal/mcp/import.go` —— `import_document`
- [ ] `internal/mcp/search.go` —— `search_knowledge_base`

#### 两个工具

| 工具 | 输入 | 输出 |
| --- | --- | --- |
| `import_document` | 文本内容 或 文件路径、标题、元数据 | 导入的片段数、文档 ID |
| `search_knowledge_base` | 查询字符串、top_k | 最相关的片段列表（**不含向量**） |

#### 最大的坑：stdio 模式下 stdout 是协议通道

MCP 官方 SDK 直接把 `os.Stdout` 包成 JSON-RPC 通信管道。
**一句 `fmt.Println` 就会污染协议流**，而且客户端只会报一个看不懂的解析错误。

```go
// ❌ 绝对不要
fmt.Println("imported:", n)

// ✅ 日志走 stderr
log.Printf("imported: %d", n)     // Go 的 log 包默认写 stderr
```

#### 验收标准

- [ ] Agent 能发现这两个工具
- [ ] `import_document` 导入后，`search_knowledge_base` 能搜到
- [ ] 返回的 JSON 里**不含 embedding 字段**（`json:"-"` 生效）
- [ ] 错误路径：文件不存在、API Key 无效、数据库断连，都要返回可读的错误而不是崩溃

#### 配置示例

Claude Desktop，Windows：

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

---

### M7 · 性能与文档

#### 目标

把项目变成能讲的简历素材。

#### 交付物

- [ ] benchmark 结果（BM25 / 向量检索 / RRF 各自耗时）
- [ ] pprof 分析报告（CPU + 内存）
- [ ] 完整的 README
- [ ] 简历项目描述

#### 任务清单

- [ ] `go test -bench . -benchmem` 建立性能基线
- [ ] `go test -cpuprofile=cpu.prof -bench .` 然后 `go tool pprof -http=:8080 cpu.prof`
- [ ] 找出至少一个真实的性能瓶颈并优化，**记录优化前后的数字**
- [ ] 补全 README 的架构图和快速开始
- [ ] 整理面试讲解稿

#### 验收标准

- [ ] 能说清 pprof 火焰图里每一层在干什么
- [ ] 至少有一处「优化前 X → 优化后 Y」的实测对比
- [ ] 简历描述里每个技术名词都能被追问三层

---

## 关键设计决策

这一节记录**为什么这么选**，避免以后反复讨论。

### 向量维度必须是 1024，不能是 4096

bge-m3 原生输出就是 1024 维，**不做任何截断**。

这个约束来自 pgvector：HNSW/IVFFlat 索引对 `vector` 类型上限 **2000 维**，
`halfvec` 上限 **4000 维**。如果用 Qwen3-Embedding-8B 的 4096 维，
**连 `halfvec` 都建不了索引**（4000 < 4096，差 96 维），只能退化成全表顺序扫描。

> 换维度 = 重建表和索引 + 重灌全量向量。这是**整个项目最贵的一次改动**，所以一开始就定死。

### 绝不能给 bge-m3 传 `dimensions` 参数

硅基流动的 `dimensions` 参数**仅对 Qwen/Qwen3 系列生效**。
对 bge-m3 传会返回 **400 错误**，不是静默忽略。

```go
// ❌ 会 400
Dimensions: openai.Int64(1024),

// ✅ 删掉
```

### 中文分词用字符 bigram

中文没有空格，`"检索系统"` 对 BM25 来说是一个词——直接做等于失效。

bigram 把 `检索系统` 切成 `检索` / `索系` / `系统`：

- 零依赖、纯 Go、约 20 行
- 中文「未登录词」（词典里没有的新词）问题天然消失
- **bleve 的 cjk 分析器内部就是这么做的**，有生产级方案背书
- RAGFlow 自研的 `token_similarity()` 同样用 bigram 且权重更高（unigram 0.4 / bigram 0.6）

> ⚠️ **bigram 不是万能方案**。它只适用于 Han / 平假名 / 片假名。
> 泰文、老挝文、高棉文、缅文虽然也没有空格，但字母本身没有语义，
> 必须用词典分词或 LSTM（Lucene 的 `CJKBigramFilter` 明确跳过这些文种）。
> 本项目第一版**只处理中英混合**，不涉及这些。

### `Chunk.Embedding` 打 `json:"-"`

MCP 工具返回搜索结果时，如果向量被序列化：

```text
10 条结果 × 1024 维 = 10240 个浮点数 ≈ 100KB 纯数字
```

而 Agent 拿到这些数字**毫无用处**——它要的只是 `Content`。
`json:"-"` 从根上堵住这个问题。

### `MatchedBy()` 是算出来的，不是存出来的

```go
func (r SearchResult) MatchedBy() []Retriever   // 由 Rank 字段计算
```

新手容易顺手加一个 `MatchedBy []Retriever` 字段，但那是**冗余状态**：
它和 `LexicalRank > 0`、`VectorRank > 0` 说的是同一件事，存两份就有不同步的可能。

> **原则：能用已有字段算出来的，就不要存第二份。**

### `SearchResult.Score` 的含义随阶段变化

| 阶段 | Score 的含义 | 典型范围 |
| --- | --- | --- |
| 单路检索 | BM25 分 或 余弦相似度 | 0~20 / -1~1 |
| RRF 融合后 | RRF 分 | 0.01~0.03 |

**不要在代码里假定 Score 有固定取值范围。**
需要在同一处比较时，永远比较同阶段的分数。

---

## 开发避坑清单

踩过的和即将要踩的，都记在这里。

### 环境

#### `-race` 需要 cgo，Windows 上需要 C 编译器

```bash
# 现象
go: -race requires cgo; enable cgo by setting CGO_ENABLED=1

# 解决（winget 可用）
winget install -e --id BrechtSanders.WinLibs.POSIX.UCRT

# 重开终端后
CGO_ENABLED=1 go test -race ./...
```

**第四天之前必须装好**——那天要写并行检索，不跑 `-race` 等于蒙眼开车。

### Go 语言

#### `len()` 是字节数，不是字符数

```go
len("检索")                    // 6，不是 2
utf8.RuneCountInString("检索") // 2 ✅
```

中文分块按 `len()` 计长度会切出 1/3 的块大小。

#### 判断汉字用 `unicode.Han`，不是 `Ideographic`

```go
unicode.Is(unicode.Ideographic, r)  // ❌ 会误收西夏文、女书，还会漏掉 CJK 部首补充区
unicode.Is(unicode.Han, r)          // ✅
```

#### 并发写 map 会 panic

M4 做并行检索时会踩到。合并两路结果时要么加锁，要么先收集到各自的切片再合并。

### 外部 API

| 坑 | 说明 |
| --- | --- |
| 单次 input 最多 **32 条** | 超一条整个请求 400，不是截断，必须客户端分块 |
| 单条最长 **8192 token** | bge-m3 的上限（Qwen3 是 32768，别记混） |
| 限流是**账户级、按模型**算 | 不是按 API Key；L0 是 RPM 2000 / TPM 1,000,000 |
| `429` / `503` 可重试，`400/401/403` 不可 | 403 常见原因是**未实名认证**，不是鉴权失败 |
| `usage.completion_tokens` 非标准字段 | 别开 `DisallowUnknownFields()` |

### 数据库

| 坑 | 说明 |
| --- | --- |
| 镜像 pin **≥ 0.8.6** | 修复 CVE-2026-3172（并行建索引堆溢出） |
| `CREATE EXTENSION` 放 init 脚本 | `AfterConnect` 里做会每条连接执行一次 DDL |
| 先插数据、后建索引 | 已有索引时插入慢 80 倍 |
| `ORDER BY` 必须与索引定义逐字一致 | 写错维度会**静默**退化成全表扫描 |
| HNSW 是近似索引 | 带 `WHERE` 过滤时返回行数可能少于 `LIMIT`，需 `SET hnsw.iterative_scan` |

### 工程习惯

- **`git add -A` 之前先看一眼 `git status`**
  （曾经有一次工具产生的 104MB 临时文件被扫进提交，需要改写历史清理）
- `.env` 必须在**第一个 commit 之前**就写进 `.gitignore`

---

## 状态

🚧 开发中 · 当前进度 **M1 完成 / M7**

| 里程碑 | 状态 |
| --- | --- |
| M0 仓库初始化 | ✅ |
| M1 核心类型 | ✅ |
| M2 切分与 Embedding | ⬜ |
| M3 内存检索 | ⬜ |
| M4 混合排序与并发 | ⬜ |
| M5 PostgreSQL 持久化 | ⬜ |
| M6 MCP 接入 | ⬜ |
| M7 性能与文档 | ⬜ |
