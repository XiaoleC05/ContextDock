// Command smoke 是端到端冒烟测试：把编译好的 contextdock 当 MCP server 跑起来，
// 用 stdio 发 JSON-RPC。
//
// 单元测试覆盖不到的接缝，只有这里能发现：
//
//   - 进程能不能启动、配置缺失时是否报可读错误
//   - stdio 协议通不通（**stdout 有没有被日志污染**）
//   - 工具和参数 schema 是否被正确暴露
//   - 嵌入失败时检索能否降级到关键词而不是整个失败
//   - 溯源字段过完 JSON-RPC 序列化之后还在不在
//
// 用法：
//
//	go run ./cmd/smoke [可执行文件路径]
//
// 默认路径是 bin/contextdock.exe（Windows）或 bin/contextdock。
// 需要本机有可用的 PostgreSQL + pgvector，或者设置
// CONTEXTDOCK_USE_MEMORY_STORE=true 走内存存储。
//
// 为什么是 Go 而不是 Python：这个项目到处都要求「装了 Go 就能跑」，
// 唯独测试链路额外要一个 Python 解释器，CI 里要单独伺候、贡献者本地要单独装。
// 换成 Go 之后 `go run ./cmd/smoke` 在任何有 Go 的机器上都能直接跑，
// 而且和被测程序共用一套错误处理习惯。
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 仓库根定位
// ---------------------------------------------------------------------------

// repoRoot 从当前目录向上找 go.mod，找到就是仓库根。
//
// 比"假定用户在根目录执行"更稳：在 cmd/smoke/ 里直接 go run . 也能跑。
// 找不到时退回当前目录——不报错，因为后续的路径检查会给出更具体的提示。
func repoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir { // 到根了
			wd, _ := os.Getwd()
			return wd
		}
		dir = parent
	}
}

// defaultExe 返回默认的可执行文件路径。
func defaultExe(root string) string {
	name := "contextdock"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(root, "bin", name)
}

// ---------------------------------------------------------------------------
// 子进程驱动
// ---------------------------------------------------------------------------

// server 驱动一个 stdio MCP server 子进程。
type server struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader

	// mu 保护 stderr 和 id。stderr 由独立的 goroutine 持续写入，
	// 主 goroutine 会读它——不保护就是数据竞争。
	mu     sync.Mutex
	stderr []string
	id     int
}

// newServer 启动子进程。
//
// exe 转成绝对路径：Windows 上 CreateProcess 不认相对路径。
// cwd 固定成仓库根，这样二进制能找到 .env（它按 CWD 相对查找）。
func newServer(exe, root string, env []string) (*server, error) {
	abs, err := filepath.Abs(exe)
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(abs)
	cmd.Dir = root
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	s := &server{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout)}
	go s.drainStderr(stderr)
	return s, nil
}

// drainStderr 持续把 stderr 收进内存。
//
// ⚠️ 必须有人读 stderr。管道缓冲区写满之后子进程会**阻塞在写日志上**，
// 表现为整个测试卡死——而且日志越多越容易触发，看起来像是被测程序的问题。
func (s *server) drainStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	// 默认上限 64KB 对单行来说够用，但日志里可能有长文档片段，给宽一点。
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		s.mu.Lock()
		s.stderr = append(s.stderr, strings.TrimRight(sc.Text(), "\r\n"))
		s.mu.Unlock()
	}
}

// stderrTail 返回最后 n 行日志。
func (s *server) stderrTail(n int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.stderr) <= n {
		return append([]string(nil), s.stderr...)
	}
	return append([]string(nil), s.stderr[len(s.stderr)-n:]...)
}

func (s *server) stderrCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.stderr)
}

// write 发一条 JSON-RPC 消息。
func (s *server) write(msg map[string]any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = s.stdin.Write(b)
	return err
}

// notify 发一条不需要响应的通知。
func (s *server) notify(method string, params any) error {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	return s.write(msg)
}

// request 发一条请求并读取响应。
func (s *server) request(method string, params any) (map[string]any, error) {
	s.mu.Lock()
	s.id++
	id := s.id
	s.mu.Unlock()

	msg := map[string]any{"jsonrpc": "2.0", "method": method, "id": id}
	if params != nil {
		msg["params"] = params
	}
	if err := s.write(msg); err != nil {
		return nil, err
	}

	line, err := s.stdout.ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return nil, fmt.Errorf("等待 %s 的响应时 server 关闭了 stdout: %w", method, err)
	}

	var resp map[string]any
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		return nil, fmt.Errorf("解析 %s 的响应失败: %w（原始内容 %q）", method, err, line)
	}
	return resp, nil
}

// callTool 调用工具，并把工具返回的文本载荷解析成 map。
func (s *server) callTool(name string, args map[string]any) (map[string]any, error) {
	resp, err := s.request("tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return nil, err
	}
	if e, ok := resp["error"]; ok {
		return nil, fmt.Errorf("工具 %s 返回错误: %v", name, e)
	}

	content := asSlice(asMap(resp["result"])["content"])
	if len(content) == 0 {
		return map[string]any{}, nil
	}
	text := asString(asMap(content[0])["text"])

	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return nil, fmt.Errorf("解析 %s 的输出失败: %w（原始内容 %q）", name, err, text)
	}
	return out, nil
}

// close 关掉子进程。
//
// 先关 stdin：stdio server 读到 EOF 会自己退出，比直接 kill 干净。
// 超时还没退再强杀，避免测试挂死。
func (s *server) close() {
	_ = s.stdin.Close()
	done := make(chan struct{})
	go func() {
		_ = s.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = s.cmd.Process.Kill()
		<-done
	}
}

// ---------------------------------------------------------------------------
// JSON 取值小工具
//
// encoding/json 解析进 any 之后全是 float64 / string / []any / map[string]any，
// 直接类型断言写起来太吵。
// ---------------------------------------------------------------------------

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asFloat(v any) float64 {
	f, _ := v.(float64)
	return f
}

// collectKeys 递归收集所有 JSON 对象的键名。
func collectKeys(v any, out map[string]bool) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			out[k] = true
			collectKeys(val, out)
		}
	case []any:
		for _, item := range t {
			collectKeys(item, out)
		}
	}
}

// ---------------------------------------------------------------------------
// 环境变量
// ---------------------------------------------------------------------------

// buildEnv 组装子进程环境。
//
// 故意留一个**假** Key 作为默认值：验证"嵌入失败时检索降级到关键词"
// 这条路径。这是真实的故障场景（限流、欠费、网络抖动），比 happy path 更值得测。
// 调用方已经设了真 Key 时不覆盖——那时走的是完整路径，断言也跟着换。
func buildEnv() []string {
	env := os.Environ()
	env = setDefaultEnv(env, "SILICONFLOW_API_KEY", "sk-fake-key-for-smoke-test")
	env = setDefaultEnv(env, "CONTEXTDOCK_DATABASE_URL",
		"postgres://postgres:postgres@localhost:5432/contextdock?sslmode=disable")
	return env
}

func setDefaultEnv(env []string, key, val string) []string {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return env
		}
	}
	return append(env, prefix+val)
}

// ---------------------------------------------------------------------------
// 主流程
// ---------------------------------------------------------------------------

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\n%s\n", err)
		os.Exit(1)
	}
}

func run() error {
	root := repoRoot()

	exe := defaultExe(root)
	if len(os.Args) > 1 {
		exe = os.Args[1]
	}
	if _, err := os.Stat(exe); err != nil {
		// 提示里给**默认**路径而不是用户传进来的那个：
		// 路径多半是打错了，让人照着错路径去构建只会绕圈。
		//
		// 而且给的是相对路径 + 正斜杠：绝对路径在 Windows 上带反斜杠，
		// 直接粘进 Git Bash 会被当成转义符。
		rel := filepath.ToSlash(filepath.Join("bin", filepath.Base(defaultExe(root))))
		return fmt.Errorf("找不到可执行文件 %s\n先跑：go build -o %s ./cmd/contextdock", exe, rel)
	}

	// 收集失败的检查项，最后统一报，不在中间中断——
	// 一次跑完看到全部问题，比修一个跑一次快得多。
	var failures []string
	check := func(cond bool, label string) {
		status := "PASS"
		if !cond {
			status = "FAIL"
			failures = append(failures, label)
		}
		fmt.Printf("  %s  %s\n", status, label)
	}

	srv, err := newServer(exe, root, buildEnv())
	if err != nil {
		return fmt.Errorf("启动 server 失败: %w", err)
	}
	defer srv.close()

	fmt.Println("=== 1. initialize 握手 ===")
	resp, err := srv.request("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "smoke-test", "version": "1.0"},
	})
	if err != nil {
		return err
	}
	check(resp["error"] == nil, "initialize 无错误")
	serverName := asString(asMap(asMap(resp["result"])["serverInfo"])["name"])
	check(serverName == "contextdock", fmt.Sprintf("server 名字正确 (%s)", serverName))
	if err := srv.notify("notifications/initialized", nil); err != nil {
		return err
	}

	fmt.Println("\n=== 2. 工具发现 ===")
	listResp, err := srv.request("tools/list", nil)
	if err != nil {
		return err
	}
	tools := asSlice(asMap(listResp["result"])["tools"])
	names := make(map[string]bool, len(tools))
	byName := make(map[string]map[string]any, len(tools))
	for _, t := range tools {
		tool := asMap(t)
		names[asString(tool["name"])] = true
		byName[asString(tool["name"])] = tool
	}
	check(names["import_document"], "发现 import_document")
	check(names["search_knowledge_base"], "发现 search_knowledge_base")

	// 参数 schema 由 SDK 从结构体反射推导，字段名是稳定契约
	props := asMap(asMap(byName["import_document"]["inputSchema"])["properties"])
	var propNames []string
	for k := range props {
		propNames = append(propNames, k)
	}
	for _, want := range []string{"content", "file_path", "title", "source"} {
		if _, ok := props[want]; !ok {
			check(false, fmt.Sprintf("import_document 缺少参数 %s（实际 %v）", want, propNames))
		}
	}
	check(true, fmt.Sprintf("import_document 参数完整: %v", sorted(propNames)))

	fmt.Println("\n=== 3. 导入文档 ===")
	imported, err := srv.callTool("import_document", map[string]any{
		"title": "冒烟测试文档",
		// 显式给一个 source，好验证它能一路透传到检索结果里。
		// 省略的话会兜底成 "inline"，那样就分不清"透传成功"和"兜底生效"了。
		"source": "smoke-test",
		"content": "# 安装指南\n\n" +
			"ContextDock 需要 Go 1.25 以上版本。\n\n" +
			"## 数据库\n\n" +
			"PostgreSQL 需要安装 pgvector 扩展，且版本不低于 0.8.6。\n" +
			"HNSW 索引对 vector 类型的上限是 2000 维。\n",
	})
	if err != nil {
		return err
	}
	docID := int(asFloat(imported["document_id"]))
	chunkCount := int(asFloat(imported["chunk_count"]))
	embedded := int(asFloat(imported["embedded_count"]))
	fmt.Printf("        document_id=%d chunks=%d embedded=%d\n", docID, chunkCount, embedded)
	if w := asString(imported["warning"]); w != "" {
		fmt.Printf("        warning=%s\n", truncate(w, 90))
	}
	check(docID > 0, "导入返回了文档 ID")
	check(chunkCount > 0, "切分出了片段")

	fmt.Println("\n=== 4. 检索 ===")
	found, err := srv.callTool("search_knowledge_base", map[string]any{
		"query": "pgvector 版本要求",
		"top_k": 5,
	})
	if err != nil {
		return err
	}
	results := asSlice(found["results"])
	fmt.Printf("        count=%v\n", found["count"])
	degraded := asString(found["degraded"])
	if degraded != "" {
		fmt.Printf("        degraded=%s\n", truncate(degraded, 80))
	}
	for i, item := range results {
		if i >= 2 {
			break
		}
		r := asMap(item)
		heading := asString(r["heading"])
		matchedBy := fmt.Sprintf("%v", r["matched_by"])
		fmt.Printf("        - [%.4f] heading=%q matched_by=%s\n",
			asFloat(r["score"]), heading, matchedBy)
		fmt.Printf("          %s...\n", truncate(asString(r["content"]), 50))
	}

	// 关键：假 API Key 下嵌入必然失败，但关键词检索应当仍然命中。
	// 这验证的是"降级链路"真的接通了，而不是只在代码里写了。
	//
	// ⚠️ 分两种情况断言，不能只写 count > 0。
	// 默认用假 Key（走降级），但配上真 Key 也能跑——那时**根本不会降级**，
	// 只断言 count > 0 的话，标签写着"降级生效"却报 PASS，和实际情况无关。
	// 标签说了谎，下一个读日志的人就会以为降级路径被验证过了。
	if degraded != "" {
		check(len(results) > 0, "嵌入失败时仍能通过关键词命中（降级生效）")
	} else {
		vectorHit := false
		for _, item := range results {
			for _, ch := range asSlice(asMap(item)["matched_by"]) {
				if asString(ch) == "vector" {
					vectorHit = true
				}
			}
		}
		check(vectorHit, "未降级时应当有结果被向量通道命中")
	}

	// 溯源字段必须真的出现在结果里。
	//
	// 这个字段曾经声明了、schema 里也写了描述，但构造它的函数从不赋值，
	// 于是它永远是空的——DTO 有、schema 有、值没有。
	// 单元测试只能证明 handler 的返回值，这里验证的是**过完 JSON-RPC
	// 序列化之后** Agent 实际收到的东西（要防的正是 omitempty 把它抹掉）。
	//
	// ⚠️ 断言的是"至少有一条带上"，不是"每条都有"：
	// 库里可能残留修复之前导入的片段，那时的 metadata 里没有 source。
	// 那是历史数据，需要跑 migrations/002_backfill_chunk_source.sql 补齐。
	// 要求"每条都有"的话，这条用例会因为库里有点旧数据而长期变红，
	// 最后被人加个 skip 绕过去——那才是真的失去守护。
	var sources []string
	hasSmokeSource := false
	for _, item := range results {
		src := asString(asMap(item)["source"])
		sources = append(sources, src)
		if src == "smoke-test" {
			hasSmokeSource = true
		}
	}
	check(hasSmokeSource, fmt.Sprintf("本次导入的结果带上了 source（实际 %q）", sources))

	// 输出里绝不能有向量。
	//
	// ⚠️ 必须**按键名**判断，不能用子串——`matched_by` 的合法取值里
	// 就有 "vector"（检索通道名），搜子串会误报。
	keys := map[string]bool{}
	collectKeys(found, keys)
	check(!keys["embedding"] && !keys["Embedding"], "输出里不含向量字段")

	fmt.Println("\n=== 5. stderr 日志 ===")
	for _, line := range srv.stderrTail(6) {
		fmt.Printf("        %s\n", line)
	}
	check(srv.stderrCount() > 0, "日志走了 stderr（stdout 必须干净）")

	fmt.Println("\n" + strings.Repeat("=", 60))
	if len(failures) > 0 {
		fmt.Printf("失败 %d 项:\n", len(failures))
		for _, f := range failures {
			fmt.Printf("  - %s\n", f)
		}
		return fmt.Errorf("冒烟测试未通过")
	}
	fmt.Println("全部通过")
	return nil
}

// truncate 按**字符**（rune）截断，不是按字节。
// Go 的 len() 是字节数，中文一个字 3 字节——按字节截断会把汉字切碎。
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
