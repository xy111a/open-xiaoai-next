package config

// P1-45 / P1-22 / P1-23 回归测试：DND 格式、唤醒词路由指向、MPD 校验、
// web_listen 全接口拒绝、music_folder 启动检查、Save 唯一临时文件、
// JSON snake_case 与 YAML 一致。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validP1Config() *Config {
	c := &Config{
		WakeWord: "你好小李", MusicFolder: "/volume1/music",
		Devices: map[string]Device{"d1": {Model: "lx06", IP: "192.168.1.2"}},
		Agents:  map[string]Agent{"hermes": {BaseURL: "http://hermes:8642/v1", Model: "deepseek-chat"}},
	}
	c.ApplyDefaults()
	c.Hub.DoNotDisturb = "23:00-07:00"
	return c
}

func TestValidateDND(t *testing.T) {
	c := validP1Config()
	for _, bad := range []string{"23:00", "25:00-07:00", "23:60-07:00", "23:00-7:00", "23:00/07:00", "23:00-07:00-08:00"} {
		c.Hub.DoNotDisturb = bad
		if err := c.Validate(); err == nil {
			t.Fatalf("DND %q 应校验失败", bad)
		}
	}
	for _, good := range []string{"23:00-07:00", "00:00-00:00", "22:30-06:15"} {
		c.Hub.DoNotDisturb = good
		if err := c.Validate(); err != nil {
			t.Fatalf("DND %q 应通过，实际 %v", good, err)
		}
	}
	c.Hub.DoNotDisturb = ""
	if err := c.Validate(); err != nil {
		t.Fatalf("DND 为空应通过，实际 %v", err)
	}
}

func TestValidateWakeWordRoutes(t *testing.T) {
	c := validP1Config()
	c.WakeWordRoutes = map[string]string{"你好小李": "hermes"}
	if err := c.Validate(); err != nil {
		t.Fatalf("指向已配 agent 的路由应通过，实际 %v", err)
	}
	c.WakeWordRoutes = map[string]string{"你好小李": "ghost"}
	if err := c.Validate(); err == nil {
		t.Fatal("指向不存在 agent 的路由应校验失败")
	}
}

func TestValidateMPD(t *testing.T) {
	c := validP1Config()
	c.Hub.MPD = "localhost:6600"
	if err := c.Validate(); err != nil {
		t.Fatalf("MPD localhost:6600 应通过，实际 %v", err)
	}
	for _, bad := range []string{"localhost", "8.8.8.8:6600", "example.com:6600"} {
		c.Hub.MPD = bad
		if err := c.Validate(); err == nil {
			t.Fatalf("MPD %q 应校验失败", bad)
		}
	}
}

func TestValidateWebListen(t *testing.T) {
	c := validP1Config()
	for _, bad := range []string{":8080", "0.0.0.0:8080", "8.8.8.8:8080"} {
		c.Hub.WebListen = bad
		if err := c.Validate(); err == nil {
			t.Fatalf("web_listen %q 应校验失败（N-P2-6）", bad)
		}
	}
	for _, good := range []string{"127.0.0.1:8080", "192.168.1.5:8080"} {
		c.Hub.WebListen = good
		if err := c.Validate(); err != nil {
			t.Fatalf("web_listen %q 应通过，实际 %v", good, err)
		}
	}
}

func TestValidateEmptyDeviceIPAllowed(t *testing.T) {
	// P1-45：种子设备空 IP 不报错（出厂 config.yaml 本身含空 IP 占位）。
	c := validP1Config()
	c.Devices = map[string]Device{
		"livingroom-speaker": {Model: "lx06", IP: ""},
		"bedroom-speaker":    {Model: "oh2p", IP: ""},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("空 IP 种子设备应通过校验，实际 %v", err)
	}
}

func TestValidateStartupMusicFolder(t *testing.T) {
	dir := t.TempDir()
	c := validP1Config()
	c.MusicFolder = filepath.Join(dir, "music")
	if err := c.ValidateStartup(); err == nil {
		t.Fatal("不存在的 music_folder 启动检查应失败")
	}
	if err := os.Mkdir(c.MusicFolder, 0755); err != nil {
		t.Fatal(err)
	}
	if err := c.ValidateStartup(); err != nil {
		t.Fatalf("存在的 music_folder 启动检查应通过，实际 %v", err)
	}
	// 是文件不是目录：失败。
	f := filepath.Join(dir, "notdir")
	if err := os.WriteFile(f, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	c.MusicFolder = f
	if err := c.ValidateStartup(); err == nil {
		t.Fatal("music_folder 是文件时启动检查应失败")
	}
}

func TestJSONTagsMatchYAML(t *testing.T) {
	// P1-22：JSON 字段名须与 YAML snake_case 一致（Web PUT/GET 用）。
	c := validP1Config()
	c.WakeWordRoutes = map[string]string{"你好小李": "hermes"}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"wake_word", "wake_word_routes", "music_folder", "devices", "hub"} {
		if _, ok := m[key]; !ok {
			t.Fatalf("JSON 应含 snake_case 字段 %q，实际 keys: %v", key, m)
		}
	}
	if _, ok := m["WakeWord"]; ok {
		t.Fatal("JSON 不应再含 PascalCase 字段 WakeWord")
	}
	hub, _ := m["hub"].(map[string]any)
	for _, key := range []string{"web_listen", "do_not_disturb", "mpd"} {
		if _, ok := hub[key]; !ok {
			t.Fatalf("hub JSON 应含 %q，实际 %v", key, hub)
		}
	}
	// snake_case JSON 能反序列化回 Config（PUT 合并路径用）。
	var back Config
	if err := json.Unmarshal([]byte(`{"wake_word":"你好小李","hub":{"web_listen":"127.0.0.1:8080"}}`), &back); err != nil {
		t.Fatal(err)
	}
	if back.WakeWord != "你好小李" || back.Hub.WebListen != "127.0.0.1:8080" {
		t.Fatalf("snake_case JSON 反序列化失败: %+v", back)
	}
}

func TestSaveUsesUniqueTempFile(t *testing.T) {
	// P1-23：Save 不再用固定 path+".tmp"（并发写会互相覆盖）。
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	c := validP1Config()
	// 连续保存两次，中间不应留下 .tmp 残留。
	for i := 0; i < 2; i++ {
		if err := c.Save(path); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("Save 后不应残留 .tmp 文件，实际 %v", entries)
		}
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.WakeWord != "你好小李" || loaded.Hub.DoNotDisturb != "23:00-07:00" {
		t.Fatalf("Save 后重读内容不对: %+v", loaded)
	}
}
