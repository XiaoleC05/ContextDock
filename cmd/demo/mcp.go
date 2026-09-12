package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// mcpServer 是一个最小的 MCP stdio 客户端。
//
// 它只实现演示需要的那几条消息：initialize → tools/list → tools/call。
// 完整实现见 cmd/smoke——那边要覆盖降级链路和差错路径，
// 这边只需要把一次正常的检索跑出来。
type mcpServer struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader

	mu sync.Mutex
	id int

	// stderr 单独收着：stdout 是 JSON-RPC 通道，日志全部走 stderr，
	// 演示里要把"日志没有污染协议"这件事显示出来。
	stderr []string
}

// startMCPServer 把 contextdock 二进制当 MCP server 拉起来。
//
// cwd 必须是仓库根：Config.Load() 从**当前工作目录**读 .env，
// 换到别处就跑不成真实嵌入了。
func startMCPServer(binPath, cwd string, extraEnv map[string]string) (*mcpServer, error) {
	cmd := exec.Command(binPath)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), flattenEnv(extraEnv)...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("创建 stdin 管道失败: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("创建 stdout 管道失败: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("创建 stderr 管道失败: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 %s 失败: %w", binPath, err)
	}

	s := &mcpServer{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout)}

	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			s.mu.Lock()
			s.stderr = append(s.stderr, sc.Text())
			s.mu.Unlock()
		}
	}()

	return s, nil
}

func flattenEnv(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

// Logs 返回到目前为止收到的 stderr 行。
func (s *mcpServer) Logs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.stderr))
	copy(out, s.stderr)
	return out
}

func (s *mcpServer) write(msg map[string]any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = s.stdin.Write(append(b, '\n'))
	return err
}

// notify 发一条不需要响应的通知。
func (s *mcpServer) notify(method string) error {
	return s.write(map[string]any{"jsonrpc": "2.0", "method": method})
}

// request 发一条请求并读回响应。
func (s *mcpServer) request(method string, params any) (map[string]any, error) {
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

// CallTool 调用工具，返回反序列化后的结构化载荷。
func (s *mcpServer) CallTool(name string, args map[string]any) (map[string]any, error) {
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

	result, _ := resp["result"].(map[string]any)

	// 优先用 structuredContent；没有就退回解析 content[0].text。
	// 两条都支持是因为协议里两者都是合法的，而演示不该因为
	// server 选了哪种表达方式就失败。
	if sc, ok := result["structuredContent"].(map[string]any); ok {
		return sc, nil
	}

	content, _ := result["content"].([]any)
	if len(content) == 0 {
		return map[string]any{}, nil
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)

	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return nil, fmt.Errorf("解析 %s 的输出失败: %w（原始内容 %q）", name, err, text)
	}
	return out, nil
}

// Close 关闭 stdin 让 server 正常退出，并等它结束。
//
// 先关 stdin 而不是直接 Kill：main.go 的退出路径要跑完 defer 里的
// 关停逻辑，演示里那条「正常退出」日志本身就是素材的一部分。
func (s *mcpServer) Close() {
	_ = s.stdin.Close()
	_ = s.cmd.Wait()
}

// Handshake 走完 initialize + notifications/initialized。
func (s *mcpServer) Handshake(clientName string) error {
	resp, err := s.request("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": clientName, "version": "0"},
	})
	if err != nil {
		return err
	}
	if e, ok := resp["error"]; ok {
		return fmt.Errorf("initialize 被拒: %v", e)
	}
	return s.notify("notifications/initialized")
}
