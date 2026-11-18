package config

// 第三轮审计 P2 回归测试（Hub 面）：
// S-14 指纹归一化两处共用同一个函数（config 与 kws）。

import (
	"strings"
	"testing"
)

// S-14：NormalizeFingerprintHex 去冒号/空格/连字符、转小写，
// 必须是 64 位 hex（大小写不限）。
func TestNormalizeFingerprintHex(t *testing.T) {
	plain := strings.Repeat("aabbccdd", 8) // 64 位
	want := plain
	// 纯小写与全大写应归一化为同一值。
	for _, in := range []string{plain, strings.ToUpper(plain)} {
		got, err := NormalizeFingerprintHex(in)
		if err != nil {
			t.Fatalf("NormalizeFingerprintHex(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("NormalizeFingerprintHex(%q) 应为 %q，实际 %q", in, want, got)
		}
	}
	// 带冒号/空格/连字符的同一指纹应归一化为同一值。
	var b strings.Builder
	for i := 0; i < 8; i++ {
		if i > 0 {
			b.WriteString("-")
		}
		b.WriteString("AABBCCDD")
	}
	got, err := NormalizeFingerprintHex(b.String())
	if err != nil {
		t.Fatalf("带连字符/大写的指纹应通过: %v", err)
	}
	if got != want {
		t.Fatalf("带连字符/大写的指纹应归一化为 %q，实际 %q", want, got)
	}
	// 长度不对 / 非 hex 字符 → 报错，且文案含“64 位 hex（大小写不限）”。
	if _, err := NormalizeFingerprintHex("aabbcc"); err == nil || !strings.Contains(err.Error(), "64 位 hex（大小写不限）") {
		t.Fatalf("短指纹应报“64 位 hex（大小写不限）”，实际 %v", err)
	}
	if _, err := NormalizeFingerprintHex(strings.Repeat("zz", 32)); err == nil {
		t.Fatal("非 hex 指纹应报错")
	}
	if _, err := NormalizeFingerprintHex(""); err == nil {
		t.Fatal("空指纹应报错")
	}
}

// S-14：config 校验接受带连字符/大写的指纹（旧代码只 strip 冒号和空格，
// 带 "-" 的指纹会被 config 拒绝、却是 kws 能接受的格式）。
func TestValidateKWSFingerprintHyphenAndCase(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 8; i++ {
		if i > 0 {
			b.WriteString("-")
		}
		b.WriteString("AABBCCDD")
	}
	c := kwsBase(t)
	c.KWS.Enabled = true
	c.KWS.URL = "ws://127.0.0.1:10400/v1/kws"
	c.KWS.Keywords = []string{"你好小李"}
	c.KWS.Fingerprint = b.String()
	if err := c.Validate(); err != nil {
		t.Fatalf("带连字符/大写的指纹应校验通过，实际: %v", err)
	}
	// 短指纹仍拒绝，文案含“64 位 hex（大小写不限）”。
	c.KWS.Fingerprint = "aabbcc"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "64 位 hex（大小写不限）") {
		t.Fatalf("短指纹应报“64 位 hex（大小写不限）”，实际 %v", err)
	}
}
