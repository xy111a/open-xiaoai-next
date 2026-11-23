package router

// P1 全量修复回归测试：P1-15 对讲触发、P1-25 每日提醒越界、
// P1-38 多 Agent 路由、P1-39 房间认领、P1-41 记忆读
// 与定时动作、P1-42 查询与查状态失败、P1-27 SetHA 热换；顺带 P2-45。

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/huajun/open-xiaoai-next/hub/internal/agent"
	"github.com/huajun/open-xiaoai-next/hub/internal/reminders"
)

// ---- P1-15：对讲触发 ----

type fakeIntercom struct {
	mu         sync.Mutex
	starts     [][2]string
	broadcasts []string
	ends       []string
}

func (f *fakeIntercom) Start(from, to string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, [2]string{from, to})
	return true
}

func (f *fakeIntercom) StartBroadcast(from string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.broadcasts = append(f.broadcasts, from)
	return true
}

func (f *fakeIntercom) End(from string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ends = append(f.ends, from)
	return true
}

func TestIntercomCall(t *testing.T) {
	deps, _, _, _ := testDeps(t)
	ic := &fakeIntercom{}
	deps.Intercom = ic
	deps.DeviceByRoom = func(room string) (string, bool) {
		if room == "卧室" {
			return "oh2p-1", true
		}
		return "", false
	}
	r := New(deps, nil)
	ctx := context.Background()

	reply, err := r.Route(ctx, "lx06-1", "呼叫卧室")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "正在呼叫卧室") {
		t.Fatalf("呼叫回复不对: %q", reply)
	}
	if len(ic.starts) != 1 || ic.starts[0] != [2]string{"lx06-1", "oh2p-1"} {
		t.Fatalf("应调 Start(lx06-1, oh2p-1)，实际 %v", ic.starts)
	}
	// 未知房间：明说找不到，不触发。
	reply, err = r.Route(ctx, "lx06-1", "呼叫书房")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "找不到书房") {
		t.Fatalf("未知房间应明说找不到，实际 %q", reply)
	}
	if len(ic.starts) != 1 {
		t.Fatalf("未知房间不应再调 Start，实际 %v", ic.starts)
	}
	// 广播与挂断。
	reply, err = r.Route(ctx, "lx06-1", "全屋广播")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "全屋广播") {
		t.Fatalf("广播回复不对: %q", reply)
	}
	if len(ic.broadcasts) != 1 || ic.broadcasts[0] != "lx06-1" {
		t.Fatalf("应调 StartBroadcast(lx06-1)，实际 %v", ic.broadcasts)
	}
	reply, err = r.Route(ctx, "lx06-1", "挂断")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "已经挂断对讲") {
		t.Fatalf("挂断回复不对: %q", reply)
	}
	if len(ic.ends) != 1 || ic.ends[0] != "lx06-1" {
		t.Fatalf("应调 End(lx06-1)，实际 %v", ic.ends)
	}
}

func TestIntercomNotConfiguredFallsThrough(t *testing.T) {
	deps, _, _, _ := testDeps(t)
	r := New(deps, nil)
	reply, err := r.Route(context.Background(), "lx06-1", "呼叫卧室")
	if err != nil {
		t.Fatal(err)
	}
	// 对讲未配时不报错、不假装接通，落到无大脑提示。
	if strings.Contains(reply, "正在呼叫") {
		t.Fatalf("对讲未配时不能假装接通，实际 %q", reply)
	}
}

// ---- P1-39：房间认领 ----

func TestRoomClaim(t *testing.T) {
	deps, _, _, _ := testDeps(t)
	var claimed [][2]string
	deps.RoomClaim = func(deviceID, room string) {
		claimed = append(claimed, [2]string{deviceID, room})
	}
	r := New(deps, nil)
	ctx := context.Background()
	reply, err := r.Route(ctx, "lx06-1", "我在客厅")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "你在客厅了") {
		t.Fatalf("认领回复不对: %q", reply)
	}
	if len(claimed) != 1 || claimed[0] != [2]string{"lx06-1", "客厅"} {
		t.Fatalf("应调 RoomClaim(lx06-1, 客厅)，实际 %v", claimed)
	}
	// “我在这里”：已知房间时确认。
	reply, err = r.Route(ctx, "lx06-1", "我在这里")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "你就在客厅") {
		t.Fatalf("已知房间时“我在这里”应确认，实际 %q", reply)
	}
}

func TestRoomHereUnknownAsks(t *testing.T) {
	deps, _, _, _ := testDeps(t)
	deps.RoomOfDevice = nil
	r := New(deps, nil)
	reply, err := r.Route(context.Background(), "lx06-1", "我在这里")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "你在哪间房") {
		t.Fatalf("未知房间时应追问房间，实际 %q", reply)
	}
}

// ---- P1-25：每日提醒越界 ----

func TestReminderDailyOutOfRange(t *testing.T) {
	deps, rem, _, _ := testDeps(t)
	r := New(deps, nil)
	ctx := context.Background()
	for _, text := range []string{"每天 25 点提醒我吃药", "每天 8 点 70 分提醒我吃药"} {
		reply, err := r.Route(ctx, "lx06-1", text)
		if err != nil {
			t.Fatalf("%q 不应返回 error（越界要回用户可听的话），实际 err=%v", text, err)
		}
		if !strings.Contains(reply, "超出了范围") {
			t.Fatalf("%q 应回时间超出范围，实际 %q", text, reply)
		}
	}
	if len(rem.List()) != 0 {
		t.Fatalf("越界提醒不应入库，实际 %+v", rem.List())
	}
}

// ---- P1-41：记忆读 + 定时动作；P2-45 顺带 ----

func TestMemoryRecall(t *testing.T) {
	deps, _, mem, _ := testDeps(t)
	r := New(deps, nil)
	ctx := context.Background()
	// 空记忆：明说还没记住过。
	reply, err := r.Route(ctx, "lx06-1", "你还记得什么")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "还没记住过什么") {
		t.Fatalf("空记忆应明说，实际 %q", reply)
	}
	// 记两条再读：都列出来。
	if _, err := r.Route(ctx, "lx06-1", "记住空调 26 度"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Route(ctx, "lx06-1", "记住周末去爬山"); err != nil {
		t.Fatal(err)
	}
	if got := mem.All("lx06-1"); len(got) != 2 {
		t.Fatalf("应存 2 条记忆，实际 %v", got)
	}
	reply, err = r.Route(ctx, "lx06-1", "你还记得什么")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "空调 26 度") || !strings.Contains(reply, "周末去爬山") {
		t.Fatalf("读记忆应列出两条，实际 %q", reply)
	}
}

func TestTimedAction(t *testing.T) {
	deps, rem, _, _ := testDeps(t)
	r := New(deps, nil)
	ctx := context.Background()
	reply, err := r.Route(ctx, "lx06-1", "2 小时后关灯")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "2 小时后帮你关客厅灯") {
		t.Fatalf("定时动作回复不对: %q", reply)
	}
	items := rem.List()
	if len(items) != 1 || items[0].Kind != reminders.KindAction || items[0].Text != "关客厅灯" {
		t.Fatalf("应存 1 条 KindAction 提醒（Text=关客厅灯），实际 %+v", items)
	}
	// 0 小时无意义：拒绝。
	reply, err = r.Route(ctx, "lx06-1", "0 小时后关灯")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "大于 0") {
		t.Fatalf("0 小时应拒绝，实际 %q", reply)
	}
	if len(rem.List()) != 1 {
		t.Fatalf("拒绝后不应再入库，实际 %+v", rem.List())
	}
}

func TestIntervalZeroRejected(t *testing.T) {
	deps, rem, _, _ := testDeps(t)
	r := New(deps, nil)
	reply, err := r.Route(context.Background(), "lx06-1", "每 0 分钟提醒我喝水")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "大于 0 分钟") {
		t.Fatalf("每 0 分钟应拒绝，实际 %q", reply)
	}
	if len(rem.List()) != 0 {
		t.Fatalf("拒绝后不应入库，实际 %+v", rem.List())
	}
}

// ---- P1-42：查询与查状态失败 ----

func TestHomeAssistantQuery(t *testing.T) {
	deps, _, _, f := testDeps(t)
	entities := map[string]string{"客厅灯": "light.living_room", "卧室灯": "light.bedroom"}
	r := New(deps, entities)
	ctx := context.Background()
	// 客厅灯 off → “开着吗”回“关着呢”，且不调任何服务。
	reply, err := r.Route(ctx, "lx06-1", "客厅灯开着吗")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "客厅灯关着呢") {
		t.Fatalf("查询回复不对: %q", reply)
	}
	f.mu.Lock()
	if len(f.calls) != 0 {
		t.Fatalf("查询不应调服务，实际 %v", f.calls)
	}
	f.mu.Unlock()
	// 卧室灯 on → 回“开着呢”。
	reply, err = r.Route(ctx, "lx06-1", "卧室灯开着吗")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "卧室灯开着呢") {
		t.Fatalf("查询回复不对: %q", reply)
	}
}

func TestHomeAssistantStateErrorNoServiceCall(t *testing.T) {
	deps, _, _, f := testDeps(t)
	// HA 客户端断开：GetEntityState 必然失败。
	deps.HA.Close()
	entities := map[string]string{"客厅灯": "light.living_room"}
	r := New(deps, entities)
	reply, err := r.Route(context.Background(), "lx06-1", "开客厅灯")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "查不到客厅灯的状态") {
		t.Fatalf("查状态失败应明说先不操作，实际 %q", reply)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != 0 {
		t.Fatalf("查状态失败时绝不能调服务，实际 %v", f.calls)
	}
}

// ---- P1-38：多 Agent 按唤醒词路由 ----

func fakeAgentServer(t *testing.T, reply string) *agent.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q}}]}`, reply)
	}))
	t.Cleanup(srv.Close)
	return agent.New(srv.URL+"/v1", "m", "")
}

func TestAgentRoutingByWakeWord(t *testing.T) {
	deps, _, _, _ := testDeps(t)
	hermes := fakeAgentServer(t, "hermes 回复")
	claw := fakeAgentServer(t, "claw 回复")
	deps.Agents = map[string]*agent.Client{"hermes": hermes, "claw": claw}
	deps.WakeWordRoutes = map[string]string{"你好小李": "hermes", "嘿小李": "claw"}
	r := New(deps, nil)
	ctx := context.Background()

	r.SetWakeWord("lx06-1", "你好小李")
	reply, err := r.Route(ctx, "lx06-1", "今天天气怎么样")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "hermes 回复" {
		t.Fatalf("唤醒词“你好小李”应走 hermes，实际 %q", reply)
	}
	r.SetWakeWord("lx06-1", "嘿小李")
	reply, err = r.Route(ctx, "lx06-1", "今天天气怎么样")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "claw 回复" {
		t.Fatalf("唤醒词“嘿小李”应走 claw，实际 %q", reply)
	}
	// 未知唤醒词：确定性回退（按 Agent 名排序首个 claw），不是随机。
	r.SetWakeWord("lx06-1", "你好小爱")
	for i := 0; i < 3; i++ {
		reply, err = r.Route(ctx, "lx06-1", "今天天气怎么样")
		if err != nil {
			t.Fatal(err)
		}
		if reply != "claw 回复" {
			t.Fatalf("未知唤醒词回退应确定性取 claw，实际 %q", reply)
		}
	}
}

func TestAgentRoutingLegacySingle(t *testing.T) {
	deps, _, _, _ := testDeps(t)
	deps.Agent = fakeAgentServer(t, "单大脑回复")
	r := New(deps, nil)
	reply, err := r.Route(context.Background(), "lx06-1", "今天天气怎么样")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "单大脑回复" {
		t.Fatalf("未配多 Agent 表时应走单大脑，实际 %q", reply)
	}
}

// ---- P1-27：SetHA 热换 ----

func TestSetHAHotSwap(t *testing.T) {
	deps, _, _, f1 := testDeps(t)
	entities := map[string]string{"客厅灯": "light.living_room"}
	r := New(deps, entities)
	ctx := context.Background()
	// 旧 HA：客厅灯 off → 开灯调 turn_on。
	if _, err := r.Route(ctx, "lx06-1", "开客厅灯"); err != nil {
		t.Fatal(err)
	}
	f1.mu.Lock()
	if len(f1.calls) != 1 {
		t.Fatalf("旧 HA 应调 1 次服务，实际 %v", f1.calls)
	}
	f1.mu.Unlock()
	// 新 HA：客厅灯 on → 热换后应直接说已经开着，不再调服务。
	f2, hac2 := startFakeHA(t)
	f2.states["light.living_room"] = "on"
	r.SetHA(hac2)
	reply, err := r.Route(ctx, "lx06-1", "开客厅灯")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "已经是开着的了") {
		t.Fatalf("热换后应读新 HA 状态，实际 %q", reply)
	}
	f1.mu.Lock()
	if len(f1.calls) != 1 {
		t.Fatalf("热换后旧 HA 不应再被调，实际 %v", f1.calls)
	}
	f1.mu.Unlock()
	f2.mu.Lock()
	if len(f2.calls) != 0 {
		t.Fatalf("已开的灯不应调服务，实际 %v", f2.calls)
	}
	f2.mu.Unlock()
}

// ---- P2-50 顺带：歌手/歌名拆分 ----

// splitArtistTrack 已随本地音源搬到 music 包
// （LocalSource 搜歌用），测试见 music/source_test.go 的
// TestSplitArtistTrack。
