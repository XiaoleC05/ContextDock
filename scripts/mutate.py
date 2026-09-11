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

    # ---- chunk ----
    # 这条对应开发时真实踩到的 bug：hitEnd 在去空白之前算，
    # 改成 false 后短段落会退化成一个字符一段。
    ("chunk: 破坏 hitEnd 判断（真实踩过的 bug）",
     "internal/chunk/chunker.go",
     "hitEnd := end >= sec.end",
     "hitEnd := false"),

    ("chunk: 不写标题面包屑",
     "internal/chunk/chunker.go",
     "if sec.breadcrumb != \"\" {",
     "if false {"),

    ("chunk: 取消片段重叠",
     "internal/chunk/chunker.go",
     "next := end - c.cfg.OverlapRunes",
     "next := end"),

    ("chunk: #hashtag 被误判为标题",
     "internal/chunk/chunker.go",
     "if n < len(line) && line[n] != ' ' && line[n] != '\\t' {\n\t\treturn 0, \"\", false\n\t}",
     "if false {\n\t\treturn 0, \"\", false\n\t}"),

    ("chunk: isSpace 不识别全角空格",
     "internal/chunk/chunker.go",
     "return unicode.IsSpace(r)",
     "return unicode.IsSpace(r) && r < 0x80"),

    ("chunk: 不校验 overlap < maxRunes",
     "internal/chunk/chunker.go",
     "if c.OverlapRunes >= c.MaxRunes {",
     "if false {"),

    ("chunk: 不在句末标点断开",
     "internal/chunk/chunker.go",
     "} else if brk := lastSentenceBreak(runes, pos, end); brk > pos {\n\t\t\tend = brk\n\t\t}",
     "}"),

    # ---- embed ----
    ("embed: 请求体里加了 dimensions 字段（DESIGN §2 红线）",
     "internal/embed/siliconflow.go",
     "\tEncodingFormat string   `json:\"encoding_format\"`\n}",
     "\tEncodingFormat string   `json:\"encoding_format\"`\n\tDimensions     int      `json:\"dimensions\"`\n}"),

    ("embed: 不按 32 条分批",
     "internal/embed/siliconflow.go",
     "for start := 0; start < len(texts); start += MaxBatchSize {",
     "for start := 0; start < len(texts); start += len(texts) {"),

    ("embed: 对 400 也重试",
     "internal/embed/siliconflow.go",
     "if json.Unmarshal(raw, &se) == nil && se.Message != \"\" {\n\t\t\treturn nil, false, &se\n\t\t}",
     "if json.Unmarshal(raw, &se) == nil && se.Message != \"\" {\n\t\t\treturn nil, true, &se\n\t\t}"),

    ("embed: 不校验返回维度",
     "internal/embed/siliconflow.go",
     "if len(d.Embedding) != types.EmbeddingDim {",
     "if false {"),

    ("embed: 不按 index 排序响应",
     "internal/embed/siliconflow.go",
     "sort.Slice(out.Data, func(i, j int) bool { return out.Data[i].Index < out.Data[j].Index })",
     "_ = sort.Slice"),

    ("embed: 假嵌入不做归一化",
     "internal/embed/fake.go",
     "norm := float32(math.Sqrt(sum))",
     "norm := float32(math.Sqrt(sum)*0 + 1)"),

    ("embed: 不校验空文本",
     "internal/embed/fake.go",
     "if strings.TrimSpace(t) == \"\" {",
     "if strings.HasPrefix(t, \"\\x00\") {"),

    # ---- retrieve / BM25 ----
    ("bm25: IDF 换回 Robertson 原始版（会出负数）",
     "internal/retrieve/bm25.go",
     "return math.Log(1 + (n-df+0.5)/(df+0.5))",
     "return math.Log((n - df + 0.5) / (df + 0.5))"),

    ("bm25: 建索引时直接读 Content 而非 IndexText",
     "internal/retrieve/bm25.go",
     "tokens := m.tk.Tokenize(c.IndexText())",
     "tokens := m.tk.Tokenize(c.Content)"),

    ("bm25: 关闭长度归一化（b=0）",
     "internal/retrieve/bm25.go",
     "DefaultB  = 0.75",
     "DefaultB  = 0"),

    ("bm25: 查询词不去重",
     "internal/retrieve/bm25.go",
     "if !seen[q] {",
     "if true {"),

    ("bm25: 名次改成 0-based",
     "internal/retrieve/bm25.go",
     "LexicalRank:  i + 1, // 1-based",
     "LexicalRank:  i, // 0-based"),

    # ---- retrieve / 向量 ----
    ("vector: 不除模长（退化成点积）",
     "internal/retrieve/vector.go",
     "sim := float64(dot(query, v.vecs[i]) / (qNorm * v.norms[i]))",
     "sim := float64(dot(query, v.vecs[i]))"),

    ("vector: 不校验查询向量维度",
     "internal/retrieve/vector.go",
     "if len(query) != types.EmbeddingDim {",
     "if false {"),

    ("vector: 零向量查询不报错",
     "internal/retrieve/vector.go",
     "if qNorm == 0 {\n\t\treturn nil, ErrZeroVector\n\t}",
     "if false {\n\t\treturn nil, ErrZeroVector\n\t}"),

    ("vector: 不跳过零模长文档",
     "internal/retrieve/vector.go",
     "if v.norms[i] == 0 {\n\t\t\tcontinue\n\t\t}",
     "if false {\n\t\t\tcontinue\n\t\t}"),

    # ---- retrieve / RRF ----
    ("rrf: 去重用 Chunk.ID 而非 StableKey（落库前全是 0）",
     "internal/retrieve/rrf.go",
     "key := r.Chunk.StableKey()",
     "key := string(rune(r.Chunk.ID))"),

    ("rrf: 直接把 0 号名次代入公式（未召回拿最高分）",
     "internal/retrieve/rrf.go",
     "s.Score = s.RRFScore(k)",
     "s.Score = 1/float64(k+s.LexicalRank) + 1/float64(k+s.VectorRank)"),

    # ---- retrieve / hybrid ----
    ("hybrid: 单路失败就整体失败（取消降级）",
     "internal/retrieve/hybrid.go",
     "if p.err != nil {\n\t\t\t\terrs = append(errs, p.err)\n\t\t\t\th.reportError(p.run.Retriever, p.err)\n\t\t\t\tcontinue\n\t\t\t}",
     "if p.err != nil {\n\t\t\t\treturn nil, p.err\n\t\t\t}"),

    ("hybrid: 每路只取 topK 不做候选放大",
     "internal/retrieve/hybrid.go",
     "n := topK * h.mult",
     "n := topK"),

    ("hybrid: 不施加超时",
     "internal/retrieve/hybrid.go",
     "ctx, cancel := context.WithTimeout(ctx, h.timeout)",
     "ctx, cancel := context.WithCancel(ctx)"),

    ("hybrid: 两路都失败时也不报错",
     "internal/retrieve/hybrid.go",
     "if len(runs) == 0 {\n\t\treturn nil, fmt.Errorf(\"%w: %v\", ErrBothRetrieversFailed, errors.Join(errs...))\n\t}",
     "if false {\n\t\treturn nil, fmt.Errorf(\"%w: %v\", ErrBothRetrieversFailed, errors.Join(errs...))\n\t}"),
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
    broken = 0
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

            # 编译失败不算「测试抓到」——那是变异本身写坏了，
            # 不能证明测试有效。必须区分开，否则会虚报通过率。
            build_failed = "[build failed]" in out or "cannot use" in out or "is not used" in out

            if code != 0 and not build_failed:
                caught += 1
                print(f"  [CAUGHT  ] {name}")
                f = first_failure(out)
                if f:
                    print(f"             -> {f}")
            elif build_failed:
                broken += 1
                print(f"  [BROKEN  ] {name}   <-- 变异导致编译失败，无法判定，请改写这条变异")
            else:
                print(f"  [MISSED  ] {name}   <-- 测试没抓到，可能是假测试")
        finally:
            shutil.copy(backup, path)
            os.remove(backup)

    total = len(items)
    print(f"\n结果：{caught}/{total} 个变异被抓到"
          + (f"，{broken} 条变异写坏了（编译失败）" if broken else ""))
    # 只有「全部被抓到、且没有写坏的变异」才算通过。
    return 0 if caught == total and broken == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
