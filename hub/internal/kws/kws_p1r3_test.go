package kws

// 第三轮审计 P1 回归测试（S-1）：边车 wake 事件里的 rms_peak 应被 Hub
// 解析进 WakeEvent，仲裁用的有效强度优先取峰值、缺失时回退均值。

import (
	"testing"
	"time"
)

// sendWakePeak 是带 rms_peak 的 wake 发送（不改旧 sendWake 的签名，
// 旧边车不发 rms_peak 的回退路径由旧测试覆盖）。
func (f *fakeSidecar) sendWakePeak(t *testing.T, deviceID, keyword string, rms, rmsPeak float64) {
	t.Helper()
	f.mu.Lock()
	conn, ok := f.conns[deviceID]
	f.mu.Unlock()
	if !ok {
		t.Fatalf("假边车没有设备 %s 的连接，无法发 wake", deviceID)
	}
	if err := f.writeJSON(conn, map[string]any{
		"type": "wake", "device_id": deviceID, "keyword": keyword,
		"rms": rms, "rms_peak": rmsPeak, "timestamp_ms": time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("发 wake 失败: %v", err)
	}
}

// TestWakeEventRMSPeakParsed：含 rms_peak 的 wake 事件应完整解析，
// 且 EffectiveRMS 取峰值（峰值 0.311 > 均值 0.137 时仲裁用 0.311）。
func TestWakeEventRMSPeakParsed(t *testing.T) {
	f := newFakeSidecar(t)
	c, err := New(f.url(), testOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	sendFrames(c, "lx06-1", 1)
	f.waitHello(t, "lx06-1", 3*time.Second)
	f.sendWakePeak(t, "lx06-1", "你好小李", 0.137, 0.311)

	select {
	case ev := <-c.Events():
		if ev.RMS != 0.137 {
			t.Fatalf("wake 事件 rms 应为 0.137，实际 %+v", ev)
		}
		if ev.RMSPeak != 0.311 {
			t.Fatalf("wake 事件 rms_peak 应为 0.311，实际 %+v", ev)
		}
		if got := ev.EffectiveRMS(); got != 0.311 {
			t.Fatalf("含 rms_peak 时仲裁强度应用峰值 0.311，实际 %v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("超时：未收到边车 wake 事件")
	}
}

// TestWakeEventEffectiveRMSFallback：不发 rms_peak 的旧边车事件，
// EffectiveRMS 回退到均值 rms，不改变旧行为。
func TestWakeEventEffectiveRMSFallback(t *testing.T) {
	ev := WakeEvent{DeviceID: "lx06-1", Keyword: "你好小李", RMS: 0.42}
	if got := ev.EffectiveRMS(); got != 0.42 {
		t.Fatalf("缺 rms_peak 时应回退 rms 0.42，实际 %v", got)
	}
	ev.RMSPeak = 0.6
	if got := ev.EffectiveRMS(); got != 0.6 {
		t.Fatalf("有 rms_peak 时应用峰值 0.6，实际 %v", got)
	}
	ev.RMSPeak = 0
	ev.RMS = 0
	if got := ev.EffectiveRMS(); got != 0 {
		t.Fatalf("两者都为 0 时应为 0，实际 %v", got)
	}
}
