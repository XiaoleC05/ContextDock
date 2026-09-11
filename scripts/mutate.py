#!/usr/bin/env python3
"""变异测试工具：验证测试套件是否真的有效。

覆盖率只说明「代码被执行过」，不说明「测试能发现问题」。
本工具故意在源码里植入 bug，然后跑测试——测试**必须失败**。
测试没失败，说明它守不住它声称守护的东西（假测试）。

用法：
    python scripts/mutate.py              # 跑全部变异
    python scripts/mutate.py tokenize     # 只跑名字含 tokenize 的

每个变异是 (名称, 文件, 原文, 替换为) 四元组。脚本会：
  1. 备份原文件
  2. 写入变异
  3. 跑 go test
  4. 无论结果如何都恢复原文件
  5. 报告 CAUGHT / NOT CAUGHT

退出码非 0 表示有变异没被抓到。
"""

import os
import shutil
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

# ---------------------------------------------------------------------------
# 变异定义
#
# 每条都应该对应一句「如果这里写错了，哪个测试会失败」。
# 如果某条变异没被抓到，要么补测试，要么承认这块其实没被守护。
# ---------------------------------------------------------------------------
MUTATIONS = [
    # ---- tokenize ----
    ("tokenize: 单字 CJK 不回吐 unigram",
     "internal/tokenize/tokenizer.go",
     "out = append(out, string(cjk))",
     "_ = cjk"),

    ("tokenize: 不做全角归一",
     "internal/tokenize/tokenizer.go",
     "case r >= 0xFF01 && r <= 0xFF5E:\n\t\t\tb.WriteRune(r - 0xFEE0)",
     "case r >= 0xFF01 && r <= 0xFF5E:\n\t\t\tb.WriteRune(r)"),

    ("tokenize: 拉丁词不转小写",
     "internal/tokenize/tokenizer.go",
     "out = append(out, strings.ToLower(string(word)))",
     "out = append(out, string(word))"),

    ("tokenize: 汉字判断改用 Ideographic",
     "internal/tokenize/tokenizer.go",
     "return unicode.Is(unicode.Han, r) ||",
     "return unicode.Is(unicode.Ideographic, r) ||"),

    ("tokenize: 标点不冲掉缓冲",
     "internal/tokenize/tokenizer.go",
     "default:\n\t\t\t// 标点、空白、符号：两者都冲掉。\n\t\t\t// 这一步不能省——否则 \"abc中文\" 会把 abc 和 中文 粘成一个段。\n\t\t\tflushCJK()\n\t\t\tflushWord()",
     "default:\n\t\t\t_ = word\n\t\t\t_ = cjk"),

    ("tokenize: bigram 循环少一位",
     "internal/tokenize/tokenizer.go",
     "for i := 0; i+1 < len(cjk); i++ {",
     "for i := 0; i+2 < len(cjk); i++ {"),

    ("tokenize: 空串返回 nil",
     "internal/tokenize/tokenizer.go",
     "if s == \"\" {\n\t\treturn []string{}\n\t}",
     "if s == \"\" {\n\t\treturn nil\n\t}"),
]


def run_tests(pkg_dir):
    r = subprocess.run(
        ["go", "test", "./" + pkg_dir.replace("\\", "/") + "/..."],
        cwd=ROOT, capture_output=True, text=True, encoding="utf-8", errors="replace",
    )
    return r.returncode, (r.stdout or "") + (r.stderr or "")


def first_failure(output):
    for line in output.splitlines():
        s = line.strip()
        if s.startswith("--- FAIL") or s.startswith("FAIL"):
            return s
    for line in output.splitlines():
        if "_test.go:" in line:
            return line.strip()
    return ""


def main():
    filt = sys.argv[1] if len(sys.argv) > 1 else None
    items = [m for m in MUTATIONS if not filt or filt in m[0]]

    if not items:
        print("没有匹配的变异定义")
        return 1

    print("=== 基线（未变异，应全过）===")
    pkgs = sorted({os.path.dirname(m[1]) for m in items})
    for p in pkgs:
        code, out = run_tests(p)
        status = "PASS" if code == 0 else "FAIL"
        print(f"  {status}  {p}")
        if code != 0:
            print("  基线就没过，后续变异结果无意义")
            print(out[-800:])
            return 1

    print(f"\n=== 变异测试（{len(items)} 条）===")
    caught = 0
    for name, rel, old, new in items:
        path = os.path.join(ROOT, rel)
        backup = path + ".mutation-backup"
        shutil.copy(path, backup)
        try:
            src = open(path, encoding="utf-8").read()
            if old not in src:
                print(f"  [SKIP    ] {name}")
                print(f"             变异点未找到：{rel}")
                continue
            open(path, "w", encoding="utf-8").write(src.replace(old, new, 1))
            code, out = run_tests(os.path.dirname(rel))
            if code != 0:
                caught += 1
                print(f"  [CAUGHT  ] {name}")
                f = first_failure(out)
                if f:
                    print(f"             -> {f}")
            else:
                print(f"  [MISSED  ] {name}   <-- 测试没抓到，可能是假测试")
        finally:
            shutil.copy(backup, path)
            os.remove(backup)

    total = len(items)
    print(f"\n结果：{caught}/{total} 个变异被抓到")
    return 0 if caught == total else 1


if __name__ == "__main__":
    sys.exit(main())
