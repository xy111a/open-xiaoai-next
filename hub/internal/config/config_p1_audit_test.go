package config

import "testing"

// P1-1（2026-11-23 审查）：kws.enabled 时 wake_word 必须在 kws.keywords 里，
// 否则 KWS 永远触发不了唤醒词，启动时就应报错，而不是等用户喊破喉咙。
func TestValidateWakeWordInKeywords(t *testing.T) {
	c := kwsBase(t)
	c.KWS.Enabled = true
	c.KWS.URL = "ws://127.0.0.1:10400/v1/kws"
	c.KWS.Keywords = []string{"你好小李"}
	if err := c.Validate(); err != nil {
		t.Fatalf("wake_word 在 keywords 里时应校验通过，实际 %v", err)
	}
	// 唤醒词不在 keywords 里：启动即报错。
	c.KWS.Keywords = []string{"你好小七"}
	if err := c.Validate(); err == nil {
		t.Fatal("wake_word 不在 kws.keywords 里时应校验失败")
	}
	// KWS 未启用时不交叉校验（唤醒词可能走别的路）。
	c.KWS.Enabled = false
	if err := c.Validate(); err != nil {
		t.Fatalf("kws 未启用时不应校验 wake_word 与 keywords 的一致性，实际 %v", err)
	}
}
