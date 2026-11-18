package router

import (
	"context"
	"strings"
	"testing"
)

// P2-49：回家场景执行前校验场景内实体 domain——
// 含门锁的场景拒绝执行；只含灯/空调的场景正常执行；
// 查不到场景内容时 fail-closed，拒绝执行。
func TestSceneComingHomeAllowlistP2(t *testing.T) {
	ctx := context.Background()

	// 只含灯/空调：正常执行。
	deps, _, _, f := testDeps(t)
	f.mu.Lock()
	f.states["scene.coming_home"] = "scening"
	f.attrs = map[string]map[string]any{
		"scene.coming_home": {"entity_id": []any{"light.living_room", "climate.bedroom"}},
	}
	f.mu.Unlock()
	rt := New(deps, map[string]string{"客厅灯": "light.living_room"})
	reply, err := rt.Route(ctx, "d1", "我回来了")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "欢迎回来") {
		t.Fatalf("合规场景应执行，实际回复 %q", reply)
	}
	sceneCalled := false
	f.mu.Lock()
	for _, c := range f.calls {
		if c == "scene/turn_on/scene.coming_home" {
			sceneCalled = true
		}
	}
	f.mu.Unlock()
	if !sceneCalled {
		t.Fatal("合规场景应调 scene.turn_on")
	}
}

func TestSceneComingHomeRefusesLockP2(t *testing.T) {
	ctx := context.Background()
	deps, _, _, f := testDeps(t)
	f.mu.Lock()
	f.states["scene.coming_home"] = "scening"
	f.attrs = map[string]map[string]any{
		"scene.coming_home": {"entity_id": []any{"light.living_room", "lock.front_door"}},
	}
	f.calls = nil
	f.mu.Unlock()
	rt := New(deps, map[string]string{"客厅灯": "light.living_room"})
	reply, err := rt.Route(ctx, "d1", "我回来了")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "不能语音控制") {
		t.Fatalf("含门锁的场景应拒绝，实际回复 %q", reply)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(c, "scene/") {
			t.Fatalf("含门锁的场景不得执行，实际调用了 %s", c)
		}
	}
}

func TestSceneComingHomeUnknownRefusedP2(t *testing.T) {
	ctx := context.Background()
	deps, _, _, _ := testDeps(t)
	// testDeps 的假 HA 里没有 scene.coming_home：查不到内容即拒绝。
	rt := New(deps, map[string]string{"客厅灯": "light.living_room"})
	reply, err := rt.Route(ctx, "d1", "我回来了")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "查不到") {
		t.Fatalf("查不到场景内容时应拒绝执行，实际回复 %q", reply)
	}
}

// P2-3：语音认领超长/非法房间名被拒绝，不进 RoomClaim。
func TestRoomClaimRejectsBadNameP2(t *testing.T) {
	ctx := context.Background()
	deps, _, _, _ := testDeps(t)
	claimed := ""
	deps.RoomClaim = func(deviceID, room string) { claimed = room }
	rt := New(deps, map[string]string{})
	reply, err := rt.Route(ctx, "d1", "我在这是一个超过十六个字符的房间名字啊啊啊")
	if err != nil {
		t.Fatal(err)
	}
	if claimed != "" {
		t.Fatalf("超长房间名不得认领，实际认领了 %q", claimed)
	}
	if !strings.Contains(reply, "记不下来") {
		t.Fatalf("超长房间名应明说记不下来，实际回复 %q", reply)
	}
}
