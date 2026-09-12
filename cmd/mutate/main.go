// Command mutate 是变异测试工具：验证测试套件是否真的有效。
//
// 覆盖率只说明「代码被执行过」，不说明「测试能发现问题」。
// 本工具故意在源码里植入 bug，然后跑测试——测试**必须失败**。
// 测试没失败，说明它守不住它声称守护的东西（假测试）。
//
// 用法：
//
//	go run ./cmd/mutate              # 跑全部变异
//	go run ./cmd/mutate tokenize     # 只跑名字含 tokenize 的
//
// 每条变异是 (名称, 文件, 原文, 替换为) 四元组。工具会：
//
//  1. 备份原文件
//  2. 写入变异
//  3. 跑 go test
//  4. 无论结果如何都恢复原文件
//  5. 报告 CAUGHT / MISSED / BROKEN
//
// 退出码非 0 表示有变异没被抓到。
//
// ⚠️ 关于「原文/替换为」的写法：这里大量使用 Go 的**裸字符串字面量**（反引号），
// 因为它能原样保留缩进和换行——比转义写法可读得多，也让片段和源码长得一样。
// 唯一的例外是片段自身含反引号时（Go 的 struct tag），那种只能用普通字符串。
//
// ⚠️ 匹配是**逐字节**的：Go 的 os.ReadFile 不做换行符转换
// （Python 的 open() 文本模式会做，这是个容易忽略的差异）。
// 本仓库的 .gitattributes 是 `* text=auto eol=lf`，磁盘上都是 LF，
// 所以含换行的片段能正常匹配。换成 CRLF 的话所有多行片段都会失配。
package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// mutation 是一次源码变异。
type mutation struct {
	name string
	file string // 相对仓库根的路径
	old  string // 必须与文件内容**逐字节**一致
	new  string
}

// mutations 是全部变异定义。
//
// 每条都应该对应一句「如果这里写错了，哪个测试会失败」。
// 如果某条变异没被抓到，要么补测试，要么承认这块其实没被守护。
var mutations = []mutation{
	// ---- tokenize ----
	{
		name: "tokenize: 单字 CJK 不回吐 unigram",
		file: "internal/tokenize/tokenizer.go",
		old:  `out = append(out, string(cjk))`,
		new:  `_ = cjk`,
	},
	{
		name: "tokenize: 不做全角归一",
		file: "internal/tokenize/tokenizer.go",
		old: `case r >= 0xFF01 && r <= 0xFF5E:
			b.WriteRune(r - 0xFEE0)`,
		new: `case r >= 0xFF01 && r <= 0xFF5E:
			b.WriteRune(r)`,
	},
	{
		name: "tokenize: 拉丁词不转小写",
		file: "internal/tokenize/tokenizer.go",
		old:  `out = append(out, strings.ToLower(string(word)))`,
		new:  `out = append(out, string(word))`,
	},
	{
		name: "tokenize: 汉字判断改用 Ideographic",
		file: "internal/tokenize/tokenizer.go",
		old:  `return unicode.Is(unicode.Han, r) ||`,
		new:  `return unicode.Is(unicode.Ideographic, r) ||`,
	},
	{
		name: "tokenize: 标点不冲掉缓冲",
		file: "internal/tokenize/tokenizer.go",
		old: `default:
			// 标点、空白、符号：两者都冲掉。
			// 这一步不能省——否则 "abc中文" 会把 abc 和 中文 粘成一个段。
			flushCJK()
			flushWord()`,
		new: `default:
			_ = word
			_ = cjk`,
	},
	{
		name: "tokenize: bigram 循环少一位",
		file: "internal/tokenize/tokenizer.go",
		old:  `for i := 0; i+1 < len(cjk); i++ {`,
		new:  `for i := 0; i+2 < len(cjk); i++ {`,
	},
	{
		name: "tokenize: 空串返回 nil",
		file: "internal/tokenize/tokenizer.go",
		old: `if s == "" {
		return []string{}
	}`,
		new: `if s == "" {
		return nil
	}`,
	},

	// ---- chunk ----
	// 这条对应开发时真实踩到的 bug：hitEnd 在去空白之前算，
	// 改成 false 后短段落会退化成一个字符一段。
	{
		name: "chunk: 破坏 hitEnd 判断（真实踩过的 bug）",
		file: "internal/chunk/chunker.go",
		old:  `hitEnd := end >= sec.end`,
		new:  `hitEnd := false`,
	},
	{
		name: "chunk: 不写标题面包屑",
		file: "internal/chunk/chunker.go",
		old:  `if sec.breadcrumb != "" {`,
		new:  `if false {`,
	},
	{
		name: "chunk: 取消片段重叠",
		file: "internal/chunk/chunker.go",
		old:  `next := end - c.cfg.OverlapRunes`,
		new:  `next := end`,
	},
	{
		name: "chunk: #hashtag 被误判为标题",
		file: "internal/chunk/chunker.go",
		old: `if n < len(line) && line[n] != ' ' && line[n] != '\t' {
		return 0, "", false
	}`,
		new: `if false {
		return 0, "", false
	}`,
	},
	{
		name: "chunk: isSpace 不识别全角空格",
		file: "internal/chunk/chunker.go",
		old:  `return unicode.IsSpace(r)`,
		new:  `return unicode.IsSpace(r) && r < 0x80`,
	},
	{
		name: "chunk: 不校验 overlap < maxRunes",
		file: "internal/chunk/chunker.go",
		old:  `if c.OverlapRunes >= c.MaxRunes {`,
		new:  `if false {`,
	},
	{
		name: "chunk: 不在句末标点断开",
		file: "internal/chunk/chunker.go",
		old: `} else if brk := lastSentenceBreak(runes, pos, end); brk > pos {
			end = brk
		}`,
		new: `}`,
	},

	// ---- embed ----
	// ⚠️ 这条是唯一含反引号的片段（Go 的 struct tag），
	// 不能用裸字符串，只能退回普通字符串 + 转义。
	{
		name: "embed: 请求体里加了 dimensions 字段（DESIGN §2 红线）",
		file: "internal/embed/siliconflow.go",
		old:  "\tEncodingFormat string   `json:\"encoding_format\"`\n}",
		new:  "\tEncodingFormat string   `json:\"encoding_format\"`\n\tDimensions     int      `json:\"dimensions\"`\n}",
	},
	{
		name: "embed: 不按 32 条分批",
		file: "internal/embed/siliconflow.go",
		old:  `for start := 0; start < len(texts); start += MaxBatchSize {`,
		new:  `for start := 0; start < len(texts); start += len(texts) {`,
	},
	{
		name: "embed: 对 400 也重试",
		file: "internal/embed/siliconflow.go",
		old: `if json.Unmarshal(raw, &se) == nil && se.Message != "" {
			return nil, false, &se
		}`,
		new: `if json.Unmarshal(raw, &se) == nil && se.Message != "" {
			return nil, true, &se
		}`,
	},
	{
		name: "embed: 不校验返回维度",
		file: "internal/embed/siliconflow.go",
		old:  `if len(d.Embedding) != types.EmbeddingDim {`,
		new:  `if false {`,
	},
	{
		name: "embed: 不按 index 排序响应",
		file: "internal/embed/siliconflow.go",
		old:  `sort.Slice(out.Data, func(i, j int) bool { return out.Data[i].Index < out.Data[j].Index })`,
		new:  `_ = sort.Slice`,
	},
	{
		name: "embed: 假嵌入不做归一化",
		file: "internal/embed/fake.go",
		old:  `norm := float32(math.Sqrt(sum))`,
		new:  `norm := float32(math.Sqrt(sum)*0 + 1)`,
	},
	{
		name: "embed: 不校验空文本",
		file: "internal/embed/fake.go",
		old:  `if strings.TrimSpace(t) == "" {`,
		new:  `if strings.HasPrefix(t, "\x00") {`,
	},

	// ---- retrieve / BM25 ----
	{
		name: "bm25: IDF 换回 Robertson 原始版（会出负数）",
		file: "internal/retrieve/bm25.go",
		old:  `return math.Log(1 + (n-df+0.5)/(df+0.5))`,
		new:  `return math.Log((n - df + 0.5) / (df + 0.5))`,
	},
	{
		name: "bm25: 建索引时直接读 Content 而非 IndexText",
		file: "internal/retrieve/bm25.go",
		old:  `tokens := m.tk.Tokenize(c.IndexText())`,
		new:  `tokens := m.tk.Tokenize(c.Content)`,
	},
	{
		name: "bm25: 关闭长度归一化（b=0）",
		file: "internal/retrieve/bm25.go",
		old:  `DefaultB  = 0.75`,
		new:  `DefaultB  = 0`,
	},
	{
		name: "bm25: 查询词不去重",
		file: "internal/retrieve/bm25.go",
		old:  `if !seen[q] {`,
		new:  `if true {`,
	},
	{
		name: "bm25: 名次改成 0-based",
		file: "internal/retrieve/bm25.go",
		old:  `LexicalRank:  i + 1, // 1-based`,
		new:  `LexicalRank:  i, // 0-based`,
	},

	// ---- retrieve / 向量 ----
	{
		name: "vector: 不除模长（退化成点积）",
		file: "internal/retrieve/vector.go",
		old:  `sim := float64(dot(query, v.vecs[i]) / (qNorm * v.norms[i]))`,
		new:  `sim := float64(dot(query, v.vecs[i]))`,
	},
	{
		name: "vector: 不校验查询向量维度",
		file: "internal/retrieve/vector.go",
		old:  `if len(query) != types.EmbeddingDim {`,
		new:  `if false {`,
	},
	{
		name: "vector: 零向量查询不报错",
		file: "internal/retrieve/vector.go",
		old: `if qNorm == 0 {
		return nil, ErrZeroVector
	}`,
		new: `if false {
		return nil, ErrZeroVector
	}`,
	},
	{
		name: "vector: 不跳过零模长文档",
		file: "internal/retrieve/vector.go",
		old: `if v.norms[i] == 0 {
			continue
		}`,
		new: `if false {
			continue
		}`,
	},

	// ---- retrieve / RRF ----
	{
		name: "rrf: 去重用 Chunk.ID 而非 StableKey（落库前全是 0）",
		file: "internal/retrieve/rrf.go",
		old:  `key := r.Chunk.StableKey()`,
		new:  `key := string(rune(r.Chunk.ID))`,
	},
	{
		name: "rrf: 直接把 0 号名次代入公式（未召回拿最高分）",
		file: "internal/retrieve/rrf.go",
		old:  `s.Score = s.RRFScore(k)`,
		new:  `s.Score = 1/float64(k+s.LexicalRank) + 1/float64(k+s.VectorRank)`,
	},

	// ---- retrieve / hybrid ----
	{
		name: "hybrid: 单路失败就整体失败（取消降级）",
		file: "internal/retrieve/hybrid.go",
		old: `if p.err != nil {
				errs = append(errs, p.err)
				h.reportError(p.run.Retriever, p.err)
				continue
			}`,
		new: `if p.err != nil {
				return nil, p.err
			}`,
	},
	{
		name: "hybrid: 每路只取 topK 不做候选放大",
		file: "internal/retrieve/hybrid.go",
		old:  `n := topK * h.mult`,
		new:  `n := topK`,
	},
	{
		name: "hybrid: 不施加超时",
		file: "internal/retrieve/hybrid.go",
		old:  `ctx, cancel := context.WithTimeout(ctx, h.timeout)`,
		new:  `ctx, cancel := context.WithCancel(ctx)`,
	},
	{
		name: "hybrid: 两路都失败时也不报错",
		file: "internal/retrieve/hybrid.go",
		old: `if len(runs) == 0 {
		return nil, fmt.Errorf("%w: %v", ErrBothRetrieversFailed, errors.Join(errs...))
	}`,
		new: `if false {
		return nil, fmt.Errorf("%w: %v", ErrBothRetrieversFailed, errors.Join(errs...))
	}`,
	},

	// ---- mcp / 溯源字段 ----
	// 下面两条对应一个**真实存在过的缺陷**：ResultItem.Source 声明了、
	// jsonschema 里也写了描述（Agent 看得见），但 toResultItem 从不给它赋值，
	// 而 Source 挂在 Document 上、检索路径里只有 Chunk —— 它永远是空的。
	//
	// 为什么值得单独记一笔：这个字段当时**没有任何测试**。
	// DTO 有字段、schema 有条目，看起来"实现了"，实际上是个空壳。
	// 声明 ≠ 赋值，这条变异就是用来钉死这个区别的。
	{
		name: "mcp: 结果不填文档来源（真实缺陷）",
		file: "internal/mcp/server.go",
		old:  "\t\tSource: r.Chunk.Metadata[types.MetadataKeySource],",
		new:  "\t\tSource: \"\",",
	},
	{
		name: "ingest: 不把文档来源下沉到片段元数据",
		file: "internal/ingest/ingest.go",
		old:  `chunks[i].SetMetadata(types.MetadataKeySource, doc.Source)`,
		new:  `_ = chunks[i]`,
	},

	// ---- eval ----
	// 评测集是这一整个里程碑的**基准尺**。尺子本身错了，后面所有数据都不可信，
	// 而且错的方式往往很安静：命中率偏低一点，看起来只是「检索效果一般」。
	// 这几条变异就是用来钉死「尺子不会悄悄坏掉」的。
	{
		// 引文定位的偏移单位。按字节算的话，中文文档上偏移会偏大近 3 倍，
		// 代码照样编译、照样运行、照样给出一个看起来合理的命中率。
		name: "eval: 引文偏移按字节算（不是 rune）",
		file: "internal/eval/locate.go",
		old:  `start = utf8.RuneCountInString(content[:idx])`,
		new:  `start = idx`,
	},
	{
		// 半开区间用 a<d && c<b 判重叠，只在区间非空时与 max/min 等价。
		name: "eval: 区间重叠判据换回 a<d && c<b",
		file: "internal/eval/locate.go",
		old:  `return max(s.Start, o.Start) < min(s.End, o.End)`,
		new:  `return s.Start < o.End && o.Start < s.End`,
	},
	{
		// 引文出现多次却取第一次：标注者以为标了 A 处、实际测的是 B 处，
		// 而结果看起来完全正常。
		name: "eval: 引文重复时不报错，直接取第一次",
		file: "internal/eval/locate.go",
		old:  `if !explicit && strings.Index(content[from:], quote) >= 0 {`,
		new:  `if false && strings.Index(content[from:], quote) >= 0 {`,
	},
	{
		name: "eval: 不校验语料指纹（语料漂移静默通过）",
		file: "internal/eval/corpus.go",
		old:  `if !strings.EqualFold(got, f.SHA256) {`,
		new:  `if false {`,
	},
	{
		// 只归一化 CRLF 而漏掉单独的 CR，跨平台指纹就会不一致。
		name: "eval: 换行归一化漏掉单独的 CR",
		file: "internal/eval/corpus.go",
		old: `	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")`,
		new: `	s = strings.ReplaceAll(s, "\r\n", "\n")
	return s`,
	},
	{
		name: "eval: 不查重复 query",
		file: "internal/eval/load.go",
		old:  `if prev, dup := seenQuery[key]; dup {`,
		new:  `if prev, dup := seenQuery[key]; false && dup {`,
	},
	{
		// 手写评测集时最常见的错误就是字段名拼错，
		// 标准解析器会静默忽略未知字段，那条期望命中就凭空消失了。
		name: "eval: 容忍未知字段（拼错的字段被静默忽略）",
		file: "internal/eval/load.go",
		old:  `dec.DisallowUnknownFields()`,
		new:  `_ = dec`,
	},
	{
		// 重打指纹时直接沿用旧值：改过语料也「看不出变化」，
		// 于是漂移被永久掩盖，而重打命令还报告「已是最新」。
		name: "eval: 重打指纹时沿用旧值（永不发现变化）",
		file: "internal/eval/corpus.go",
		old:  `got := Fingerprint(text)`,
		new:  `got := f.SHA256`,
	},
	{
		name: "eval: 省略的 grade 补成「部分相关」而非「完全回答」",
		file: "internal/eval/load.go",
		old:  `e.Grade = GradeFull`,
		new:  `e.Grade = GradePartial`,
	},
}

// ---------------------------------------------------------------------------
// 中断保护
//
// 变异的过程是「改坏源码 → 跑测试 → 改回来」。如果在中间被 Ctrl-C 打断，
// 源码会**停在改坏的状态**——下次编译莫名其妙就挂了，而且很难联想到是这个工具干的。
// 所以装一个信号处理器，退出前把当前变异还原回去。
// ---------------------------------------------------------------------------

var (
	restoreMu     sync.Mutex
	currentFile   string
	currentBackup string
)

func installInterruptGuard() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		restoreMu.Lock()
		if currentFile != "" && currentBackup != "" {
			if err := copyFile(currentBackup, currentFile); err == nil {
				fmt.Fprintf(os.Stderr, "\n已中断，源码已还原：%s\n", currentFile)
			} else {
				fmt.Fprintf(os.Stderr, "\n已中断，但还原失败（%v）！备份在 %s\n",
					err, currentBackup)
			}
			currentFile, currentBackup = "", ""
		}
		restoreMu.Unlock()
		os.Exit(130)
	}()
}

// ---------------------------------------------------------------------------
// 文件与子进程
// ---------------------------------------------------------------------------

// repoRoot 从当前目录向上找 go.mod。
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("向上找不到 go.mod，请在仓库内运行")
		}
		dir = parent
	}
}

// copyFile 复制文件并保留原权限。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// writeFileKeepingMode 覆盖写文件，保留原权限。
//
// 不直接用 os.WriteFile 是因为它会把权限重置成传入的 mode，
// 脚本类文件的可执行位可能因此丢掉。
func writeFileKeepingMode(path string, data []byte) error {
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode()
	}
	return os.WriteFile(path, data, mode)
}

// runTests 跑某个包（含子包）的测试，返回退出码和合并后的输出。
func runTests(root, pkgDir string) (int, string) {
	cmd := exec.Command("go", "test", "./"+filepath.ToSlash(pkgDir)+"/...")
	cmd.Dir = root

	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), string(out)
	}
	return -1, string(out)
}

// firstFailure 从测试输出里挑出最能说明问题的一行。
func firstFailure(output string) string {
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "--- FAIL") || strings.HasPrefix(s, "FAIL") {
			return s
		}
	}
	for _, line := range lines {
		if strings.Contains(line, "_test.go:") {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// isBuildFailure 判断失败是不是「变异本身写坏了」。
//
// ⚠️ 编译失败不算「测试抓到」——那是变异写错了，不能证明测试有效。
// 必须区分开，否则会虚报通过率：一个永远编译不过的变异看起来"总能失败"。
func isBuildFailure(output string) bool {
	for _, marker := range []string{"[build failed]", "cannot use", "is not used"} {
		if strings.Contains(output, marker) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 主流程
// ---------------------------------------------------------------------------

func main() {
	os.Exit(run())
}

func run() int {
	installInterruptGuard()

	root, err := repoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	var filter string
	if len(os.Args) > 1 {
		filter = os.Args[1]
	}

	var items []mutation
	for _, m := range mutations {
		if filter == "" || strings.Contains(m.name, filter) {
			items = append(items, m)
		}
	}
	if len(items) == 0 {
		fmt.Println("没有匹配的变异定义")
		return 1
	}

	// ---- 基线：未变异时必须全过 ----
	//
	// 基线红了的话，后面所有变异都会"失败"，结果全是 CAUGHT——
	// 那是彻底的假阳性。必须先确认起点是绿的。
	fmt.Println("=== 基线（未变异，应全过）===")
	pkgSet := map[string]bool{}
	var pkgs []string
	for _, m := range items {
		p := filepath.ToSlash(filepath.Dir(m.file))
		if !pkgSet[p] {
			pkgSet[p] = true
			pkgs = append(pkgs, p)
		}
	}
	for _, p := range pkgs {
		code, out := runTests(root, p)
		status := "PASS"
		if code != 0 {
			status = "FAIL"
		}
		fmt.Printf("  %s  %s\n", status, p)
		if code != 0 {
			fmt.Println("  基线就没过，后续变异结果无意义")
			fmt.Println(tail(out, 800))
			return 1
		}
	}

	fmt.Printf("\n=== 变异测试（%d 条）===\n", len(items))
	caught, broken, skipped := 0, 0, 0

	for _, m := range items {
		status, detail := runOne(root, m)
		switch status {
		case "CAUGHT":
			caught++
			fmt.Printf("  [CAUGHT  ] %s\n", m.name)
			if detail != "" {
				fmt.Printf("             -> %s\n", detail)
			}
		case "BROKEN":
			broken++
			fmt.Printf("  [BROKEN  ] %s   <-- 变异导致编译失败，无法判定，请改写这条变异\n", m.name)
		case "SKIP":
			skipped++
			fmt.Printf("  [SKIP    ] %s\n", m.name)
			fmt.Printf("             变异点未找到：%s\n", m.file)
		default:
			fmt.Printf("  [MISSED  ] %s   <-- 测试没抓到，可能是假测试\n", m.name)
		}
	}

	fmt.Printf("\n结果：%d/%d 个变异被抓到", caught, len(items))
	if broken > 0 {
		fmt.Printf("，%d 条变异写坏了（编译失败）", broken)
	}
	if skipped > 0 {
		fmt.Printf("，%d 条变异点失配（源码改过了？）", skipped)
	}
	fmt.Println()

	// 只有「全部被抓到、且没有写坏的、也没有失配的」才算通过。
	// 失配也必须算失败：它意味着变异悄悄没跑，而输出看起来一切正常。
	if caught == len(items) && broken == 0 && skipped == 0 {
		return 0
	}
	return 1
}

// runOne 执行一条变异，返回状态和附加说明。
func runOne(root string, m mutation) (status, detail string) {
	path := filepath.Join(root, filepath.FromSlash(m.file))
	backup := path + ".mutation-backup"

	src, err := os.ReadFile(path)
	if err != nil {
		return "SKIP", fmt.Sprintf("读取失败: %v", err)
	}
	if !strings.Contains(string(src), m.old) {
		return "SKIP", ""
	}

	if err := copyFile(path, backup); err != nil {
		return "SKIP", fmt.Sprintf("备份失败: %v", err)
	}

	// 登记当前变异，中断处理器要靠它还原
	restoreMu.Lock()
	currentFile, currentBackup = path, backup
	restoreMu.Unlock()

	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		_ = copyFile(backup, path)
		_ = os.Remove(backup)
		restoreMu.Lock()
		currentFile, currentBackup = "", ""
		restoreMu.Unlock()
	}
	// 无论走哪条分支、还是中途 return，都必须还原
	defer restore()

	mutated := strings.Replace(string(src), m.old, m.new, 1)
	if err := writeFileKeepingMode(path, []byte(mutated)); err != nil {
		return "SKIP", fmt.Sprintf("写入失败: %v", err)
	}

	code, out := runTests(root, filepath.ToSlash(filepath.Dir(m.file)))

	switch {
	case isBuildFailure(out):
		return "BROKEN", ""
	case code != 0:
		return "CAUGHT", firstFailure(out)
	default:
		return "MISSED", ""
	}
}

// tail 返回末尾 n 个字节（按 rune 边界对齐，避免切碎中文）。
func tail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n:])
}
