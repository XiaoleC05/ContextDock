# ContextDock

> 基于 Go 的混合检索 MCP 服务，为本地 Agent 提供向量 + BM25 混合召回

[![CI](https://github.com/XiaoleC05/ContextDock/actions/workflows/ci.yml/badge.svg)](https://github.com/XiaoleC05/ContextDock/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/XiaoleC05/ContextDock?label=release)](https://github.com/XiaoleC05/ContextDock/releases/latest)
[![v1.1.0](https://img.shields.io/github/milestones/progress/XiaoleC05/ContextDock/10?label=v1.1.0)](https://github.com/XiaoleC05/ContextDock/milestone/10)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

```text
   用户 ──► Agent (Claude / Cursor / VS Code)
              │  MCP stdio（JSON-RPC over stdin/stdout）
              ▼
   ┌────────────────────────────────┐
   │          ContextDock           │
   │    import_document             │
   │    search_knowledge_base       │
   │             │                  │
   │             ▼                  │
   │    BM25 ────┐                  │
   │    向量 ────┴──►  RRF 融合     │
   └───────┬────────────────┬───────┘
           ▼                ▼
   ┌─────────────┐    ┌────────────────┐
   │ SiliconFlow │    │   PostgreSQL   │
   │  bge-m3     │    │   + pgvector   │
   │  d = 1024   │    │  vector(1024)  │
   └─────────────┘    └────────────────┘
```

ContextDock 是一个**本机运行**的 MCP Server。它把文档切分成片段、生成向量、存进 PostgreSQL，
在 Agent 提问时用「关键词检索 + 向量检索 + RRF 融合」找出最相关的片段返回。

## 目录

- [这是什么](#这是什么)
- [快速开始](#快速开始)
- [关键特性](#关键特性)
- [用法](#用法)
- [配置](#配置)
- [架构](#架构)
- [技术选型](#技术选型)
- [测试](#测试)
- [已知限制](#已知限制)
- [贡献](#贡献)
- [文档](#文档)
- [License](#license)

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

## 快速开始

**前置**：Go ≥ 1.25、Docker Desktop（启用 Linux 容器模式）。

### 1. 构建

```bash
git clone https://github.com/XiaoleC05/ContextDock.git
cd ContextDock

# Windows
go build -o bin/contextdock.exe ./cmd/contextdock
# Linux / macOS
go build -o bin/contextdock ./cmd/contextdock
```

> 后面的示例统一用 Windows 的 `.exe` 路径，其他平台换成 `bin/contextdock` 即可。

### 2. 起数据库、填 API Key

```bash
docker compose -f deploy/docker-compose.yml up -d
cp .env.example .env      # 然后把 SILICONFLOW_API_KEY 填进去
```

> `.env` 已在 `.gitignore` 中。**不要在代码里硬编码 key。**
> 其余配置项见[配置](#配置)。

### 3. 接到 Agent

编辑 `%APPDATA%\Claude\claude_desktop_config.json`（Claude Desktop）：

```json
{
  "mcpServers": {
    "contextdock": {
      "command": "d:/05_Code/ContextDock/bin/contextdock.exe",
      "env": {
        "SILICONFLOW_API_KEY": "sk-xxx",
        "CONTEXTDOCK_DATABASE_URL": "postgres://postgres:postgres@localhost:5432/contextdock?sslmode=disable"
      }
    }
  }
}
```

改完**必须从系统托盘完全退出 Agent 再重启**——热重载无效。

> **Windows 注意**：`command` 直接指向编译好的 `.exe` 时**不需要** `cmd /c` 包装。
> 只有 `npx` / `uvx` 这类脚本才需要。

### 从旧版本升级

**先建表，再升级**。已经跑过 `001_init.sql` 的库需要补上后续迁移：

```bash
docker exec -i contextdock-pg psql -U postgres -d contextdock     < migrations/003_document_dedup.sql
```

> ⚠️ 它给存量文档发 `legacy:<id>` 作为去重键，**永远匹配不上新导入**。
> 也就是说存量文档重新导入一次会多出一份，需要人工清理一次——
> 这比"自动替换掉一份不确定是哪份的旧记录"安全。见 [DESIGN §13](docs/DESIGN.md)。

下面这条只对 v1.0.0 **之前**导入过文档的库有意义，全新安装跳过。

那些片段的元数据里没有 `source`，检索结果的溯源字段会是空的。跑一次回填补齐：

```bash
docker exec -i contextdock-pg psql -U postgres -d contextdock \
    < migrations/002_backfill_chunk_source.sql
```

幂等，重复执行不产生任何变化。

> ⚠️ 它**不会**被自动执行：`/docker-entrypoint-initdb.d/` 里的脚本只在
> 数据目录为空时跑一次，数据卷已存在时新增的脚本不生效。

---

## 关键特性

- **混合检索**：BM25 关键词 + 向量语义，RRF 融合（k=60）
- **中英混合分词**：按书写系统分流——CJK 走字符 bigram，拉丁字母按词切，零外部依赖
- **两套存储实现**：内存版用于测试和 benchmark，pgvector 版用于持久化——检索核心不依赖数据库
- **可测的检索核心**：`FakeEmbedder` 不访问网络，BM25 与向量检索都能在纯内存里跑测试
- **逐级降级**：嵌入接口挂了不会让检索整个失败，关键词检索仍然顶上，并在结果里标注降级原因
- **相邻片段合并**：同一处内容连着命中好几条时合并成一条，让 top-k 覆盖到更多文档
- **重复导入去重**：同一份文档导入两次会**替换**而不是新增，库里不会出现两条一样的结果
- **MCP stdio 接入**：两个工具 `import_document` / `search_knowledge_base`

---

## 用法

### MCP 工具

| 工具 | 输入 | 输出 |
| --- | --- | --- |
| `import_document` | `content` 或 `file_path`（.txt / .md）、`title`、`source` | 文档 ID、切分出的片段数、成功嵌入的片段数 |
| `search_knowledge_base` | `query`、`top_k` | 片段正文 + `heading` 面包屑 + `source` 来源 + `matched_by` 命中通道 + `score`（融合分）+ `lexical_score` / `vector_score`（原始分）+ `context_before` / `context_after` 相邻片段摘要 |

> `source` 是每个片段的**溯源标签**：传 `file_path` 时默认是文件路径，
> 直接传文本时是 `"inline"`，也可以显式指定。它让 Agent 能回答
> 「这段话出自哪份文档」，而不是只拿到一堆无出处的文本。
>
> `context_before` / `context_after` 是命中片段**前后相邻片段的一小段摘要**
> （各截断到 120 字符，前一段取尾、后一段取头）。命中片段常常只写着
> 「运行 go build」，带上前一段的尾部才知道它在讲什么。截断而不是整段带出，
> 是因为 Agent 的上下文窗口是稀缺资源，而相邻片段本来就重叠着 60 个字符。
>
> `search_knowledge_base` 的输出**不含向量**。这不是靠 `json:"-"` 标签挡的，
> 而是用了独立的 DTO——**从类型上**保证向量进不了 Agent 的上下文
> （10 条 × 1024 维 ≈ 100KB 纯数字，对 Agent 毫无用处）。

---

## 配置

全部通过环境变量，进程环境变量优先于 `.env` 文件。

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `SILICONFLOW_API_KEY` | *（必填）* | 硅基流动 API Key，[获取地址](https://cloud.siliconflow.cn/account/ak) |
| `CONTEXTDOCK_DATABASE_URL` | *（必填）* | PostgreSQL 连接串 |
| `SILICONFLOW_BASE_URL` | `https://api.siliconflow.cn/v1` | Embedding 接口地址 |
| `CONTEXTDOCK_EMBEDDING_MODEL` | `Pro/BAAI/bge-m3` | 模型名。⚠️ 换模型必须同时改维度，见下 |
| `CONTEXTDOCK_USE_MEMORY_STORE` | `false` | 置 `true` 走内存存储，**不需要数据库**（测试用） |
| `CONTEXTDOCK_TOP_K` | `10` | 检索默认返回条数 |
| `CONTEXTDOCK_SEARCH_TIMEOUT` | `5s` | 单次检索超时 |
| `CONTEXTDOCK_CHUNK_MAX_RUNES` | `400` | 单个片段的最大**字符**数（不是字节） |
| `CONTEXTDOCK_CHUNK_OVERLAP` | `60` | 相邻片段的重叠字符数 |
| `CONTEXTDOCK_POOL_MAX_CONNS` | `8` | 数据库连接池上限 |
| `CONTEXTDOCK_CONTEXT_NEIGHBORS` | `1` | 每条结果带出前后各几段相邻片段的**摘要**（0 = 关） |
| `CONTEXTDOCK_MERGE_ADJACENT` | `true` | 把同一文档里序号连续的命中合并成一条，腾出结果位给别的文档 |

> ⚠️ **换 Embedding 模型要三处同改**：模型名、`types.EmbeddingDim` 常量、建表语句的
> `vector(1024)`，然后重建表和索引、重灌全量向量。这是全项目最贵的一次改动。
> 维度不能随便调大：pgvector 的 HNSW 索引对 `vector` 类型上限 2000 维。

---

## 架构

**读图要点**（见顶部）：左边是唯一的外部依赖（Embedding API），右边是自己的仓库。
中间那条「BM25 ─ 向量 ─► RRF」是项目真正要写的东西，也是[已知限制](#已知限制)里
「向量检索目前是暴力扫描」那条的由来。

### 目录结构

```text
ContextDock/
├── cmd/
│   ├── contextdock/       # 程序入口：依赖注入的组装点
│   ├── smoke/             # 端到端冒烟测试（把二进制当 MCP server 跑）
│   └── mutate/            # 变异测试工具（验证测试本身有效）
├── internal/
│   ├── types/             # 核心数据结构（零值语义、校验）
│   ├── config/            # 配置加载与启动期校验
│   ├── tokenize/          # 按书写系统分流的分词器
│   ├── chunk/             # 文档切分（按标题分层 + 句边界）
│   ├── embed/             # Embedder 接口 + Fake + SiliconFlow
│   ├── ingest/            # 导入编排：切分 → 嵌入 → 落库
│   ├── retrieve/          # BM25 / 向量 / RRF / 并行检索
│   ├── store/             # 存储接口 + 内存实现 + pgvector 实现
│   ├── service/           # 应用门面：索引重建、降级策略
│   └── mcp/               # MCP 工具注册（薄适配器）
├── docs/                  # 设计、避坑、性能文档
├── migrations/            # 建表 SQL + 数据回填
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
go test ./...                  # 单元测试
go test -cover ./...           # 覆盖率
go test -race ./...            # 竞态检测（需要 C 编译器）
go test -bench . -benchmem ./...   # 性能基准
```

### 集成测试（需要数据库）

```bash
docker compose -f deploy/docker-compose.yml up -d
go test -v -tags=integration ./internal/store/
```

覆盖真实数据库的往返、向量读写、级联删除、事务回滚，以及**重启后从库重建索引**。

### 端到端冒烟测试

```bash
go run ./cmd/smoke bin/contextdock.exe
```

把二进制当 MCP server 跑起来，用 stdio 发 JSON-RPC——验证握手、工具发现、导入、检索、
**嵌入失败时的降级链路**，以及溯源字段过完序列化之后还在不在。
单元测试覆盖不到的接缝（比如 stdout 被日志污染）只有这里能发现。

### 测试有效性用变异测试验证

**覆盖率高不代表测试有效。** 本项目用 `cmd/mutate` 故意在源码里植入 bug，确认测试会失败：

```bash
go run ./cmd/mutate          # 全部
go run ./cmd/mutate bm25     # 只跑名字含 bm25 的
```

> 两个测试工具都是 Go 写的，`go run` 就能跑——**跑测试的链路不需要额外装 Python**。

当前 **38/38 个变异全部被捕获**。它能发现的问题很具体，例如：

| 植入的 bug | 抓住它的测试 |
| --- | --- |
| 删掉 `Embedding` 的 `json:"-"` | `TestChunkJSONKeySetIsExact` |
| RRF 去重改用 `Chunk.ID`（落库前全是 0） | `TestFuseRRFUsesStableKeyNotID` |
| RRF 直接把 0 号名次代入公式 | `TestRRFScoreIgnoresUnrecalledChannel` |
| 请求体里加上 `dimensions` 字段 | `TestRequestHasNoDimensionsField` |
| BM25 建索引时直接读 `Content` 而非 `IndexText` | `TestBM25IndexesHeadingBreadcrumb` |
| 单路检索失败就让整个查询失败 | `TestHybridDegradesWhenOnePathFails` |
| `truncateRunes` 改成按字节截断 | `TestTruncateRunesHandlesMultiByte` |
| 检索结果不填文档来源（**真实缺陷**） | `TestSearchReturnsSource` |
| 导入时不下沉文档来源 | `TestIngestSinksDocumentSourceIntoChunks` |

> 最后两条来自一个**发布前发现并修掉的真实缺陷**：`ResultItem.Source`
> 声明了、schema 里也写了描述（Agent 看得见），但构造它的函数从不赋值，
> 于是它永远是空的。**声明 ≠ 赋值**，而它当时**一条测试都没有**。
>
> 同一类教训还有一次：最初的断言用 `strings.Contains(raw, "embedding")`（小写），
> 而 Go 序列化出的是 `"Embedding"`（**大写 E**）——大小写不匹配导致断言**永不触发**，
> 删掉 `json:"-"` 测试照样绿。详见 [docs/PITFALLS.md](docs/PITFALLS.md)。

---

## 已知限制

- **没有相关性阈值，而且试过之后决定不做**：只要索引非空，检索**总会返回 top-K 条**，
  不会因为「知识库里没有相关内容」而返回空。
  实测（v1.1.0，8 条语料里根本没有答案的查询做对照）：
  - **RRF 分完全不可分**——无答案时的最高分与真正命中时的最高分**完全相同**（都是 `0.0328`），
    三个分布的 min / P50 / max 几乎重合。它是按名次算的，只要两路各凑出前十名，分数就落在同一个窄区间
  - **原始余弦分部分可分**——无答案的最高 0.5604、有答案的最低 0.5097，两段重叠。
    要让 8 条无答案查询全被识别出来（阈值 0.58），43 条有答案的查询会有 **13 条被误杀**，精确率仅 0.38

  所以工具**不过滤**，而是把 `lexical_score` / `vector_score` 原样交给 Agent 自己判断——
  它能看着内容判断，而一个固定阈值只能看到一个数。
  每次有结果时还会附一句说明，点明「score 判断不了相关性，请读 content 自己判断」。
  这句说明**不做任何断言**，所以不可能把真正的命中误判成不相关。
  好消息是工具**只返回原始片段、不生成答案**，所以不存在「硬编答案」；
  但它也不阻止 Agent 过度解读无关片段。
- 只处理 **中英混合**的文本；其他语种（泰/老/高棉等无空格语言）需要词典分词，未实现
- 只支持 `.txt` / `.md` 和直接传入的文本，**不含 PDF / Word 解析**
- **代码块和图表会被切碎**：切分按标题和句末标点断句，但**不识别围栏代码块**。
  一张 1500 字的架构 ASCII 图会被 400 字上限切成 5 片，这些片段语义为空、向量是噪声。
  另外重叠是**盲退 60 个字符**、不吸附行边界，所以硬截断出来的片段常从半行开始
- **内存检索是暴力扫描**：向量检索会对全部向量算一遍余弦相似度。千级片段是毫秒级，
  但十万级以上应当走 pgvector 的 HNSW 索引（建表时已经建好了，目前检索路径还没用它）
- **BM25 是内存索引**，每次启动从数据库全量重建，大文档库下启动会变慢
- 切分参数（400 字 / 60 字重叠）是**社区经验值**，没有用真实查询集做 recall 评测校准
- 第一版**无并发写入保护**，不适合多进程同时导入；未做多租户隔离，单机单用户场景

---

## 贡献

欢迎 issue 和 PR。

- **[开 issue](https://github.com/XiaoleC05/ContextDock/issues/new)** —— 说明复现步骤或期望行为
- **提 PR** —— 从 `main` 切分支，CI 会跑 `gofmt` / `go vet` / `go test -race` 和集成测试
- **动手改之前先看 [docs/DESIGN.md](docs/DESIGN.md)** —— 里面 12 条决策**有相互依赖**，
  改一条（比如向量维度）要连带改建表语句和全量向量

测试怎么跑见上面的[测试](#测试)一节。

---

## 文档

| 文档 | 内容 |
| --- | --- |
| [docs/DESIGN.md](docs/DESIGN.md) | 12 条关键设计决策，含被否决的替代方案 |
| [docs/PITFALLS.md](docs/PITFALLS.md) | 踩过的坑：环境 / Go 语言 / 外部 API / 数据库 |
| [docs/BENCHMARKS.md](docs/BENCHMARKS.md) | 性能基准与优化记录（含 pprof 分析） |
| [docs/EVAL.md](docs/EVAL.md) | 检索评测集格式与校验命令 |
| [Issues](https://github.com/XiaoleC05/ContextDock/issues) | 开发任务，一个 issue 一个可交付物 |

---

## License

[MIT](LICENSE) © 2026 XiaoleC05
