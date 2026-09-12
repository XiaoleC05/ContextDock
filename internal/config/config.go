// Package config 把散落的环境变量收口到一处，并在启动时校验。
//
// 为什么必须收口：如果 SiliconFlowEmbedder 和 postgres Store 各自写
// os.Getenv，就会出现"某个变量缺了但没人发现"的情况——而且症状是
// 第一次检索才 401/403，排查时先怀疑的是网络和鉴权，不是配置。
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/XiaoleC05/ContextDock/internal/tokenize"
	"github.com/XiaoleC05/ContextDock/internal/types"
)

// 环境变量名。集中在这里，避免各处写字符串字面量写错。
const (
	EnvSiliconFlowAPIKey = "SILICONFLOW_API_KEY"
	EnvSiliconFlowBase   = "SILICONFLOW_BASE_URL"
	EnvEmbeddingModel    = "CONTEXTDOCK_EMBEDDING_MODEL"
	EnvDatabaseURL       = "CONTEXTDOCK_DATABASE_URL"
	EnvUseMemoryStore    = "CONTEXTDOCK_USE_MEMORY_STORE"
	EnvTopK              = "CONTEXTDOCK_TOP_K"
	EnvSearchTimeout     = "CONTEXTDOCK_SEARCH_TIMEOUT"
	EnvChunkMaxRunes     = "CONTEXTDOCK_CHUNK_MAX_RUNES"
	EnvChunkOverlap      = "CONTEXTDOCK_CHUNK_OVERLAP"
	EnvPoolMaxConns      = "CONTEXTDOCK_POOL_MAX_CONNS"

	// EnvContextNeighbors 控制上下文扩展：每条结果带出前后各几段（#49）。
	EnvContextNeighbors = "CONTEXTDOCK_CONTEXT_NEIGHBORS"

	// EnvMergeAdjacent 控制相邻片段合并（#50）。
	EnvMergeAdjacent = "CONTEXTDOCK_MERGE_ADJACENT"

	// EnvFakeEmbedder 打开假嵌入，用于「clone 完就能跑」的演示路径。
	//
	// 它同时**豁免** EnvSiliconFlowAPIKey 这条必填校验——假嵌入不联网。
	EnvFakeEmbedder = "CONTEXTDOCK_FAKE_EMBEDDER"

	// EnvHNSWEfSearch 是 HNSW 索引的搜索宽度（pgvector 的 hnsw.ef_search）。
	//
	// 它不改变召回**排序**，只决定「找得多宽」：值越小越快、
	// 但可能漏掉真正的近邻。pgvector 的默认值是 40，偏低。
	EnvHNSWEfSearch = "CONTEXTDOCK_HNSW_EF_SEARCH"
)

// 默认值。
const (
	DefaultTopK          = 10
	DefaultSearchTimeout = 5 * time.Second
	DefaultChunkMaxRunes = 400
	DefaultChunkOverlap  = 60

	// 默认连接池上限。
	//
	// ⚠️ pgx 的默认值是 max(4, runtime.NumCPU())，在 28 线程的机器上会开到
	// 28 条连接。单机 MCP server 根本用不到那么多，还会和容器里的
	// max_connections 叠加。这里显式收窄。
	DefaultPoolMaxConns = 8

	// DefaultContextNeighbors 是上下文扩展默认带出的相邻片段数。
	//
	// 取 1 而不是 0：命中片段常常只写着「运行 go build」，
	// 没有前后文的话 Agent 不知道它在讲什么。带一段就够定位语境了。
	//
	// 上限由输出体积决定，不由"能带几段"决定——见 mcp 包里的截断说明。
	DefaultContextNeighbors = 1

	// DefaultHNSWEfSearch 是 HNSW 搜索宽度的默认值。
	//
	// 取 100 而不是 pgvector 的默认 40：docs/PITFALLS.md 记着这个值
	// 偏低、建议 100 起步再用真实查询集测 recall 后定。**这个默认值是
	// 起点不是结论**——`cmd/eval -store=postgres` 跑过 ef_search 扫描
	// 之后，结论写进 BENCHMARKS.md。
	//
	// ⚠️ 它必须 >= 单次检索要取的候选数（topK × mult），否则候选池被
	// 截断、召回悄悄下降。实现里按 max(本值, 候选数) 兜底，见
	// store.Postgres.SearchByEmbedding。
	DefaultHNSWEfSearch = 100

	// DefaultMergeAdjacent 是相邻片段合并的默认开关。
	//
	// 默认**开**，依据是实测（四组切分参数，见 BENCHMARKS.md）：
	// recall 两平两升、无一下降，NDCG@10 全部提升 0.05~0.09，
	// top-k 覆盖的不同文档数从 3.3~3.6 升到 3.6~3.95。
	//
	// 它解决的问题是「top-10 里三四条都是同一处内容，别的文档挤不进来」，
	// 而那个问题光看 recall 是看不出来的。
	DefaultMergeAdjacent = true
)

var (
	// ErrMissingAPIKey 表示没配置 API Key。
	ErrMissingAPIKey = errors.New("config: 缺少 " + EnvSiliconFlowAPIKey)

	// ErrMissingDatabaseURL 表示选了 postgres 存储但没配连接串。
	ErrMissingDatabaseURL = errors.New("config: 缺少 " + EnvDatabaseURL)

	// ErrBadValue 表示某个变量的值不合法。
	ErrBadValue = errors.New("config: 环境变量的值不合法")
)

// Config 是全部运行时配置。
type Config struct {
	// Embedding
	SiliconFlowAPIKey  string
	SiliconFlowBaseURL string
	EmbeddingModel     string

	// FakeEmbedder 为真时用 token 哈希造的假向量替代真实 Embedding 调用。
	//
	// ⚠️ **不是降级方案，是演示/开发开关**。它不联网、不要 API Key、
	// 完全确定，能让任何人 clone 完就跑通「导入 → 检索」全链路；
	// 代价是**没有任何语义理解**——只有"共享词越多越像"的粗粒度相似性。
	// 所以它验证的是链路通不通，不是检索好不好。
	FakeEmbedder bool

	// 存储
	UseMemoryStore bool
	DatabaseURL    string
	PoolMaxConns   int32

	// HNSWEfSearch 是向量近邻检索的搜索宽度（pgvector 的 hnsw.ef_search）。
	//
	// 只在 pgvector 后端生效：内存暴力扫描是精确的，没有这个旋钮。
	// 实现会按 max(本值, topK×mult) 兜底，避免候选池被截断。
	HNSWEfSearch int

	// 检索
	TopK          int
	SearchTimeout time.Duration

	// 切分
	ChunkMaxRunes int
	ChunkOverlap  int

	// ContextNeighbors 是每条检索结果带出的相邻片段数（前后各这么多段），
	// 0 表示不做上下文扩展。
	ContextNeighbors int

	// MergeAdjacent 为真时，把同一文档里序号连续的命中合并成一条（#50）。
	//
	// 目的是腾出结果位：同一处内容占掉三四条时，别的文档就挤不进来了。
	MergeAdjacent bool

	// 分词方案（CJK 部分）。
	//
	// ⚠️ **刻意不接环境变量。** 它是 #45 对比实验用的旋钮，
	// 不是用户配置：当前选型（bigram）是 DESIGN §3 的结论，
	// 多暴露一个用户可调的旋钮，就多一组需要长期维护和测试的组合。
	// 实验做完、结论落地之后，这个字段要么被写死，要么按结论换掉默认值。
	TokenizeScheme tokenize.Scheme

	// EmbeddingDim 是向量维度，从 types 带入，方便统一引用。
	EmbeddingDim int
}

// Getenv 是读取环境变量的函数签名。
//
// 第二个返回值表示"这个变量是否存在"，用于区分**未设置**和**设为空字符串**：
// 前者应该报错，后者应该报另一种错（配置写错了）。
type Getenv func(key string) (string, bool)

// Load 从 .env 文件 + 进程环境变量读取配置。
//
// 优先级：**进程环境变量 > .env 文件**。
// 这样 Agent 的配置文件里写的 env 块能覆盖开发时的 .env。
//
// .env 文件不存在不算错误——生产环境通常直接用进程环境变量注入。
func Load() (*Config, error) {
	dotenv, err := ReadDotEnv(".env")
	if err != nil {
		return nil, err
	}
	return LoadWith(func(key string) (string, bool) {
		if v, ok := os.LookupEnv(key); ok {
			return v, true
		}
		v, ok := dotenv[key]
		return v, ok
	})
}

// LoadWith 用给定的取值函数加载配置，便于测试。
func LoadWith(getenv Getenv) (*Config, error) {
	cfg := &Config{
		SiliconFlowBaseURL: "https://api.siliconflow.cn/v1",
		EmbeddingModel:     "Pro/BAAI/bge-m3",
		UseMemoryStore:     false,
		TopK:               DefaultTopK,
		SearchTimeout:      DefaultSearchTimeout,
		ChunkMaxRunes:      DefaultChunkMaxRunes,
		ChunkOverlap:       DefaultChunkOverlap,
		TokenizeScheme:     tokenize.SchemeBigram,
		ContextNeighbors:   DefaultContextNeighbors,
		MergeAdjacent:      DefaultMergeAdjacent,
		PoolMaxConns:       DefaultPoolMaxConns,
		EmbeddingDim:       types.EmbeddingDim,
		HNSWEfSearch:       DefaultHNSWEfSearch,
	}

	// ---- 假嵌入开关 ----
	//
	// ⚠️ 必须**先于** API Key 的校验读出来：它决定 API Key 还算不算必填。
	if v, ok := getenv(EnvFakeEmbedder); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("%w: %s=%q 不是合法的布尔值",
				ErrBadValue, EnvFakeEmbedder, v)
		}
		cfg.FakeEmbedder = b
	}

	// ---- 必填 ----
	//
	// API Key 只在**真实嵌入**下必填。开了假嵌入就不联网了，
	// 再要求一个 Key 只会挡住「clone 完想先跑跑看」的人。
	//
	// ⚠️ 但默认路径的校验一点没放松：没开这个开关时，
	// Key 缺失或为空仍然是启动期硬失败。
	if !cfg.FakeEmbedder {
		key, ok := getenv(EnvSiliconFlowAPIKey)
		if !ok {
			return nil, ErrMissingAPIKey
		}
		if strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("%w: %s 存在但为空，请填入真实的 API Key",
				ErrMissingAPIKey, EnvSiliconFlowAPIKey)
		}
		cfg.SiliconFlowAPIKey = strings.TrimSpace(key)
	} else if key, ok := getenv(EnvSiliconFlowAPIKey); ok {
		// 开了假嵌入但 Key 也在：留着，String() 会打码显示"已设置"，
		// 便于确认自己没配错。它不会被使用。
		cfg.SiliconFlowAPIKey = strings.TrimSpace(key)
	}

	// ---- 可选，带默认值 ----
	if v, ok := getenv(EnvSiliconFlowBase); ok {
		cfg.SiliconFlowBaseURL = v
	}
	if v, ok := getenv(EnvEmbeddingModel); ok {
		cfg.EmbeddingModel = v
	}
	if v, ok := getenv(EnvDatabaseURL); ok {
		cfg.DatabaseURL = v
	}

	if v, ok := getenv(EnvMergeAdjacent); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("%w: %s=%q 不是合法的布尔值",
				ErrBadValue, EnvMergeAdjacent, v)
		}
		cfg.MergeAdjacent = b
	}

	if v, ok := getenv(EnvUseMemoryStore); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("%w: %s=%q 不是合法的布尔值",
				ErrBadValue, EnvUseMemoryStore, v)
		}
		cfg.UseMemoryStore = b
	}

	// 选了 postgres 就必须有连接串——在启动期拦住，
	// 而不是等第一次查询时连接失败。
	if !cfg.UseMemoryStore && cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("%w（或设置 %s=true 使用内存存储）",
			ErrMissingDatabaseURL, EnvUseMemoryStore)
	}

	var err error
	if cfg.TopK, err = intVar(getenv, EnvTopK, cfg.TopK); err != nil {
		return nil, err
	}
	if cfg.ChunkMaxRunes, err = intVar(getenv, EnvChunkMaxRunes, cfg.ChunkMaxRunes); err != nil {
		return nil, err
	}
	if cfg.ChunkOverlap, err = intVar(getenv, EnvChunkOverlap, cfg.ChunkOverlap); err != nil {
		return nil, err
	}
	if cfg.ContextNeighbors, err = intVar(getenv, EnvContextNeighbors, cfg.ContextNeighbors); err != nil {
		return nil, err
	}
	if cfg.HNSWEfSearch, err = intVar(getenv, EnvHNSWEfSearch, cfg.HNSWEfSearch); err != nil {
		return nil, err
	}
	if n, err := intVar(getenv, EnvPoolMaxConns, int(cfg.PoolMaxConns)); err != nil {
		return nil, err
	} else {
		cfg.PoolMaxConns = int32(n)
	}
	if v, ok := getenv(EnvSearchTimeout); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("%w: %s=%q 不是合法的时长（如 5s、500ms）",
				ErrBadValue, EnvSearchTimeout, v)
		}
		cfg.SearchTimeout = d
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate 做跨字段的一致性校验。
//
// 只在 LoadWith 里做单字段校验是不够的——比如 TopK 和 ChunkOverlap
// 各自都合法，但组合起来可能没意义。
func (c *Config) Validate() error {
	if c.TopK <= 0 {
		return fmt.Errorf("%w: TopK 必须为正数，实际 %d", ErrBadValue, c.TopK)
	}
	if c.ChunkMaxRunes <= 0 {
		return fmt.Errorf("%w: ChunkMaxRunes 必须为正数，实际 %d", ErrBadValue, c.ChunkMaxRunes)
	}
	if c.ChunkOverlap < 0 || c.ChunkOverlap >= c.ChunkMaxRunes {
		return fmt.Errorf("%w: ChunkOverlap(%d) 必须 >=0 且小于 ChunkMaxRunes(%d)",
			ErrBadValue, c.ChunkOverlap, c.ChunkMaxRunes)
	}
	// ⚠️ 空值放行，表示「用默认方案」。
	//
	// 与 ChunkOverlap 那条**刻意不同**：那边 0 是一个有意义的取值，
	// 所以不能拿零值当哨兵；这边空串没有任何合理解释，
	// 「空 = 默认」是安全的，也让手写的 config.Config{} 仍然可用。
	//
	// 一致性由 tokenize.NewWith 兜底：它对非法方案也回落成 bigram。
	if c.TokenizeScheme != "" && !c.TokenizeScheme.Valid() {
		return fmt.Errorf("%w: TokenizeScheme=%q 不是合法方案（%v）",
			ErrBadValue, c.TokenizeScheme, tokenize.AllSchemes)
	}
	// 允许 0（关掉上下文扩展），但不能为负——负数在 mcp 那边会退化成
	// "不取邻居"，静默地什么也不做，而配置看起来是生效的。
	if c.ContextNeighbors < 0 {
		return fmt.Errorf("%w: ContextNeighbors 不能为负，实际 %d",
			ErrBadValue, c.ContextNeighbors)
	}
	if c.SearchTimeout <= 0 {
		return fmt.Errorf("%w: SearchTimeout 必须为正数，实际 %v", ErrBadValue, c.SearchTimeout)
	}
	if c.PoolMaxConns <= 0 {
		return fmt.Errorf("%w: PoolMaxConns 必须为正数，实际 %d", ErrBadValue, c.PoolMaxConns)
	}
	// ef_search 必须为正。它同时是「搜索宽度」的下界，
	// 0 会让实现退化成只返回极少数候选，而看起来毫无异常。
	if c.HNSWEfSearch <= 0 {
		return fmt.Errorf("%w: HNSWEfSearch 必须为正数，实际 %d", ErrBadValue, c.HNSWEfSearch)
	}
	return nil
}

// String 实现 fmt.Stringer，**故意隐藏 API Key 与连接串**。
//
// 日志里打印配置是很常见的调试手段。如果 String() 原样输出，
// 一个 log.Printf("%+v", cfg) 就会把密钥写进日志文件。
func (c Config) String() string {
	return fmt.Sprintf(
		"Config{model:%s baseURL:%s fakeEmbedder:%v apiKey:%s db:%s memoryStore:%v "+
			"topK:%d timeout:%v chunk:%d/%d poolMaxConns:%d hnswEfSearch:%d}",
		c.EmbeddingModel, c.SiliconFlowBaseURL, c.FakeEmbedder,
		maskSecret(c.SiliconFlowAPIKey),
		maskSecret(c.DatabaseURL), c.UseMemoryStore, c.TopK, c.SearchTimeout,
		c.ChunkMaxRunes, c.ChunkOverlap, c.PoolMaxConns, c.HNSWEfSearch)
}

// maskSecret 只保留长度信息，不泄露内容。
func maskSecret(s string) string {
	if s == "" {
		return "(未设置)"
	}
	return fmt.Sprintf("(已设置，%d 字符)", len(s))
}

func intVar(getenv Getenv, key string, def int) (int, error) {
	v, ok := getenv(key)
	if !ok {
		return def, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("%w: %s=%q 不是合法的整数", ErrBadValue, key, v)
	}
	return n, nil
}

// ReadDotEnv 读取 .env 文件，返回键值对。
//
// 文件不存在返回空 map 而不是错误——生产环境通常不落 .env 文件。
//
// 自己实现解析而不是引 godotenv：格式很简单（KEY=VALUE），
// 而本项目其他部分刻意保持零依赖。代价是不支持多行值和变量展开，
// 那对本项目的配置来说用不上。
func ReadDotEnv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("config: 读取 %s 失败: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	out := make(map[string]string)
	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// 允许 `export KEY=VALUE` 的写法
		line = strings.TrimPrefix(line, "export ")

		k, v, found := strings.Cut(line, "=")
		if !found {
			return nil, fmt.Errorf("config: %s 第 %d 行不是 KEY=VALUE 格式: %q",
				path, lineNo, line)
		}
		k = strings.TrimSpace(k)
		if k == "" {
			return nil, fmt.Errorf("config: %s 第 %d 行缺少变量名", path, lineNo)
		}
		v = strings.TrimSpace(v)
		// 去掉成对的引号
		if len(v) >= 2 {
			if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
				v = v[1 : len(v)-1]
			}
		}
		out[k] = v
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("config: 读取 %s 失败: %w", path, err)
	}
	return out, nil
}
