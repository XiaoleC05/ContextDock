// Command contextdock 是 ContextDock 的入口。
//
// 它是**依赖注入的组装点**：唯一知道"用哪个 Embedder、哪个 Store、
// 怎么把它们接起来"的地方。其他包都只依赖接口，不知道彼此的存在。
//
// ⚠️ 这个程序用 MCP 的 stdio 传输：**stdout 是 JSON-RPC 协议通道**。
// 任何一行 fmt.Println 都会破坏协议，而且客户端只会报一个
// 看不懂的解析错误。所有日志必须走 stderr —— Go 的 log 包默认就是 stderr。
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/XiaoleC05/ContextDock/internal/config"
	"github.com/XiaoleC05/ContextDock/internal/embed"
	mcpserver "github.com/XiaoleC05/ContextDock/internal/mcp"
	"github.com/XiaoleC05/ContextDock/internal/service"
	"github.com/XiaoleC05/ContextDock/internal/store"
)

// version 可以在构建时通过 -ldflags 注入：
//
//	go build -ldflags "-X main.version=$(git describe --tags)" ./cmd/contextdock
var version = "dev"

func main() {
	// 显式把日志指向 stderr。Go 的 log 默认就是 stderr，
	// 但显式写出来是为了让"不能碰 stdout"这条约束在代码里看得见。
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[contextdock] ")

	if err := run(); err != nil {
		// 退出码非 0，让上层（Agent、脚本）能发现启动失败
		log.Fatalf("启动失败: %v", err)
	}
}

func run() error {
	// ---- 信号处理：Ctrl+C 和 SIGTERM 都能触发优雅退出 ----
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ---- 1. 配置 ----
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// Config 的 String() 会把密钥打码，所以这样打日志是安全的
	log.Printf("配置: %s", cfg)

	// ---- 2. Embedder ----
	embedder, err := buildEmbedder(cfg)
	if err != nil {
		return err
	}

	// ---- 3. Store ----
	st, err := buildStore(ctx, cfg)
	if err != nil {
		return err
	}
	// 无论后面哪一步失败都要关掉连接池
	defer func() {
		if err := st.Close(); err != nil {
			log.Printf("关闭存储失败: %v", err)
		}
	}()

	// ---- 4. Service ----
	svc, err := service.New(cfg, embedder, st)
	if err != nil {
		return fmt.Errorf("创建服务失败: %w", err)
	}
	defer func() { _ = svc.Close() }()

	// ---- 5. 从数据库重建内存索引 ----
	//
	// ⚠️ 这一步不能省。BM25 是内存索引，进程重启后就没了。
	// 不重建的话，重启后检索会**静默地**只剩向量一路——
	// 结果变差，但不报任何错。
	rebuildCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	err = svc.Rebuild(rebuildCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("重建索引失败: %w", err)
	}
	chunks, embedded := svc.Stats()
	log.Printf("索引已重建: %d 个片段，其中 %d 个带向量", chunks, embedded)

	// ---- 6. 启动 MCP server ----
	srv := mcpserver.NewServer(svc, version)
	log.Printf("ContextDock %s 已就绪，等待 MCP 客户端连接（stdio）", version)

	if err := srv.Run(ctx, &sdkmcp.StdioTransport{}); err != nil {
		// 客户端正常断开时 Run 也会返回错误，这不是异常情况
		if errors.Is(err, context.Canceled) {
			log.Printf("收到退出信号，正在关闭…")
			return nil
		}
		return fmt.Errorf("MCP server 退出: %w", err)
	}

	log.Printf("正常退出")
	return nil
}

// buildEmbedder 根据配置选择 Embedder 实现。
//
// 假嵌入（CONTEXTDOCK_FAKE_EMBEDDER=true）让任何人 clone 完就能跑通
// 「导入 → 检索」全链路：不用 API Key、不联网、不起数据库
// （再配上 CONTEXTDOCK_USE_MEMORY_STORE=true）。
//
// ⚠️ 它按 token 哈希造向量，只有「共享词越多越像」这一层粗粒度相似性，
// **没有任何语义理解**——问「怎么让程序跑得快」，它找不到写着「性能优化」
// 的段落，而那正是混合检索存在的理由。所以它验证的是**链路通不通**，
// 不是检索好不好。要判断质量请用 cmd/eval，要真实效果请配真 Key。
//
// 这条路径唯一的用途是降低陌生人的进入成本，因此每次启动都要
// 把上面这句话打到 stderr 上——默认路径的校验一点没放松。
func buildEmbedder(cfg *config.Config) (embed.Embedder, error) {
	if cfg.FakeEmbedder {
		log.Printf("⚠️  假嵌入已开启（%s=true）：不联网、不校验 API Key。"+
			"向量由 token 哈希生成，**没有语义理解**，检索质量无意义——"+
			"它只用来验证链路是否跑通", config.EnvFakeEmbedder)
		return embed.NewFake(), nil
	}

	embedder, err := embed.NewSiliconFlow(cfg.SiliconFlowAPIKey,
		embed.WithBaseURL(cfg.SiliconFlowBaseURL),
		embed.WithModel(cfg.EmbeddingModel),
	)
	if err != nil {
		return nil, fmt.Errorf("创建 Embedder 失败: %w", err)
	}
	return embedder, nil
}

// buildStore 根据配置选择存储实现。
//
// 内存实现不是"降级方案"，而是**开发时真正有用**的选项：
// 不想起数据库、只想试试检索效果时，一个环境变量就能跑起来。
func buildStore(ctx context.Context, cfg *config.Config) (store.Store, error) {
	if cfg.UseMemoryStore {
		log.Printf("使用内存存储（数据不会持久化，重启即丢失）")
		return store.NewMemory(), nil
	}

	st, err := store.NewPostgres(ctx, cfg.DatabaseURL, cfg.PoolMaxConns)
	if err != nil {
		return nil, err
	}
	log.Printf("已连接 PostgreSQL（连接池上限 %d）", cfg.PoolMaxConns)
	return st, nil
}
