package embed

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/XiaoleC05/ContextDock/internal/types"
)

// counting 记录上游被调用了几次、拿到了哪些文本。
type counting struct {
	calls int
	seen  []string
	err   error
}

func (c *counting) Embed(_ context.Context, texts []string) ([][]float32, error) {
	c.calls++
	c.seen = append(c.seen, texts...)
	if c.err != nil {
		return nil, c.err
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = vec(texts[i])
	}
	return out, nil
}

// vec 造一个由文本决定的确定性向量。
func vec(s string) []float32 {
	v := make([]float32, types.EmbeddingDim)
	for i := range v {
		v[i] = float32(len(s)%7) + float32(i%3)
	}
	return v
}

func TestCacheRoundTripSkipsUpstream(t *testing.T) {
	up := &counting{}
	c := NewCache(t.TempDir(), "m1", up)

	first, err := c.Embed(context.Background(), []string{"甲", "乙"})
	if err != nil {
		t.Fatalf("首次失败: %v", err)
	}
	if up.calls != 1 {
		t.Fatalf("首次应当调用上游一次，实际 %d", up.calls)
	}

	second, err := c.Embed(context.Background(), []string{"甲", "乙"})
	if err != nil {
		t.Fatalf("二次失败: %v", err)
	}
	if up.calls != 1 {
		t.Errorf("二次不该再调用上游，实际共 %d 次", up.calls)
	}
	for i := range first {
		if len(second[i]) != len(first[i]) {
			t.Fatalf("第 %d 个向量长度不同", i)
		}
		for j := range first[i] {
			if first[i][j] != second[i][j] {
				t.Fatalf("第 %d 个向量内容不同：缓存没有真正复用", i)
			}
		}
	}

	st := c.Stats()
	if st.Hits != 2 || st.Misses != 2 {
		t.Errorf("统计应为 命中2/未中2，实际 命中%d/未中%d", st.Hits, st.Misses)
	}
}

func TestCacheOnlySendsMissesUpstream(t *testing.T) {
	// 逐个查缓存，只把未命中的交给上游。
	// 整体查或整体放的话，一次导入里只要有一片是新的，
	// 整批都会重算——命中率会永远接近 0，而这正是缓存要解决的问题。
	up := &counting{}
	c := NewCache(t.TempDir(), "m1", up)

	if _, err := c.Embed(context.Background(), []string{"甲"}); err != nil {
		t.Fatal(err)
	}
	up.seen = nil

	if _, err := c.Embed(context.Background(), []string{"甲", "乙", "甲"}); err != nil {
		t.Fatal(err)
	}
	if len(up.seen) != 1 || up.seen[0] != "乙" {
		t.Errorf("上游应当只收到未命中的「乙」，实际 %q", up.seen)
	}
}

func TestCacheKeyIncludesModel(t *testing.T) {
	// ⚠️ 键里必须带模型名。
	//
	// 换模型是这套系统里最贵的一次改动。如果键里不含模型名，
	// 换完之后会**静默读到旧模型的向量**——维度一样、格式一样、跑得通，
	// 只是语义空间完全不同。症状是"检索质量莫名下降"，
	// 几乎不可能靠直觉定位到缓存。
	dir := t.TempDir()
	a := NewCache(dir, "model-A", &counting{})
	b := NewCache(dir, "model-B", &counting{})

	if _, err := a.Embed(context.Background(), []string{"同一段文字"}); err != nil {
		t.Fatal(err)
	}

	// 换个模型名重新构造缓存，指向同一个目录：必须未命中。
	up := &counting{}
	b2 := NewCache(dir, "model-B", up)
	if _, err := b2.Embed(context.Background(), []string{"同一段文字"}); err != nil {
		t.Fatal(err)
	}
	if up.calls != 1 {
		t.Error("换了模型名却命中了旧缓存——键里漏了模型名")
	}
	_ = b
}

func TestCacheIsNotAFactSource(t *testing.T) {
	// 删掉缓存目录只会变慢，不会改变任何数字。
	// 任何"删了缓存结果就不同"的情况都说明缓存键设计错了。
	up := &counting{}
	dir := t.TempDir()

	c1 := NewCache(dir, "m", up)
	v1, err := c1.Embed(context.Background(), []string{"丙"})
	if err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	c2 := NewCache(dir, "m", up)
	v2, err := c2.Embed(context.Background(), []string{"丙"})
	if err != nil {
		t.Fatal(err)
	}
	for j := range v1[0] {
		if v1[0][j] != v2[0][j] {
			t.Fatalf("清空缓存后向量变了（第 %d 维）", j)
		}
	}
}

func TestCachePropagatesUpstreamError(t *testing.T) {
	// 上游报错时不能静默返回空向量：那会让评测在"少了一部分向量"
	// 的状态下继续跑，而那些查询会莫名地召不回来。
	boom := fmt.Errorf("上游挂了")
	c := NewCache(t.TempDir(), "m", &counting{err: boom})
	if _, err := c.Embed(context.Background(), []string{"甲"}); err == nil {
		t.Fatal("期望把上游的错误透传出来")
	}
}

func TestNewCacheWithEmptyDirIsPassthrough(t *testing.T) {
	// dir 为空时不该缓存，但仍然要能正常工作——调用方不必分叉。
	up := &counting{}
	c := NewCache("", "m", up)

	if _, err := c.Embed(context.Background(), []string{"甲"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Embed(context.Background(), []string{"甲"}); err != nil {
		t.Fatal(err)
	}
	if up.calls != 2 {
		t.Errorf("关掉缓存后每次都该走上游，实际只调用了 %d 次", up.calls)
	}
	if st := c.Stats(); st.Hits != 0 {
		t.Errorf("关掉缓存时不该有命中，实际 %d", st.Hits)
	}
}
