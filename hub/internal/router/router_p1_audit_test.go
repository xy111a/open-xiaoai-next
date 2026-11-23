package router

// P1-12/P1-13/P1-15 回归测试（2026-11-23 审查）。

import (
	"context"
	"strings"
	"testing"
)

// P1-12：定时动作补房间名前先查目标是否含任意已知房间；建提醒前校验
// 拼好的命令能在别名表命中，否则当场拒绝、不入库。
func TestTimedActionKeepsOtherRoom(t *testing.T) {
	deps, rem, _, _ := testDeps(t)
	deps.KnownRooms = func() []string { return []string{"客厅", "卧室"} }
	entities := map[string]string{"卧室灯": "light.bedroom", "客厅灯": "light.living_room"}
	r := New(deps, entities)
	ctx := context.Background()

	// 在客厅说“2 小时后关卧室灯”：目标已含已知房间“卧室”，原样不动。
	reply, err := r.Route(ctx, "lx06-1", "2 小时后关卧室灯")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "2 小时后帮你关卧室灯") {
		t.Fatalf("已带房间名时不应再补当前房间，实际 %q", reply)
	}
	items := rem.List()
	if len(items) != 1 || items[0].Text != "关卧室灯" {
		t.Fatalf("应存 Text=关卧室灯，实际 %+v", items)
	}

	// “2 小时后关灯”：目标无房间名，补当前房间“客厅”。
	reply, err = r.Route(ctx, "lx06-1", "2 小时后关灯")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "2 小时后帮你关客厅灯") {
		t.Fatalf("无房间名时应补当前房间，实际 %q", reply)
	}
	items = rem.List()
	if len(items) != 2 || items[1].Text != "关客厅灯" {
		t.Fatalf("应存 Text=关客厅灯，实际 %+v", items)
	}

	// 拼不出的别名当场拒绝，不入库。
	reply, err = r.Route(ctx, "lx06-1", "2 小时后关不存在的灯")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "没找到") {
		t.Fatalf("拼不出别名时应明说没找到，实际 %q", reply)
	}
	if len(rem.List()) != 2 {
		t.Fatalf("拒绝后不应入库，实际 %+v", rem.List())
	}
}

// P1-13：房间认领只认已知房间或“我在<2~4 字房间名>”；未知房间名先确认，
// 带多余内容时不是认领，不写注册表。
func TestRoomClaimKnownUnknownAndFallthrough(t *testing.T) {
	deps, _, _, _ := testDeps(t)
	claimed := map[string]string{}
	deps.RoomClaim = func(deviceID, room string) { claimed[deviceID] = room }
	deps.KnownRooms = func() []string { return []string{"客厅", "卧室"} }
	r := New(deps, nil)
	ctx := context.Background()

	// 已知房间：直接认领。
	reply, err := r.Route(ctx, "lx06-1", "我在客厅")
	if err != nil {
		t.Fatal(err)
	}
	if claimed["lx06-1"] != "客厅" {
		t.Fatalf("已知房间应直接认领，实际 claimed=%v reply=%q", claimed, reply)
	}

	// “我在客厅吃饭”：带多余内容，不是认领，不写注册表，落到其它分支。
	delete(claimed, "lx06-1")
	reply, err = r.Route(ctx, "lx06-1", "我在客厅吃饭")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := claimed["lx06-1"]; ok {
		t.Fatalf("带多余内容时不应认领，实际 claimed=%v", claimed)
	}
	if !strings.Contains(reply, "没接大脑") {
		t.Fatalf("落到 Agent 分支时（未配 Agent）应明说，实际 %q", reply)
	}

	// 未知房间名：先确认，不写注册表。
	reply, err = r.Route(ctx, "lx06-1", "我在书房")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := claimed["lx06-1"]; ok {
		t.Fatalf("未知房间名确认前不应写入，实际 claimed=%v", claimed)
	}
	if !strings.Contains(reply, "确认") || !strings.Contains(reply, "书房") {
		t.Fatalf("未知房间名应向用户确认，实际 %q", reply)
	}

	// 按确认句式重复一次：认领。
	reply, err = r.Route(ctx, "lx06-1", "确认，我在书房")
	if err != nil {
		t.Fatal(err)
	}
	if claimed["lx06-1"] != "书房" {
		t.Fatalf("确认后应认领书房，实际 claimed=%v reply=%q", claimed, reply)
	}
}

// 无 KnownRooms 时（首次认领、注册表还空）保留旧规则：2~4 字名直接认领。
func TestRoomClaimNoKnownRoomsFallback(t *testing.T) {
	deps, _, _, _ := testDeps(t)
	claimed := map[string]string{}
	deps.RoomClaim = func(deviceID, room string) { claimed[deviceID] = room }
	deps.KnownRooms = nil
	r := New(deps, nil)
	if _, err := r.Route(context.Background(), "lx06-1", "我在客厅"); err != nil {
		t.Fatal(err)
	}
	if claimed["lx06-1"] != "客厅" {
		t.Fatalf("无 KnownRooms 时 2~4 字房间名应直接认领，实际 %v", claimed)
	}
}

// P1-15：HA 查询分支不吐原始英文 state；“多少度”读属性，读不到明说。
func TestHomeQueryClimateAndCover(t *testing.T) {
	deps, _, _, f := testDeps(t)
	f.states["climate.bedroom"] = "heat"
	f.attrs = map[string]map[string]any{
		"climate.bedroom": {"current_temperature": 26.0, "temperature": 24.0},
	}
	entities := map[string]string{
		"卧室空调": "climate.bedroom",
		"客厅灯":  "light.living_room",
		"卧室窗帘": "cover.bedroom",
	}
	r := New(deps, entities)
	ctx := context.Background()

	// 查温度：读 current_temperature，不拿 state 拼“度”。
	reply, err := r.Route(ctx, "lx06-1", "卧室空调多少度")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "卧室空调现在 26 度。" {
		t.Fatalf("应回“现在 26 度”，实际 %q", reply)
	}

	// 查开/关：climate 的 state 是 heat（运行模式），即开着。
	reply, err = r.Route(ctx, "lx06-1", "卧室空调开着吗")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "卧室空调开着呢。" {
		t.Fatalf("climate heat 应判开着，实际 %q", reply)
	}

	// cover open 即开着。
	f.states["cover.bedroom"] = "open"
	reply, err = r.Route(ctx, "lx06-1", "卧室窗帘开着吗")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "卧室窗帘开着呢。" {
		t.Fatalf("cover open 应判开着，实际 %q", reply)
	}

	// 属性缺失：明说查不到温度，不拿 state 拼“度”。
	delete(f.attrs, "climate.bedroom")
	reply, err = r.Route(ctx, "lx06-1", "卧室空调多少度")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "查不到温度") {
		t.Fatalf("读不到温度时应明说查不到，实际 %q", reply)
	}
	if strings.Contains(reply, "heat") {
		t.Fatalf("不应吐 HA 原始英文 state，实际 %q", reply)
	}
}
