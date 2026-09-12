// Command eval 是检索质量评测器。
//
// 用法：
//
//	go run ./cmd/eval -validate     只校验评测集与语料指纹，不跑检索
//
// # 为什么评测器直接调 service 层，不走 MCP stdio
//
// 评测要跑几百次检索。每次 fork 一个进程、走一遍 JSON-RPC 握手，
// 开销全花在协议上而不是检索上，而且失败时多一层「是协议错了还是检索错了」
// 的不确定性。MCP 那条路的正确性由 cmd/smoke 负责，各测各的。
package main

import (
	"flag"
	"fmt"
	"os"

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
	if _, err := eval.Load(*root); err != nil {
		return err
	}

	if *validate {
		// 校验通过时**不输出任何东西**，退出码 0。
		//
		// 这条要求不是洁癖：这个命令会被脚本和 CI 调用，
		// 有噪音就得写过滤器，而过滤器本身又是一处会过期的假设。
		return nil
	}

	// 跑分路径在 #38 实现。
	return fmt.Errorf("eval: 尚未实现跑分，请先用 -validate（评测执行见 #38）")
}
