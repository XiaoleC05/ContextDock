package types

import (
	"bytes"
	"encoding/json"
	"math"
	"testing"
)

// ---------------------------------------------------------------------------
// 序列化护栏
//
// 历史教训：本文件曾经用 strings.Contains(raw, "embedding")（小写）做断言，
// 而 Go 在字段没有 json 标签时序列化出来的键是 "Embedding"（大写）。
// 大小写不匹配 → 断言永远不触发 → 这个测试根本守不住 json:"-"，
// 删掉标签它照样绿。现已改为**按键集合精确断言**，大小写不再有逃逸空间。
// ---------------------------------------------------------------------------

// chunkJSONKeys 返回序列化后的顶层键集合。
func chunkJSONKeys(t *testing.T, v any) map[string]json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	return m
}

// TestChunkJSONKeySetIsExact 精确断言 Chunk 的 JSON 键集合。
//
// 这是防回归测试：删掉 chunk.go 里的 json:"-" 标签会让本测试立刻失败，
// 而不是等到某天发现 MCP 返回了 100KB 垃圾数据才察觉。
//
// 用"键集合精确相等"而不是"文本里没有某个词"，是为了同时挡住两种逃逸：
//   - 大小写变化（"Embedding" / "embedding"）
//   - 字段改名（"EmbeddingVec"）
func TestChunkJSONKeySetIsExact(t *testing.T) {
	c := Chunk{
		ID:         1,
		DocumentID: 10,
		Ordinal:    0,
		Content:    "ContextDock 是一个混合检索服务",
		Embedding:  []float32{0.1, 0.2, 0.3},
	}

	got := chunkJSONKeys(t, c)

	// Metadata 为 nil 时被 omitempty 省略，所以这里只应该有 4 个键。
	want := map[string]bool{
		"id":           true,
		"document_id":  true,
		"ordinal":      true,
		"start_offset": true,
		"end_offset":   true,
		"content":      true,
	}

	for k := range got {
		if !want[k] {
			t.Errorf("JSON 里出现了不该有的键 %q —— 是不是 Embedding 的 json:\"-\" 标签被删了？", k)
		}
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("JSON 里缺少键 %q", k)
		}
	}

	// 反序列化回来，Embedding 必须是空的。
	var back Chunk
	raw, _ := json.Marshal(c)
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if len(back.Embedding) != 0 {
		t.Errorf("反序列化后 Embedding 应为空，实际长度 %d", len(back.Embedding))
	}
	// 其他字段必须完整保留，否则就是"忽略过头"了。
	if back.Content != c.Content || back.DocumentID != c.DocumentID {
		t.Errorf("其他字段丢失: %+v", back)
	}
}

// TestMCPEnvelopeCarriesNoEmbedding 验证真实的 MCP 输出信封（结果切片）不含向量。
//
// 单测一条 Chunk 是不够的——真正返回给 Agent 的是 []SearchResult。
// 这里同时做行为断言（体积），因为"不返回向量"的最终目的是"不浪费带宽"。
func TestMCPEnvelopeCarriesNoEmbedding(t *testing.T) {
	results := make([]SearchResult, 10)
	for i := range results {
		results[i] = SearchResult{
			Chunk: Chunk{
				ID:         int64(i + 1),
				DocumentID: 1,
				Ordinal:    i,
				Content:    "这是一段用来测试序列化体积的正文内容",
				Embedding:  make([]float32, EmbeddingDim), // 1024 维全零
			},
			Score:       1.0 / 61,
			LexicalRank: i + 1,
			VectorRank:  i + 3,
		}
	}

	raw, err := json.Marshal(results)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}

	if bytes.Contains(raw, []byte("Embedding")) || bytes.Contains(raw, []byte("embedding")) {
		t.Error("结果信封里出现了向量字段")
	}
	// 10 条结果若带上向量，光是数字就要 100KB 以上。
	if len(raw) > 4000 {
		t.Errorf("序列化结果 %d 字节，疑似把向量也发出去了（10 条结果应远小于 4KB）", len(raw))
	}
	// 也应该看不到数组形态的大段数字。
	if bytes.Count(raw, []byte(",")) > 500 {
		t.Errorf("逗号数量异常（%d），疑似有长数组被序列化", bytes.Count(raw, []byte(",")))
	}
}

// TestEmptyMetadataOmitted 验证空 Metadata 不会产生 "metadata": null。
func TestEmptyMetadataOmitted(t *testing.T) {
	got := chunkJSONKeys(t, Chunk{ID: 1, DocumentID: 1, Content: "正文"})
	if _, ok := got["metadata"]; ok {
		t.Error("nil Metadata 应被 omitempty 省略，但出现了 metadata 键")
	}

	// 有值时必须出现。
	c := Chunk{ID: 1, DocumentID: 1, Content: "正文"}
	c.SetMetadata(MetadataKeyHeading, "安装指南")
	got = chunkJSONKeys(t, c)
	if _, ok := got["metadata"]; !ok {
		t.Error("非空 Metadata 应该被序列化，但没有出现 metadata 键")
	}
}

// ---------------------------------------------------------------------------
// 零值安全性
// ---------------------------------------------------------------------------

// TestSetMetadataOnNilMapDoesNotPanic 守护裸 map 的写入 panic。
//
// 背景：Chunk 通常用结构体字面量构造，此时 Metadata 必然是 nil。
// 切分器最自然的写法 `c.Metadata["heading"] = ...` 会直接 panic，
// 而且只在"某个文档恰好有标题"时才触发，容易拖到很后面才暴露。
func TestSetMetadataOnNilMapDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("SetMetadata 在 nil map 上 panic 了: %v", r)
		}
	}()

	c := Chunk{DocumentID: 1, Content: "正文"}
	if c.Metadata != nil {
		t.Fatal("前置条件错误：Metadata 应为 nil")
	}
	c.SetMetadata(MetadataKeyHeading, "快速开始")
	if c.Metadata[MetadataKeyHeading] != "快速开始" {
		t.Errorf("写入失败，实际: %v", c.Metadata)
	}

	d := Document{Title: "t", Content: "c"}
	d.SetMetadata("ext", ".md")
	if d.Metadata["ext"] != ".md" {
		t.Errorf("Document 写入失败，实际: %v", d.Metadata)
	}
}

// TestIsPersistedAndIsEmbedded 验证零值判断收口到了方法里。
func TestIsPersistedAndIsEmbedded(t *testing.T) {
	c := Chunk{DocumentID: 1, Ordinal: 0, Content: "正文"}
	if c.IsPersisted() || c.IsEmbedded() {
		t.Error("新建的 Chunk 不应是已落库或已嵌入状态")
	}
	c.ID = 7
	c.Embedding = []float32{0.1}
	if !c.IsPersisted() || !c.IsEmbedded() {
		t.Error("赋值后应变为已落库且已嵌入")
	}
}

// ---------------------------------------------------------------------------
// 融合键与索引文本
// ---------------------------------------------------------------------------

// TestStableKey 守护跨通道融合键。
//
// 背景：RRF 要按某个键把两路结果对应起来。若直接用 Chunk.ID，
// 落库前所有 ID 都是 0，**不同片段会被合并成一条**，且不报错、只是结果变差。
func TestStableKey(t *testing.T) {
	persisted := Chunk{ID: 42, DocumentID: 1, Ordinal: 3}
	unpersisted := Chunk{ID: 0, DocumentID: 1, Ordinal: 3}

	if persisted.StableKey() == unpersisted.StableKey() {
		t.Error("已落库与未落库的 chunk 应有不同的稳定键")
	}

	// 同一文档的不同片段必须不同键——这是最容易踩的坑。
	a := Chunk{DocumentID: 1, Ordinal: 3}
	b := Chunk{DocumentID: 1, Ordinal: 4}
	if a.StableKey() == b.StableKey() {
		t.Errorf("不同片段撞键了: %q", a.StableKey())
	}

	// 不同文档的同序号片段也必须不同。
	c := Chunk{DocumentID: 2, Ordinal: 3}
	if a.StableKey() == c.StableKey() {
		t.Errorf("跨文档撞键了: %q", a.StableKey())
	}

	// 未落库时键应当稳定可复现。
	if a.StableKey() != (Chunk{DocumentID: 1, Ordinal: 3}).StableKey() {
		t.Error("相同内容的 StableKey 应当可复现")
	}
}

// TestIndexText 守护"被索引文本"的唯一来源。
//
// 背景：Embedder 和 BM25 必须共用同一个文本。若两边各自拼一次
// （分隔符不同、或一边忘了拼面包屑），两路检索搜的就不是同一个文本，
// 召回不可比、RRF 质量下降，而且极难排查。
func TestIndexText(t *testing.T) {
	plain := Chunk{DocumentID: 1, Content: "运行 go build"}
	if plain.IndexText() != plain.Content {
		t.Errorf("无面包屑时 IndexText 应等于 Content，实际 %q", plain.IndexText())
	}

	withHeading := Chunk{DocumentID: 1, Content: "运行 go build"}
	withHeading.SetMetadata(MetadataKeyHeading, "安装指南 > 快速开始")
	want := "安装指南 > 快速开始\n运行 go build"
	if withHeading.IndexText() != want {
		t.Errorf("IndexText 格式变了:\n期望 %q\n实际 %q", want, withHeading.IndexText())
	}

	// 面包屑为空字符串时，应退化为纯 Content（不能多出一个换行）。
	empty := Chunk{DocumentID: 1, Content: "运行 go build"}
	empty.SetMetadata(MetadataKeyHeading, "")
	if empty.IndexText() != empty.Content {
		t.Errorf("空面包屑应退化为 Content，实际 %q", empty.IndexText())
	}

	// 文档来源**不进**索引文本，即使和面包屑同时存在。
	//
	// 文件路径里全是 `d`、`05`、`code`、`exe` 这类噪声 token。
	// 一旦有人顺手把它拼进来，BM25 和向量搜的文本就变了——
	// 检索质量下降但**不报任何错**，而且现有索引全部作废。
	withSource := Chunk{DocumentID: 1, Content: "运行 go build"}
	withSource.SetMetadata(MetadataKeyHeading, "安装指南 > 快速开始")
	withSource.SetMetadata(MetadataKeySource, "d:/05_Code/ContextDock/README.md")
	if got := withSource.IndexText(); got != want {
		t.Errorf("来源不该进索引文本:\n期望 %q\n实际 %q", want, got)
	}
}

// ---------------------------------------------------------------------------
// RRF
// ---------------------------------------------------------------------------

// TestRRFScoreIgnoresUnrecalledChannel 是全套测试里最重要的一个。
//
// 背景：LexicalRank / VectorRank 用 0 表示"该通道未召回"。
// 如果把 0 直接代入公式：
//
//	1/(60+0) = 0.016667   ← 未召回，却比第一名还高
//	1/(60+1) = 0.016393   ← 真正的第一名
//
// "未召回"会拿到比第一名更高的分。这个 bug 不会报错，
// 只是排序错乱，排查时会先怀疑分词和模型。
func TestRRFScoreIgnoresUnrecalledChannel(t *testing.T) {
	// 只被向量召回，且是向量第 1 名。
	r := SearchResult{VectorRank: 1}

	got := r.RRFScore(RRFK)
	want := 1.0 / float64(RRFK+1) // 只应有向量那一路的贡献

	if math.Abs(got-want) > 1e-12 {
		t.Errorf("未召回的通道不应贡献分数:\n期望 %.10f（仅向量路）\n实际 %.10f\n"+
			"若实际值约等于 %.10f，说明 0 被当成了第 0 名代入公式",
			want, got, want+1.0/float64(RRFK))
	}

	// 两路都召回时应为两路之和。
	both := SearchResult{LexicalRank: 1, VectorRank: 1}
	wantBoth := 2.0 / float64(RRFK+1)
	if math.Abs(both.RRFScore(RRFK)-wantBoth) > 1e-12 {
		t.Errorf("两路都召回: 期望 %.10f，实际 %.10f", wantBoth, both.RRFScore(RRFK))
	}

	// 两路都没召回（理论上的空结果）应为 0。
	none := SearchResult{}
	if none.RRFScore(RRFK) != 0 {
		t.Errorf("两路都未召回应为 0，实际 %v", none.RRFScore(RRFK))
	}

	// 名次越靠前分越高。
	first := SearchResult{LexicalRank: 1}.RRFScore(RRFK)
	second := SearchResult{LexicalRank: 2}.RRFScore(RRFK)
	if !(first > second) {
		t.Errorf("第 1 名应高于第 2 名: %.10f vs %.10f", first, second)
	}
}

// TestRRFScoreDefaultK 验证 k<=0 时回落到默认值。
func TestRRFScoreDefaultK(t *testing.T) {
	r := SearchResult{LexicalRank: 1}
	if r.RRFScore(0) != r.RRFScore(RRFK) {
		t.Error("k<=0 时应使用默认值 RRFK")
	}
	if r.RRFScore(-5) != r.RRFScore(RRFK) {
		t.Error("负数 k 时应使用默认值 RRFK")
	}
}

// TestRRFConstantIsCanonical 钉住 k=60 这个论文推荐值。
func TestRRFConstantIsCanonical(t *testing.T) {
	if RRFK != 60 {
		t.Errorf("RRFK 应为论文推荐值 60，实际 %d", RRFK)
	}
}

// ---------------------------------------------------------------------------
// 通道标识
// ---------------------------------------------------------------------------

// TestSearchResultMatchedBy 用表驱动的方式覆盖 MatchedBy 的全部分支。
//
// 表驱动（table-driven）是 Go 测试的惯用写法：把输入和期望值放进一个
// 切片里循环跑。好处是加用例只要加一行，不用复制粘贴整个测试函数。
func TestSearchResultMatchedBy(t *testing.T) {
	tests := []struct {
		name        string
		lexicalRank int
		vectorRank  int
		want        []Retriever
		wantBoth    bool
	}{
		{
			name:        "两路都召回",
			lexicalRank: 1,
			vectorRank:  3,
			want:        []Retriever{RetrieverLexical, RetrieverVector},
			wantBoth:    true,
		},
		{
			name:        "只有关键词召回",
			lexicalRank: 2,
			vectorRank:  0,
			want:        []Retriever{RetrieverLexical},
			wantBoth:    false,
		},
		{
			name:        "只有向量召回",
			lexicalRank: 0,
			vectorRank:  5,
			want:        []Retriever{RetrieverVector},
			wantBoth:    false,
		},
		{
			name:        "两路都没召回",
			lexicalRank: 0,
			vectorRank:  0,
			want:        []Retriever{},
			wantBoth:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := SearchResult{LexicalRank: tt.lexicalRank, VectorRank: tt.vectorRank}

			got := r.MatchedBy()
			// 断言非 nil，让调用方可以直接 range 而不用先判空。
			if got == nil {
				t.Fatal("MatchedBy 不应返回 nil")
			}
			if len(got) != len(tt.want) {
				t.Fatalf("MatchedBy() 长度: 期望 %v，实际 %v", tt.want, got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("MatchedBy()[%d]: 期望 %q，实际 %q", i, tt.want[i], got[i])
				}
			}

			if r.MatchedByBoth() != tt.wantBoth {
				t.Errorf("MatchedByBoth(): 期望 %v，实际 %v", tt.wantBoth, r.MatchedByBoth())
			}
		})
	}
}

// TestRetrieverWireValues 钉住 Retriever 的线协议值。
//
// 这些字符串会出现在 MCP 的 JSON 输出里，一旦改动就是破坏性变更。
// 只测 lexical 是不够的——两个常量都必须钉住。
func TestRetrieverWireValues(t *testing.T) {
	cases := map[Retriever]string{
		RetrieverLexical: `"lexical"`,
		RetrieverVector:  `"vector"`,
	}
	for r, want := range cases {
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("序列化失败: %v", err)
		}
		if string(raw) != want {
			t.Errorf("%v 的线协议值应为 %s，实际 %s", r, want, raw)
		}
	}
}

// ---------------------------------------------------------------------------
// 校验
// ---------------------------------------------------------------------------

func TestChunkValidate(t *testing.T) {
	valid := Chunk{DocumentID: 1, Ordinal: 0, Content: "正文"}
	if err := valid.Validate(); err != nil {
		t.Errorf("合法 chunk 不应报错: %v", err)
	}

	// 带着正确维度的向量也应当合法。
	withEmbedding := Chunk{DocumentID: 1, Content: "正文", Embedding: make([]float32, EmbeddingDim)}
	if err := withEmbedding.Validate(); err != nil {
		t.Errorf("维度正确的 chunk 不应报错: %v", err)
	}

	tests := []struct {
		name string
		c    Chunk
	}{
		{"DocumentID 为 0", Chunk{DocumentID: 0, Content: "正文"}},
		{"Ordinal 为负", Chunk{DocumentID: 1, Ordinal: -1, Content: "正文"}},
		{"Content 为空", Chunk{DocumentID: 1, Content: ""}},
		{"Content 全是空白", Chunk{DocumentID: 1, Content: "  \n\t "}},
		{"向量维度错误", Chunk{DocumentID: 1, Content: "正文", Embedding: make([]float32, 768)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.c.Validate(); err == nil {
				t.Error("应当报错，但返回了 nil")
			}
		})
	}
}

func TestDocumentValidate(t *testing.T) {
	if err := (Document{Title: "标题", Content: "正文"}).Validate(); err != nil {
		t.Errorf("合法文档不应报错: %v", err)
	}
	if err := (Document{Title: "", Content: "正文"}).Validate(); err == nil {
		t.Error("空标题应当报错")
	}
	if err := (Document{Title: "标题", Content: "   "}).Validate(); err == nil {
		t.Error("空正文应当报错")
	}
}

// ---------------------------------------------------------------------------
// 日志护栏
// ---------------------------------------------------------------------------

// TestStringDoesNotDumpEmbedding 守护 fmt 的日志泄漏。
//
// json:"-" 只挡住 encoding/json，**挡不住 %v / %+v**。
// 单条 Chunk 的向量是 1024 个浮点数（约 8KB 文本），几十条就能淹掉日志。
func TestStringDoesNotDumpEmbedding(t *testing.T) {
	c := Chunk{
		ID:         1,
		DocumentID: 2,
		Ordinal:    3,
		Content:    "一段比较长的中文正文内容，用来验证日志输出会被截断",
		Embedding:  make([]float32, EmbeddingDim),
	}
	c.Embedding[0] = 0.123456

	s := c.String()
	if len(s) > 300 {
		t.Errorf("String() 输出过长（%d 字节），疑似把向量打出来了:\n%s", len(s), s)
	}
	// 必须包含可定位的信息，否则日志没有价值。
	for _, want := range []string{"id:1", "doc:2", "ordinal:3", "1024"} {
		if !bytes.Contains([]byte(s), []byte(want)) {
			t.Errorf("String() 输出缺少 %q: %s", want, s)
		}
	}
	// 不应包含具体的向量分量。
	if bytes.Contains([]byte(s), []byte("0.123456")) {
		t.Errorf("String() 泄漏了向量分量: %s", s)
	}

	// Document 同理：不应把整篇原文打出来。
	d := Document{ID: 1, Title: "标题", Content: string(make([]byte, 100000))}
	if len(d.String()) > 300 {
		t.Errorf("Document.String() 输出过长: %d 字节", len(d.String()))
	}
}

// TestTruncateRunesHandlesMultiByte 验证截断按字符而不是字节。
//
// Go 的 len() 是字节数，一个汉字占 3 字节。按字节截断会把汉字切成半个，
// 输出乱码。这是中文场景下最容易踩的语言级 bug。
func TestTruncateRunesHandlesMultiByte(t *testing.T) {
	// 40 个汉字 = 120 字节。若按字节截断会切碎多字节字符。
	s := "中文测试文本中文测试文本中文测试文本中文测试文本中文测试文本中文测试文本"
	got := truncateRunes(s, 10)
	want := "中文测试文本中文测试…"
	if got != want {
		t.Errorf("按字符截断失败:\n期望 %q\n实际 %q", want, got)
	}
	// 截断结果必须是合法 UTF-8。
	for _, r := range got {
		if r == '�' {
			t.Error("截断产生了非法 UTF-8（乱码）")
			break
		}
	}
}
