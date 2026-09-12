package embed

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"

	"github.com/XiaoleC05/ContextDock/internal/types"
)

// Cache 给任意 Embedder 套一层磁盘缓存。
//
// # 为什么需要它
//
// 参数扫描（MaxRunes × Overlap 的各种组合）要反复重灌语料。切分参数一变，
// 片段边界就全变——但**内容大范围重叠**：400 字切成两片和切成三片，
// 大部分文字还是那些文字。按内容寻址的缓存能命中绝大部分，于是几十种
// 配置的扫描从「几小时」降到「几分钟」。
//
// # 为什么按内容寻址，而不是按片段序号
//
// 按序号缓存是错的：换了参数，第 7 个片段已经不是原来那段文字了。
// 按内容存取则天然正确——同一段文字永远拿到同一个向量，
// 这也顺带让评测**可复现**：同样的输入必然得到同样的向量，
// 不受上游接口抖动的影响。
//
// # 缓存不是事实来源
//
// 删掉缓存目录只会变慢，不会出错，也不会改变任何数字。
// 任何「删了缓存结果就不同」的情况都说明缓存键设计错了。
type Cache struct {
	inner Embedder
	dir   string
	model string

	mu    sync.Mutex
	stats Stats
}

// Stats 记录缓存的命中情况，供评测报告引用。
//
// 之所以要报出来：命中率直接决定了「跑一次扫描要花多少 API 调用」。
// 命中率意外地低，通常意味着键里混进了不该有的东西（比如把参数写进了键），
// 那样缓存就形同虚设，而人只会觉得"扫描怎么这么慢"。
type Stats struct {
	Hits   int
	Misses int
}

// NewCache 创建缓存。dir 为空时返回一个**透传**的实例（不缓存）。
//
// 返回具体类型而不是 Embedder 接口：调用方常常还要读 Stats() 看命中率，
// 而接口会把那个方法藏掉，逼调用方写一次类型断言。
// 透传而不是返回 nil，是为了让"关掉缓存"不必在调用方分叉出一条分支。
func NewCache(dir, model string, inner Embedder) *Cache {
	return &Cache{inner: inner, dir: dir, model: model}
}

// enabled 判断是否真的走缓存。
func (c *Cache) enabled() bool { return c.dir != "" && c.inner != nil }

// Stats 返回当前命中统计。
func (c *Cache) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

// Embed 实现 Embedder。
//
// 逐个查缓存，只把未命中的那部分交给上游——**不是**整体查或整体放。
// 一次导入里通常是"前几片是新的、后面大部分命过"，整体查会让
// 命中率永远接近 0。
func (c *Cache) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if !c.enabled() {
		return c.inner.Embed(ctx, texts)
	}
	if len(texts) == 0 {
		return nil, ErrEmptyInput
	}

	out := make([][]float32, len(texts))

	// pending 记录没命中的下标，保持原顺序交给上游。
	var (
		pendingIdx  []int
		pendingText []string
	)
	var hits, misses int

	for i, t := range texts {
		if v, ok := c.load(t); ok {
			out[i] = v
			hits++
			continue
		}
		pendingIdx = append(pendingIdx, i)
		pendingText = append(pendingText, t)
		misses++
	}

	if len(pendingText) > 0 {
		vecs, err := c.inner.Embed(ctx, pendingText)
		if err != nil {
			// 部分成功也先把命中的部分记下来，但错误照样返回——
			// 静默吞掉错误会让评测在"少了一部分向量"的状态下继续跑，
			// 而那些查询会莫名地召不回来。
			c.addStats(hits, misses)
			return nil, err
		}
		if len(vecs) != len(pendingText) {
			c.addStats(hits, misses)
			return nil, fmt.Errorf("embed: 缓存回填时上游返回 %d 个向量，期望 %d 个",
				len(vecs), len(pendingText))
		}
		for k, idx := range pendingIdx {
			out[idx] = vecs[k]
			c.save(pendingText[k], vecs[k])
		}
	}

	c.addStats(hits, misses)
	return out, nil
}

func (c *Cache) addStats(hits, misses int) {
	c.mu.Lock()
	c.stats.Hits += hits
	c.stats.Misses += misses
	c.mu.Unlock()
}

// key 返回一段文本的缓存键。
//
// 键里必须带上**模型名与维度**：换模型是这套系统里最贵的一次改动，
// 如果键里不含模型名，换完之后会静默读到旧模型的向量——
// 维度一样、格式一样、跑得通，只是语义空间完全不同。
// 那种错误的症状是"检索质量莫名下降"，几乎不可能靠直觉定位到缓存。
func (c *Cache) key(text string) string {
	h := sha256.New()
	h.Write([]byte(c.model))
	h.Write([]byte{0})
	fmt.Fprintf(h, "%d", types.EmbeddingDim)
	h.Write([]byte{0})
	h.Write([]byte(text))
	return hex.EncodeToString(h.Sum(nil))
}

// path 把键摊成两级目录。
//
// 单目录放几十万个文件在 Windows 上会明显变慢，两级各取 2 个字符
// 就能把单目录文件数压到千级。
func (c *Cache) path(key string) string {
	return filepath.Join(c.dir, key[:2], key[2:4], key+".vec")
}

func (c *Cache) load(text string) ([]float32, bool) {
	raw, err := os.ReadFile(c.path(c.key(text)))
	if err != nil {
		return nil, false
	}
	// 每维 4 字节（float32）。长度对不上就当作没命中——
	// 可能是写入中途被打断，重算一次即可，不必报错。
	if len(raw) != types.EmbeddingDim*4 {
		return nil, false
	}
	v := make([]float32, types.EmbeddingDim)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	return v, true
}

func (c *Cache) save(text string, v []float32) {
	if len(v) != types.EmbeddingDim {
		// 维度不对就不缓存：写进去下次读出来也是错的，
		// 而且会把一次性问题变成永久性问题。
		return
	}
	raw := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(raw[i*4:], math.Float32bits(f))
	}

	p := c.path(c.key(text))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return // 缓存写不了只是变慢，不该让评测失败
	}

	// 先写临时文件再改名。
	//
	// ⚠️ 直接写目标文件的话，进程被杀（或两次评测并发跑）会留下
	// **半截文件**；而下次读到长度不对只当作"没命中"，重新算一遍——
	// 看起来没事。但如果半截文件的长度碰巧是 4096 字节呢？那就是
	// 一个内容被截断却长度合法的向量，静默污染结果。
	// 改名在同一个目录内是原子的，从根上避免这种状态。
	// 临时文件名带随机后缀，避免两次并发评测互相踩。
	tmp := fmt.Sprintf("%s.%d.tmp", p, os.Getpid())
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
	}
}
