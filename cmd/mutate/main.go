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

	{
		// 切到某方案时其实没切换：两组数据一模一样，
		// 然后被当成「方案没影响」——而那是实验做废了，不是结论。
		name: "tokenize: unigram/both 方案不产出单字",
		file: "internal/tokenize/tokenizer.go",
		old:  `if t.Scheme() != SchemeBigram {`,
		new:  `if false {`,
	},
	{
		name: "tokenize: unigram 方案也产出 bigram",
		file: "internal/tokenize/tokenizer.go",
		old:  `if t.Scheme() != SchemeUnigram {`,
		new:  `if true {`,
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
		// 切分点不再优先落在句末标点：一句话会被从中间劈开，
		// 两个片段都变得语义不完整。
		name: "chunk: 不在句末标点断开",
		file: "internal/chunk/chunker.go",
		old:  `cut := lastSentenceBreak(runes, pos, limit)`,
		new:  `cut := limit`,
	},

	{
		// 切分点退得太靠近起点 → 下一片只前进一个字符，
		// 一段文字被切成几十个碎片（实测 545 字符切出 27 片）而不报错。
		name: "chunk: 断点不设最小前进距离（切分空转）",
		file: "internal/chunk/chunker.go",
		old:  `if cut <= minEnd {`,
		new:  `if false {`,
	},
	{
		// 在保护区起点结束时仍然回退重叠：起点被拉回块前，
		// 刚保护好的整块又放不进下一片的窗口里。
		name: "chunk: 在保护区起点结束时仍回退重叠",
		file: "internal/chunk/chunker.go",
		old:  `if _, ok := regionStartingAt(prot, cutBeforeTrim); ok {`,
		new:  `if _, ok := regionStartingAt(prot, cutBeforeTrim); false && ok {`,
	},

	{
		// ⚠️ 这条对应一个**真实存在过的 bug**：0 被当成「未设置」的哨兵，
		// 于是 OverlapRunes=0（完全不重叠，一个合法配置）会被静默改成 60，
		// 用户根本没有办法关掉重叠。
		// 违反的是本项目自己写在 types.Chunk.Ordinal 上的规则。
		name: "chunk: OverlapRunes=0 被当成未设置（真实缺陷）",
		file: "internal/chunk/chunker.go",
		old:  `if c.OverlapRunes < 0 {`,
		new:  `if c.OverlapRunes <= 0 {`,
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

	{
		// 有结果时不说明分数的局限：Agent 手上没有任何信号，
		// 会把一堆可能完全不相关的结果当成答案照单全收。
		name: "mcp: 有结果时不说明分数判断不了相关性",
		file: "internal/mcp/server.go",
		old:  `			out.Hint = scoreNote`,
		new:  `			out.Hint = ""`,
	},
	{
		// 不暴露原始分：Agent 手上只剩一个完全无法判断相关性的数字
		// （实测无答案时的最高 RRF 分与真正命中时完全相同）。
		name: "mcp: 结果不带原始分",
		file: "internal/mcp/server.go",
		old:  `		VectorScore:  r.VectorScore,`,
		new:  `		VectorScore:  0,`,
	},

	// ---- 分数分布（#52）----
	{
		// 把 norel 组的结果混进「不相关」桶：阈值要挡的是"库里根本没答案"，
		// 混进来会把"查得到但没排好"误当成"查不到"，阈值随之定得过高。
		name: "eval: norel 组的结果没单独分桶",
		file: "internal/eval/run.go",
		old:  `noRel := group == GroupNoRel`,
		new:  `noRel := false`,
	},
	{
		// norel 组按定义没有期望命中。恢复"必须非空"的校验，
		// 这一组就再也建不出来——而它是 #52 唯一的实验依据。
		name: "eval: 要求所有组都有期望命中",
		file: "internal/eval/load.go",
		old:  `		if group != GroupNoRel {`,
		new:  `		if true {`,
	},
	{
		name: "eval: 分数分位数算错一位",
		file: "internal/eval/metrics.go",
		old:  `		i := int(math.Ceil(p/100*float64(len(s)))) - 1`,
		new:  `		i := int(math.Ceil(p/100*float64(len(s))))`,
	},
	{
		name: "eval: 算分数分位数时就地排序了输入",
		file: "internal/eval/metrics.go",
		old:  `	s := append([]float64(nil), vs...)`,
		new:  `	s := vs`,
	},

	// ---- 相邻片段合并（#50）----
	{
		// 序号不相邻也合并：会把"中间没被命中的内容"一并包进来，
		// 造出一段原文里并不连续的正文。
		name: "merge: 序号不相邻也合并",
		file: "internal/retrieve/merge.go",
		old:  `results[idxs[j]].Chunk.Ordinal == results[idxs[j-1]].Chunk.Ordinal+1`,
		new:  `results[idxs[j]].Chunk.Ordinal == results[idxs[j-1]].Chunk.Ordinal`,
	},
	{
		// 合并后取序号最小的那条当代表，而不是名次最好的那条：
		// 合并结果整体后移，把本可以露出的别的文档又挤回去。
		name: "merge: 代表取序号最小而非名次最好",
		file: "internal/retrieve/merge.go",
		old: `if k < head {
					head = k
				}`,
		new: `if false {
					head = k
				}`,
	},
	{
		// 拼接时不去掉重叠：正文里会出现重复段落，
		// 而且它不再等于原文的 [start,end)——那条不变量一破，
		// 上下文扩展和「重新切分对齐」都会错位。
		name: "merge: 拼接正文时不去掉重叠",
		file: "internal/retrieve/merge.go",
		old:  `skip := prevEnd - c.StartOffset`,
		new:  `skip := 0`,
	},
	{
		// 不按文档分组：不同文档的片段也会被合并，
		// 造出一段横跨两份文档、原文里根本不存在的正文。
		//
		// 用 Chunk.ID 代替 DocumentID：在测试里片段 ID 都没设（全是 0），
		// 于是所有结果落进同一组、跨文档合并真的会发生。
		name: "merge: 跨文档合并",
		file: "internal/retrieve/merge.go",
		old:  `groups[r.Chunk.DocumentID] = append(groups[r.Chunk.DocumentID], i)`,
		new:  `groups[r.Chunk.ID] = append(groups[r.Chunk.ID], i)`,
	},
	{
		// ⚠️ 这条对应一个**真实踩过的坑**：为了让合并有截断空间而给
		// Search 传更大的 topK，会连带放大每路的候选数，改变 RRF 排名。
		// 实测 recall 掉 7 个百分点。
		name: "hybrid: SearchAll 也截断（合并失去腾挪空间）",
		file: "internal/retrieve/hybrid.go",
		old:  `	return h.search(ctx, query, queryVec, topK, false)`,
		new:  `	return h.search(ctx, query, queryVec, topK, true)`,
	},
	{
		name: "hybrid: 不截断时仍按 topK 融合",
		file: "internal/retrieve/hybrid.go",
		old:  `		fuseK = 0 // FuseRRF 的 0 表示不截断`,
		new:  `		fuseK = topK`,
	},

	// ---- 上下文扩展（#49）----
	{
		// 前一段取头部：给 Agent 看一段它根本接不上的话，比不给还糟。
		name: "mcp: context_before 取了前一段的头部",
		file: "internal/mcp/server.go",
		old:  `item.ContextBefore = tailRunes(prev[n-1].Content, contextSnippetRunes)`,
		new:  `item.ContextBefore = headRunes(prev[n-1].Content, contextSnippetRunes)`,
	},
	{
		name: "mcp: context_after 取了后一段的尾部",
		file: "internal/mcp/server.go",
		old:  `item.ContextAfter = headRunes(next[0].Content, contextSnippetRunes)`,
		new:  `item.ContextAfter = tailRunes(next[0].Content, contextSnippetRunes)`,
	},
	{
		// 按字节截断：中文会被切成半个、输出乱码，而且不报错。
		name: "mcp: 上下文摘要按字节截断",
		file: "internal/mcp/server.go",
		old:  `return string(r[:n]) + "…"`,
		new:  `return s[:n] + "…"`,
	},
	{
		// 找不到自己时瞎猜一个位置：会把别的片段的正文当成"上下文"贴上去，
		// 而 Agent 无从分辨。
		name: "service: 取邻居时找不到自己也不返回空",
		file: "internal/service/neighbors.go",
		old:  `if i >= len(doc) || doc[i].Ordinal != c.Ordinal {`,
		new:  `if false {`,
	},

	// ---- eval 指标 ----
	// 下面这几条都属于同一类：**不报错，只是数字变好看**。
	// 指标算错了评测照样跑完、照样打印一张像模像样的表格，
	// 而所有基于它的结论都是错的。这类变异正是变异测试存在的理由。
	{
		// 没有来源的期望一律不命中。手搓 Expect 时 source 是空的，
		// 而空来源会与同样没有 source 元数据的片段"相等"，
		// 于是零值区间被当成压在文档开头，所有查询都轻松命中。
		name: "eval: 命中判定不防「没有来源的期望」",
		file: "internal/eval/metrics.go",
		old:  `if e.Source == "" {`,
		new:  `if false {`,
	},
	{
		// 不校验来源：区间相同但出自另一份语料也算命中，recall 虚高。
		name: "eval: 命中判定不校验来源语料",
		file: "internal/eval/metrics.go",
		old:  `if r.Chunk.Metadata[types.MetadataKeySource] != e.Source {`,
		new:  `if false {`,
	},
	{
		// recall 的分母写错：多跳查询只命中一半会被算成 1，高估多跳场景。
		name: "eval: recall 分母写错（多跳只命中一半算满分）",
		file: "internal/eval/metrics.go",
		old:  `sc.Recall = float64(len(sc.Matched)) / float64(sc.Total)`,
		new:  `sc.Recall = float64(len(sc.Matched))`,
	},
	{
		// 分级失效：部分相关与完全回答拿到同样的增益。
		name: "eval: NDCG 增益忽略相关性分级",
		file: "internal/eval/metrics.go",
		old:  `return math.Pow(2, float64(e.Grade)) - 1`,
		new:  `return 1`,
	},
	{
		// 同一片段压住两条期望时重复计分，NDCG 会超过 1。
		name: "eval: 同一片段的增益被重复计入",
		file: "internal/eval/metrics.go",
		old:  `if matched[i] || !covers(r, expects[i]) {`,
		new:  `if !covers(r, expects[i]) {`,
	},
	{
		name: "eval: NDCG 不按截断位置截断",
		file: "internal/eval/metrics.go",
		old:  `if rank <= ndcgK {`,
		new:  `if true {`,
	},

	// ---- 嵌入缓存 ----
	{
		// 分位数算错一位：P95 报出来的是别的值。看这个数的人
		// 想知道的恰恰是"最慢的那几次有多慢"，报错了他也看不出来。
		name: "eval: 延迟分位数算错一位",
		file: "internal/eval/run.go",
		old:  `i := int(math.Ceil(p/100*float64(len(s)))) - 1`,
		new:  `i := int(math.Ceil(p/100*float64(len(s))))`,
	},
	{
		// 分位数就地排序：把调用方的切片改掉了，后续用它的顺序全乱。
		name: "eval: 算分位数时就地排序了输入切片",
		file: "internal/eval/run.go",
		old:  `s := append([]float64(nil), ms...)`,
		new:  `s := ms`,
	},
	{
		// 同上，在评测这一层：`-overlap 0` 被静默换成 60，
		// 参数扫描里 Overlap=0 那一整列都是假的。
		name: "eval: 候选重叠 0 被当成未传",
		file: "internal/eval/run.go",
		old:  `if o.Overlap < 0 {`,
		new:  `if o.Overlap <= 0 {`,
	},
	{
		// 候选倍数写成 1：基线跑在了一个不存在的配置上，
		// 「融合比单路好多少」测的其实是「候选不够时融合好不好」。
		// 写成 -2 而不是直接写 1：直接写 1 会让 retrieve 包失去引用，
		// 编译不过（变异会被判 BROKEN 而不是 CAUGHT），
		// 那样这条变异就失去意义了——我们要的是「能编译但行为错」。
		name: "eval: 候选倍数默认写成 1 而非生产值",
		file: "internal/eval/run.go",
		old:  `o.Mult = retrieve.DefaultCandidateMultiplier`,
		new:  `o.Mult = retrieve.DefaultCandidateMultiplier - 2`,
	},
	{
		// 键里不带模型名：换模型后会静默读到旧模型的向量，
		// 维度一样、跑得通，只是语义空间完全不同。
		name: "embed: 缓存键里不带模型名",
		file: "internal/embed/cache.go",
		old:  `h.Write([]byte(c.model))`,
		new:  `_ = c.model`,
	},
	{
		// 缓存不生效：命中率永远 0，参数扫描从几分钟变几小时。
		name: "embed: 缓存从不命中",
		file: "internal/embed/cache.go",
		old:  `if v, ok := c.load(t); ok {`,
		new:  `if v, ok := c.load(t); false && ok {`,
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
