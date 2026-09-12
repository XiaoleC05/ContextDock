// Command demo 生成 README 用的演示素材。
//
// 它跑两条真实路径，把输出渲染成一张终端样式的 SVG：
//
//  1. go run ./cmd/eval   —— 检索质量评测（假嵌入，确定、不花钱）
//  2. 一次 MCP 检索       —— 真实嵌入，走完整的 initialize → 导入 → 检索
//
// ⚠️ 这张图里的**每一行都是真实命令的输出**，渲染器只加了一个终端外框，
// 不做换行、不做截断、不重排、不"美化"数字。素材要能被当作证据，
// 前提就是它没有经过任何会改变内容的手。
//
// 用法：
//
//	go run ./cmd/demo                     # 生成 docs/demo.svg 并打印到终端
//	go run ./cmd/demo -query "你的问题"    # 换一条查询
//	go run ./cmd/demo -no-search          # 跳过真实检索（没有 API Key 时）
//
// 真实检索需要 .env 里有可用的 SILICONFLOW_API_KEY，且**会真的调用 API**
// （一次导入 + 一次查询，成本不到一分钱）。日志里 Key 是打码的，
// 不会进素材。
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// eval 输出里用来切片的分节标记。
//
// ⚠️ 这两个常量依赖 cmd/eval 的输出文案，改那边的表头就要同步改这里。
//
// 没有自动化测试盯着它们——真去跑一遍 cmd/eval 要好几秒，为一个字符串
// 常量在每次 go test 里付这个代价不划算。兜底的是**生成期硬失败**：
// sliceEval 找不到标记就报错退出，不会静默地退回"整段输出全塞进图里"。
// 也就是说素材可能过期，但不会悄悄变成一张错的图。
const (
	markerEvalStart = "按子集分通道成绩："
	markerEvalEnd   = "top-k 平均覆盖的不同文档数："
)

func main() {
	var (
		out      = flag.String("out", filepath.Join("docs", "demo.svg"), "SVG 输出路径")
		bin      = flag.String("bin", "", "contextdock 可执行文件；留空则构建到临时目录")
		query    = flag.String("query", "为什么不直接把两路分数加权求和", "演示用的检索问题")
		doc      = flag.String("doc", filepath.Join("docs", "DESIGN.md"), "演示时导入的文档")
		topK     = flag.Int("topk", 3, "演示时返回的结果条数")
		noSearch = flag.Bool("no-search", false, "跳过真实检索（没有 API Key 时）")
		evalFake = flag.Bool("eval-fake-embed", false,
			"评测表用假嵌入跑（不联网、完全确定，但数字不代表真实质量）")
		root = flag.String("root", ".", "仓库根目录")
	)
	flag.Parse()

	if err := run(*root, *out, *bin, *doc, *query, *topK, *noSearch, *evalFake); err != nil {
		fmt.Fprintf(os.Stderr, "生成演示素材失败: %v\n", err)
		os.Exit(1)
	}
}

func run(root, out, binPath, doc, query string, topK int, noSearch, evalFake bool) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}

	var lines []Line

	// ---- 第一段：评测表 ----
	//
	// ⚠️ 必须标出用的是哪种嵌入。项目里同时存在两套数字：
	// CI 的质量门禁跑**假嵌入**（能不联网、完全确定，所以能进 CI），
	// 而真实质量得用真嵌入跑。两者相差很大（融合 recall 0.490 vs 0.667），
	// 不标注的话读者会以为其中一个是错的。
	evalArgs := []string{"run", "./cmd/eval", "-quiet"}
	if evalFake {
		evalArgs = append(evalArgs, "-fake-embed")
		lines = append(lines, Prompt("$ go run ./cmd/eval -fake-embed"))
		lines = append(lines, Dim("# 假嵌入：不联网、完全确定。数字**不代表真实检索质量**，只用于回归门禁。"))
	} else {
		lines = append(lines, Prompt("$ go run ./cmd/eval"))
		lines = append(lines, Dim("# 真实嵌入（bge-m3）：数字是真实检索质量，需要 API Key。"))
	}
	lines = append(lines, Plain(""))

	evalOut, err := runEval(absRoot, evalArgs)
	if err != nil {
		return err
	}
	section, err := sliceEval(evalOut)
	if err != nil {
		return err
	}
	for _, l := range strings.Split(strings.TrimRight(section, "\n"), "\n") {
		lines = append(lines, Plain(l))
	}

	// ---- 第二段：真实检索 ----
	lines = append(lines, Plain(""))
	if noSearch {
		lines = append(lines, Dim("（已跳过真实检索：-no-search）"))
	} else {
		searchLines, err := runSearch(absRoot, binPath, doc, query, topK)
		if err != nil {
			return err
		}
		lines = append(lines, searchLines...)
	}

	footer := fmt.Sprintf("由 go run ./cmd/demo 生成 · %s · 内容为真实运行输出，未经改写",
		time.Now().Format("2006-01-02"))

	svg := RenderSVG("contextdock — 检索质量与一次真实检索", lines, footer)

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return fmt.Errorf("创建输出目录失败: %w", err)
	}
	if err := os.WriteFile(out, []byte(svg), 0o644); err != nil {
		return fmt.Errorf("写 %s 失败: %w", out, err)
	}

	// 同时打到终端：不落盘也能一眼看到素材内容，方便先看再决定要不要用
	for _, l := range lines {
		fmt.Println(l.Text)
	}
	fmt.Println()
	fmt.Println(footer)
	fmt.Printf("\n→ 已写入 %s\n", out)

	return nil
}

// runEval 跑评测器并返回完整 stdout。
//
// 刻意走子进程而不是直接调 internal/eval：这样素材里出现的
// 就是**用户自己跑那条命令会看到的东西**，包括参数行和说明行。
// 在进程内重排一遍表格，图会更好看，但也就失去了"这是命令输出"的意义。
func runEval(root string, args []string) (string, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = root

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go run ./cmd/eval 失败: %w\n%s", err, stderr.String())
	}
	return stdout.String(), nil
}

// sliceEval 只保留检索质量那一段。
//
// 完整输出还有参数、指纹、延迟、分数分布等小节，几十行。
// 素材要让人 30 秒看懂，所以只留"各通道成绩 + 融合相对单路的增益"这两张表——
// 第二张正是「融合不如单路」那条反直觉结论的证据。
func sliceEval(full string) (string, error) {
	start := strings.Index(full, markerEvalStart)
	if start < 0 {
		return "", fmt.Errorf("cmd/eval 的输出里找不到标记 %q——"+
			"多半是表头文案改了，请同步更新 cmd/demo 的常量", markerEvalStart)
	}
	end := strings.Index(full, markerEvalEnd)
	if end < 0 || end < start {
		// 不复用上面那条的文案：错误信息常常是被单独贴出来看的，
		// "同上"在脱离上下文时等于没说。
		return "", fmt.Errorf("cmd/eval 的输出里找不到标记 %q（或它在起始标记之前）——"+
			"多半是表头文案改了，请同步更新 cmd/demo 的常量", markerEvalEnd)
	}
	return full[start:end], nil
}

// runSearch 拉一个 MCP server，做一次真实的导入 + 检索。
func runSearch(root, binPath, doc, query string, topK int) ([]Line, error) {
	if binPath == "" {
		var err error
		binPath, err = buildBinary(root)
		if err != nil {
			return nil, err
		}
		defer func() { _ = os.Remove(binPath) }()
	}

	// 内存存储：演示不该往用户的库里写东西。
	// 真实嵌入：假嵌入找不到语义相关但用词不同的段落，
	// 而"用词不同也能找到"恰恰是要展示的东西。
	srv, err := startMCPServer(binPath, root, map[string]string{
		"CONTEXTDOCK_USE_MEMORY_STORE": "true",
		"CONTEXTDOCK_FAKE_EMBEDDER":    "false",
	})
	if err != nil {
		return nil, err
	}
	defer srv.Close()

	if err := srv.Handshake("cmd/demo"); err != nil {
		return nil, fmt.Errorf("%w\n启动日志：\n%s", err, strings.Join(srv.Logs(), "\n"))
	}

	content, err := os.ReadFile(filepath.Join(root, doc))
	if err != nil {
		return nil, fmt.Errorf("读取演示文档 %s 失败: %w", doc, err)
	}

	importResp, err := srv.CallTool("import_document", map[string]any{
		"content": string(content),
		"title":   filepath.Base(doc),
		"source":  filepath.ToSlash(doc),
	})
	if err != nil {
		return nil, err
	}

	out := []Line{
		Plain(""),
		Dim("# 接上 Agent 之后，一次真实嵌入的检索（不是假嵌入）"),
		Query(fmt.Sprintf("> search_knowledge_base { \"query\": %q, \"top_k\": %d }", query, topK)),
		Dim(fmt.Sprintf("  已导入 %s：%v 个片段，%v 个带向量",
			filepath.Base(doc), importResp["chunk_count"], importResp["embedded_count"])),
		Plain(""),
	}

	searchResp, err := srv.CallTool("search_knowledge_base", map[string]any{
		"query": query,
		"top_k": topK,
	})
	if err != nil {
		return nil, err
	}

	results, _ := searchResp["results"].([]any)
	if len(results) == 0 {
		out = append(out, Dim("  （没有命中结果）"))
		return out, nil
	}

	for _, raw := range results {
		r, _ := raw.(map[string]any)
		out = append(out, formatResult(r)...)
	}
	return out, nil
}

// formatResult 把一条检索结果排成两三行。
//
// 这是**演示里唯一做了格式化**的地方，且只做取舍（截断正文、拼标题行），
// 不改数字：分数字段原样用 fmt 打出，连精度都不调整。
func formatResult(r map[string]any) []Line {
	heading := stringField(r, "heading")
	if heading == "" {
		heading = "(无标题)"
	}
	matched := stringSliceField(r, "matched_by")

	head := fmt.Sprintf("  [%.4f] %s", floatField(r, "score"), heading)
	if len(matched) > 0 {
		head += "  matched_by=[" + strings.Join(matched, " ") + "]"
	}
	out := []Line{{Text: head, Style: StyleEmph}}

	body := strings.Join(strings.Fields(stringField(r, "content")), " ")
	out = append(out, Plain("     "+truncate(body, 96)))

	if src := stringField(r, "source"); src != "" {
		out = append(out, Dim("     source="+src))
	}
	return out
}

// truncate 按**显示宽度**截断，避免把中文从中间切断成半个词。
func truncate(s string, maxCols int) string {
	if displayWidth(s) <= maxCols {
		return s
	}
	var b strings.Builder
	w := 0
	for _, r := range s {
		rw := 1
		if isWide(r) {
			rw = 2
		}
		if w+rw > maxCols-1 {
			break
		}
		b.WriteRune(r)
		w += rw
	}
	return b.String() + "…"
}

func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func floatField(m map[string]any, key string) float64 {
	f, _ := m[key].(float64)
	return f
}

func stringSliceField(m map[string]any, key string) []string {
	raw, _ := m[key].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// buildBinary 把 contextdock 构建到临时目录。
func buildBinary(root string) (string, error) {
	dir, err := os.MkdirTemp("", "contextdock-demo-")
	if err != nil {
		return "", fmt.Errorf("创建临时目录失败: %w", err)
	}
	name := "contextdock"
	if filepath.Separator == '\\' {
		name += ".exe"
	}
	path := filepath.Join(dir, name)

	cmd := exec.Command("go", "build", "-o", path, "./cmd/contextdock")
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("构建 contextdock 失败: %w\n%s", err, stderr.String())
	}
	return path, nil
}
