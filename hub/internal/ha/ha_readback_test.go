package ha

// 读回验证回归测试（2026-11-23）：调服务成功只表示 HA 接受了请求，
// 读回状态比对才算确认；不一致与读不到要严格区分，不谎报成功、
// 也不谎报失败。

import (
	"testing"
	"time"
)

func TestCallServiceWithReadbackConfirmed(t *testing.T) {
	_, url := startFakeHA(t)
	c, err := Dial(url, "hass-token", 5*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	outcome, err := c.CallServiceWithReadback("light", "turn_on", "light.living_room", "on")
	if err != nil {
		t.Fatalf("CallServiceWithReadback: %v", err)
	}
	if outcome != ReadbackConfirmed {
		t.Fatalf("turn_on 后读回应为 on，应确认成功，实际 %v", outcome)
	}
}

func TestCallServiceWithReadbackMismatch(t *testing.T) {
	f, url := startFakeHA(t)
	f.stubborn = true // 调服务不改状态：HA 接受了，但设备没动。
	c, err := Dial(url, "hass-token", 5*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	outcome, err := c.CallServiceWithReadback("light", "turn_on", "light.living_room", "on")
	if err != nil {
		t.Fatalf("CallServiceWithReadback: %v", err)
	}
	if outcome != ReadbackMismatch {
		t.Fatalf("状态 3 次都没变，应为不一致，实际 %v", outcome)
	}
}

func TestVerifyStateUnknown(t *testing.T) {
	_, url := startFakeHA(t)
	c, err := Dial(url, "hass-token", 5*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	if outcome := c.VerifyState("light.nonexistent", "on"); outcome != ReadbackUnknown {
		t.Fatalf("不存在的实体应为未知，实际 %v", outcome)
	}
}

func TestVerifyStateRetriesThenConfirms(t *testing.T) {
	f, url := startFakeHA(t)
	// 状态在第 2 次读取时才翻成 on：第一次读到 off 不符、重试，第 2 次确认。
	f.flipAfterReads = 2
	f.flipEntity = "light.living_room"
	f.flipTo = "on"
	c, err := Dial(url, "hass-token", 5*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	if outcome := c.VerifyState("light.living_room", "on"); outcome != ReadbackConfirmed {
		t.Fatalf("第 2 次读到 on 时应确认成功，实际 %v", outcome)
	}
}

func TestVerifyStateChanged(t *testing.T) {
	f, url := startFakeHA(t)
	f.mu.Lock()
	f.states["scene.coming_home"] = "unknown"
	f.mu.Unlock()
	c, err := Dial(url, "hass-token", 5*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	// 场景触发：turn_on 后 state 从 unknown 变成 on（真 HA 里是触发时间戳，
	// 这里只验证“变了即确认”的逻辑）。
	if err := c.CallService("scene", "turn_on", "scene.coming_home"); err != nil {
		t.Fatalf("CallService: %v", err)
	}
	if outcome := c.VerifyStateChanged("scene.coming_home", "unknown"); outcome != ReadbackConfirmed {
		t.Fatalf("场景状态变了应确认成功，实际 %v", outcome)
	}
	// 状态没变：3 次都相同 → 不一致。
	if outcome := c.VerifyStateChanged("scene.coming_home", "on"); outcome != ReadbackMismatch {
		t.Fatalf("场景状态没变应为不一致，实际 %v", outcome)
	}
}

func TestReadStateTimeout(t *testing.T) {
	f, url := startFakeHA(t)
	f.slowGetStates = 5 * time.Second // 服务端 5s 才回
	c, err := Dial(url, "hass-token", 5*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	// callWithTimeout 100ms 应快失败，不等服务端 5s 回复。
	start := time.Now()
	if _, err := c.callWithTimeout("get_states", nil, 100*time.Millisecond); err == nil {
		t.Fatal("超时调用应返回错误")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("100ms 超时应在 2s 内返回，实际 %v", elapsed)
	}
	// ReadState 用 2s 超时：同样快失败，VerifyState 应判未知。
	start = time.Now()
	if outcome := c.VerifyState("light.living_room", "on"); outcome != ReadbackUnknown {
		t.Fatalf("读回超时应为未知，实际 %v", outcome)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("读回超时应在 4s 内返回未知，实际 %v", elapsed)
	}
}
