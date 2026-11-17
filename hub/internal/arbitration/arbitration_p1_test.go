package arbitration

// P1-13 回归测试：时间戳去重/去陈旧/冷却、输家 RMS 降序。

import (
	"testing"
	"time"
)

func waitResult(t *testing.T, ch chan [2]any, timeout time.Duration) (string, []string) {
	t.Helper()
	select {
	case r := <-ch:
		w, _ := r[0].(string)
		l, _ := r[1].([]string)
		return w, l
	case <-time.After(timeout):
		t.Fatal("超时：仲裁未出结果")
		return "", nil
	}
}

func newTestArbiter(window time.Duration) (*Arbiter, chan [2]any) {
	ch := make(chan [2]any, 4)
	a := New(window, func(winner string, losers []string) {
		ch <- [2]any{winner, losers}
	})
	return a, ch
}

func TestDedupSameDevice(t *testing.T) {
	a, ch := newTestArbiter(100 * time.Millisecond)
	now := time.Now()
	// 同设备双通道重复：本地 wake 与边车 wake 时间戳相差 300ms，应只算一次。
	a.Submit(WakeEvent{DeviceID: "lx06-1", WakeWord: "你好小李", RMSdB: -10, Timestamp: now})
	a.Submit(WakeEvent{DeviceID: "lx06-1", WakeWord: "你好小李", RMSdB: -12, Timestamp: now.Add(300 * time.Millisecond)})
	w, losers := waitResult(t, ch, 2*time.Second)
	if w != "lx06-1" || len(losers) != 0 {
		t.Fatalf("去重后应只有 lx06-1 赢、无输家，实际赢 %q 输 %v", w, losers)
	}
	// 1s 后同一设备再唤醒是新一轮，不应被去重。
	time.Sleep(1100 * time.Millisecond)
	a.Submit(WakeEvent{DeviceID: "lx06-1", WakeWord: "你好小李", RMSdB: -10, Timestamp: time.Now()})
	if w, _ := waitResult(t, ch, 2*time.Second); w != "lx06-1" {
		t.Fatalf("冷却期后新唤醒应正常裁决，实际赢 %q", w)
	}
}

func TestCooldownDropsLateDuplicate(t *testing.T) {
	a, ch := newTestArbiter(50 * time.Millisecond)
	a.Submit(WakeEvent{DeviceID: "lx06-1", WakeWord: "你好小李", RMSdB: -10, Timestamp: time.Now()})
	if w, _ := waitResult(t, ch, 2*time.Second); w != "lx06-1" {
		t.Fatalf("首轮应赢 lx06-1，实际 %q", w)
	}
	// 冷却期内用旧时间戳补一个迟到重复：不应再开一轮。
	a.Submit(WakeEvent{DeviceID: "lx06-1", WakeWord: "你好小李", RMSdB: -10, Timestamp: time.Now().Add(-2 * time.Second)})
	select {
	case r := <-ch:
		t.Fatalf("冷却期内迟到重复不应再裁决，实际 %+v", r)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestStaleEventDropped(t *testing.T) {
	a, ch := newTestArbiter(100 * time.Millisecond)
	now := time.Now()
	a.Submit(WakeEvent{DeviceID: "lx06-1", WakeWord: "你好小李", RMSdB: -10, Timestamp: now})
	// 窗口已开后，另一台设备用 2 秒前的时间戳迟到：丢弃，不进本轮。
	a.Submit(WakeEvent{DeviceID: "oh2p-1", WakeWord: "你好小李", RMSdB: -5, Timestamp: now.Add(-2 * time.Second)})
	w, losers := waitResult(t, ch, 2*time.Second)
	if w != "lx06-1" || len(losers) != 0 {
		t.Fatalf("陈旧事件应被丢弃，实际赢 %q 输 %v", w, losers)
	}
}

func TestLosersRankedByRMS(t *testing.T) {
	a, ch := newTestArbiter(100 * time.Millisecond)
	now := time.Now()
	a.Submit(WakeEvent{DeviceID: "a", WakeWord: "你好小李", RMSdB: -20, Timestamp: now})
	a.Submit(WakeEvent{DeviceID: "b", WakeWord: "你好小李", RMSdB: -5, Timestamp: now})
	a.Submit(WakeEvent{DeviceID: "c", WakeWord: "你好小李", RMSdB: -12, Timestamp: now})
	w, losers := waitResult(t, ch, 2*time.Second)
	if w != "b" {
		t.Fatalf("RMS 最大的 b 应赢，实际 %q", w)
	}
	if len(losers) != 2 || losers[0] != "c" || losers[1] != "a" {
		t.Fatalf("输家应按 RMS 降序 [c a]（赢家失败补位用），实际 %v", losers)
	}
}
