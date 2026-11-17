package ota

// P1-9 回归测试：pushing 超时标失败、List 快照。

import (
	"strings"
	"testing"
	"time"
)

func TestSweepPushingTimeout(t *testing.T) {
	m := New()
	m.PushTimeout = time.Minute
	sha := strings.Repeat("a", 64)
	if _, err := m.Push("lx06-1", "1.0.0", "http://x/fw.bin", sha); err != nil {
		t.Fatal(err)
	}
	// 30s 后扫荡：pushing 未到 1 分钟超时，不应标失败。
	if failed := m.Sweep(time.Now().Add(30 * time.Second)); len(failed) != 0 {
		t.Fatalf("未超时时不应标失败，实际 %v", failed)
	}
	if j, _ := m.Get("lx06-1"); j.Status != StatusPushing {
		t.Fatalf("未超时时应仍为 pushing，实际 %s", j.Status)
	}
	// 61s 后扫荡：标失败。
	if failed := m.Sweep(time.Now().Add(61 * time.Second)); len(failed) != 1 || failed[0] != "lx06-1" {
		t.Fatalf("超时后应标失败 1 个，实际 %v", failed)
	}
	if j, _ := m.Get("lx06-1"); j.Status != StatusFailed {
		t.Fatalf("超时后状态应为 failed，实际 %s", j.Status)
	}
	// 失败后可重新 Push（旧任务不再挡路）。
	if _, err := m.Push("lx06-1", "1.0.1", "http://x/fw2.bin", sha); err != nil {
		t.Fatalf("失败后重新 Push 应成功，实际 %v", err)
	}
}

func TestList(t *testing.T) {
	m := New()
	sha := strings.Repeat("b", 64)
	if _, err := m.Push("lx06-1", "1.0.0", "http://x/a.bin", sha); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Push("oh2p-1", "1.0.0", "http://x/b.bin", sha); err != nil {
		t.Fatal(err)
	}
	list := m.List()
	if len(list) != 2 {
		t.Fatalf("List 应返回 2 条，实际 %d", len(list))
	}
	ids := map[string]bool{}
	for _, j := range list {
		ids[j.DeviceID] = true
	}
	if !ids["lx06-1"] || !ids["oh2p-1"] {
		t.Fatalf("List 应含两台设备，实际 %v", ids)
	}
}
