# 评测语料来源与许可

本目录下的 `.md` 文件是**评测语料**，全部内容冻结在版本库里，评测时不从网络拉取。
清单与指纹见 [`../corpus.json`](../corpus.json)。

本目录**不参与** ContextDock 的运行——它只在跑评测时被读入。

## 本仓库自有文档

| 文件 | 来源 | 许可 |
| --- | --- | --- |
| `readme.md` | 本仓库 `README.md` 的快照 | MIT（同本仓库） |
| `design.md` | 本仓库 `docs/DESIGN.md` 的快照 | MIT（同本仓库） |
| `pitfalls.md` | 本仓库 `docs/PITFALLS.md` 的快照 | MIT（同本仓库） |

三份都是**快照**，不跟随源文件更新。理由见 [docs/EVAL.md](../../docs/EVAL.md)：
语料漂移会让历史评测数字不可比，所以语料必须冻结；要动它得是一次显式动作（重打指纹）。

## 外部文档

| 文件 | 来源 | 许可 |
| --- | --- | --- |
| `rust-book-zh-testing.md` | 《Rust 程序设计语言》中文版 11.1「如何编写测试」 | MIT OR Apache-2.0 |
| `rust-book-zh-errors.md` | 《Rust 程序设计语言》中文版 9.2「用 Result 处理可恢复的错误」 | MIT OR Apache-2.0 |

- 上游仓库：<https://github.com/rust-lang-cn/book-cn>
- 固定版本：commit `cde74c448e301ce8ac7960a0d3dc879efd83635d`
- 原文：The Rust Programming Language，<https://github.com/rust-lang/book>

### 改动说明

按 Apache-2.0 §4(b) 的要求说明对原文做过的改动，改动仅一项：

1. 换行符统一为 LF（原文经 Git 检出后可能为 CRLF）

内容本身**逐字未改**。

### 为什么需要外部语料

本仓库三份文档合计约 40KB、切出不到 100 个片段。语料这么小的话 `recall@10` 几乎必中，
切分参数扫描的区分度会被压缩到看不出来。这两份 Rust 文档把候选池拉到近 200 个片段，
同时带来**中文散文 + 英文代码块**的中英混排结构，正是本项目分词方案要吃住的场景。

## 许可副本

上表外部文档所依据的许可全文随附在本目录：

- [`LICENSE-MIT.txt`](LICENSE-MIT.txt)
- [`LICENSE-Apache-2.0.txt`](LICENSE-Apache-2.0.txt)

这两个文件**不是评测语料**，只用于满足再分发时的署名与许可随附要求。
