package kws

// 第三轮审计 P2 回归测试（Hub 面）：
// S-14 指纹归一化与 config 共用同一个函数。

import (
	"strings"
	"testing"
)

// S-14：kws 的 normalizeFingerprintHex 与 config 共用同一函数——
// 带连字符/大写的指纹应通过，短指纹报错文案含“64 位 hex（大小写不限）”。
func TestNormalizeFingerprintHexSharedWithConfig(t *testing.T) {
	plain := strings.Repeat("aabbccdd", 8) // 64 位
	var b strings.Builder
	for i := 0; i < 8; i++ {
		if i > 0 {
			b.WriteString("-")
		}
		b.WriteString("AABBCCDD")
	}
	got, err := normalizeFingerprintHex(b.String())
	if err != nil {
		t.Fatalf("带连字符/大写的指纹应通过: %v", err)
	}
	if got != plain {
		t.Fatalf("归一化结果应为 %q，实际 %q", plain, got)
	}
	if _, err := normalizeFingerprintHex("xyz"); err == nil || !strings.Contains(err.Error(), "64 位 hex（大小写不限）") {
		t.Fatalf("短指纹应报“64 位 hex（大小写不限）”，实际 %v", err)
	}
}

// S-14：New() 用带连字符的指纹也不应报错（与 config 校验口径一致）。
func TestNewAcceptsHyphenatedFingerprint(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 8; i++ {
		if i > 0 {
			b.WriteString(":")
		}
		b.WriteString("AABBCCDD")
	}
	opts := testOpts()
	opts.TLSFingerprint = b.String()
	c, err := New("ws://127.0.0.1:10400/v1/kws", opts)
	if err != nil {
		t.Fatalf("带冒号/大写的指纹应被 New 接受，实际: %v", err)
	}
	defer c.Close()
}
