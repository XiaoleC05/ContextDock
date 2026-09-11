#!/usr/bin/env python3
"""端到端冒烟测试：把编译好的 contextdock 当 MCP server 跑起来，用 stdio 发 JSON-RPC。

单元测试覆盖不到的接缝，只有这里能发现：

  - 进程能不能启动、配置缺失时是否报可读错误
  - stdio 协议通不通（**stdout 有没有被日志污染**）
  - 工具和参数 schema 是否被正确暴露
  - 用假 API Key 时检索能否降级到关键词而不是整个失败

用法：
    python scripts/smoke.py [可执行文件路径]

默认路径是 bin/contextdock.exe（Windows）或 bin/contextdock。
需要本机有可用的 PostgreSQL + pgvector，或者设置
CONTEXTDOCK_USE_MEMORY_STORE=true 走内存存储。
"""

import json
import os
import subprocess
import sys
import threading

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

DEFAULT_EXE = os.path.join(
    ROOT, "bin", "contextdock.exe" if os.name == "nt" else "contextdock"
)


def build_env():
    env = dict(os.environ)
    # 故意用假 Key：验证"嵌入失败时检索降级到关键词"这条路径。
    # 这是真实的故障场景（限流、欠费、网络抖动），比 happy path 更值得测。
    env.setdefault("SILICONFLOW_API_KEY", "sk-fake-key-for-smoke-test")
    env.setdefault(
        "CONTEXTDOCK_DATABASE_URL",
        "postgres://postgres:postgres@localhost:5432/contextdock?sslmode=disable",
    )
    return env


class Server:
    """驱动一个 stdio MCP server 子进程。"""

    def __init__(self, exe, env):
        self.proc = subprocess.Popen(
            [exe],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
            bufsize=0,
        )
        self.stderr_lines = []
        self._id = 0
        threading.Thread(target=self._drain_stderr, daemon=True).start()

    def _drain_stderr(self):
        for line in self.proc.stderr:
            self.stderr_lines.append(line.decode("utf-8", "replace").rstrip())

    def request(self, method, params=None, notify=False):
        msg = {"jsonrpc": "2.0", "method": method}
        if not notify:
            self._id += 1
            msg["id"] = self._id
        if params is not None:
            msg["params"] = params
        self.proc.stdin.write((json.dumps(msg) + "\n").encode("utf-8"))
        self.proc.stdin.flush()
        if notify:
            return None

        line = self.proc.stdout.readline()
        if not line:
            raise RuntimeError(f"等待 {method} 的响应时，server 关闭了 stdout")
        return json.loads(line.decode("utf-8"))

    def call_tool(self, name, arguments):
        """调用工具并把 structuredContent 之外的文本载荷解析出来。"""
        resp = self.request("tools/call", {"name": name, "arguments": arguments})
        if "error" in resp:
            raise RuntimeError(f"工具 {name} 返回错误: {resp['error']}")
        content = resp.get("result", {}).get("content", [])
        if not content:
            return {}
        return json.loads(content[0]["text"])

    def close(self):
        self.proc.terminate()
        try:
            self.proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            self.proc.kill()


def main():
    exe = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_EXE
    if not os.path.exists(exe):
        print(f"找不到可执行文件 {exe}，先跑 go build -o {exe} ./cmd/contextdock")
        return 1

    failures = []

    def check(cond, label):
        print(f"  {'PASS' if cond else 'FAIL'}  {label}")
        if not cond:
            failures.append(label)

    srv = Server(exe, build_env())
    try:
        print("=== 1. initialize 握手 ===")
        resp = srv.request("initialize", {
            "protocolVersion": "2025-06-18",
            "capabilities": {},
            "clientInfo": {"name": "smoke-test", "version": "1.0"},
        })
        check("error" not in resp, "initialize 无错误")
        info = resp.get("result", {}).get("serverInfo", {})
        check(info.get("name") == "contextdock", f"server 名字正确 ({info.get('name')})")
        srv.request("notifications/initialized", notify=True)

        print("\n=== 2. 工具发现 ===")
        tools = srv.request("tools/list").get("result", {}).get("tools", [])
        names = [t["name"] for t in tools]
        check("import_document" in names, "发现 import_document")
        check("search_knowledge_base" in names, "发现 search_knowledge_base")

        # 参数 schema 由 SDK 从结构体反射推导，字段名是稳定契约
        by_name = {t["name"]: t for t in tools}
        import_props = set(
            (by_name.get("import_document", {}).get("inputSchema") or {})
            .get("properties", {}).keys()
        )
        check({"content", "file_path", "title", "source"} <= import_props,
              f"import_document 参数完整: {sorted(import_props)}")

        print("\n=== 3. 导入文档 ===")
        imported = srv.call_tool("import_document", {
            "title": "冒烟测试文档",
            "content": (
                "# 安装指南\n\n"
                "ContextDock 需要 Go 1.25 以上版本。\n\n"
                "## 数据库\n\n"
                "PostgreSQL 需要安装 pgvector 扩展，且版本不低于 0.8.6。\n"
                "HNSW 索引对 vector 类型的上限是 2000 维。\n"
            ),
        })
        doc_id = imported.get("document_id", 0)
        checked = imported.get("chunk_count", 0)
        embedded = imported.get("embedded_count", 0)
        print(f"        document_id={doc_id} chunks={checked} embedded={embedded}")
        if imported.get("warning"):
            print(f"        warning={imported['warning'][:90]}")
        check(doc_id > 0, "导入返回了文档 ID")
        check(checked > 0, "切分出了片段")

        print("\n=== 4. 检索 ===")
        found = srv.call_tool("search_knowledge_base",
                              {"query": "pgvector 版本要求", "top_k": 5})
        print(f"        count={found.get('count')}")
        if found.get("degraded"):
            print(f"        degraded={found['degraded'][:80]}")
        for item in found.get("results", [])[:2]:
            print(f"        - [{item.get('score', 0):.4f}] "
                  f"heading={item.get('heading')!r} matched_by={item.get('matched_by')}")
            print(f"          {item.get('content', '')[:50]}...")

        # 关键：假 API Key 下嵌入必然失败，但关键词检索应当仍然命中。
        # 这验证的是"降级链路"真的接通了，而不是只在代码里写了。
        check(found.get("count", 0) > 0, "嵌入失败时仍能通过关键词命中（降级生效）")

        # 输出里绝不能有向量。按键名判断，不要用子串——
        # matched_by 的合法取值里就有 "vector"（检索通道名）。
        keys = set()
        stack = [found]
        while stack:
            cur = stack.pop()
            if isinstance(cur, dict):
                keys |= set(cur.keys())
                stack.extend(cur.values())
            elif isinstance(cur, list):
                stack.extend(cur)
        check(not ({"embedding", "Embedding"} & keys),
              "输出里不含向量字段")

        print("\n=== 5. stderr 日志 ===")
        for line in srv.stderr_lines[-6:]:
            print(f"        {line}")
        check(len(srv.stderr_lines) > 0, "日志走了 stderr（stdout 必须干净）")

    finally:
        srv.close()

    print("\n" + "=" * 60)
    if failures:
        print(f"失败 {len(failures)} 项:")
        for f in failures:
            print(f"  - {f}")
        return 1
    print("全部通过")
    return 0


if __name__ == "__main__":
    sys.exit(main())
