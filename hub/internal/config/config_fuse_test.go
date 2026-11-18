package config

// 融合 B 回归测试：旧配置含 netease.music_assistant 时 Load 明确报错。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRejectsMusicAssistant(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yamlWithMA := "wake_word: \"你好小李\"\nmusic_folder: /volume1/music\nnetease:\n  music_assistant: \"http://music-assistant:8095\"\n  api: \"http://netease-api:3000\"\nhub:\n  listen: \":4399\"\n"
	if err := os.WriteFile(path, []byte(yamlWithMA), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("旧配置含 music_assistant 时 Load 应报错")
	}
	if !strings.Contains(err.Error(), "netease.music_assistant 已废弃") {
		t.Fatalf("报错应明说 music_assistant 已废弃，实际: %v", err)
	}
}

func TestLoadWithoutMusicAssistant(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yamlOK := "wake_word: \"你好小李\"\nmusic_folder: /volume1/music\nnetease:\n  api: \"http://netease-api:3000\"\nhub:\n  listen: \":4399\"\n"
	if err := os.WriteFile(path, []byte(yamlOK), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("不含 music_assistant 的配置应能 Load，实际 %v", err)
	}
	if cfg.Netease.API != "http://netease-api:3000" {
		t.Fatalf("netease.api 应读到，实际 %q", cfg.Netease.API)
	}
}
