package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// 插件式 Agent / 音源 / 技能的配置校验与默认值。

func pluginBase() *Config {
	c := &Config{WakeWord: "你好小李", MusicFolder: "/volume1/music",
		Devices: map[string]Device{"d1": {Model: "lx06", IP: "192.168.1.2"}}}
	c.ApplyDefaults()
	return c
}

func TestAgentAPIKeyEnv(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "sk-deepseek-123")
	t.Setenv("LLM_API_KEY", "sk-global-456")
	// 配了 api_key_env → 读它。
	a := Agent{BaseURL: "https://api.deepseek.com/v1", Model: "deepseek-chat", APIKeyEnv: "DEEPSEEK_API_KEY"}
	if got := a.ResolveAPIKey(); got != "sk-deepseek-123" {
		t.Fatalf("应读 DEEPSEEK_API_KEY，实际 %q", got)
	}
	// 没配 → 读全局 LLM_API_KEY。
	b := Agent{BaseURL: "http://hermes:8642/v1", Model: "deepseek-chat"}
	if got := b.ResolveAPIKey(); got != "sk-global-456" {
		t.Fatalf("没配 api_key_env 时应读 LLM_API_KEY，实际 %q", got)
	}
	// 配了但环境里没有 → 空（不带 Authorization 发请求）。
	c := Agent{BaseURL: "https://api.deepseek.com/v1", Model: "deepseek-chat", APIKeyEnv: "DOES_NOT_EXIST_XYZ"}
	if got := c.ResolveAPIKey(); got != "" {
		t.Fatalf("变量不存在时应返回空，实际 %q", got)
	}
}

func TestAgentValidation(t *testing.T) {
	// 非法环境变量名。
	c := pluginBase()
	c.Agents = map[string]Agent{"x": {BaseURL: "http://hermes:8642/v1", Model: "m", APIKeyEnv: "BAD-NAME"}}
	if err := c.Validate(); err == nil {
		t.Fatal("api_key_env 带连字符应校验失败")
	}
	// system_prompt 超长。
	c = pluginBase()
	c.Agents = map[string]Agent{"x": {BaseURL: "http://hermes:8642/v1", Model: "m", SystemPrompt: strings.Repeat("啊", 4001)}}
	if err := c.Validate(); err == nil {
		t.Fatal("system_prompt 超 4000 字符应校验失败")
	}
	// file:// 的 agent。
	c = pluginBase()
	c.Agents = map[string]Agent{"x": {BaseURL: "file:///etc/passwd", Model: "m"}}
	if err := c.Validate(); err == nil {
		t.Fatal("file:// agent base_url 应校验失败")
	}
	// 合法组合：本地 http + 云端 https + 各自的 key 变量。
	c = pluginBase()
	c.Agents = map[string]Agent{
		"hermes":   {BaseURL: "http://hermes:8642/v1", Model: "deepseek-chat"},
		"deepseek": {BaseURL: "https://api.deepseek.com/v1", Model: "deepseek-chat", APIKeyEnv: "DEEPSEEK_API_KEY"},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("合法 Agent 组合应校验通过: %v", err)
	}
}

func TestMusicOrderDefaults(t *testing.T) {
	// 全没写 → netease → local。
	c := &Config{}
	c.ApplyDefaults()
	if c.Music.Primary != "netease" || c.Music.Fallback != "local" {
		t.Fatalf("默认音源顺序应为 netease → local，实际 %+v", c.Music)
	}
	// 只写 primary=local → fallback 保持空（不兜底，可显式关闭）。
	c = pluginBase()
	c.Music = Music{Primary: "local"}
	c.ApplyDefaults()
	if c.Music.Primary != "local" || c.Music.Fallback != "" {
		t.Fatalf("只写 primary=local 时 fallback 应保持空，实际 %+v", c.Music)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("primary=local、fallback 为空应校验通过: %v", err)
	}
	// 非法取值 / 相同音源。
	for _, m := range []Music{{Primary: "spotify"}, {Primary: "netease", Fallback: "spotify"}, {Primary: "local", Fallback: "local"}} {
		c = pluginBase()
		c.Music = m
		if err := c.Validate(); err == nil {
			t.Fatalf("音源顺序 %+v 应校验失败", m)
		}
	}
}

func TestSkillsValidation(t *testing.T) {
	good := Skill{Name: "news", Triggers: []string{"新闻"}, Endpoint: "http://127.0.0.1:18080/v1/skill/news", TimeoutSeconds: 10}
	c := pluginBase()
	c.Skills = []Skill{good}
	if err := c.Validate(); err != nil {
		t.Fatalf("合法技能应校验通过: %v", err)
	}
	cases := []struct {
		name   string
		skills []Skill
	}{
		{"空名", []Skill{{Triggers: []string{"新闻"}, Endpoint: "http://127.0.0.1:18080/x"}}},
		{"重名", []Skill{good, good}},
		{"无触发词", []Skill{{Name: "x", Endpoint: "http://127.0.0.1:18080/x"}}},
		{"空触发词", []Skill{{Name: "x", Triggers: []string{""}, Endpoint: "http://127.0.0.1:18080/x"}}},
		{"公网端点", []Skill{{Name: "x", Triggers: []string{"新闻"}, Endpoint: "https://example.com/skill"}}},
		{"file 端点", []Skill{{Name: "x", Triggers: []string{"新闻"}, Endpoint: "file:///tmp/x"}}},
		{"超时为负", []Skill{{Name: "x", Triggers: []string{"新闻"}, Endpoint: "http://127.0.0.1:18080/x", TimeoutSeconds: -1}}},
		{"超时过大", []Skill{{Name: "x", Triggers: []string{"新闻"}, Endpoint: "http://127.0.0.1:18080/x", TimeoutSeconds: 121}}},
	}
	for _, tc := range cases {
		c = pluginBase()
		c.Skills = tc.skills
		if err := c.Validate(); err == nil {
			t.Fatalf("技能校验 %s 应失败", tc.name)
		}
	}
}

// 真实 config.yaml 里的插件配置（music 顺序 + news 示例技能 +
// hermes 的 system_prompt）必须能加载且通过校验。
func TestLoadRealConfigPlugins(t *testing.T) {
	cfg, err := Load("../../config.yaml")
	if err != nil {
		t.Fatalf("Load 真实 config.yaml 失败: %v", err)
	}
	if cfg.Music.Primary != "netease" || cfg.Music.Fallback != "local" {
		t.Fatalf("真实配置的音源顺序应为 netease → local，实际 %+v", cfg.Music)
	}
	if len(cfg.Skills) != 1 || cfg.Skills[0].Name != "news" {
		t.Fatalf("真实配置应含 news 示例技能，实际 %+v", cfg.Skills)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("真实 config.yaml（含插件配置）校验失败: %v", err)
	}
	hermes, ok := cfg.Agents["hermes"]
	if !ok || hermes.SystemPrompt == "" {
		t.Fatalf("真实配置的 hermes 应配 system_prompt，实际 %+v", hermes)
	}
	// 往返保存不丢插件配置。
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("重读失败: %v", err)
	}
	if got.Music != cfg.Music || len(got.Skills) != 1 || got.Skills[0].Endpoint != cfg.Skills[0].Endpoint {
		t.Fatalf("Save/Load 往返丢插件配置: %+v %+v", got.Music, got.Skills)
	}
	if got.Agents["hermes"].SystemPrompt != hermes.SystemPrompt {
		t.Fatalf("Save/Load 往返丢 system_prompt，实际 %q", got.Agents["hermes"].SystemPrompt)
	}
}
