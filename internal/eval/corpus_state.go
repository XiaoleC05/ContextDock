package eval

import (
	"fmt"

	"github.com/XiaoleC05/ContextDock/internal/chunk"
)

// CorpusState 是语料在一组切分参数下的**确定状态**。
//
// # 为什么要有它
//
// 参数扫描、CI 回归（#57）都需要一个能断言的前提：
// "这批语料在当前参数下切成多少个片段"。没有这个断言的话，
// 评测数字变了时分不清是**检索质量变了**还是**语料/切分变了**——
// 而这两件事的处理方式完全相反。
//
// # 它为什么不需要"清空"这一步
//
// 评测器每次运行都从一个**空的**内存存储开始（见 Run 的说明），
// 灌语料是每次运行的一部分。所以不存在"残留数据"，
// 也就不存在"重置"这个动作——它被架构消掉了。
//
// 留下的是"状态可断言"这一半，由这个结构体提供。
type CorpusState struct {
	MaxRunes int `json:"max_runes"`
	Overlap  int `json:"overlap"`

	// Files 是每份语料的片段数，顺序与清单一致。
	Files []CorpusStat `json:"files"`

	// TotalChunks 是片段总数。这是最常被断言的那个数。
	TotalChunks int `json:"total_chunks"`

	// Fingerprints 是每份语料的 sha256。
	//
	// 一起报出来是刻意的：**片段数变了有两种可能**——
	// 切分参数变了，或者语料内容变了。只看片段数分不清，
	// 而指纹能直接回答"语料是不是同一批"。
	Fingerprints map[string]string `json:"fingerprints"`
}

// DescribeCorpus 描述语料在给定参数下的切分结果。
//
// ⚠️ 它**只切分、不嵌入**，所以不花任何 API 调用，也不碰存储。
// 这让它在"先确认状态再跑扫描"的场合很便宜。
//
// 代价是它不报告向量数——向量数等于 TotalChunks（每个片段一个向量），
// 要确认那件事得跑一次完整评测（报告里有）。
func DescribeCorpus(suite *Suite, opt Options) (*CorpusState, error) {
	opt = opt.Normalize()

	chunker, err := chunk.New(chunk.Config{
		MaxRunes:     opt.MaxRunes,
		OverlapRunes: opt.Overlap,
	})
	if err != nil {
		return nil, fmt.Errorf("eval: 创建切分器失败: %w", err)
	}

	st := &CorpusState{
		MaxRunes:     opt.MaxRunes,
		Overlap:      opt.Overlap,
		Fingerprints: make(map[string]string, len(suite.Corpus.Files)),
	}
	for _, f := range suite.Corpus.Files {
		// 用 suite 里的字符串，与评测时**完全同一个来源**——
		// 这里重新读一次磁盘文件的话，报出来的片段数可能和评测对不上，
		// 而这个函数存在的全部意义就是"可靠地预言评测会看到什么"。
		text, ok := suite.Content(f.Source)
		if !ok {
			return nil, fmt.Errorf("eval: 语料 %s 没有内容", f.Source)
		}
		n, err := chunkCount(chunker, text)
		if err != nil {
			return nil, fmt.Errorf("eval: 切分 %s 失败: %w", f.Source, err)
		}
		st.Files = append(st.Files, CorpusStat{Source: f.Source, Chunks: n})
		st.TotalChunks += n
		st.Fingerprints[f.Source] = f.SHA256
	}
	return st, nil
}
