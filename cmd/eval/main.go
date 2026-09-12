// Command eval 是检索质量评测器。
//
// 用法：
//
//	go run ./cmd/eval -validate     只校验评测集与语料指纹，不跑检索
//	go run ./cmd/eval -stamp        重打语料指纹（破坏性）
//	go run ./cmd/eval               跑一次完整评测
//
// # 为什么评测器直接调 service 层，不走 MCP stdio
//
// 评测要跑几百次检索。每次 fork 一个进程、走一遍 JSON-RPC 握手，
// 开销全花在协议上而不是检索上，而且失败时多一层「是协议错了还是检索错了」
// 的不确定性。MCP 那条路的正确性由 cmd/smoke 负责，各测各的。
//
// # 为什么不需要数据库
//
// 评测用内存存储，理由见 internal/eval.Run 的注释。所以这里**不需要**
// PostgreSQL，也不会写任何数据库——只要求一个可用的 Embedding 接口。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/XiaoleC05/ContextDock/internal/config"
	"github.com/XiaoleC05/ContextDock/internal/embed"
	"github.com/XiaoleC05/ContextDock/internal/eval"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		root     = flag.String("root", ".", "仓库根目录（eval/ 所在的目录）")
		validate = flag.Bool("validate", false, "只校验评测集与语料指纹，不跑检索")
		stamp    = flag.Bool("stamp", false, "重打语料指纹（**破坏性**，只在有意改过语料后用）")

		// 检索与切分参数。全部可覆盖，因为参数扫描（#43/#44）
		// 靠的就是「同一份评测集 + 不同参数 → 可比的数字」。
		topK     = flag.Int("topk", 0, "每条查询保留的结果条数（recall@k 的 k），默认 10")
		rrfK     = flag.Int("k", 0, "RRF 平滑常数，默认 60")
		mult     = flag.Int("mult", 0, "融合前的候选放大倍数，默认 3")
		maxRunes = flag.Int("maxrunes", 0, "切分最大字符数，默认 400")
		overlap  = flag.Int("overlap", -1, "切分重叠字符数，默认 60（0 是合法值，故默认 -1）")
		ndcgK    = flag.Int("ndcgk", 0, "NDCG 的截断位置，默认 10")

		asJSON = flag.Bool("json", false, "输出机读 JSON 而不是表格")
		detail = flag.Bool("detail", false, "报告里带上逐条查询明细（供失败分析）")
		cache  = flag.String("cache", ".evalcache",
			"嵌入缓存目录；设为空串则关闭缓存（会慢很多，但数字不变）")
		quiet = flag.Bool("quiet", false, "不打印导入进度")
	)
	flag.Parse()

	// -stamp 必须排在加载之前：它的用途正是修复「指纹对不上」的语料清单，
	// 走 Load 会被指纹校验拦死，根本走不到修复那一步。
	if *stamp {
		changes, err := eval.Stamp(*root)
		if err != nil {
			return err
		}
		if len(changes) == 0 {
			fmt.Println("语料指纹已是最新，未做改动。")
			return nil
		}
		fmt.Printf("语料清单已更新（%d 份）：\n", len(changes))
		for _, c := range changes {
			fmt.Printf("  %s\n", c.Info())
		}
		fmt.Println("\n⚠️ 历史评测数字与这批语料不再可比，需重跑基线。")
		return nil
	}

	// 校验与跑分走同一条加载路径：**能跑起来的评测集一定是校验过的**。
	// 如果给 -validate 单独写一条只检查格式的捷径，那条捷径迟早会与
	// 真正的加载逻辑漂移，变成「校验通过但跑不起来」。
	suite, err := eval.Load(*root)
	if err != nil {
		return err
	}

	if *validate {
		// 校验通过时**不输出任何东西**，退出码 0。
		//
		// 这条要求不是洁癖：这个命令会被脚本和 CI 调用，
		// 有噪音就得写过滤器，而过滤器本身又是一处会过期的假设。
		return nil
	}

	emb, err := buildEmbedder(*root, *cache)
	if err != nil {
		return err
	}

	opt := eval.Options{
		TopK: *topK, RRFK: *rrfK, Mult: *mult,
		MaxRunes: *maxRunes, Overlap: *overlap, NDCGK: *ndcgK,
		KeepDetail: *detail || *asJSON,
	}
	if *overlap >= 0 {
		opt.Overlap = *overlap
	}
	// 注意：Overlap 的 0 是合法配置（完全不重叠），不能用零值判断"没传"。
	// 所以 flag 默认是 -1，只有 >= 0 才覆盖，之后交给 Normalize 兜底。

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logf := func(format string, args ...any) {
		if !*quiet {
			fmt.Fprintf(os.Stderr, format+"\n", args...)
		}
	}

	rep, err := eval.Run(ctx, suite, emb, opt, logf)
	if err != nil {
		return err
	}

	if *asJSON {
		return eval.WriteJSON(os.Stdout, rep)
	}
	return eval.WriteTable(os.Stdout, rep)
}

// buildEmbedder 组装 Embedding 客户端，并按需套上磁盘缓存。
//
// # 为什么强制内存存储
//
// 评测不需要数据库：检索路径是内存里的暴力扫描，存储只是启动时
// 把数据读进来的来源。评测自己灌语料，用内存版即可。
//
// 强制而不是"允许"，是因为一旦有人把 CONTEXTDOCK_DATABASE_URL 配上了，
// 评测就会去写那个库——**把评测语料混进真实知识库**，
// 而且很难发现：结果照样出，只是那个库脏了。
func buildEmbedder(root, cacheDir string) (embed.Embedder, error) {
	dotenv, err := config.ReadDotEnv(filepath.Join(root, ".env"))
	if err != nil {
		return nil, err
	}
	cfg, err := config.LoadWith(func(key string) (string, bool) {
		if key == config.EnvUseMemoryStore {
			return "true", true
		}
		if v, ok := os.LookupEnv(key); ok {
			return v, true
		}
		v, ok := dotenv[key]
		return v, ok
	})
	if err != nil {
		return nil, err
	}

	emb, err := embed.NewSiliconFlow(cfg.SiliconFlowAPIKey,
		embed.WithBaseURL(cfg.SiliconFlowBaseURL),
		embed.WithModel(cfg.EmbeddingModel))
	if err != nil {
		return nil, err
	}

	if cacheDir == "" {
		return emb, nil
	}
	if !filepath.IsAbs(cacheDir) {
		cacheDir = filepath.Join(root, cacheDir)
	}
	// 缓存目录按模型分开：换模型时旧向量绝不能命中，见 embed.Cache 的说明。
	return embed.NewCache(filepath.Join(cacheDir, safeName(cfg.EmbeddingModel)), cfg.EmbeddingModel, emb), nil
}

// safeName 把模型名变成能当目录名的形式（"Pro/BAAI/bge-m3" 里有斜杠）。
func safeName(s string) string {
	b := []rune(s)
	for i, r := range b {
		if r == '/' || r == '\\' {
			b[i] = '_'
		}
	}
	return string(b)
}
