package config

import (
	"testing"
	"time"
)

func TestSessionTimeoutDefault(t *testing.T) {
	c := &Config{}
	if got := c.SessionTimeout(); got != 60*time.Second {
		t.Fatalf("未配 session_timeout_seconds 时应默认 60s，实际 %v", got)
	}
	c.Hub.SessionTimeoutSeconds = 120
	if got := c.SessionTimeout(); got != 120*time.Second {
		t.Fatalf("配 120 时超时应为 120s，实际 %v", got)
	}
	c.Hub.SessionTimeoutSeconds = 0
	if got := c.SessionTimeout(); got != 60*time.Second {
		t.Fatalf("配 0 时应默认 60s，实际 %v", got)
	}
}

func TestSessionTimeoutValidation(t *testing.T) {
	c := &Config{WakeWord: "你好小李", MusicFolder: "/volume1/music"}
	c.Hub.Listen = ":4399"
	c.Hub.SessionTimeoutSeconds = -5
	if err := c.Validate(); err == nil {
		t.Fatal("session_timeout_seconds 为负数时校验应失败")
	}
	c.Hub.SessionTimeoutSeconds = 120
	if err := c.Validate(); err != nil {
		t.Fatalf("session_timeout_seconds=120 时校验应通过，实际 %v", err)
	}
}

func TestRealConfigSessionTimeout(t *testing.T) {
	cfg, err := Load("../../config.yaml")
	if err != nil {
		t.Fatalf("Load 真实 config.yaml 失败: %v", err)
	}
	if got := cfg.SessionTimeout(); got != 60*time.Second {
		t.Fatalf("真实 config.yaml 的会话超时应为 60s，实际 %v", got)
	}
}
