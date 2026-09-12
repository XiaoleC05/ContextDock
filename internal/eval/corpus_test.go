package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStampRewritesHashAndChunks(t *testing.T) {
	root := setupTree(t, map[string]string{
		"zh.json": zhSet(`{"id":"zh-001","query":"q","expect":[{"source":"doc.md","quote":"结尾"}]}`),
	}, corpusDoc)

	// 先按现状打一次，把 chunks_at_default 填上——setupTree 只管指纹，
	// 刚写出来的清单里片段数还是 0。
	if _, err := Stamp(root); err != nil {
		t.Fatalf("首次 Stamp 失败: %v", err)
	}
	if _, err := Stamp(root); err != nil {
		t.Fatalf("第二次 Stamp 失败: %v", err)
	}

	// 第二次不该有任何变化：Stamp 必须是**幂等**的，
	// 否则每次跑都会报「语料变了」，这条报警很快就没人看了。
	// （幂等性由上面这句不报错 + 下面读到稳定值共同保证。）

	corpus, err := LoadCorpus(filepath.Join(root, "eval", CorpusManifest))
	if err != nil {
		t.Fatalf("读取清单失败: %v", err)
	}
	if corpus.Files[0].ChunksAtDefault != 1 {
		t.Errorf("片段数应为 1，实际 %d", corpus.Files[0].ChunksAtDefault)
	}
	if !strings.EqualFold(corpus.Files[0].SHA256, Fingerprint(corpusDoc)) {
		t.Error("指纹应等于按当前内容算出的值")
	}

	// 改内容后重打，指纹必须跟着变。
	changed := corpusDoc + "又加了一行，长到需要切成两片。" +
		strings.Repeat("填充。", 300)
	write(t, filepath.Join(root, "eval", "corpus", "doc.md"), changed)

	changes, err := Stamp(root)
	if err != nil {
		t.Fatalf("Stamp 失败: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("应报告 1 个文件变化，实际 %d", len(changes))
	}
	if changes[0].OldHash == changes[0].NewHash {
		t.Error("内容变了，新旧指纹不该相同")
	}
	if changes[0].NewChunks <= changes[0].OldChunks {
		t.Errorf("内容变长了，片段数应增加：%d → %d",
			changes[0].OldChunks, changes[0].NewChunks)
	}

	// 重打之后校验必须能过——这正是 Stamp 存在的意义。
	if _, err := Load(root); err != nil {
		t.Fatalf("重打指纹后应通过校验，实际: %v", err)
	}
}

func TestStampLeavesFileAloneWhenNothingChanged(t *testing.T) {
	// 没有变化就**不写文件**：时间戳变化会让人以为改过什么。
	root := setupTree(t, map[string]string{
		"zh.json": zhSet(`{"id":"zh-001","query":"q","expect":[{"source":"doc.md","quote":"结尾"}]}`),
	}, corpusDoc)

	if _, err := Stamp(root); err != nil {
		t.Fatalf("首次 Stamp 失败: %v", err)
	}

	manifest := filepath.Join(root, "eval", CorpusManifest)
	before, err := os.Stat(manifest)
	if err != nil {
		t.Fatalf("stat 失败: %v", err)
	}

	if _, err := Stamp(root); err != nil {
		t.Fatalf("Stamp 失败: %v", err)
	}

	after, err := os.Stat(manifest)
	if err != nil {
		t.Fatalf("stat 失败: %v", err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("没有变化时不该重写清单文件")
	}
}

func TestContentsRejectsUnknownFile(t *testing.T) {
	// 清单里写了路径但文件不在——这比指纹不符更基础，
	// 但它必须报成「读取失败并指出哪个文件」，而不是一个裸的 os 错误。
	root := setupTree(t, map[string]string{
		"zh.json": zhSet(`{"id":"zh-001","query":"q","expect":[{"source":"doc.md","quote":"结尾"}]}`),
	}, corpusDoc)

	if err := os.Remove(filepath.Join(root, "eval", "corpus", "doc.md")); err != nil {
		t.Fatalf("删除语料失败: %v", err)
	}

	_, err := Load(root)
	if err == nil {
		t.Fatal("期望报错")
	}
	if !strings.Contains(err.Error(), "doc.md") {
		t.Errorf("错误里应指出是哪个文件，实际:\n%v", err)
	}
}

// ---- 语料状态（#56）----

func TestDescribeCorpusMatchesChunkCount(t *testing.T) {
	// 这个函数存在的全部意义是**可靠地预言评测会看到什么**。
	// 它和评测器用了同一个切分器，所以两者的片段数必须一致——
	// 不一致的话，CI 断言的数和实际跑的数对不上，而那种错最难查。
	root := setupTree(t, map[string]string{
		"zh.json": zhSet(`{"id":"zh-001","query":"q","expect":[{"source":"doc.md","quote":"结尾"}]}`),
	}, corpusDoc)

	suite, err := Load(root)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}

	st, err := DescribeCorpus(suite, Options{MaxRunes: 400, Overlap: 60})
	if err != nil {
		t.Fatalf("描述失败: %v", err)
	}
	if st.TotalChunks == 0 {
		t.Fatal("应当切出片段")
	}
	if len(st.Files) != 1 || st.Files[0].Source != "doc.md" {
		t.Errorf("应当报出每份语料的片段数，实际 %+v", st.Files)
	}
	// 指纹要一起报出来：片段数变了有两种可能（切分参数变了 / 语料变了），
	// 只看片段数分不清，指纹能。
	if st.Fingerprints["doc.md"] == "" {
		t.Error("应当带上语料指纹")
	}
}

func TestDescribeCorpusRespectsParams(t *testing.T) {
	// 换切分参数，片段数必须跟着变——否则这个函数报的是一个
	// 与参数无关的常数，而它正是用来"预言某组参数下的结果"的。
	// 引文必须真的出现在语料里——校验会拦住不存在的引文，
	// 而那是刻意的（错标的评测集比读不进来危险得多）。
	root := setupTree(t, map[string]string{
		"zh.json": zhSet(`{"id":"zh-001","query":"q",
			"expect":[{"source":"doc.md","quote":"这是一句用来撑长度","occurrence":1}]}`),
	}, strings.Repeat("这是一句用来撑长度的中文内容。", 30))

	suite, err := Load(root)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}

	big, err := DescribeCorpus(suite, Options{MaxRunes: 400, Overlap: 60})
	if err != nil {
		t.Fatal(err)
	}
	small, err := DescribeCorpus(suite, Options{MaxRunes: 80, Overlap: 20})
	if err != nil {
		t.Fatal(err)
	}
	// 报告出来的参数也要跟得上——它是"这组数字是在什么参数下测的"的记录，
	// 写死会让报告与实际结果对不上。
	if big.MaxRunes != 400 || small.MaxRunes != 80 || small.Overlap != 20 {
		t.Errorf("状态里应当带上实际用的参数：big=%d/%d small=%d/%d",
			big.MaxRunes, big.Overlap, small.MaxRunes, small.Overlap)
	}
	if small.TotalChunks <= big.TotalChunks {
		t.Errorf("切得更碎时片段数应当更多：400/60 得 %d，80/20 得 %d",
			big.TotalChunks, small.TotalChunks)
	}
}
