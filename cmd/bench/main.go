// Command bench 量的是**规模**：片段数涨上去之后，导入、内存、检索各是什么样。
//
// 它存在的理由很直接：在此之前，README 里「千级片段是毫秒级」这句话是在
// **196 个片段**上测出来的，属于外推而不是测量——而这个项目自己的标准是
// 「参数是测出来的，不是拍脑袋定的」。见 issue #69。
//
// 用法：
//
//	go run ./cmd/bench -n 10000
//	go run ./cmd/bench -n 100000 -store=postgres -dsn=... -store-reset
//	go run ./cmd/bench -n 5000 -real-embed -recall
//
// # ⚠️ 假嵌入与真实嵌入的分工
//
// 默认用**假嵌入**（token 哈希，不联网、不花钱、几秒跑完）：
//
//   - **可信**：导入耗时、内存占用、检索延迟——它们取决于片段数与维度，
//     与向量的**分布**无关
//   - **不可信**：召回率。假嵌入产出的是稀疏哈希向量，HNSW 在这种分布上
//     的表现**不代表** bge-m3 的稠密向量
//
// 所以 `-recall` 那部分要用 `-real-embed` 在**较小规模**上单独跑。
// 报告里会把这条限制打在数字旁边，而不是藏在文档里。
package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/XiaoleC05/ContextDock/internal/config"
	"github.com/XiaoleC05/ContextDock/internal/embed"
	"github.com/XiaoleC05/ContextDock/internal/service"
	"github.com/XiaoleC05/ContextDock/internal/store"
	"github.com/XiaoleC05/ContextDock/internal/types"
)

func main() {
	var (
		target    = flag.Int("n", 10000, "目标片段数（实际数量可能略有出入，以报告为准）")
		storeKind = flag.String("store", "memory", "检索后端：memory | postgres")
		dsn       = flag.String("dsn", "", "postgres 后端的连接串；留空取 "+config.EnvDatabaseURL)
		reset     = flag.Bool("store-reset", false, "允许清空目标库（**会删数据**）")
		realEmbed = flag.Bool("real-embed", false,
			"用真实嵌入（要 API Key、会联网、按量计费）。默认假嵌入")
		nQueries = flag.Int("queries", 200, "测延迟用的查询数")
		doRecall = flag.Bool("recall", false,
			"额外测 ANN 召回：以库内片段自身的向量为查询，比较库内检索与暴力扫描的 top-k 重合率")
		topK     = flag.Int("topk", 10, "检索返回条数")
		efSearch = flag.Int("ef-search", 0, "postgres 后端的 HNSW 搜索宽度（0=默认）")
	)
	flag.Parse()

	if err := run(*target, *storeKind, *dsn, *reset, *realEmbed,
		*nQueries, *doRecall, *topK, *efSearch); err != nil {
		fmt.Fprintf(os.Stderr, "基准失败: %v\n", err)
		os.Exit(1)
	}
}

func run(target int, storeKind, dsn string, reset, realEmbed bool,
	nQueries int, doRecall bool, topK, efSearch int) error {

	ctx := context.Background()

	emb, embedLabel, err := buildEmbedder(realEmbed)
	if err != nil {
		return err
	}

	fmt.Printf("== ContextDock 规模基准 ==\n")
	fmt.Printf("目标片段数   %d\n", target)
	fmt.Printf("检索后端     %s\n", storeKind)
	fmt.Printf("嵌入         %s\n", embedLabel)
	fmt.Printf("Go           %s/%s，GOMAXPROCS=%d\n\n",
		runtime.Version(), runtime.GOOS, runtime.GOMAXPROCS(0))

	// ---- 1. 生成语料 ----
	//
	// 生成的是**文档**而不是片段：让正常的导入链路去切分，
	// 这样量到的是「导入一篇文档」的真实成本，而不是绕过切分器的理想值。
	docs := generateCorpus(target, 20)
	var corpusRunes int
	for _, d := range docs {
		corpusRunes += len([]rune(d.Content))
	}

	st, err := buildStore(ctx, storeKind, dsn, reset, efSearch)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	cfg := &config.Config{
		ChunkMaxRunes:  config.DefaultChunkMaxRunes,
		ChunkOverlap:   config.DefaultChunkOverlap,
		TopK:           topK,
		SearchTimeout:  60 * time.Second,
		UseMemoryStore: storeKind != "postgres",
		DatabaseURL:    dsn,
		HNSWEfSearch:   efSearch,
		PoolMaxConns:   8,
		EmbeddingDim:   types.EmbeddingDim,
		MergeAdjacent:  false, // 量检索本身，别让合并干扰
	}
	svc, err := service.New(cfg, emb, st)
	if err != nil {
		return fmt.Errorf("组装服务失败: %w", err)
	}
	defer func() { _ = svc.Close() }()

	// ---- 2. 基线内存（语料已在内存里，所以增量就是索引与存储的开销）----
	corpusBytes := int64(corpusRunes * 3) // 中文按 3 字节粗算
	before := heapAlloc()

	// ---- 3. 导入 ----
	importStart := time.Now()
	for i, d := range docs {
		if _, err := svc.Import(ctx, d); err != nil {
			return fmt.Errorf("导入第 %d 篇失败: %w", i, err)
		}
	}
	importMs := time.Since(importStart).Milliseconds()

	chunks, embedded := svc.Stats()
	// svc.IndexStats()
	after := heapAlloc()

	fmt.Printf("== 导入 ==\n")
	fmt.Printf("文档数       %d\n", len(docs))
	fmt.Printf("片段数       %d（其中 %d 带向量）\n", chunks, embedded)
	fmt.Printf("正文          %.1f MB\n", float64(corpusBytes)/1e6)
	fmt.Printf("耗时         %d ms（%.0f 片段/秒）\n",
		importMs, float64(chunks)/float64(importMs+1)*1000)
	fmt.Printf("堆增量       %.1f MB（索引 + 存储；语料本身不计入）\n\n",
		float64(after-before)/1e6)

	// ---- 4. 检索延迟 ----
	if chunks == 0 {
		return fmt.Errorf("没有导入任何片段，后面的测量没有意义")
	}
	_, lat, err := measureLatency(ctx, svc, docs, nQueries, topK)
	if err != nil {
		return err
	}
	fmt.Printf("== 检索延迟（%d 次查询）==\n", nQueries)
	fmt.Printf("P50          %.2f ms\n", lat.P50)
	fmt.Printf("P95          %.2f ms\n", lat.P95)
	fmt.Printf("最慢         %.2f ms\n\n", lat.Max)

	// ---- 4.5 单次全量重建 = 进程启动时的那一次（#70）----
	//
	// 导入路径会重建很多次，这里要的是**一次**的耗时：
	// 它就是"重启一次 MCP server 要等多久"。
	rbStart := time.Now()
	if err := svc.Rebuild(ctx); err != nil {
		return fmt.Errorf("重建失败: %w", err)
	}
	rebuildMs := time.Since(rbStart).Milliseconds()
	heapAfterRebuild := heapAlloc()
	fmt.Printf("== 启动重建（Rebuild 一次，即重启进程要等多久）==\n")
	fmt.Printf("耗时         %d ms\n", rebuildMs)
	fmt.Printf("堆占用       %.1f MB\n\n", float64(heapAfterRebuild)/1e6)

	// ---- 5. ANN 召回（可选）----
	if doRecall {
		if err := measureRecall(ctx, st, svc, docs, topK, realEmbed); err != nil {
			return err
		}
	}

	fmt.Printf("== 这组数字能说明什么 ==\n")
	if realEmbed {
		fmt.Printf("嵌入是真实的，所以召回数字可用；但规模受限于 API 调用成本。\n")
	} else {
		fmt.Printf("⚠️ 用的是**假嵌入**：导入耗时、内存、延迟可信（它们取决于片段数\n")
		fmt.Printf("   与维度，与向量分布无关）；**召回率不可信**——假嵌入产出的是稀疏\n")
		fmt.Printf("   哈希向量，HNSW 在那种分布上的表现不代表真实的稠密向量。\n")
		fmt.Printf("   要测召回请加 -real-embed，并在较小规模上跑。\n")
	}
	return nil
}

// buildEmbedder 按开关造嵌入器，并返回一个用于报告的中文标签。
func buildEmbedder(real bool) (embed.Embedder, string, error) {
	if !real {
		return embed.NewFake(), "假嵌入（token 哈希，不联网）", nil
	}
	key := os.Getenv(config.EnvSiliconFlowAPIKey)
	if key == "" {
		if v, ok := readDotEnvKey(); ok {
			key = v
		}
	}
	if key == "" {
		return nil, "", fmt.Errorf("要 -real-embed 就得配 %s（或 .env）", config.EnvSiliconFlowAPIKey)
	}
	e, err := embed.NewSiliconFlow(key)
	if err != nil {
		return nil, "", err
	}
	return e, "真实嵌入（bge-m3，联网、按量计费）", nil
}

// readDotEnvKey 从 .env 里取 Key。
//
// 复用 config 的解析而不是再写一个：格式规则（引号、export 前缀）只有一处实现，
// 两处会漂移。
func readDotEnvKey() (string, bool) {
	m, err := config.ReadDotEnv(".env")
	if err != nil {
		return "", false
	}
	v, ok := m[config.EnvSiliconFlowAPIKey]
	return v, ok
}

// buildStore 造存储，并做与 cmd/eval 同样的**安全护栏**。
func buildStore(ctx context.Context, kind, dsn string, reset bool, efSearch int) (store.Store, error) {
	switch kind {
	case "memory":
		return store.NewMemory(), nil

	case "postgres":
		if dsn == "" {
			dsn = os.Getenv(config.EnvDatabaseURL)
		}
		if dsn == "" {
			return nil, fmt.Errorf("-store=postgres 需要 -dsn 或 %s", config.EnvDatabaseURL)
		}
		p, err := store.NewPostgres(ctx, dsn, 8, efSearch)
		if err != nil {
			return nil, err
		}
		var n int
		if err := p.Pool().QueryRow(ctx, `SELECT count(*) FROM chunk`).Scan(&n); err != nil {
			_ = p.Close()
			return nil, fmt.Errorf("统计 chunk 行数失败: %w", err)
		}
		if n > 0 {
			if !reset {
				_ = p.Close()
				return nil, fmt.Errorf("目标库里已有 %d 个片段。基准必须跑在干净的库上，"+
					"否则数字建立在残留数据上而报告看不出来。\n"+
					"确认这是专用库之后加 -store-reset", n)
			}
			fmt.Printf("清空目标库（%d 个片段）…\n", n)
			if _, err := p.Pool().Exec(ctx, `TRUNCATE document RESTART IDENTITY CASCADE`); err != nil {
				_ = p.Close()
				return nil, fmt.Errorf("清空失败: %w", err)
			}
		}
		return p, nil

	default:
		return nil, fmt.Errorf("未知的 -store=%q（可选 memory / postgres）", kind)
	}
}

// heapAlloc 返回强制 GC 之后的堆占用。
//
// 不先 GC 的话读到的是"还没回收的垃圾"，同一份数据两次能差出几十 MB，
// 那个数字没有任何意义。
func heapAlloc() uint64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// Latency 是检索耗时的分布（毫秒）。
type Latency struct{ P50, P95, Max float64 }

// measureLatency 跑 nQueries 次真实检索，返回分位数。
//
// 查询词从语料里取（保证 BM25 有事可做），避免出现"全都不命中"的
// 退化情形——那种情况下的延迟不代表有结果时的延迟。
func measureLatency(ctx context.Context, svc *service.Service,
	docs []*types.Document, nQueries, topK int) (time.Duration, Latency, error) {

	queries := pickQueries(docs, nQueries)
	samples := make([]float64, 0, len(queries))

	start := time.Now()
	for _, q := range queries {
		t0 := time.Now()
		if _, err := svc.Search(ctx, q, topK); err != nil {
			return 0, Latency{}, fmt.Errorf("检索失败: %w", err)
		}
		samples = append(samples, float64(time.Since(t0).Microseconds())/1000)
	}
	return time.Since(start), percentiles(samples), nil
}

// percentiles 返回 P50 / P95 / 最大值。
//
// 用最近秩法（nearest-rank）而不是插值：样本量不大时插值会给出
// 一个"比任何一次真实查询都快"的 P95，那是编出来的数。
func percentiles(xs []float64) Latency {
	if len(xs) == 0 {
		return Latency{}
	}
	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)

	at := func(p float64) float64 {
		idx := int(p*float64(len(sorted)) + 0.99999) // 向上取整
		if idx < 1 {
			idx = 1
		}
		if idx > len(sorted) {
			idx = len(sorted)
		}
		return sorted[idx-1]
	}
	return Latency{P50: at(0.50), P95: at(0.95), Max: sorted[len(sorted)-1]}
}

// measureRecall 测 ANN 的召回损失。
//
// # 方法：自召回，不需要标注
//
// 随机取若干片段，**用它们自己的向量当查询**，比较两条路返回的 top-k：
//
//   - 库内检索（postgres → HNSW，近似）
//   - 暴力扫描（内存，精确）
//
// 重合率就是 HNSW 在这个语料上的自召回。这条查询的"正确答案"是确定的
// （它自己），所以不需要人工标注，也不依赖任何评测集。
//
// ⚠️ 自召回是**偏乐观**的估计：查询向量与库内向量同分布，
// 而真实查询的措辞与文档不同。它衡量的是"近似索引丢了多少"，
// 不是"检索质量好不好"。
func measureRecall(ctx context.Context, st store.Store, svc *service.Service,
	docs []*types.Document, topK int, realEmbed bool) error {

	es, ok := st.(store.EmbeddingSearcher)
	if !ok {
		return fmt.Errorf("-recall 需要 -store=postgres（内存后端本来就是精确的，没有损失可测）")
	}

	// 把库里的向量读出来做暴力基线。
	all, err := st.AllChunks(ctx)
	if err != nil {
		return fmt.Errorf("读取全部片段失败: %w", err)
	}
	var withVec []types.Chunk
	for _, c := range all {
		if len(c.Embedding) == types.EmbeddingDim {
			withVec = append(withVec, c)
		}
	}
	if len(withVec) == 0 {
		return fmt.Errorf("库里没有带向量的片段")
	}
	brute := newBruteIndex(withVec)

	// 抽样查询：自己查自己。
	const nProbe = 50
	if len(withVec) < nProbe {
		return fmt.Errorf("片段太少（%d），抽样测不出什么", len(withVec))
	}
	step := len(withVec) / nProbe

	var sumOverlap float64
	var selfHit int
	for i := 0; i < nProbe; i++ {
		probe := withVec[i*step]
		approx, err := es.SearchByEmbedding(ctx, probe.Embedding, topK)
		if err != nil {
			return fmt.Errorf("库内检索失败: %w", err)
		}
		exact := brute.search(probe.Embedding, topK)

		sumOverlap += overlapRatio(approx, exact)
		for _, r := range approx {
			if r.Chunk.ID == probe.ID {
				selfHit++
				break
			}
		}
	}

	fmt.Printf("== ANN 召回（%d 次自召回探针，top-%d）==\n", nProbe, topK)
	fmt.Printf("与暴力扫描的 top-k 重合率   %.3f\n", sumOverlap/nProbe)
	fmt.Printf("自己出现在自己的前 %d 名里   %d/%d\n\n", topK, selfHit, nProbe)

	if !realEmbed {
		fmt.Printf("⚠️ 上面这组召回数字**不可信**：用的是假嵌入，向量是稀疏哈希，\n")
		fmt.Printf("   而 HNSW 在那种分布上的行为不代表真实的稠密向量。\n")
		fmt.Printf("   要拿这组数字下结论，请加 -real-embed 重跑。\n\n")
	}
	return nil
}

// overlapRatio 是两份结果里**同一批片段**的占比（按集合算，不看名次）。
func overlapRatio(a, b []types.SearchResult) float64 {
	if len(b) == 0 {
		return 0
	}
	inB := make(map[int64]struct{}, len(b))
	for _, r := range b {
		inB[r.Chunk.ID] = struct{}{}
	}
	hit := 0
	for _, r := range a {
		if _, ok := inB[r.Chunk.ID]; ok {
			hit++
		}
	}
	return float64(hit) / float64(len(b))
}

// bruteIndex 是精确的暴力扫描基线。
//
// 刻意不复用 retrieve.VectorIndex：那个要 types.Chunk 且会做一堆
// 本文用不上的校验，而这里只要一个干净的对照。
type bruteIndex struct{ chunks []types.Chunk }

func newBruteIndex(cs []types.Chunk) *bruteIndex { return &bruteIndex{chunks: cs} }

func (b *bruteIndex) search(q []float32, topK int) []types.SearchResult {
	type scored struct {
		c types.Chunk
		s float64
	}
	out := make([]scored, 0, len(b.chunks))
	for _, c := range b.chunks {
		out = append(out, scored{c, cosine(q, c.Embedding)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].s > out[j].s })
	if topK > 0 && len(out) > topK {
		out = out[:topK]
	}
	res := make([]types.SearchResult, len(out))
	for i, s := range out {
		res[i] = types.SearchResult{Chunk: s.c, Score: s.s}
	}
	return res
}

// ---------------------------------------------------------------- 语料生成

// generateCorpus 造 n 个片段左右的文档。
//
// perDoc 是每篇文档放多少个片段。分多篇而不是一篇：一篇巨型文档
// 会掩盖"导入 N 篇"这条路径上的开销。
func generateCorpus(n, perDoc int) []*types.Document {
	if perDoc < 1 {
		perDoc = 1
	}
	nDocs := (n + perDoc - 1) / perDoc

	// 固定种子：同一组参数必须跑出同一份语料，否则两次测量不可比。
	rng := rand.New(rand.NewSource(20260912))
	vocab := []string{
		"检索", "向量", "索引", "片段", "文档", "查询", "关键词", "融合",
		"配置", "数据库", "连接池", "超时", "降级", "缓存", "评测", "参数",
		"相似度", "余弦", "分词", "切分", "重叠", "标题", "来源", "元数据",
		"并发", "锁", "事务", "迁移", "回填", "去重", "指纹", "基线",
	}
	// 每篇文档的正文按 perDoc 个片段 × 约 400 字符造，
	// 切分参数是 400/60，所以大致就落在 perDoc 个片段上。
	out := make([]*types.Document, 0, nDocs)
	for d := 0; d < nDocs; d++ {
		var b strings.Builder
		fmt.Fprintf(&b, "# 合成文档 %d\n\n", d)
		for seg := 0; seg < perDoc; seg++ {
			fmt.Fprintf(&b, "## 第 %d 节\n\n", seg)
			for w := 0; w < 60; w++ {
				b.WriteString(vocab[rng.Intn(len(vocab))])
				if w%12 == 11 {
					b.WriteString("。")
				}
			}
			b.WriteString("\n\n")
		}
		out = append(out, &types.Document{
			Title:   fmt.Sprintf("合成文档 %d", d),
			Source:  fmt.Sprintf("bench/synthetic-%d.md", d),
			Content: b.String(),
		})
	}
	return out
}

// pickQueries 从语料里取查询词。
//
// 取片段的**开头若干字**而不是随机词：随机的单字在 BM25 下几乎不命中，
// 量到的会是"空结果时"的延迟。
func pickQueries(docs []*types.Document, n int) []string {
	if len(docs) == 0 {
		return nil
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		runes := []rune(docs[i%len(docs)].Content)
		end := 24
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[:end]))
	}
	return out
}

// cosine 是两个向量的余弦相似度。零向量返回 0（不是 NaN）。
func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
