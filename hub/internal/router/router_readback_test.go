package router

// HA 闭环读回验证回归测试（2026-11-23）：调服务成功只表示 HA 接受了请求，
// 读回状态比对才算确认；不一致要如实说没生效，不谎报成功。
// Unknown（读不到）只在 ha 层单测——Route 的前置查状态已保证实体存在，
// 调服务后读不到只发生在 HA 中途断线时，走同一条 switch default 分支。

import (
	"context"
	"strings"
	"testing"
)

func TestHomeAssistantReadbackConfirmed(t *testing.T) {
	deps, _, _, _ := testDeps(t)
	r := New(deps, map[string]string{"客厅灯": "light.living_room"})

	reply, err := r.Route(context.Background(), "lx06-1", "开客厅灯")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "客厅灯开了。" {
		t.Fatalf("读回确认成功时回复应为“客厅灯开了。”，实际 %q", reply)
	}
}

func TestHomeAssistantReadbackMismatch(t *testing.T) {
	deps, _, _, f := testDeps(t)
	f.stubborn = true // HA 接受了指令，但灯没动。
	r := New(deps, map[string]string{"客厅灯": "light.living_room"})

	reply, err := r.Route(context.Background(), "lx06-1", "开客厅灯")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "好像没开") || !strings.Contains(reply, "可能没生效") {
		t.Fatalf("读回不一致时应如实说没开、可能没生效，实际 %q", reply)
	}
}

func TestSceneReadbackConfirmedAndMismatch(t *testing.T) {
	// 确认：场景触发后 state 从 scening 变成 on，读回确认成功。
	deps, _, _, f := testDeps(t)
	f.states["scene.coming_home"] = "scening"
	f.attrs = map[string]map[string]any{
		"scene.coming_home": {"entity_id": []any{"light.living_room", "climate.bedroom"}},
	}
	r := New(deps, nil)
	reply, err := r.Route(context.Background(), "d1", "我回来了")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "欢迎回来") {
		t.Fatalf("场景读回确认成功时应说欢迎回来，实际 %q", reply)
	}

	// 不一致：stubborn 时场景 state 不变，3 次读回都相同。
	deps2, _, _, f2 := testDeps(t)
	f2.stubborn = true
	f2.states["scene.coming_home"] = "scening"
	f2.attrs = map[string]map[string]any{
		"scene.coming_home": {"entity_id": []any{"light.living_room", "climate.bedroom"}},
	}
	r2 := New(deps2, nil)
	reply, err = r2.Route(context.Background(), "d1", "我回来了")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "没有触发") || !strings.Contains(reply, "可能没生效") {
		t.Fatalf("场景读回不一致时应如实说没有触发、可能没生效，实际 %q", reply)
	}
}

// 白名单合并：fan 与 input_boolean 新增可控，climate 保留，门锁仍拒绝。
func TestAllowedVoiceDomainMerged(t *testing.T) {
	deps, _, _, f := testDeps(t)
	f.states["fan.living_room"] = "off"
	f.states["input_boolean.test"] = "off"
	r := New(deps, map[string]string{
		"风扇":   "fan.living_room",
		"测试开关": "input_boolean.test",
		"前门锁":  "lock.front_door",
	})

	if reply, err := r.Route(context.Background(), "lx06-1", "开风扇"); err != nil || reply != "风扇开了。" {
		t.Fatalf("风扇应可语音控制，实际 reply=%q err=%v", reply, err)
	}
	if reply, err := r.Route(context.Background(), "lx06-1", "开测试开关"); err != nil || reply != "测试开关开了。" {
		t.Fatalf("input_boolean 应可语音控制，实际 reply=%q err=%v", reply, err)
	}
	if reply, err := r.Route(context.Background(), "lx06-1", "开前门锁"); err != nil || !strings.Contains(reply, "不能语音直接控制") {
		t.Fatalf("门锁仍应拒绝语音控制，实际 reply=%q err=%v", reply, err)
	}
}
