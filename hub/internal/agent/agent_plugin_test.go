package agent

import (
	"testing"
)

// 插件式 Agent：per-Agent system_prompt 与 NewFull。
func TestNewFullPrompt(t *testing.T) {
	c := NewFull("http://hermes:8642/v1", "deepseek-chat", "k", "你是管家，只说一句话。")
	if got := c.Prompt(); got != "你是管家，只说一句话。" {
		t.Fatalf("配了 system_prompt 时 Prompt 应返回配的值，实际 %q", got)
	}
	if c.BaseURL != "http://hermes:8642/v1" || c.Model != "deepseek-chat" || c.APIKey != "k" {
		t.Fatalf("NewFull 字段不对: %+v", c)
	}
}

func TestPromptDefault(t *testing.T) {
	c := New("http://hermes:8642/v1", "deepseek-chat", "")
	if got := c.Prompt(); got != DefaultSystemPrompt {
		t.Fatalf("没配 system_prompt 时 Prompt 应返回默认提示词，实际 %q", got)
	}
	if DefaultSystemPrompt == "" {
		t.Fatal("默认提示词不应为空")
	}
}
