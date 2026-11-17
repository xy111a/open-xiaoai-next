package config

// P1-26 回归测试：MPD / Snapcast 分组/延迟配置项存在且有默认值。

import "testing"

func TestMPDAndSnapcastConfig(t *testing.T) {
	cfg, err := Load("../../config.yaml")
	if err != nil {
		t.Fatalf("Load 真实 config.yaml 失败: %v", err)
	}
	if cfg.Hub.MPD != "localhost:6600" {
		t.Fatalf("hub.mpd 应为 localhost:6600，实际 %q", cfg.Hub.MPD)
	}
	if cfg.Hub.SnapcastStream != "NAS音乐" {
		t.Fatalf("hub.snapcast_stream 应为 NAS音乐，实际 %q", cfg.Hub.SnapcastStream)
	}
	if got, ok := cfg.Hub.SnapcastLatencies["卧室"]; !ok || got != 15 {
		t.Fatalf("hub.snapcast_latencies[卧室] 应为 15，实际 %v（存在=%v）", got, ok)
	}
}

func TestMPDAndSnapcastDefaults(t *testing.T) {
	c := &Config{}
	c.ApplyDefaults()
	if c.Hub.MPD != "localhost:6600" {
		t.Fatalf("MPD 默认应为 localhost:6600，实际 %q", c.Hub.MPD)
	}
	if c.Hub.SnapcastStream != "NAS音乐" {
		t.Fatalf("SnapcastStream 默认应为 NAS音乐，实际 %q", c.Hub.SnapcastStream)
	}
	if c.Hub.SnapcastGroup != "" {
		t.Fatalf("SnapcastGroup 默认应为空（不强制切流），实际 %q", c.Hub.SnapcastGroup)
	}
}
