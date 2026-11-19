// Package main 的打断（barge-in）单测：DeviceState 状态机。
//
// 覆盖 handleWake / markSpeaking / clearSpeaking / openSession /
// OnDisconnect 的转移表（不走网络；走真实 socket 的端到端见
// barge_in_integration_test.go）:
//
//	Speaking + 唤醒 → Listening，且必须发停播命令；
//	Idle / Listening + 唤醒 → Listening，且不发停播命令；
//	一台设备的唤醒绝不改另一台设备的态；
//	播完定时器（旧代）不得把打断后的 Listening 误清回 Idle；
//	播完回 Listening 还是 Idle，取决于 20s 会话是否还活着；
//	断线清态。
package main

import (
	"testing"
	"time"

	"github.com/huajun/open-xiaoai-next/hub/internal/arbitration"
	"github.com/huajun/open-xiaoai-next/hub/internal/intercom"
	"github.com/huajun/open-xiaoai-next/hub/internal/registry"
	"github.com/huajun/open-xiaoai-next/hub/internal/transport"
)

// newBargeInHandler 建一个只含状态机的 hubHandler（server/router/
// pipeline 全 nil；OnWake 里 server 为 nil 时只改态、不发命令，
// 不 panic——见 TestOnWakeWithoutServerDoesNotPanic）。
func newBargeInHandler() *hubHandler {
	return &hubHandler{
		reg:      registry.New(),
		arbiter:  arbitration.New(time.Millisecond, func(string, []string) {}),
		intercom: intercom.New(func(string, []byte) error { return nil }),
		sessions: map[string]time.Time{},
		states:   map[string]DeviceState{},
		playGen:  map[string]uint64{},
	}
}

func TestHandleWakeSpeakingToListeningNeedsStop(t *testing.T) {
	h := newBargeInHandler()
	gen := h.markSpeaking("lx06-127-0-0-1")
	if got := h.deviceState("lx06-127-0-0-1"); got != DeviceStateSpeaking {
		t.Fatalf("markSpeaking 后应为 Speaking，实际 %q", got)
	}
	// Speaking + 唤醒：必须停播，且进 Listening。
	if !h.handleWake("lx06-127-0-0-1") {
		t.Fatalf("Speaking 时唤醒必须返回 stopPlayback=true")
	}
	if got := h.deviceState("lx06-127-0-0-1"); got != DeviceStateListening {
		t.Fatalf("打断后应为 Listening，实际 %q", got)
	}
	// 打断加了代：旧播放的播完定时器（持旧代号）不得再动这个态。
	h.clearSpeaking("lx06-127-0-0-1", gen)
	if got := h.deviceState("lx06-127-0-0-1"); got != DeviceStateListening {
		t.Fatalf("旧代定时器不得把打断后的 Listening 误清，实际 %q", got)
	}
	// 第二次唤醒（已在听）：不再要求停播。
	if h.handleWake("lx06-127-0-0-1") {
		t.Fatalf("Listening 时唤醒不应再要求停播")
	}
	if got := h.deviceState("lx06-127-0-0-1"); got != DeviceStateListening {
		t.Fatalf("Listening 时唤醒应保持 Listening，实际 %q", got)
	}
}

func TestHandleWakeIdleNeedsNoStop(t *testing.T) {
	h := newBargeInHandler()
	// 从未出现过的设备即 Idle；唤醒后进 Listening，但不发停播。
	if got := h.deviceState("lx06-127-0-0-1"); got != DeviceStateIdle {
		t.Fatalf("未知设备应为 Idle，实际 %q", got)
	}
	if h.handleWake("lx06-127-0-0-1") {
		t.Fatalf("Idle 时唤醒不应要求停播")
	}
	if got := h.deviceState("lx06-127-0-0-1"); got != DeviceStateListening {
		t.Fatalf("Idle 唤醒后应为 Listening，实际 %q", got)
	}
}

func TestHandleWakeOtherDeviceUnaffected(t *testing.T) {
	h := newBargeInHandler()
	h.markSpeaking("oh2p-127-0-0-2")
	// A 唤醒：A 进 Listening；B 仍在 Speaking，且 B 的停播还没被消费。
	if h.handleWake("lx06-127-0-0-1") {
		t.Fatalf("A 在 Idle 时唤醒不应要求停播")
	}
	if got := h.deviceState("lx06-127-0-0-1"); got != DeviceStateListening {
		t.Fatalf("A 应为 Listening，实际 %q", got)
	}
	if got := h.deviceState("oh2p-127-0-0-2"); got != DeviceStateSpeaking {
		t.Fatalf("B 必须仍为 Speaking（A 的唤醒不得停 B），实际 %q", got)
	}
	// B 自己的唤醒才要求停 B。
	if !h.handleWake("oh2p-127-0-0-2") {
		t.Fatalf("B 在 Speaking 时唤醒必须要求停播")
	}
	if got := h.deviceState("oh2p-127-0-0-2"); got != DeviceStateListening {
		t.Fatalf("B 打断后应为 Listening，实际 %q", got)
	}
}

func TestClearSpeakingReturnsToListeningWhileSessionLive(t *testing.T) {
	h := newBargeInHandler()
	// 会话期内播完：回 Listening（20s 连聊期内不用再喊唤醒词）。
	h.openSession("lx06-127-0-0-1")
	if got := h.deviceState("lx06-127-0-0-1"); got != DeviceStateListening {
		t.Fatalf("openSession 后应为 Listening，实际 %q", got)
	}
	gen := h.markSpeaking("lx06-127-0-0-1")
	h.clearSpeaking("lx06-127-0-0-1", gen)
	if got := h.deviceState("lx06-127-0-0-1"); got != DeviceStateListening {
		t.Fatalf("会话期内播完应回 Listening，实际 %q", got)
	}
}

func TestClearSpeakingReturnsToIdleWhenSessionDead(t *testing.T) {
	h := newBargeInHandler()
	// 无会话播完（如提醒播报）：回 Idle。
	gen := h.markSpeaking("lx06-127-0-0-1")
	h.clearSpeaking("lx06-127-0-0-1", gen)
	if got := h.deviceState("lx06-127-0-0-1"); got != DeviceStateIdle {
		t.Fatalf("无会话播完应回 Idle，实际 %q", got)
	}
	// 会话已过期：同样回 Idle（sessions 里放一个过去的截止时间）。
	h.sessMu.Lock()
	h.sessions["lx06-127-0-0-1"] = time.Now().Add(-time.Second)
	h.sessMu.Unlock()
	gen = h.markSpeaking("lx06-127-0-0-1")
	h.clearSpeaking("lx06-127-0-0-1", gen)
	if got := h.deviceState("lx06-127-0-0-1"); got != DeviceStateIdle {
		t.Fatalf("会话过期后播完应回 Idle，实际 %q", got)
	}
}

func TestClearSpeakingIgnoresStaleGeneration(t *testing.T) {
	h := newBargeInHandler()
	gen1 := h.markSpeaking("lx06-127-0-0-1")
	// 同设备又起一轮播报（代际 +1）：旧代定时器不得清新播报。
	gen2 := h.markSpeaking("lx06-127-0-0-1")
	if gen2 == gen1 {
		t.Fatalf("两次 markSpeaking 的代号必须不同")
	}
	h.clearSpeaking("lx06-127-0-0-1", gen1)
	if got := h.deviceState("lx06-127-0-0-1"); got != DeviceStateSpeaking {
		t.Fatalf("旧代定时器不得清新播报，实际 %q", got)
	}
	h.clearSpeaking("lx06-127-0-0-1", gen2)
	if got := h.deviceState("lx06-127-0-0-1"); got != DeviceStateIdle {
		t.Fatalf("本代定时器应把播完的设备置回 Idle，实际 %q", got)
	}
}

func TestOpenSessionSetsListening(t *testing.T) {
	h := newBargeInHandler()
	h.openSession("lx06-127-0-0-1")
	if got := h.deviceState("lx06-127-0-0-1"); got != DeviceStateListening {
		t.Fatalf("openSession 后应为 Listening，实际 %q", got)
	}
	if !h.hasLiveSession("lx06-127-0-0-1") {
		t.Fatalf("openSession 后应有未过期会话")
	}
}

func TestSweepSessionsExpiresListeningToIdle(t *testing.T) {
	h := newBargeInHandler()
	h.openSession("lx06-127-0-0-1")
	// 会话只剩 100ms：清扫 tick（1s）后应过期回 Idle。
	h.sessMu.Lock()
	h.sessions["lx06-127-0-0-1"] = time.Now().Add(100 * time.Millisecond)
	h.sessMu.Unlock()
	done := make(chan struct{})
	go h.sweepSessions(done)
	defer close(done)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := h.deviceState("lx06-127-0-0-1"); got == DeviceStateIdle {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("会话过期后设备应回 Idle，实际 %q", h.deviceState("lx06-127-0-0-1"))
}

func TestOnWakeWithoutServerDoesNotPanic(t *testing.T) {
	h := newBargeInHandler()
	// server 为 nil（单测构造）：OnWake 只改态、不发命令，不 panic。
	// Speaking 时的打断转移本身仍发生（态进 Listening）。
	h.markSpeaking("lx06-127-0-0-1")
	h.OnWake("lx06-127-0-0-1", transport.Wake{WakeWord: "你好小李", RMSdB: -20})
	if got := h.deviceState("lx06-127-0-0-1"); got != DeviceStateListening {
		t.Fatalf("OnWake 打断后应为 Listening，实际 %q", got)
	}
	// Idle 设备同样只改态。
	h.OnWake("oh2p-127-0-0-2", transport.Wake{WakeWord: "你好小李", RMSdB: -20})
	if got := h.deviceState("oh2p-127-0-0-2"); got != DeviceStateListening {
		t.Fatalf("OnWake 后 Idle 设备应为 Listening，实际 %q", got)
	}
}

func TestOnDisconnectClearsState(t *testing.T) {
	h := newBargeInHandler()
	h.markSpeaking("lx06-127-0-0-1")
	h.openSession("lx06-127-0-0-1")
	h.OnDisconnect("lx06-127-0-0-1")
	if got := h.deviceState("lx06-127-0-0-1"); got != DeviceStateIdle {
		t.Fatalf("断线后设备应回 Idle，实际 %q", got)
	}
	if h.hasLiveSession("lx06-127-0-0-1") {
		t.Fatalf("断线后会话应已清除")
	}
}
