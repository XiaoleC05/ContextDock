# 性能基准

记录各阶段的实测数字，供优化前后对比。

**没有基线的优化等于没有优化**——数字不说清"从多少降到多少"，
就只能说"我觉得快了"。

## 测试环境

| 项 | 值 |
| --- | --- |
| CPU | Intel Core i7-14700KF |
| OS | Windows 11 |
| Go | 1.26.4 windows/amd64 |
| 数据集 | 1000 条片段（中英混合，见 `bench_test.go` 的 `benchChunks`） |
| topK | 10 |

复现命令：

```bash
go test -bench . -benchmem -benchtime 300x ./internal/retrieve/
```

---

## 基线（M3 · 2026-09-11）

| 基准 | ns/op | B/op | allocs/op |
| --- | ---: | ---: | ---: |
| `BenchmarkBM25Index` | 2,619,198 | 2,889,290 | 35,036 |
| `BenchmarkBM25Search` | 283,735 | 120,338 | 2,245 |
| `BenchmarkVectorIndex` | 386,590 | 118,784 | 3 |
| `BenchmarkVectorSearch` | 707,272 | 155,736 | 8,900 |
| `BenchmarkFuseRRF` | 18,514 | 33,008 | 258 |
| `BenchmarkHybridSearch` | 743,249 | 320,390 | 13,194 |
| `BenchmarkTokenize` | 234,632 | 273,273 | 3,510 |

---

## 优化（M7 · 2026-09-12）

### 先用 pprof 找热点，不猜

```bash
go test -bench . -benchtime 300x -cpuprofile=cpu.prof ./internal/retrieve/
go tool pprof -top -nodecount=20 cpu.prof
```

火焰图的前几名：

| 热点 | 占比 | 位置 |
| --- | ---: | --- |
| `tokenize.Tokenize` | 24.4% | 分词（字符串转换与 map 操作密集） |
| `VectorIndex.Search` | 20.0% | 向量检索 |
| `dot`（内联） | 10.7% | 余弦相似度内层循环 |
| `norm` | 4.9% | 模长计算 |
| **`tokenCount`** | **6.3%** | **每次查询都遍历词频 map 数 token 数** |

### 改了什么

`tokenCount` 那条是**设计缺陷，不是调优**：

```go
// 之前：每次查询、对每篇文档，重新遍历词频 map 数一遍 token
dl := float64(tokenCount(tf))                                  // O(词数)
score += ... / (f + m.k1*(1 - m.b + m.b*dl/m.avgdl))          // 每词项一次除法
```

这两个值**只和文档有关、和查询无关**，却在查询热路径上被反复计算。
改成索引时算好：

```go
// 现在：Index 时算一次，查询时直接取
norm := m.normCache[i]                                          // O(1)
score += ... / (f + norm)
```

### 结果

| 基准 | 优化前 | 优化后 | 变化 |
| --- | ---: | ---: | ---: |
| **`BenchmarkBM25Search`** | **283,735** | **114,114** | **−60%（2.49×）** |
| `BenchmarkBM25Index` | 2,619,198 | 2,665,346 | +1.8%（噪声） |
| `BenchmarkVectorIndex` | 386,590 | 380,210 | −1.6%（噪声） |
| `BenchmarkVectorSearch` | 707,272 | 704,900 | −0.3%（噪声） |
| `BenchmarkFuseRRF` | 18,514 | 17,526 | −5.3%（噪声） |
| `BenchmarkHybridSearch` | 743,249 | 752,820 | +1.3%（噪声） |
| `BenchmarkTokenize` | 234,632 | 361,582 | **+54%（见下）** |

> ⚠️ **关于测量噪声**：`BenchmarkTokenize` 的两次测量差了 54%，
> 而它的代码**一行都没改**。这说明本机 benchmark 的噪声不小
> （CPU 睿频、后台负载、`-benchtime` 差异都会影响）。
>
> 所以只有**远大于噪声**的变化才算数。BM25Search 的 2.49× 是可信的；
> 其余几个百分之几的差异**不要当成结论**。
>
> 要更严格的数字，应该用固定频率、多次取中位数，或者干脆在 CI 上跑。

### 为什么 `BM25Index` 没有变快反而略慢

新增了一趟循环（算 `avgdl` 和 `normCache`）。这一趟是 O(文档数)，
相对于建词频 map 的 O(总词数) 可以忽略，`+1.8%` 在噪声范围内。
用一次性的一点索引成本，换查询路径上 2.49× 的加速，是划算的。

### 已知但**没有**优化的地方

`tokenize.Tokenize` 占了 24.4%，是现在最大的热点。没动它的原因：

- 它的开销主要来自 `strings.ToLower(string(word))` 和 `string(cjk[i:i+2])`
  这两处**必然的字符串分配**（分词器的输出契约就是 `[]string`）。
  要真正优化得改接口，而接口是被 BM25 和测试共同依赖的。
- 收益和风险不成比例：当前 1000 条片段的检索是百微秒级，
  对本地 MCP 场景（单用户、几十毫秒的容忍度）完全够用。
- **优化要有明确的收益对象**。在没有真实性能问题的场景下改接口，
  只是把复杂度换成了想象出来的速度。

如果将来文档量增长到检索变慢，这里是第一个该动的地方——
接口可以先加一个"往调用方提供的 buffer 里写"的变体，让热点路径复用切片。

---

## 小结

这次优化最大的收获不是那 2.49×，而是**火焰图纠正了我的直觉**：

我原以为瓶颈在向量计算（`dot` 看起来最显眼），
实际最大的浪费在一个 6 行的辅助函数里——它每次都重算早就该算好的东西。

**先测再改**这件事，说多少遍都不如自己踩一次。
