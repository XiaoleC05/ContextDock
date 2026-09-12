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
	)
	flag.Parse()

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
