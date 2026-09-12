package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/XiaoleC05/ContextDock/internal/types"
)

// env 把 map 包成 Getenv。
func env(m map[string]string) Getenv {
	return func(key string) (string, bool) {
		v, ok := m[key]
		return v, ok
	}
}

// minimal 是能通过校验的最小配置。
func minimal(extra map[string]string) map[string]string {
	m := map[string]string{
		EnvSiliconFlowAPIKey: "sk-test",
		EnvUseMemoryStore:    "true",
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestLoadWithDefaults(t *testing.T) {
	cfg, err := LoadWith(env(minimal(nil)))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}

	if cfg.SiliconFlowAPIKey != "sk-test" {
		t.Errorf("API Key 不对: %q", cfg.SiliconFlowAPIKey)
	}
	if cfg.SiliconFlowBaseURL != "https://api.siliconflow.cn/v1" {
		t.Errorf("base URL 默认值不对: %q", cfg.SiliconFlowBaseURL)
	}
	if cfg.EmbeddingModel != "Pro/BAAI/bge-m3" {
		t.Errorf("模型默认值不对: %q", cfg.EmbeddingModel)
	}
	if cfg.TopK != DefaultTopK {
		t.Errorf("TopK 默认值应为 %d，实际 %d", DefaultTopK, cfg.TopK)
	}
	if cfg.ChunkMaxRunes != DefaultChunkMaxRunes || cfg.ChunkOverlap != DefaultChunkOverlap {
		t.Errorf("切分参数默认值不对: %d/%d", cfg.ChunkMaxRunes, cfg.ChunkOverlap)
	}
	if cfg.EmbeddingDim != types.EmbeddingDim {
		t.Errorf("向量维度应来自 types，实际 %d", cfg.EmbeddingDim)
	}
	// 连接池必须被收窄到 8，不能用 pgx 的 max(4, NumCPU)
	if cfg.PoolMaxConns != DefaultPoolMaxConns {
		t.Errorf("连接池上限应为 %d，实际 %d", DefaultPoolMaxConns, cfg.PoolMaxConns)
	}
}

// TestLoadRequiresAPIKey 验证缺 Key 时启动期就失败。
//
// 不在启动期拦的话，症状是第一次检索才 401/403——
// 排查时会先怀疑网络和鉴权，而不是"配置没加载到"。
func TestLoadRequiresAPIKey(t *testing.T) {
	_, err := LoadWith(env(map[string]string{EnvUseMemoryStore: "true"}))
	if !errors.Is(err, ErrMissingAPIKey) {
		t.Errorf("应返回 ErrMissingAPIKey，实际 %v", err)
	}
}

// TestLoadDistinguishesUnsetFromEmpty 验证能区分"没设置"和"设成了空字符串"。
//
// 两者都是错误，但排查方向完全不同：前者是忘了配，
// 后者多半是把变量写进了配置但值忘了填。
func TestLoadDistinguishesUnsetFromEmpty(t *testing.T) {
	_, err := LoadWith(env(map[string]string{
		EnvSiliconFlowAPIKey: "",
		EnvUseMemoryStore:    "true",
	}))
	if !errors.Is(err, ErrMissingAPIKey) {
		t.Fatalf("空 Key 也应返回 ErrMissingAPIKey，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "为空") {
		t.Errorf("错误信息应当说明是「存在但为空」，方便区分两种情况: %v", err)
	}

	// 只含空白的值也应当被拦下
	_, err = LoadWith(env(map[string]string{
		EnvSiliconFlowAPIKey: "   ",
		EnvUseMemoryStore:    "true",
	}))
	if !errors.Is(err, ErrMissingAPIKey) {
		t.Errorf("全是空白的 Key 应当报错，实际 %v", err)
	}
}

// TestFakeEmbedderWaivesAPIKey 验证开了假嵌入就不要求 API Key。
//
// 这是「clone 完不填 key 也能跑通」这条路径的全部依据：
// 缺了它，第一次跑的人会先撞上 ErrMissingAPIKey。
func TestFakeEmbedderWaivesAPIKey(t *testing.T) {
	// 注意：这里连 EnvSiliconFlowAPIKey 这个键都不存在，
	// 而不只是值为空——要和「设置了但没填」区分开。
	cfg, err := LoadWith(env(map[string]string{
		EnvFakeEmbedder:   "true",
		EnvUseMemoryStore: "true",
	}))
	if err != nil {
		t.Fatalf("开了假嵌入就不该要求 API Key，实际报错: %v", err)
	}
	if !cfg.FakeEmbedder {
		t.Error("FakeEmbedder 应为 true")
	}
	if cfg.SiliconFlowAPIKey != "" {
		t.Errorf("没配 Key 时应为空，实际 %q", cfg.SiliconFlowAPIKey)
	}
}

// TestFakeEmbedderOffStillRequiresAPIKey 守住默认路径没有被放松。
//
// ⚠️ 这条比 TestLoadRequiresAPIKey 更严格一点：它显式把开关设成 false，
// 确认「写了这个变量但设成假」不会意外绕开校验。
func TestFakeEmbedderOffStillRequiresAPIKey(t *testing.T) {
	_, err := LoadWith(env(map[string]string{
		EnvFakeEmbedder:   "false",
		EnvUseMemoryStore: "true",
	}))
	if !errors.Is(err, ErrMissingAPIKey) {
		t.Errorf("开关为 false 时仍然必须要求 API Key，实际 %v", err)
	}
}

// TestFakeEmbedderDoesNotWaiveStore 验证假嵌入只豁免 Embedding 这一项。
//
// 假嵌入不联网，但它和「数据存哪儿」毫无关系。如果这里放行了，
// 用户会拿到一个"启动了但一查就崩"的进程——比启动期报错难查得多。
func TestFakeEmbedderDoesNotWaiveStore(t *testing.T) {
	_, err := LoadWith(env(map[string]string{
		EnvFakeEmbedder:   "true",
		EnvUseMemoryStore: "false",
		// 故意不设 EnvDatabaseURL
	}))
	if !errors.Is(err, ErrMissingDatabaseURL) {
		t.Errorf("假嵌入不该豁免数据库校验，实际 %v", err)
	}
}

// TestFakeEmbedderKeepsKeyWhenAlsoSet 验证两者同时存在时 Key 被保留。
//
// 保留不是为了用，而是为了 String() 能显示「已设置，N 字符」——
// 让人一眼确认自己是"忘了删 Key"而不是"Key 被吞了"。
func TestFakeEmbedderKeepsKeyWhenAlsoSet(t *testing.T) {
	cfg, err := LoadWith(env(map[string]string{
		EnvFakeEmbedder:      "true",
		EnvSiliconFlowAPIKey: "sk-still-here",
		EnvUseMemoryStore:    "true",
	}))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.SiliconFlowAPIKey != "sk-still-here" {
		t.Errorf("Key 应被保留，实际 %q", cfg.SiliconFlowAPIKey)
	}
}

// TestFakeEmbedderShowsInString 验证日志里能看出跑的是哪条路。
//
// 假嵌入下检索质量无意义，如果 String() 不体现它，
// 一份看起来正常的启动日志会掩盖"结果为什么这么差"。
func TestFakeEmbedderShowsInString(t *testing.T) {
	cfg, err := LoadWith(env(map[string]string{
		EnvFakeEmbedder:   "true",
		EnvUseMemoryStore: "true",
	}))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if !strings.Contains(cfg.String(), "fakeEmbedder:true") {
		t.Errorf("String() 应体现假嵌入已开启，实际 %q", cfg.String())
	}
}

// TestLoadRequiresDatabaseURL 验证选了 postgres 就必须有连接串。
func TestLoadRequiresDatabaseURL(t *testing.T) {
	m := minimal(nil)
	m[EnvUseMemoryStore] = "false"

	_, err := LoadWith(env(m))
	if !errors.Is(err, ErrMissingDatabaseURL) {
		t.Errorf("选了 postgres 但没配连接串应报错，实际 %v", err)
	}

	m[EnvDatabaseURL] = "postgres://localhost/test"
	if _, err := LoadWith(env(m)); err != nil {
		t.Errorf("配了连接串应当成功，实际 %v", err)
	}
}

func TestLoadParsesOptionalValues(t *testing.T) {
	cfg, err := LoadWith(env(minimal(map[string]string{
		EnvTopK:           "20",
		EnvSearchTimeout:  "3s",
		EnvChunkMaxRunes:  "600",
		EnvChunkOverlap:   "100",
		EnvPoolMaxConns:   "16",
		EnvEmbeddingModel: "custom/model",
	})))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}

	if cfg.TopK != 20 {
		t.Errorf("TopK 应为 20，实际 %d", cfg.TopK)
	}
	if cfg.SearchTimeout != 3*time.Second {
		t.Errorf("超时应为 3s，实际 %v", cfg.SearchTimeout)
	}
	if cfg.ChunkMaxRunes != 600 || cfg.ChunkOverlap != 100 {
		t.Errorf("切分参数不对: %d/%d", cfg.ChunkMaxRunes, cfg.ChunkOverlap)
	}
	if cfg.PoolMaxConns != 16 {
		t.Errorf("连接池上限应为 16，实际 %d", cfg.PoolMaxConns)
	}
	if cfg.EmbeddingModel != "custom/model" {
		t.Errorf("模型应被覆盖，实际 %q", cfg.EmbeddingModel)
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{"TopK 不是数字", map[string]string{EnvTopK: "abc"}},
		{"超时格式错误", map[string]string{EnvSearchTimeout: "5 seconds"}},
		{"布尔值错误", map[string]string{EnvUseMemoryStore: "maybe"}},
		{"连接池不是数字", map[string]string{EnvPoolMaxConns: "many"}},
		{"TopK 为 0", map[string]string{EnvTopK: "0"}},
		{"TopK 为负", map[string]string{EnvTopK: "-1"}},
		{"overlap 不小于 chunk 大小",
			map[string]string{EnvChunkMaxRunes: "100", EnvChunkOverlap: "100"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadWith(env(minimal(tt.env)))
			if err == nil {
				t.Error("应当报错")
			}
			if !errors.Is(err, ErrBadValue) {
				t.Errorf("应返回 ErrBadValue，实际 %v", err)
			}
		})
	}
}

// TestValidateCrossField 验证跨字段校验。
//
// 单字段各自合法、组合起来没意义的情况，必须在 Validate 里拦住。
func TestValidateCrossField(t *testing.T) {
	base := Config{
		SiliconFlowAPIKey: "sk-x",
		TopK:              10,
		SearchTimeout:     time.Second,
		ChunkMaxRunes:     400,
		ChunkOverlap:      60,
		PoolMaxConns:      8,
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("基准配置应当合法: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"overlap 等于 chunk 大小", func(c *Config) { c.ChunkOverlap = c.ChunkMaxRunes }},
		{"overlap 大于 chunk 大小", func(c *Config) { c.ChunkOverlap = c.ChunkMaxRunes + 1 }},
		{"overlap 为负", func(c *Config) { c.ChunkOverlap = -1 }},
		{"超时为 0", func(c *Config) { c.SearchTimeout = 0 }},
		{"连接池为 0", func(c *Config) { c.PoolMaxConns = 0 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base
			tt.mutate(&c)
			if err := c.Validate(); !errors.Is(err, ErrBadValue) {
				t.Errorf("应返回 ErrBadValue，实际 %v", err)
			}
		})
	}
}

// TestStringMasksSecrets 守护日志不泄露密钥。
//
// 打印配置是很常见的调试手段。String() 如果原样输出，
// 一个 log.Printf("%+v", cfg) 就把密钥写进日志文件了。
func TestStringMasksSecrets(t *testing.T) {
	const secret = "sk-super-secret-key-value"
	cfg, err := LoadWith(env(minimal(map[string]string{
		EnvDatabaseURL: "postgres://user:password@host/db",
	})))
	if err != nil {
		t.Fatal(err)
	}
	cfg.SiliconFlowAPIKey = secret

	s := cfg.String()
	if strings.Contains(s, secret) {
		t.Errorf("String() 泄露了 API Key:\n%s", s)
	}
	if strings.Contains(s, "password") {
		t.Errorf("String() 泄露了连接串里的密码:\n%s", s)
	}
	// 但应当能看出"有没有配"
	if !strings.Contains(s, "已设置") {
		t.Errorf("String() 应当表明密钥已设置:\n%s", s)
	}
}

func TestReadDotEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := `# 注释行

SILICONFLOW_API_KEY=sk-from-file
QUOTED="带引号的值"
SINGLE='单引号'
export EXPORTED=导出写法
WITH_SPACES = 两侧有空格
EMPTY=
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ReadDotEnv(path)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	want := map[string]string{
		"SILICONFLOW_API_KEY": "sk-from-file",
		"QUOTED":              "带引号的值",
		"SINGLE":              "单引号",
		"EXPORTED":            "导出写法",
		"WITH_SPACES":         "两侧有空格",
		"EMPTY":               "",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: 期望 %q，实际 %q", k, v, got[k])
		}
	}
}

func TestReadDotEnvMissingFileIsNotError(t *testing.T) {
	got, err := ReadDotEnv(filepath.Join(t.TempDir(), "不存在"))
	if err != nil {
		t.Errorf("文件不存在不应报错（生产环境用进程环境变量注入），实际 %v", err)
	}
	if len(got) != 0 {
		t.Errorf("应返回空 map，实际 %v", got)
	}
}

func TestReadDotEnvRejectsMalformed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("这行没有等号\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDotEnv(path); err == nil {
		t.Error("格式错误应当报错，而不是静默忽略")
	}
}

// TestProcessEnvOverridesDotEnv 验证优先级。
//
// Agent 配置里的 env 块必须能覆盖开发时的 .env，
// 否则"临时换个 key 试试"就得去改文件。
func TestProcessEnvOverridesDotEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := EnvSiliconFlowAPIKey + "=sk-from-file\n" + EnvUseMemoryStore + "=true\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	dotenv, err := ReadDotEnv(path)
	if err != nil {
		t.Fatal(err)
	}

	// 模拟 Load 的取值逻辑
	process := map[string]string{EnvSiliconFlowAPIKey: "sk-from-process"}
	getenv := func(key string) (string, bool) {
		if v, ok := process[key]; ok {
			return v, true
		}
		v, ok := dotenv[key]
		return v, ok
	}

	cfg, err := LoadWith(getenv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SiliconFlowAPIKey != "sk-from-process" {
		t.Errorf("进程环境变量应当覆盖 .env，实际用了 %q", cfg.SiliconFlowAPIKey)
	}
}
