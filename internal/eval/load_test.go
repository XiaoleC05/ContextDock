package eval

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- 测试脚手架 ----

// corpusDoc 是测试用的语料正文（故意含中文，用来验证 rune 偏移）。
const corpusDoc = "第一段：这是开头。\n" +
	"第二段：gojieba 依赖 cgo，交叉编译会失败。\n" +
	"第三段：RRF 只用名次，不用分数。\n" +
	"第四段：结尾。\n"

// setupTree 在临时目录里搭出一套完整的 eval/ 结构。
//
// 语料指纹**由 Fingerprint 现算**，而不是把常量抄进测试：
// 抄常量的话，改动归一化逻辑（比如不再把 CRLF 转 LF）会让指纹校验
// 在测试里也一起失效，两边同错，反而测不出问题。
func setupTree(t *testing.T, queries map[string]string, corpusText string) string {
	t.Helper()
	root := t.TempDir()

	write(t, filepath.Join(root, "eval", "corpus", "doc.md"), corpusText)
	write(t, filepath.Join(root, "eval", CorpusManifest), fmt.Sprintf(`{
  "version": 1,
  "files": [
    {"source": "doc.md", "path": "eval/corpus/doc.md",
     "sha256": %q, "origin": "测试", "why": "测试"}
  ]
}`, Fingerprint(corpusText)))

	for name, body := range queries {
		write(t, filepath.Join(root, "eval", QueriesDir, name), body)
	}
	return root
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("写入 %s 失败: %v", path, err)
	}
}

// zhSet 包一个合法的中文子集文件。
func zhSet(queries string) string {
	return fmt.Sprintf(`{
  "version": 1, "group": "zh", "title": "中文提问",
  "queries": [%s]
}`, queries)
}

// ---- 正常路径 ----

func TestLoadResolvesQuoteToRuneSpan(t *testing.T) {
	// 这条测试的核心是**单位**：引文之前有多少个汉字，定位结果就该是多少。
	// 按字节算会得到 3 倍的值，而且不会有任何报错——
	// 命中判定随后整体错位，表现只是「命中率莫名其妙偏低」。
	root := setupTree(t, map[string]string{
		"zh.json": zhSet(`{
			"id": "zh-001", "query": "为什么不用中文分词库",
			"expect": [{"source": "doc.md", "quote": "gojieba 依赖 cgo"}]
		}`),
	}, corpusDoc)

	suite, err := Load(root)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}

	got := suite.Queries()[0].Expect[0]

	// 手工数（**故意不复用被测代码**，否则算错了也会一起错）：
	//   "第一段：这是开头。\n"  → 第一段(3) ：(1) 这是开头(4) 。(1) \n(1) = 10
	//   "第二段："              → 第二段(3) ：(1)                  = 4
	//   合计 14
	//
	// 若按字节算，前 14 个字符里有 9 个汉字 × 3 字节，会得到远大于 14 的值。
	wantStart := 14
	wantEnd := wantStart + len([]rune("gojieba 依赖 cgo"))

	if got.Start != wantStart || got.End != wantEnd {
		t.Errorf("引文定位错误：得到 [%d, %d)，期望 [%d, %d)",
			got.Start, got.End, wantStart, wantEnd)
	}

	// 断言定位结果确实切回了原文——只有单位对了才可能成立。
	content, _ := suite.Content("doc.md")
	runes := []rune(content)
	if snippet := string(runes[got.Start:got.End]); snippet != "gojieba 依赖 cgo" {
		t.Errorf("按定位区间取回的文本是 %q，期望 %q", snippet, "gojieba 依赖 cgo")
	}
}

func TestLoadDefaultsGradeToFull(t *testing.T) {
	root := setupTree(t, map[string]string{
		"zh.json": zhSet(`{
			"id": "zh-001", "query": "q",
			"expect": [{"source": "doc.md", "quote": "RRF 只用名次"}]
		}`),
	}, corpusDoc)

	suite, err := Load(root)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if g := suite.Queries()[0].Expect[0].Grade; g != GradeFull {
		t.Errorf("省略 grade 时应补成 GradeFull(%d)，实际 %d", GradeFull, g)
	}
}

func TestLoadOrdersSetsByAllGroups(t *testing.T) {
	// 报告要逐次比对，子集顺序就必须固定，不能随文件名或目录遍历顺序变。
	root := setupTree(t, map[string]string{
		"zh.json": zhSet(`{"id":"zh-001","query":"中文","expect":[{"source":"doc.md","quote":"结尾"}]}`),
		"en.json": `{
			"version": 1, "group": "en", "title": "English",
			"queries": [{"id":"en-001","query":"english",
			             "expect":[{"source":"doc.md","quote":"结尾"}]}]
		}`,
	}, corpusDoc)

	suite, err := Load(root)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if len(suite.Sets) != 2 {
		t.Fatalf("应加载 2 个子集，实际 %d", len(suite.Sets))
	}
	if suite.Sets[0].Group != GroupZH || suite.Sets[1].Group != GroupEN {
		t.Errorf("子集顺序应为 [zh en]，实际 [%s %s]", suite.Sets[0].Group, suite.Sets[1].Group)
	}
}

// ---- 定位的边界 ----

func TestLocateQuoteOccurrence(t *testing.T) {
	const doc = "安装前请阅读。后续安装步骤见下。安装完成后重启。"

	tests := []struct {
		name       string
		quote      string
		occurrence int
		want       string // 期望切回的原文本
		wantErr    error
	}{
		{name: "第一次", quote: "安装", occurrence: 1, want: "安装"},
		{name: "第二次", quote: "安装", occurrence: 2, want: "安装"},
		{name: "第三次", quote: "安装", occurrence: 3, want: "安装"},
		{name: "省略 occurrence 但出现多次应报错", quote: "安装", occurrence: 0,
			wantErr: ErrQuoteAmbiguous},
		{name: "出现次数不够应报错", quote: "安装", occurrence: 4, wantErr: ErrQuoteAmbiguous},
		{name: "找不到", quote: "不存在的内容", occurrence: 1, wantErr: ErrQuoteNotFound},
		{name: "空引文", quote: "", occurrence: 1, wantErr: ErrQuoteEmpty},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			start, end, err := LocateQuote(doc, tc.quote, tc.occurrence)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("期望错误 %v，实际 %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("不该报错: %v", err)
			}
			if got := string([]rune(doc)[start:end]); got != tc.want {
				t.Errorf("定位到 %q，期望 %q", got, tc.want)
			}
			// 逐次出现必须落在不同位置，否则 occurrence 形同虚设。
			if tc.occurrence == 3 && start == 0 {
				t.Error("第三次出现定位到了开头")
			}
		})
	}
}

func TestLocateQuoteOccurrencesDoNotOverlap(t *testing.T) {
	// "aaaa" 里找 "aa"：不重叠语义下第 2 次在 rune 2，不是 1。
	// 重叠语义会让 occurrence 的含义无法向标注者解释。
	start, _, err := LocateQuote("aaaa", "aa", 2)
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if start != 2 {
		t.Errorf("第 2 次出现应起于 rune 2，实际 %d", start)
	}
}

func TestSpanOverlapsIsHalfOpen(t *testing.T) {
	tests := []struct {
		name string
		a, b Span
		want bool
	}{
		{"完全包含", Span{0, 10}, Span{2, 4}, true},
		{"部分重叠", Span{0, 10}, Span{8, 20}, true},
		{"端点相接不算重叠", Span{0, 10}, Span{10, 20}, false},
		{"分离", Span{0, 10}, Span{20, 30}, false},
		{"零长度区间不重叠", Span{5, 5}, Span{0, 10}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a.Overlaps(tc.b); got != tc.want {
				t.Errorf("%v.Overlaps(%v) = %v，期望 %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// ---- 错误路径 ----

func TestLoadReportsWhichQuery(t *testing.T) {
	// 报错必须指到**具体哪一条**。只说「格式错误」的话，
	// 一份几十条的评测集要人工从头翻一遍才能找到。
	tests := []struct {
		name    string
		query   string
		wantSub string // 错误里必须出现的内容
	}{
		{
			name:    "引文不在原文里",
			query:   `{"id":"zh-007","query":"问点什么","expect":[{"source":"doc.md","quote":"这段话不存在"}]}`,
			wantSub: "zh-007",
		},
		{
			name:    "引文多次出现未指定",
			query:   `{"id":"zh-008","query":"问点什么","expect":[{"source":"doc.md","quote":"段"}]}`,
			wantSub: "zh-008",
		},
		{
			name:    "query 为空",
			query:   `{"id":"zh-009","query":"   ","expect":[{"source":"doc.md","quote":"结尾"}]}`,
			wantSub: "query 为空",
		},
		{
			name:    "没有期望命中",
			query:   `{"id":"zh-010","query":"问点什么","expect":[]}`,
			wantSub: "没有任何期望命中",
		},
		{
			name:    "source 不在清单里",
			query:   `{"id":"zh-011","query":"问点什么","expect":[{"source":"nope.md","quote":"结尾"}]}`,
			wantSub: "nope.md",
		},
		{
			name:    "kind 非法",
			query:   `{"id":"zh-012","query":"问点什么","kind":"随便写的","expect":[{"source":"doc.md","quote":"结尾"}]}`,
			wantSub: "kind",
		},
		{
			name:    "grade 非法",
			query:   `{"id":"zh-013","query":"问点什么","expect":[{"source":"doc.md","quote":"结尾","grade":9}]}`,
			wantSub: "grade",
		},
		{
			name:    "字段名拼错",
			query:   `{"id":"zh-014","query":"问点什么","expect":[{"source":"doc.md","quotes":"结尾"}]}`,
			wantSub: "quotes",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := setupTree(t, map[string]string{"zh.json": zhSet(tc.query)}, corpusDoc)
			_, err := Load(root)
			if err == nil {
				t.Fatal("期望报错，实际通过了校验")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("错误信息里应出现 %q，实际是:\n%v", tc.wantSub, err)
			}
		})
	}
}

func TestLoadRejectsDuplicateQueryAcrossFiles(t *testing.T) {
	// 跨文件查重：只差首尾空格、只差大小写的两个 query 实际是同一条，
	// 它们会让整体指标里这一条被算两遍。
	root := setupTree(t, map[string]string{
		"zh.json": zhSet(`{"id":"zh-001","query":"为什么用 RRF","expect":[{"source":"doc.md","quote":"结尾"}]}`),
		"en.json": `{
			"version": 1, "group": "en", "title": "English",
			"queries": [{"id":"en-001","query":"  为什么用 RRF  ",
			             "expect":[{"source":"doc.md","quote":"结尾"}]}]
		}`,
	}, corpusDoc)

	_, err := Load(root)
	if err == nil {
		t.Fatal("期望报重复，实际通过了校验")
	}
	if !strings.Contains(err.Error(), "en-001") {
		t.Errorf("错误里应指出后一条的 id，实际:\n%v", err)
	}
}

func TestLoadRejectsDuplicateID(t *testing.T) {
	root := setupTree(t, map[string]string{
		"zh.json": zhSet(
			`{"id":"zh-001","query":"问题一","expect":[{"source":"doc.md","quote":"结尾"}]},
			 {"id":"zh-001","query":"问题二","expect":[{"source":"doc.md","quote":"开头"}]}`),
	}, corpusDoc)

	_, err := Load(root)
	if err == nil {
		t.Fatal("期望报 id 重复，实际通过了校验")
	}
	if !strings.Contains(err.Error(), "id") {
		t.Errorf("错误里应提到 id，实际:\n%v", err)
	}
}

func TestLoadRejectsGroupFilenameMismatch(t *testing.T) {
	// 文件叫 zh.json 但 group 写成 en —— 这会让「统计进哪个组」
	// 与「文件叫什么」出现两个真相，只有对着报告逐条核对才看得出来。
	root := setupTree(t, map[string]string{
		"zh.json": `{
			"version": 1, "group": "en", "title": "写错了",
			"queries": [{"id":"zh-001","query":"q","expect":[{"source":"doc.md","quote":"结尾"}]}]
		}`,
	}, corpusDoc)

	_, err := Load(root)
	if err == nil {
		t.Fatal("期望报 group 与文件名不符")
	}
	if !strings.Contains(err.Error(), "en.json") {
		t.Errorf("错误里应指出正确的文件名，实际:\n%v", err)
	}
}

func TestLoadReportsSyntaxErrorWithLineNumber(t *testing.T) {
	// 手工编辑评测集时最常见的错误就是漏了个逗号。
	// 「第 12345 字节附近有问题」帮不上忙——得先知道那在第几行。
	root := setupTree(t, map[string]string{
		"zh.json": `{
  "version": 1,
  "group": "zh",
  "title": "中文提问"
  "queries": []
}`,
	}, corpusDoc)

	_, err := Load(root)
	if err == nil {
		t.Fatal("期望报语法错误")
	}
	if !strings.Contains(err.Error(), "行") {
		t.Errorf("语法错误里应带行号，实际:\n%v", err)
	}
}

func TestLoadRejectsCorpusDrift(t *testing.T) {
	// 语料被改过而指纹没更新时，必须**报错退出**而不是静默重跑：
	// 静默重跑会产出一份与历史不可比的数字，而没有任何人知道。
	root := setupTree(t, map[string]string{
		"zh.json": zhSet(`{"id":"zh-001","query":"q","expect":[{"source":"doc.md","quote":"结尾"}]}`),
	}, corpusDoc)

	// 改动语料一字，指纹立刻失配。
	write(t, filepath.Join(root, "eval", "corpus", "doc.md"), corpusDoc+"多出来的一行\n")

	_, err := Load(root)
	if !errors.Is(err, ErrCorpusDrift) {
		t.Fatalf("期望 ErrCorpusDrift，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "doc.md") {
		t.Errorf("错误里应指出是哪个文件，实际:\n%v", err)
	}
}

func TestLoadIsSilentOnSuccess(t *testing.T) {
	// 校验通过时 Problems 必须为空 —— cmd/eval -validate 靠它保证
	// 「通过时不输出任何噪音」。
	root := setupTree(t, map[string]string{
		"zh.json": zhSet(`{"id":"zh-001","query":"q","expect":[{"source":"doc.md","quote":"结尾"}]}`),
	}, corpusDoc)

	if _, err := Load(root); err != nil {
		t.Fatalf("应通过校验，实际: %v", err)
	}

	// Problems 本身也要在工作正常时不产生 error。
	p := &Problems{}
	if !p.Empty() || p.Err() != nil {
		t.Error("没有问题时 Problems 应为空且 Err() 返回 nil")
	}
}

func TestProblemsReportsAllAndTruncates(t *testing.T) {
	p := &Problems{}
	p.Addf("问题 %d", 1)
	p.Addf("问题 %d", 2)
	err := p.Err()
	if err == nil {
		t.Fatal("应返回错误")
	}
	if !strings.Contains(err.Error(), "问题 1") || !strings.Contains(err.Error(), "问题 2") {
		t.Errorf("应一次报出全部问题，实际:\n%v", err)
	}

	// 超过上限时截断，但要写明还剩多少，不做无声丢弃。
	p2 := &Problems{}
	for i := 0; i < maxProblems+5; i++ {
		p2.Addf("第 %d 条", i)
	}
	if msg := p2.Err().Error(); !strings.Contains(msg, fmt.Sprintf("还有 %d 条", 5)) {
		t.Errorf("截断提示不对:\n%s", msg)
	}
}

// ---- 指纹 ----

func TestFingerprintIgnoresNewlineStyle(t *testing.T) {
	// Windows 上 checkout 出来是 CRLF，CI 上是 LF。不归一化的话
	// 同一份语料在两个平台上指纹不同，CI 会报「语料漂移」——
	// 与真实原因（换行符）完全无关，排查方向会从一开始就错。
	if Fingerprint("a\nb\n") != Fingerprint("a\r\nb\r\n") {
		t.Error("LF 与 CRLF 的指纹应相同")
	}
	if Fingerprint("a\nb\n") != Fingerprint("a\rb\r") {
		t.Error("LF 与 CR 的指纹应相同")
	}
}
