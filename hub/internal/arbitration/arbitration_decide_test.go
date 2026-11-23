package arbitration

// 2026-11-23 仲裁升级回归测试：三级裁决（响度门限 → 到达时间 →
// 固定设备序平局）+ 会话粘性加分 + 审计字段。

import (
	"testing"
	"time"
)

func TestDecideLoudness(t *testing.T) {
	now := time.Now()
	d := Decide([]WakeEvent{
		{DeviceID: "lx06-1", RMSdB: -30, Timestamp: now},
		{DeviceID: "oh2p-1", RMSdB: -18, Timestamp: now.Add(10 * time.Millisecond)},
	}, "", time.Time{}, now)
	if d.Winner != "oh2p-1" || d.Reason != ReasonLoudness {
		t.Fatalf("应响度胜出 oh2p-1，实际 %+v", d)
	}
	if d.MarginDB != 12 {
		t.Fatalf("响度差应为 12dB，实际 %.1f", d.MarginDB)
	}
	if d.MarginMS != -10 {
		t.Fatalf("赢家比第二名晚到 10ms，MarginMS 应为 -10，实际 %d", d.MarginMS)
	}
	if len(d.Losers) != 1 || d.Losers[0] != "lx06-1" {
		t.Fatalf("输家应为 [lx06-1]，实际 %v", d.Losers)
	}
	if d.Considered != 2 {
		t.Fatalf("参与数应为 2，实际 %d", d.Considered)
	}
}

func TestDecideArrivalWithinMargin(t *testing.T) {
	now := time.Now()
	// 响度差 2dB < 3dB 门限：不认响度胜出，改按到达时间——先到的赢。
	d := Decide([]WakeEvent{
		{DeviceID: "lx06-1", RMSdB: -20, Timestamp: now},
		{DeviceID: "oh2p-1", RMSdB: -18, Timestamp: now.Add(50 * time.Millisecond)},
	}, "", time.Time{}, now)
	if d.Winner != "lx06-1" || d.Reason != ReasonArrival {
		t.Fatalf("门限内应按到达时间选先到的 lx06-1，实际 %+v", d)
	}
	if d.MarginDB != -2 {
		t.Fatalf("赢家比最响者轻 2dB，MarginDB 应为 -2，实际 %.1f", d.MarginDB)
	}
}

func TestDecideFloorBelowMinLoudness(t *testing.T) {
	now := time.Now()
	// 最响者 -55dBFS < -50dBFS 下限：响度不可信，同样按到达时间裁决。
	d := Decide([]WakeEvent{
		{DeviceID: "lx06-1", RMSdB: -60, Timestamp: now},
		{DeviceID: "oh2p-1", RMSdB: -55, Timestamp: now.Add(30 * time.Millisecond)},
	}, "", time.Time{}, now)
	if d.Winner != "lx06-1" || d.Reason != ReasonArrival {
		t.Fatalf("低于下限时应按到达时间选先到的 lx06-1，实际 %+v", d)
	}
}

func TestDecideTieBreak(t *testing.T) {
	now := time.Now()
	// 响度相同、时间戳相同：完全平局 → 固定设备序（字典序），记 TieBreak。
	d := Decide([]WakeEvent{
		{DeviceID: "oh2p-1", RMSdB: -20, Timestamp: now},
		{DeviceID: "lx06-1", RMSdB: -20, Timestamp: now},
	}, "", time.Time{}, now)
	if d.Winner != "lx06-1" || d.Reason != ReasonTieBreak {
		t.Fatalf("完全平局应按设备序选 lx06-1 并记 TieBreak，实际 %+v", d)
	}
}

func TestDecideStickyBonus(t *testing.T) {
	now := time.Now()
	// 上一轮赢家 lx06-1 在 5 秒前赢过：有效响度 -19+4=-15，比 -18 响 3dB；
	// 不加分时它比对方轻 1dB，本赢不了 → 记 Sticky。
	d := Decide([]WakeEvent{
		{DeviceID: "lx06-1", RMSdB: -19, Timestamp: now},
		{DeviceID: "oh2p-1", RMSdB: -18, Timestamp: now.Add(10 * time.Millisecond)},
	}, "lx06-1", now.Add(-5*time.Second), now)
	if d.Winner != "lx06-1" || d.Reason != ReasonSticky {
		t.Fatalf("粘性加分应让上一轮赢家 lx06-1 赢并记 Sticky，实际 %+v", d)
	}
	if !d.Sticky {
		t.Fatalf("赢家应标记 Sticky=true")
	}
}

func TestDecideStickyExpired(t *testing.T) {
	now := time.Now()
	// 上一轮赢家是 25 秒前的事：超出 20s 粘性窗口，加分失效，
	// 响度差 4dB ≥ 3dB 门限，响的 oh2p-1 正常赢。
	d := Decide([]WakeEvent{
		{DeviceID: "lx06-1", RMSdB: -22, Timestamp: now},
		{DeviceID: "oh2p-1", RMSdB: -18, Timestamp: now.Add(10 * time.Millisecond)},
	}, "lx06-1", now.Add(-25*time.Second), now)
	if d.Winner != "oh2p-1" || d.Reason != ReasonLoudness {
		t.Fatalf("粘性过期后应响度胜出 oh2p-1，实际 %+v", d)
	}
	if d.Sticky {
		t.Fatalf("赢家不应标记 Sticky")
	}
}

func TestDecideSoloAndEmpty(t *testing.T) {
	now := time.Now()
	d := Decide([]WakeEvent{{DeviceID: "lx06-1", RMSdB: -40, Timestamp: now}}, "", time.Time{}, now)
	if d.Winner != "lx06-1" || d.Reason != ReasonSolo || d.Considered != 1 {
		t.Fatalf("单台应直接赢并记 Solo，实际 %+v", d)
	}
	if d := Decide(nil, "", time.Time{}, now); d.Winner != "" {
		t.Fatalf("空事件应返回 \"\"，实际 %+v", d)
	}
}

// Arbiter 的粘性状态是真的在轮与轮之间传递：第一轮 lx06-1 单台赢，
// 第二轮它轻 1dB 但靠粘性加分赢回来。
func TestArbiterStickyAcrossRounds(t *testing.T) {
	a, ch := newTestArbiter(50 * time.Millisecond)
	now := time.Now()
	a.Submit(WakeEvent{DeviceID: "lx06-1", WakeWord: "你好小李", RMSdB: -10, Timestamp: now})
	if w, _ := waitResult(t, ch, 2*time.Second); w != "lx06-1" {
		t.Fatalf("首轮应赢 lx06-1，实际 %q", w)
	}
	a.Submit(WakeEvent{DeviceID: "lx06-1", WakeWord: "你好小李", RMSdB: -19, Timestamp: time.Now()})
	a.Submit(WakeEvent{DeviceID: "oh2p-1", WakeWord: "你好小李", RMSdB: -18, Timestamp: time.Now()})
	if w, _ := waitResult(t, ch, 2*time.Second); w != "lx06-1" {
		t.Fatalf("第二轮粘性加分应让 lx06-1 赢回来，实际 %q", w)
	}
}
