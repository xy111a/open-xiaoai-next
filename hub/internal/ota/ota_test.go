package ota

import (
	"testing"
	"time"
)

const goodSHA = "a56a38619fd98e467adb8c998757f80109c4dac89db48d62dae985e3062476ed" // LX06 1.94.13 的真实 SHA256

func TestSHA256HexKnown(t *testing.T) {
	if got := SHA256Hex([]byte("abc")); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("SHA256(\"abc\") 不对: %s", got)
	}
}

func TestPushValidation(t *testing.T) {
	m := New()
	if _, err := m.Push("lx06-1", "1.94.14", "https://example.com/f.bin", "zz"); err == nil {
		t.Fatal("非法 sha256（非 64 hex）应拒绝")
	}
	if _, err := m.Push("lx06-1", "", "https://example.com/f.bin", goodSHA); err == nil {
		t.Fatal("空 version 应拒绝")
	}
	if _, err := m.Push("", "1.94.14", "https://example.com/f.bin", goodSHA); err == nil {
		t.Fatal("空 deviceID 应拒绝")
	}
	job, err := m.Push("lx06-1", "1.94.14", "https://example.com/f.bin", goodSHA)
	if err != nil {
		t.Fatalf("合法 Push 应成功: %v", err)
	}
	if job.Status != StatusPushing {
		t.Fatalf("Push 后状态应为 pushing，实际 %q", job.Status)
	}
	if _, ok := m.Get("lx06-1"); !ok {
		t.Fatal("Push 后 Get 应找到 job")
	}
}

func TestConfirmHelloFlow(t *testing.T) {
	m := New()
	if _, err := m.Push("lx06-1", "1.94.14", "https://example.com/f.bin", goodSHA); err != nil {
		t.Fatal(err)
	}
	m.MarkRebooted("lx06-1")
	job, _ := m.Get("lx06-1")
	if job.Status != StatusVerifying {
		t.Fatalf("MarkRebooted 后应为 verifying，实际 %q", job.Status)
	}
	// 旧版本 hello 不算成功。
	if ok := m.ConfirmHello("lx06-1", "1.94.13"); ok {
		t.Fatal("旧版本 hello 不应确认成功")
	}
	job, _ = m.Get("lx06-1")
	if job.Status != StatusVerifying {
		t.Fatalf("旧版本 hello 后仍应为 verifying，实际 %q", job.Status)
	}
	// 新版本 hello → OK。
	if ok := m.ConfirmHello("lx06-1", "1.94.14"); !ok {
		t.Fatal("新版本 hello 应确认成功")
	}
	job, _ = m.Get("lx06-1")
	if job.Status != StatusOK {
		t.Fatalf("确认后应为 ok，实际 %q", job.Status)
	}
}

// 超时扫荡是真计时逻辑：VerifyBy 已过且仍在 verifying → failed。
func TestSweepTimeout(t *testing.T) {
	m := New()
	if _, err := m.Push("lx06-1", "1.94.14", "https://example.com/f.bin", goodSHA); err != nil {
		t.Fatal(err)
	}
	m.MarkRebooted("lx06-1")
	// 未来时间扫荡：不应失败。
	if failed := m.Sweep(time.Now()); len(failed) != 0 {
		t.Fatalf("未超时不应扫出失败，实际 %v", failed)
	}
	// 30s VerifyTimeout 过后扫荡：应失败。
	if failed := m.Sweep(time.Now().Add(31 * time.Second)); len(failed) != 1 || failed[0] != "lx06-1" {
		t.Fatalf("超时后应扫出 lx06-1 失败，实际 %v", failed)
	}
	job, _ := m.Get("lx06-1")
	if job.Status != StatusFailed {
		t.Fatalf("超时后状态应为 failed，实际 %q", job.Status)
	}
}
