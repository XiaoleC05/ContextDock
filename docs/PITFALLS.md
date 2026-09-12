# 避坑清单

踩过的和即将要踩的，都记在这里。**每踩一个新坑就加一条。**

标注含义：`✅ 已验证` = 本项目实际踩过并解决 · `⚠️ 已知风险` = 已查证但还没遇到

---

## 环境

### ✅ `-race` 需要 cgo，Windows 上需要 C 编译器

```bash
# 现象
go: -race requires cgo; enable cgo by setting CGO_ENABLED=1

# 原因
Go 的竞态检测器底层是 C 代码。本机 CGO_ENABLED=0，且没有 gcc。

# 解决（winget 可用）
winget install -e --id BrechtSanders.WinLibs.POSIX.UCRT
# 重开终端后
CGO_ENABLED=1 go test -race ./...
```

**为什么必须在写并发代码之前解决**：数据竞争是最难排查的一类 bug——
可能测试全过、一上量就崩。不跑 `-race` 等于蒙眼开车。

> CI 上不需要额外配置：GitHub 的 Ubuntu runner 自带 gcc。

### ✅ git 连不上 github.com

本机 `github.com:443` 超时，但 `api.github.com` 正常。已有代理在 `127.0.0.1:7890`。

```bash
# 只对本仓库生效
git config http.proxy http://127.0.0.1:7890
git config https.proxy http://127.0.0.1:7890
```

### ✅ `git add -A` 之前先看一眼 `git status`

本项目真实事故：工具产生的 104MB 临时文件（`.tmp_research/`）被 `git add -A`
扫进了提交，而仓库还没推送过——只能改写 HEAD 清理。

```bash
git status --short    # 提交前扫一眼，成本 1 秒
```

---

## Go 语言

### ✅ `len()` 是字节数，不是字符数

```go
len("检索")                    // 6，不是 2
utf8.RuneCountInString("检索") // 2 ✅
```

**这是中文场景下最容易踩的语言级 bug。** 按 `len()` 切分中文会切出 1/3 的块大小，
按字节截断会把汉字切成半个，输出乱码。

### ✅ 判断汉字用 `unicode.Han`，不是 `Ideographic`

```go
unicode.Is(unicode.Ideographic, r)  // ❌
unicode.Is(unicode.Han, r)          // ✅
```

`Ideographic` 双向都不等价：会误收西夏文、女书、契丹小字，
又会漏掉 CJK 部首补充区（U+2E80–2EFF）。实测计数：
`Han` = 98408 个码点，`Ideographic` = 105854 个。

### ⚠️ Go 标准库没有 `unicode.Script(r)` 反查

只有 `unicode.Is(unicode.Han, r)` 这类正向判断，以及 `unicode.Scripts` 映射表（163 项）。
也没有 UAX#29 分词包（`x/text` 里没有 script 包，golang/go#14820 至今未合并）。

**影响**：需要"按书写系统分流"时，得自己写判断逻辑，不能指望标准库。

### ⚠️ 裸 map 写入会 panic

```go
c := Chunk{Content: "..."}      // Metadata 是 nil
c.Metadata["heading"] = "安装"   // panic: assignment to entry in nil map
```

本项目用 `SetMetadata()` 收口，内部自动初始化。
**不要把这个留给"记得自己初始化"的约定。**

### ⚠️ 并发写 map 会 panic

M4 做并行检索时会踩到。合并两路结果时：先各自收集到切片，再合并。
不要两路 goroutine 直接往同一个 map 里写。

### ⚠️ `omitempty` 对 struct 永远无效

`encoding/json` 的 `isEmptyValue` 只处理 Array / Map / Slice / String / Bool /
数字 / Interface / Pointer。**struct 永远不算空。**

所以 `time.Time` 加 `omitempty` 照样会输出 `"0001-01-01T00:00:00Z"`。
要真的可省，得用 `*time.Time`。

### ⚠️ `json:"-"` 挡不住 `%v` / `%+v`

两套机制。`json:"-"` 只管 `encoding/json`，日志打印照样会把 1024 个浮点数倒出来
（单条约 8KB 纯数字）。本项目给 `Chunk` 加了 `String()` 方法解决。

---

## 外部 API（硅基流动）

| 坑 | 说明 |
| --- | --- |
| ✅ **绝对不能传 `dimensions`** | 仅对 Qwen3 系列生效，对 bge-m3 传会 **400**，不是静默忽略 |
| ✅ **实测维度** | `Pro/BAAI/bge-m3` = **1024**；`Qwen/Qwen3-Embedding-8B` = 4096 |
| ⚠️ 单次 input 文档写的是 **32 条** | 超一条**整个请求** 400（`input batch size N > maximum allowed batch size 32`），不是截断，必须客户端分块 |
| ✅ **但 32 条实际上没有强制执行** | 2026-09-12 实测单次 **256 条**正常返回 200。仍然按 32 分批——文档写的就是 32，别的账号或模型可能真会拒。保守分块只慢一点，撞上限是整个请求失败 |
| ⚠️ 单条最长 **8192 token** | bge-m3 的上限（Qwen3 是 32768，别记混） |
| ⚠️ 限流是**账户级、按模型**算 | 不是按 API Key。L0 是 RPM 2000 / TPM 1,000,000 |
| ⚠️ `429` / `503` / `504` 可重试 | `400` / `401` / `403` **不可重试** |
| ⚠️ `403` 常见原因是**未实名认证** | 不是鉴权失败，容易误判 |
| ⚠️ `usage.completion_tokens` 是非标准字段 | 硅基流动特有。别开 `DisallowUnknownFields()`，否则会报错 |
| ⚠️ 官方未文档化 `Retry-After` 头 | 自己实现指数退避 + 抖动，别依赖它 |
| ⚠️ 长文本 embedding 耗时较长 | http client 超时别设太短（建议 60s+） |
| ⚠️ 文档站已迁移 | 老的 `docs.siliconflow.cn/en/...` 路径全部 404，现在是 `docs.siliconflow.com` |
| ⚠️ GitHub 上的官方 `openapi.yaml` 已过期 | 还停留在只有 bge 系列、没有 `dimensions` 字段的版本，别拿它当依据 |

### ✅ Windows 上 `curl.exe` 会把中文参数搞坏

**排查了一个多小时，最后发现 API 一直是好的，是命令行工具的问题。**

同一个端点、同一个 Key、同样的中文内容：

```bash
# ❌ 报 20015 "The parameter is invalid"
curl -d '{"model":"Pro/BAAI/bge-m3","input":["测试"]}' https://api.siliconflow.cn/v1/embeddings

# ✅ 正常返回
curl -d @req.json https://api.siliconflow.cn/v1/embeddings
```

**原因**：`curl.exe` 是 C 程序，在 Windows 上从命令行读参数走的是 **ANSI 代码页**（中文系统上是 GBK），
不是 UTF-8。中文被转成 GBK 字节后，服务端按 UTF-8 解码就是非法字符。

**怎么识别**：

- 错误码是**参数无效**（20015），而不是鉴权或模型不存在
- **换英文内容就正常** —— 这是最关键的信号
- `echo '测试' | xxd` 会显示正确的 UTF-8，但那只证明 **bash** 没问题，**不代表 curl 发出的字节是对的**

**规避**：非 ASCII 内容一律用 `-d @file` 或 `--data-binary @file`。

> 这跟本项目自己的代码**无关**——Go 的 `encoding/json` 序列化出来就是标准 UTF-8。
> 只有手工用 curl 做验证时才会踩到。

---

## PostgreSQL / pgvector

| 坑 | 说明 |
| --- | --- |
| ✅ 镜像 pin **≥ 0.8.6** | 修复 CVE-2026-3172（并行建索引堆溢出，CVSS 8.1） |
| ⚠️ HNSW 索引上限 **2000 维**（vector） | 4096 维**连 halfvec（4000）都建不了** |
| ⚠️ `CREATE EXTENSION` 放 init 脚本 | 放 `AfterConnect` 会每条新连接执行一次 DDL，且需要 CREATE 权限 |
| ⚠️ init 脚本只在数据目录为空时执行 | 数据卷已存在时，新增脚本不会生效 |
| ⚠️ **先插数据、后建索引** | 已有 HNSW 索引时插入 100 万条 256 维约 1 小时；无索引只要 45 秒 |
| ⚠️ `ORDER BY` 必须与索引定义**逐字一致** | 写错维度会**静默**退化成全表扫描 |
| ⚠️ HNSW 是近似索引 | 带 `WHERE` 过滤时返回行数可能少于 `LIMIT`，需要 `SET hnsw.iterative_scan` |
| ⚠️ `hnsw.ef_search` 默认 40 偏低 | 建议 100 起步，用真实查询集测 recall@10 后定。用 `SET LOCAL`，别用 `SET`（会污染连接池里复用的连接） |
| ⚠️ **规划器用不用 HNSW 是「非单调」的** | 实测：1000 片段→顺序扫描，**5000→走索引**，20000→又回顺序扫描。所以「表越大越会用索引」是错的，也**别在某个固定 N 上断言"必须走索引"**——那是个会随 pgvector 版本和统计信息漂移的脆弱断言。要断言就断言「禁掉顺序扫描后索引可用」（`SET LOCAL enable_seqscan = off`），那才抓得到算子写错 |
| ⚠️ `SET LOCAL` 要求有事务 | 连接池下必须 `BEGIN; SET LOCAL …; SELECT …; COMMIT;`。直接用 `pool.Exec("SET …")` 会落在**随机一条**连接上，而且因为不是 `LOCAL` 还会留在池里污染后续查询 |
| ⚠️ 近邻查询**必须**把 `metadata` 一起 select | 评测的命中判定读 `Metadata["source"]`。漏了它，**所有 recall 会静默归零**，而报告照样打印出一张像模像样的表 |
| ⚠️ 近邻查询**不要** select `embedding` | 它是全表最贵的一列（1024 维转 text 有十几 KB），而检索下游没有任何地方读 `Chunk.Embedding`。带上它等于把想省的内存和带宽原样搬回来 |
| ⚠️ `<=>` 返回的是**距离**不是相似度 | 填进 `Score`/`VectorScore` 前要 `1 - distance`。填反了**排序不变**（SQL 里已经排好），融合结果照常正确，只有暴露给 Agent 的 `vector_score` 是错的——而 RRF 不看分数，现有测试一个都抓不到 |
| ⚠️ 建索引消耗大量内存 | 受 `maintenance_work_mem` 限制，默认值偏小会退化成磁盘构建，**慢 10–50 倍** |

### pgx 专属

| 坑 | 说明 |
| --- | --- |
| ⚠️ `CopyFrom` **强制 binary format** | 忘记注册 pgvector 类型会报 **极具误导性**的 `vector cannot have more than 16000 dimensions`（维度其实完全正确） |
| ⚠️ `CopyFrom` 不支持 `ON CONFLICT` 和 `RETURNING` | 需要 upsert 时退回 batch INSERT。这个限制无法绕过 |
| ⚠️ `CopyFrom` 表名参数是 `pgx.Identifier` | **不是 `string`**，写成字符串字面量编译不过 |
| ⚠️ `CopyFrom` 的值必须包成 `pgvector.NewVector(...)` | 直接传 `[]float32` 会失败 |
| ⚠️ 连接池默认 `MaxConns = max(4, NumCPU)` | 多核机器上会开到几十条，单机 MCP server 应显式收窄到 8~16 |

---

## 工程习惯

- ✅ **`.env` 必须在第一个 commit 之前**就写进 `.gitignore`
  ——不然 API Key 可能已经进过一次 commit，清理历史很麻烦
- ✅ **写测试要验证测试本身有效**——本项目真实事故：
  `TestChunkEmbeddingIsNotSerialized` 用 `strings.Contains(raw, "embedding")`（小写）断言，
  而 Go 在没有 json 标签时序列化出的键是 `"Embedding"`（**大写 E**）。
  大小写不匹配导致**断言永远不触发**，删掉 `json:"-"` 测试照样绿。
  **"有测试"和"测试有效"是两件事**——用变异测试验证
- ⚠️ **提交信息用 `closes #N`**，GitHub 会自动关闭 issue 并更新里程碑进度

### ✅ 声明 ≠ 赋值：DTO 上的字段可以是个空壳

**本项目真实缺陷。** `ResultItem.Source` 长这样：

```go
Source string `json:"source,omitempty" jsonschema:"所属文档的来源"`
```

声明了，`jsonschema` 描述也写了——Agent 在 `tools/list` 里看得见它，
工具的 schema 里明明白白写着一行"所属文档的来源"。

但 `toResultItem()` 是**唯一**的构造点，它**从不给这个字段赋值**。
根因更隐蔽：`Source` 挂在 `Document` 上，而 `SearchResult` 内嵌的是 `Chunk`
——**检索路径里根本拿不到它**。所以这个字段从写下的第一天起就永远为空。

**为什么测试全绿也没发现**：`server_test.go` 全文**零次**提到 `Source`。
DTO 有字段、schema 有条目，看起来"实现了"，实际上是个空壳。
**覆盖率再高也覆盖不到一个没人断言过的字段。**

**同类信号**：一个字段如果"声明了但没有任何测试碰过"，大概率不是它太简单，
而是它**根本没接上**。

> 顺带一提：这类字段危害比"少个功能"更大——Agent 会**按 schema 做计划**。
> 它在 schema 里看到有 `source`，可能会说"我先按来源过滤一下"，
> 然后拿到一堆空值，还不知道是自己用错了还是服务坏了。

### ⚠️ 变异测试是**按包**跑的，跨包的测试抓不到

跑 `go run ./cmd/mutate` 时，某个变异报 `[MISSED]`，
第一反应是"测试是假的"。但本项目这次的真实原因是**测试放错了包**：

脚本跑的是 `go test ./<被变异的文件所在包>/...`。
一条变异改的是 `internal/ingest/ingest.go`，而唯一覆盖它的测试写在
`internal/mcp/` 里——**根本没被执行**。

**判断方法**：看到 `MISSED` 先别急着补断言，确认那个包自己有没有测试。
没有的话，补在**被变异文件所在的包**里，而不是它的调用方。
（本次补完之后 38/38 全中。）

### ✅ 反范式字段只对**新写入**的数据生效

给片段加了 `metadata["source"]` 之后，新导入的文档立刻带上了，
但**修复之前入库的片段仍然是空的**——因为那是一次**数据**变更，
不是一次**代码**变更。代码改完只影响未来的写入。

**所以凡是"往已有结构里补一个新字段"，都要同时问一句：存量数据怎么办？**

本项目补了 `migrations/002_backfill_chunk_source.sql`：

```sql
UPDATE chunk AS c
SET metadata = coalesce(c.metadata, '{}'::jsonb)
               || jsonb_build_object('source', d.source)
FROM document AS d
WHERE c.document_id = d.id
  AND d.source <> ''
  AND (c.metadata ->> 'source') IS DISTINCT FROM d.source;   -- ← 幂等靠这句
```

⚠️ **init 脚本不会自动跑它**：`/docker-entrypoint-initdb.d/` 里的东西
只在数据目录为空时执行一次，数据卷已存在时新增脚本不生效（同 `001` 那条）。

> 端到端测试正是在这里立功的：单元测试全绿，一跑真实链路就发现
> 5 条结果里只有 1 条带 `source`——**假绿**。
