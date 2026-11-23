package ha

import (
	"testing"
	"time"
)

// P1-7（2026-11-23 审查）：整条家电命令给总预算，超时即回，不再等满 10s+。
func TestCallServiceWithReadbackPredBudgetServiceHang(t *testing.T) {
	f, url := startFakeHA(t)
	f.hangService = true // call_service 永远不回。
	c, err := Dial(url, "hass-token", 5*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	start := time.Now()
	outcome, err := c.CallServiceWithReadbackPredBudget("light", "turn_on", "light.living_room",
		func(s string) bool { return s == "on" }, 3*time.Second)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("调服务挂起时应返回错误，实际 outcome=%v", outcome)
	}
	// 预算 3s：调服务占一半（1.5s）即超时返回；不限预算的版本要等满 10s。
	if elapsed > 5*time.Second {
		t.Fatalf("总预算 3s 时应在约 3s 内返回，实际 %v", elapsed)
	}
}

func TestCallServiceWithReadbackPredBudgetReadbackHang(t *testing.T) {
	f, url := startFakeHA(t)
	f.slowGetStates = 5 * time.Second // 读回永远赶不上预算。
	c, err := Dial(url, "hass-token", 5*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	start := time.Now()
	outcome, err := c.CallServiceWithReadbackPredBudget("light", "turn_on", "light.living_room",
		func(s string) bool { return s == "on" }, 3*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("调服务成功、读回赶不上预算时应回 Unknown 而不是错误，实际 %v", err)
	}
	if outcome != ReadbackUnknown {
		t.Fatalf("读回赶不上预算时应为 Unknown，实际 %v", outcome)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("总预算 3s 时应在约 3s 内返回，实际 %v", elapsed)
	}
}

// P1-15 支持：ReadEntity 应把属性（current_temperature 等）一起带回来，
// 供 router 的“多少度”查询用，不拿 state 拼“度”。
func TestReadEntityReturnsAttributes(t *testing.T) {
	f, url := startFakeHA(t)
	f.states["climate.bedroom"] = "heat"
	f.attrs = map[string]map[string]any{
		"climate.bedroom": {"current_temperature": 26.0, "temperature": 24.0},
	}
	c, err := Dial(url, "hass-token", 5*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	st, err := c.ReadEntity("climate.bedroom")
	if err != nil {
		t.Fatalf("ReadEntity: %v", err)
	}
	if st.State != "heat" {
		t.Fatalf("state 应为 heat，实际 %q", st.State)
	}
	if v, ok := st.Attributes["current_temperature"]; !ok || v != 26.0 {
		t.Fatalf("应带回 current_temperature=26，实际 %v", st.Attributes)
	}
}
