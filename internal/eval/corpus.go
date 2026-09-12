package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/XiaoleC05/ContextDock/internal/chunk"
)

var (
	// ErrCorpusDrift 表示语料文件的内容与清单里的指纹对不上。
	ErrCorpusDrift = errors.New("eval: 语料内容与清单指纹不符")

	// ErrCorpusSourceUnknown 表示评测集引用了一个清单里没有的 source。
	ErrCorpusSourceUnknown = errors.New("eval: 评测集引用了语料清单里没有的 source")
)

// CorpusFile 是语料清单里的一份文档。
type CorpusFile struct {
	// Source 是评测集里引用它时用的标识（如 "readme.md"）。
	// 与 Path 分开：标识是给评测集看的，路径是给加载器看的，
	// 换存放位置时评测集不用动。
	Source string `json:"source"`

	// Path 是相对仓库根的文件路径。
	Path string `json:"path"`

	// SHA256 是文件内容的十六进制指纹，小写。
	//
	// 为什么必须有指纹：**语料漂移会让历史数据不可比**。
	// 半年后重跑同样的评测集却得到不同结果，届时根本分不清是代码变了
	// 还是语料变了——而这两种情况的处理方式完全相反。
	// 有了指纹，至少能把「语料变了」这个变量先钉死。
	SHA256 string `json:"sha256"`

	// Origin 记录这份文档从哪来、是什么版本，供人工核对。
	Origin string `json:"origin"`

	// Why 记录**为什么选它**：覆盖什么规模、什么语言、什么结构。
	//
	// 这不是装饰。语料选取是评测设计的一部分，不写下来，
	// 后人只会看到一堆随机文件名，既不敢改也不知道缺什么。
	Why string `json:"why"`

	// ChunksAtDefault 是这份文档在**切分器默认参数**（400 字 / 60 字重叠）下
	// 切出的片段数。
	//
	// 为什么是「记录下来」而不是「每次现算」：它是语料规模的一个快照，
	// 用来回答「这批语料大概多少个片段」这类问题，不必为此起一次完整检索。
	//
	// ⚠️ 它**跟着切分参数走**。所以此值只由 Stamp 写入，与指纹同批更新——
	// 手改必然过期，而一个悄悄过期的数字比没有数字更糟。
	// 评测运行时看到的片段数应以评测报告为准，不是这个值。
	ChunksAtDefault int `json:"chunks_at_default"`
}

// Corpus 是语料清单。
type Corpus struct {
	// Version 必须等于 FormatVersion。
	Version int `json:"version"`

	// Files 是全部语料，加载时必须非空。
	Files []CorpusFile `json:"files"`
}

// LoadCorpus 读取并校验语料清单本身（不读语料内容）。
func LoadCorpus(path string) (*Corpus, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("eval: 读取语料清单 %s 失败: %w", path, err)
	}
	var c Corpus
	if err := unmarshalStrict(raw, path, &c); err != nil {
		return nil, err
	}
	if c.Version != FormatVersion {
		return nil, fmt.Errorf("eval: %s 的 version=%d，本程序只认 %d",
			path, c.Version, FormatVersion)
	}
	if len(c.Files) == 0 {
		return nil, fmt.Errorf("eval: %s 里没有任何语料", path)
	}

	seen := make(map[string]string, len(c.Files))
	for i, f := range c.Files {
		where := fmt.Sprintf("%s 的第 %d 个语料", path, i+1)
		if strings.TrimSpace(f.Source) == "" {
			return nil, fmt.Errorf("eval: %s 缺少 source", where)
		}
		if strings.TrimSpace(f.Path) == "" {
			return nil, fmt.Errorf("eval: %s（source=%s）缺少 path", where, f.Source)
		}
		if len(f.SHA256) != 64 {
			return nil, fmt.Errorf("eval: %s（source=%s）的 sha256 不是 64 位十六进制",
				where, f.Source)
		}
		if prev, dup := seen[f.Source]; dup {
			return nil, fmt.Errorf("eval: source %q 重复出现（也在 %s 里）", f.Source, prev)
		}
		seen[f.Source] = where
	}
	return &c, nil
}

// Contents 读取全部语料内容并**逐个校验指纹**，返回 source → 内容 的映射。
//
// 指纹不符时**报错退出，不静默重跑**。
//
// 这是 #32 的核心要求，也解释了为什么不做「自动更新指纹」：
// 自动更新等于把这道防线取消掉——语料变了评测器默默认了，跑出来的数字
// 与历史数字不可比，而没有任何人知道。要更新的必须是人，且是一次显式动作。
//
// 路径以 root 为基准解析，让本函数不依赖进程的工作目录。
//
// ⚠️ **导入语料时必须用这里返回的字符串，不要再从磁盘读一次文件。**
//
// 返回值已经做过换行归一化，而引文定位出的 rune 区间是按**这份文本**算的。
// 如果导入端自己去读原始文件（Windows 上是 CRLF），rune 偏移就会与标注
// 差出一堆 \r 的量，命中判定随之整体错位——但代码不报错，只是命中率
// 莫名其妙地低，排查时几乎必然会先去怀疑检索算法。
//
// 一句话：**内容、指纹、偏移必须出自同一个字符串**，这是本函数的契约。
func (c *Corpus) Contents(root string) (map[string]string, error) {
	out := make(map[string]string, len(c.Files))

	for _, f := range c.Files {
		abs := filepath.Join(root, filepath.FromSlash(f.Path))
		raw, err := os.ReadFile(abs)
		if err != nil {
			return nil, fmt.Errorf("eval: 读取语料 %s 失败: %w", f.Path, err)
		}

		// 统一换行再算指纹：Windows 上 checkout 出来的文件是 CRLF，
		// 而 Linux/CI 上是 LF。不归一化的话，同一份语料在两个平台上
		// 指纹不同，CI 必然报错——而报的是「语料漂移」，与真实原因
		// （换行符）完全无关，排查会从语料查起，方向全错。
		//
		// 归一化之后，评测在 Windows 和 CI 上跑的是**同一个字节序列**，
		// 这也顺带保证了跨机器可复现（#58）。
		text := normalizeNewlines(string(raw))

		sum := sha256.Sum256([]byte(text))
		got := hex.EncodeToString(sum[:])
		if !strings.EqualFold(got, f.SHA256) {
			return nil, fmt.Errorf(
				"%w: %s\n  期望 %s\n  实际 %s\n"+
					"  如果是有意改的语料，请重打指纹；评测数字会与历史不可比，需重跑基线",
				ErrCorpusDrift, f.Path, f.SHA256, got)
		}
		out[f.Source] = text
	}
	return out, nil
}

// Fingerprint 计算一段文本的指纹，口径与 Contents 一致。
//
// 单独导出是为了让「重打指纹」的工具能算出与校验端**完全一样**的值。
// 两处各写一遍归一化逻辑，迟早会漂移，而漂移的表现是校验永远失败。
func Fingerprint(text string) string {
	sum := sha256.Sum256([]byte(normalizeNewlines(text)))
	return hex.EncodeToString(sum[:])
}

// StampChange 记录一次重打指纹里单个文件的变化。
type StampChange struct {
	Path      string
	OldHash   string
	NewHash   string
	OldChunks int
	NewChunks int
}

// HashChanged 表示这份语料的内容指纹变了。
//
// 单独暴露出来而不是让调用方自己比较：比较要记得大小写不敏感
// （指纹是小写，但历史值可能是从别处抄来的大写），漏了会很隐蔽。
func (c StampChange) HashChanged() bool {
	return !strings.EqualFold(c.OldHash, c.NewHash)
}

// Info 返回一行人可读的变化摘要。
func (c StampChange) Info() string {
	switch {
	case c.HashChanged():
		return fmt.Sprintf("%s\n    sha256 %s\n       →   %s\n    片段数 %d → %d",
			c.Path, c.OldHash, c.NewHash, c.OldChunks, c.NewChunks)
	case c.OldChunks == 0:
		// 首次记录片段数：内容是没动的，别把它说成「内容变了」。
		return fmt.Sprintf("%s\n    内容未变，补记片段数 %d", c.Path, c.NewChunks)
	default:
		// 内容没变而片段数变了 → 动过的是切分逻辑或参数，不是语料。
		// 这两种情况的后果完全不同（一个要查语料、一个要查代码），
		// 所以提示要把话说满，别让人去翻语料。
		return fmt.Sprintf("%s\n    内容未变，但片段数 %d → %d（改过切分逻辑或参数？）",
			c.Path, c.OldChunks, c.NewChunks)
	}
}

// chunkCount 用给定切分器数一遍片段数。
//
// 空文档返回 0 而不是错误：语料里出现一个空文件是配置问题，
// 但那是校验该管的事，不该让重打指纹这个动作整个失败。
func chunkCount(c *chunk.Chunker, text string) (int, error) {
	chunks, err := c.Split(1, text)
	if err != nil {
		if errors.Is(err, chunk.ErrEmptyDocument) {
			return 0, nil
		}
		return 0, err
	}
	return len(chunks), nil
}

// Stamp 重新计算语料清单里每个文件的 sha256 并写回清单，返回发生变化的文件。
//
// ⚠️ **这是破坏性操作，绝不能在校验路径上被自动调用。**
//
// 自动更新指纹等于把「语料漂移」这道防线整个取消掉：语料变了评测器默默认了，
// 跑出来的数字与历史不可比，而没有任何人知道。要更新指纹的必须是人，
// 且是一次显式动作——所以它只能由 -stamp 这个显式开关触发。
//
// 复用同一个 Fingerprint 而不是让调用方自己算，是为了保证
// 「重打」和「校验」两端口径完全一致。两处各写一遍的后果是：
// 重打完指纹，校验照样失败，而两边代码看起来都对。
func Stamp(root string) ([]StampChange, error) {
	manifestPath := filepath.Join(root, filepath.FromSlash(Dir), CorpusManifest)

	corpus, err := LoadCorpus(manifestPath)
	if err != nil {
		return nil, err
	}

	// 片段数也要一起重算：它和指纹一样是**内容的派生事实**，
	// 分开维护就一定会有一边过期。
	chunker, err := chunk.New(chunk.DefaultConfig())
	if err != nil {
		return nil, fmt.Errorf("eval: 创建切分器失败: %w", err)
	}

	var changes []StampChange
	for i := range corpus.Files {
		f := &corpus.Files[i]
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.Path)))
		if err != nil {
			return nil, fmt.Errorf("eval: 读取语料 %s 失败: %w", f.Path, err)
		}
		// 用归一化后的文本，与 Contents 读到的、以及引文偏移所依据的
		// 是同一个字符串。
		text := normalizeNewlines(string(raw))

		chunks, err := chunkCount(chunker, text)
		if err != nil {
			return nil, fmt.Errorf("eval: 试切分 %s 失败: %w", f.Path, err)
		}

		got := Fingerprint(text)
		if strings.EqualFold(got, f.SHA256) && chunks == f.ChunksAtDefault {
			continue
		}
		changes = append(changes, StampChange{
			Path: f.Path, OldHash: f.SHA256, NewHash: got,
			OldChunks: f.ChunksAtDefault, NewChunks: chunks,
		})
		f.SHA256 = got
		f.ChunksAtDefault = chunks
	}

	// 没有任何变化就不动文件：时间戳变化会让人以为改过什么。
	if len(changes) == 0 {
		return nil, nil
	}

	out, err := json.MarshalIndent(corpus, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("eval: 序列化语料清单失败: %w", err)
	}
	if err := os.WriteFile(manifestPath, append(out, '\n'), 0o644); err != nil {
		return nil, fmt.Errorf("eval: 写入语料清单失败: %w", err)
	}
	return changes, nil
}

// normalizeNewlines 把 CRLF / CR 统一成 LF。
func normalizeNewlines(s string) string {
	if !strings.ContainsRune(s, '\r') {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}
