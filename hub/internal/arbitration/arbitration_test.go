package arbitration

import (
	"testing"
	"time"
)

func TestArbitrate(t *testing.T) {
	// 响的赢。
	if w := Arbitrate([]WakeEvent{
		{DeviceID: "lx06-1", RMSdB: -30},
		{DeviceID: "oh2p-1", RMSdB: -18},
	}); w != "oh2p-1" {
		t.Fatalf("应选 RMS -18 的 oh2p-1，实际 %q", w)
	}
	// 一台直接赢。
	if w := Arbitrate([]WakeEvent{{DeviceID: "lx06-1", RMSdB: -40}}); w != "lx06-1" {
		t.Fatalf("单台应直接赢，实际 %q", w)
	}
	// 空窗口都不回。
	if w := Arbitrate(nil); w != "" {
		t.Fatalf("空事件应返回 \"\"，实际 %q", w)
	}
	// 同分先到者赢（> 不是 >=）。
	if w := Arbitrate([]WakeEvent{
		{DeviceID: "lx06-1", RMSdB: -20},
		{DeviceID: "oh2p-1", RMSdB: -20},
	}); w != "lx06-1" {
		t.Fatalf("同分应先到者赢，实际 %q", w)
	}
}

// 窗口计时是真计时：两条事件背靠背进 100ms 窗口，只出一个赢家。
// C-12：旧代码在两事件间 Sleep(50ms)，50ms 睡眠对 100ms 窗口只剩
// 50ms 裕量，CI 负载下调度超调即漏窗、赢家断言 flake；背靠背提交
// 仍在同一窗口内，测试意图不变，flake 根除。生产仲裁逻辑不动。
func TestArbiterWindow(t *testing.T) {
	done := make(chan struct{}, 1)
	var winner string
	var losers []string
	a := New(100*time.Millisecond, func(w string, l []string) {
		winner = w
		losers = l
		done <- struct{}{}
	})
	a.Submit(WakeEvent{DeviceID: "lx06-1", RMSdB: -30})
	a.Submit(WakeEvent{DeviceID: "oh2p-1", RMSdB: -15})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("仲裁窗口 100ms 内未关闭（超时 2s）")
	}
	if winner != "oh2p-1" {
		t.Fatalf("赢家应为 oh2p-1，实际 %q", winner)
	}
	if len(losers) != 1 || losers[0] != "lx06-1" {
		t.Fatalf("输家应为 [lx06-1]，实际 %v", losers)
	}
}

func TestArbiterSingle(t *testing.T) {
	done := make(chan struct{}, 1)
	var winner string
	a := New(50*time.Millisecond, func(w string, l []string) {
		winner = w
		done <- struct{}{}
	})
	a.Submit(WakeEvent{DeviceID: "lx06-1", RMSdB: -25})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("单台仲裁未关闭")
	}
	if winner != "lx06-1" {
		t.Fatalf("单台赢家应为 lx06-1，实际 %q", winner)
	}
}
