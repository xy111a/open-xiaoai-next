package config

import (
	"os"
	"path/filepath"
	"testing"
)

// KWS 配置：随仓库发的 config.yaml 应带已启用的 kws 一节（默认边车地址 + “你好小李”）。
func TestShippedConfigKWS(t *testing.T) {
	cfg, err := Load("../../config.yaml")
	if err != nil {
		t.Fatalf("加载随仓库发的 config.yaml 失败: %v", err)
	}
	if !cfg.KWS.Enabled {
		t.Fatal("随仓库发的 config.yaml 里 kws.enabled 应为 true")
	}
	if cfg.KWS.URL != "ws://127.0.0.1:10400/v1/kws" {
		t.Fatalf("kws.url 应为默认边车地址，实际 %q", cfg.KWS.URL)
	}
	found := false
	for _, kw := range cfg.KWS.Keywords {
		if kw == "你好小李" {
			found = true
		}
	}
	if !found {
		t.Fatalf("kws.keywords 应含“你好小李”，实际 %v", cfg.KWS.Keywords)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("真实 config.yaml 校验失败: %v", err)
	}
}

func kwsBase(t *testing.T) *Config {
	t.Helper()
	c := &Config{WakeWord: "你好小李", MusicFolder: "/volume1/music",
		Devices: map[string]Device{"d1": {Model: "lx06", IP: "192.168.1.2"}}}
	c.ApplyDefaults()
	return c
}

// 启用时缺 url / 错 scheme / 公网 host / 无路径 / 空 keywords 都应启动即报错，
// 而不是静默用错地址。
func TestValidateKWSEnabled(t *testing.T) {
	c := kwsBase(t)
	c.KWS.Enabled = true
	c.KWS.URL = ""
	c.KWS.Keywords = []string{"你好小李"}
	if err := c.Validate(); err == nil {
		t.Fatal("kws.enabled=true 但 url 为空时应校验失败")
	}
	c.KWS.URL = "http://127.0.0.1:10400/v1/kws"
	if err := c.Validate(); err == nil {
		t.Fatal("kws.url 用 http scheme 时应校验失败")
	}
	c.KWS.URL = "ws://203.0.113.1:10400/v1/kws"
	if err := c.Validate(); err == nil {
		t.Fatal("kws.url 用公网 host 时应校验失败")
	}
	c.KWS.URL = "ws://example.com/v1/kws"
	if err := c.Validate(); err == nil {
		t.Fatal("kws.url 用公网域名时应校验失败")
	}
	c.KWS.URL = "ws://127.0.0.1:10400"
	if err := c.Validate(); err == nil {
		t.Fatal("kws.url 无路径时应校验失败")
	}
	c.KWS.URL = "ws://user:pass@127.0.0.1:10400/v1/kws"
	if err := c.Validate(); err == nil {
		t.Fatal("kws.url 带 userinfo 时应校验失败")
	}
	c.KWS.URL = "ws://127.0.0.1:10400/v1/kws"
	c.KWS.Keywords = nil
	if err := c.Validate(); err == nil {
		t.Fatal("kws.enabled=true 但 keywords 为空时应校验失败")
	}
	c.KWS.Keywords = []string{"你好小李", " "}
	if err := c.Validate(); err == nil {
		t.Fatal("kws.keywords 里有空唤醒词时应校验失败")
	}
	// 内网组合应通过：回环 / 私有 IP / Docker 单标签服务名。
	for _, u := range []string{
		"ws://127.0.0.1:10400/v1/kws",
		"ws://192.168.1.5:10400/v1/kws",
		"ws://kws-sidecar:10400/v1/kws",
		"wss://kws-sidecar:10400/v1/kws",
	} {
		c.KWS.URL = u
		c.KWS.Keywords = []string{"你好小李"}
		if err := c.Validate(); err != nil {
			t.Fatalf("kws.url=%q 应校验通过，实际报错: %v", u, err)
		}
	}
}

// 未启用时 url/keywords 不校验（忽略），Hub 以 disabled 状态启动。
func TestValidateKWSDisabledIgnoresURL(t *testing.T) {
	c := kwsBase(t)
	c.KWS.Enabled = false
	c.KWS.URL = "http://203.0.113.1:10400/v1/kws"
	c.KWS.Keywords = nil
	if err := c.Validate(); err != nil {
		t.Fatalf("kws 未启用时 url 不应校验，实际报错: %v", err)
	}
}

// kws 一节 Save/Load 往返不丢数据。
func TestKWSSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	c := kwsBase(t)
	c.KWS = KWS{Enabled: true, URL: "ws://kws-sidecar:10400/v1/kws", Keywords: []string{"你好小李", "你好龙虾"}}
	if err := c.Save(path); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("重读失败: %v", err)
	}
	if !got.KWS.Enabled || got.KWS.URL != "ws://kws-sidecar:10400/v1/kws" || len(got.KWS.Keywords) != 2 {
		t.Fatalf("kws 一节 Save/Load 往返丢数据: %+v", got.KWS)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("Save 后不应残留 .tmp 文件")
	}
}
