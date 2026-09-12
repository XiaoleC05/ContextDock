package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// normalizeNewlines 把 CRLF / CR 统一成 LF。
func normalizeNewlines(s string) string {
	if !strings.ContainsRune(s, '\r') {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}
