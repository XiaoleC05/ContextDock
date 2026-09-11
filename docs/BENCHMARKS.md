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
go test -bench . -benchmem -benchtime 200x ./internal/retrieve/
```

---

## 基线（M3 · 2026-09-11）

这是**优化前**的数字。M7 做完性能分析后会在下面追加对比。

| 基准 | ns/op | B/op | allocs/op |
| --- | ---: | ---: | ---: |
| `BenchmarkBM25Index` | 2,619,198 | 2,889,290 | 35,036 |
| `BenchmarkBM25Search` | 283,735 | 120,338 | 2,245 |
| `BenchmarkVectorIndex` | 386,590 | 118,784 | 3 |
| `BenchmarkVectorSearch` | 707,272 | 155,736 | 8,900 |
| `BenchmarkFuseRRF` | 18,514 | 33,008 | 258 |
| `BenchmarkHybridSearch` | 743,249 | 320,390 | 13,194 |
| `BenchmarkTokenize` | 234,632 | 273,273 | 3,510 |

### 初步观察（待 M7 用 pprof 验证）

- **`BM25Search` 的分配次数偏高（2245 次/op）**。每次查询都要重新分词、
  建 map。查询很短，这些分配看起来是可以避免的。
- **`VectorSearch` 有 8900 次分配**，但只有 3 次是 Index 的——
  说明分配集中在检索路径（很可能是 `sortAndTrim` 的切片增长
  和每个结果的拷贝）。
- **`BM25Index` 的 35k 次分配**符合预期：每条片段都要建一个词频 map。
  如果要优化，方向是复用 map 或改用更紧凑的表示。
- **`FuseRRF` 是最快的一环**（18.5μs），不是瓶颈——符合预期，
  它只做 map 合并和排序。

> ⚠️ 以上只是从数字推测的方向。**M7 必须用 pprof 拿到真实的火焰图再动手**，
> 否则很容易优化了一个根本不热的地方。

---

## 优化后（M7）

*待补*
