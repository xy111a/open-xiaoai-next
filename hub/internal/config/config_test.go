package config

import (
	"os"
	"path/filepath"
	"testing"
)

// 真实项目配置必须能加载且通过校验（不走 mock，读 ../../config.yaml）。
func TestLoadRealConfig(t *testing.T) {
	cfg, err := Load("../../config.yaml")
	if err != nil {
		t.Fatalf("Load 真实 config.yaml 失败: %v", err)
	}
	if cfg.WakeWord == "" {
		t.Fatal("wake_word 为空")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("真实 config.yaml 校验失败: %v", err)
	}
	if !cfg.TLSEnabled() {
		t.Fatal("TLS 应默认开启（config.yaml 未显式关）")
	}
	if cfg.Hub.Listen != ":4399" {
		t.Fatalf("Listen 应为 :4399，实际 %q", cfg.Hub.Listen)
	}
	if cfg.Hub.WebListen != "192.168.88.247:8081" {
		t.Fatalf("WebListen 应为 192.168.88.247:8081（2026-11-19 bai 定：局域网直开），实际 %q", cfg.Hub.WebListen)
	}
}

func TestDefaults(t *testing.T) {
	c := &Config{}
	c.ApplyDefaults()
	if c.Hub.Listen != ":4399" || c.Hub.DataDir != "./hub-data" || c.Hub.STT != "localhost:10300" ||
		c.Hub.TTS != "localhost:10200" || c.Hub.TTSVoice != "zh_CN-huayan-medium" ||
		c.Hub.Snapcast != "localhost:1705" || c.Hub.WebListen != "127.0.0.1:8080" {
		t.Fatalf("默认值不对: %+v", c.Hub)
	}
	if !c.TLSEnabled() {
		t.Fatal("TLS 没写时应默认 true")
	}
	// 显式写 false 才能关。
	f := false
	c.Hub.TLS = &f
	if c.TLSEnabled() {
		t.Fatal("TLS 显式 false 时 TLSEnabled 应为 false")
	}
	// HA 融合 b：media_player 上报没写时默认开，显式 false 才关。
	if !c.HomeAssistant.MediaPlayerEnabledOrDefault() {
		t.Fatal("media_player_enabled 没写时应默认 true")
	}
	c.HomeAssistant.MediaPlayerEnabled = &f
	if c.HomeAssistant.MediaPlayerEnabledOrDefault() {
		t.Fatal("media_player_enabled 显式 false 时应为 false")
	}
}

func TestValidateRejects(t *testing.T) {
	c := &Config{WakeWord: "你好小李", MusicFolder: "/volume1/music",
		Devices: map[string]Device{"d1": {Model: "lx07", IP: "192.168.1.2"}}}
	if err := c.Validate(); err == nil {
		t.Fatal("未知型号 lx07 应校验失败")
	}
	c.Devices = map[string]Device{"d1": {Model: "lx06", IP: "192.168.1.2"}}
	c.MusicFolder = ""
	if err := c.Validate(); err == nil {
		t.Fatal("music_folder 为空应校验失败")
	}
}

// P0-1：下游地址只许 http(s)/ws(s) + 内网 host，拒绝公网与 file://。
func TestValidateDownstreamURLs(t *testing.T) {
	base := func() *Config {
		c := &Config{WakeWord: "你好小李", MusicFolder: "/volume1/music",
			Devices: map[string]Device{"d1": {Model: "lx06", IP: "192.168.1.2"}}}
		c.ApplyDefaults()
		return c
	}
	// 公网 agent：https 允许（云端 LLM 的 API 本来就在公网，见
	// validateAgentURL），http 明文拒绝（密钥会裸奔）。
	c := base()
	c.Agents = map[string]Agent{"deepseek": {BaseURL: "https://api.deepseek.com/v1", Model: "deepseek-chat"}}
	if err := c.Validate(); err != nil {
		t.Fatalf("https 公网 agent 应校验通过: %v", err)
	}
	c = base()
	c.Agents = map[string]Agent{"hermes": {BaseURL: "http://203.0.113.1:8642/v1", Model: "deepseek-chat"}}
	if err := c.Validate(); err == nil {
		t.Fatal("http 公网 agent base_url 应校验失败")
	}
	// file:// 网易云 → 拒绝。
	c = base()
	c.Netease.API = "file:///etc/passwd"
	if err := c.Validate(); err == nil {
		t.Fatal("file:// 下游地址应校验失败")
	}
	// 公网 HA → 拒绝。
	c = base()
	c.HomeAssistant.URL = "http://203.0.113.1:8123"
	if err := c.Validate(); err == nil {
		t.Fatal("公网 home_assistant.url 应校验失败")
	}
	// 公网 scan_cidr → 拒绝。
	c = base()
	c.Hub.ScanCIDR = "8.8.8.0/24"
	if err := c.Validate(); err == nil {
		t.Fatal("公网 scan_cidr 应校验失败")
	}
	// 公网设备 IP → 拒绝。
	c = base()
	c.Devices = map[string]Device{"d1": {Model: "lx06", IP: "203.0.113.5"}}
	if err := c.Validate(); err == nil {
		t.Fatal("公网 devices IP 应校验失败")
	}
	// 内网组合（真实 config 风格：私有 IP + Docker 单标签主机名）→ 通过。
	c = base()
	c.HomeAssistant.URL = "ws://192.168.88.247:8123"
	c.Agents = map[string]Agent{"hermes": {BaseURL: "http://hermes:8642/v1", Model: "deepseek-chat"}}
	c.Netease.API = "http://netease-api:3000"
	c.Hub.ScanCIDR = "192.168.1.0/24"
	if err := c.Validate(); err != nil {
		t.Fatalf("内网下游组合应校验通过: %v", err)
	}
}

func TestSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	c := &Config{WakeWord: "你好小李", MusicFolder: "/volume1/music",
		Devices:    map[string]Device{"lx06-1": {Model: "lx06", IP: "192.168.1.2", Room: "客厅"}},
		HAEntities: map[string]string{"客厅灯": "light.living_room"}}
	c.ApplyDefaults()
	if err := c.Save(path); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("Save 后不应残留 .tmp 文件")
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("重读失败: %v", err)
	}
	if got.Devices["lx06-1"].Room != "客厅" || got.HAEntities["客厅灯"] != "light.living_room" {
		t.Fatalf("Save/Load 往返丢数据: %+v", got)
	}
}

// TestSaveFallbackDirectWrite：P0-2 的 rename 回退路径——直接写原文件
// 应写出内容、权限 0600，二次覆盖也应成功。
// 注：EBUSY 分支本身（rename 到 bind-mount 挂载点被内核拒绝）已在
// unshare -rm 命名空间里真复现验证过：把目标文件 bind-mount 到自身后
// Save 成功走回退写出，内容与 0600 权限都对，日志有“改走直接写原文件”
// 一行；本测试覆盖回退的写路径本身。
func TestSaveFallbackDirectWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := writeFileSync(path, []byte("wake_word: 你好小李\n"), 0600); err != nil {
		t.Fatalf("writeFileSync 失败: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "wake_word: 你好小李\n" {
		t.Fatalf("回退写出内容不对: %q", b)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0600 {
		t.Fatalf("回退写出权限应为 0600，实际 %o", st.Mode().Perm())
	}
	// 二次写覆盖同一文件也应成功（模拟 rename 失败后反复保存）。
	if err := writeFileSync(path, []byte("wake_word: 你好小明\n"), 0600); err != nil {
		t.Fatalf("二次 writeFileSync 失败: %v", err)
	}
	b, _ = os.ReadFile(path)
	if string(b) != "wake_word: 你好小明\n" {
		t.Fatalf("二次写出内容不对: %q", b)
	}
}

func TestTokenFromEnv(t *testing.T) {
	t.Setenv("OPEN_XIAOAI_TOKEN", "tok-123")
	if got := Token(); got != "tok-123" {
		t.Fatalf("Token 应读环境变量，实际 %q", got)
	}
}
