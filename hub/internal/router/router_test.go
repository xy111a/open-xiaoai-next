package router

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/huajun/open-xiaoai-next/hub/internal/agent"
	"github.com/huajun/open-xiaoai-next/hub/internal/ha"
	"github.com/huajun/open-xiaoai-next/hub/internal/music"
	"github.com/huajun/open-xiaoai-next/hub/internal/reminders"
)

func websocketUpgrader() websocket.Upgrader { return websocket.Upgrader{} }

// ---- 假 MPD（最小版：握手 + 每条命令回 OK，记录命令）----

type fakeMPD struct {
	t    *testing.T
	ln   net.Listener
	mu   sync.Mutex
	cmds []string
}

func startFakeMPD(t *testing.T) *fakeMPD {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeMPD{t: t, ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = c.Write([]byte("OK MPD 0.23.0\n"))
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					f.mu.Lock()
					f.cmds = append(f.cmds, strings.TrimSpace(line))
					f.mu.Unlock()
					_, _ = c.Write([]byte("OK\n"))
				}
			}(conn)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeMPD) client(t *testing.T) *music.MPDClient {
	c, err := music.DialMPD(f.ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("DialMPD: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func (f *fakeMPD) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.cmds))
	copy(out, f.cmds)
	return out
}

// ---- 假 HA（最小版：auth + get_states + call_service，状态真实变化）----

type fakeHA struct {
	mu     sync.Mutex
	states map[string]string
	calls  []string // "domain/service/entity_id"
}

func startFakeHA(t *testing.T) (*fakeHA, *ha.Client) {
	f := &fakeHA{states: map[string]string{"light.living_room": "off", "light.bedroom": "on"}}
	up := websocketUpgrader()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.WriteJSON(map[string]any{"type": "auth_required"})
		var auth map[string]any
		if err := c.ReadJSON(&auth); err != nil {
			return
		}
		_ = c.WriteJSON(map[string]any{"type": "auth_ok"})
		for {
			var msg map[string]any
			if err := c.ReadJSON(&msg); err != nil {
				return
			}
			id, _ := msg["id"].(float64)
			switch msg["type"] {
			case "get_states":
				f.mu.Lock()
				var out []map[string]any
				for eid, st := range f.states {
					out = append(out, map[string]any{"entity_id": eid, "state": st})
				}
				f.mu.Unlock()
				_ = c.WriteJSON(map[string]any{"id": int(id), "type": "result", "success": true, "result": out})
			case "call_service":
				f.mu.Lock()
				eid, _ := msg["target"].(map[string]any)["entity_id"].(string)
				f.calls = append(f.calls, msg["domain"].(string)+"/"+msg["service"].(string)+"/"+eid)
				if msg["service"] == "turn_on" {
					f.states[eid] = "on"
				} else if msg["service"] == "turn_off" {
					f.states[eid] = "off"
				}
				f.mu.Unlock()
				_ = c.WriteJSON(map[string]any{"id": int(id), "type": "result", "success": true, "result": []any{}})
			default:
				_ = c.WriteJSON(map[string]any{"id": int(id), "type": "result", "success": true, "result": nil})
			}
		}
	}))
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	client, err := ha.Dial(url, "tok", 0)
	if err != nil {
		t.Fatalf("Dial 假 HA: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return f, client
}

func testDeps(t *testing.T) (Deps, *reminders.Store, *Memory, *fakeHA) {
	dir := t.TempDir()
	rem := reminders.New(filepath.Join(dir, "r.json"), nil)
	mem := NewMemory(filepath.Join(dir, "m.json"))
	f, hac := startFakeHA(t)
	deps := Deps{
		HA: hac, Reminders: rem, Memory: mem,
		RoomOfDevice: func(deviceID string) string { return "客厅" },
		Notify:       func(ctx context.Context, text string) error { return nil },
	}
	return deps, rem, mem, f
}

func TestEmergency(t *testing.T) {
	deps, _, _, _ := testDeps(t)
	r := New(deps, nil)
	reply, err := r.Route(context.Background(), "lx06-1", "救命")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "呼叫急救") {
		t.Fatalf("紧急呼叫回复应含“呼叫急救”，实际 %q", reply)
	}
}

// ---- 紧急呼叫取消状态机（FR-15，任务 C）----

// countingNotifyDeps 返回带计数 Notify 的 Deps（每次触发只应调一次）。
func countingNotifyDeps(t *testing.T) (Deps, *int) {
	deps, _, _, _ := testDeps(t)
	n := 0
	deps.Notify = func(ctx context.Context, text string) error {
		n++
		return nil
	}
	return deps, &n
}

func TestEmergencyCancel(t *testing.T) {
	deps, notified := countingNotifyDeps(t)
	r := New(deps, nil)
	// 触发 → Notify 恰好 1 次，状态为进行中。
	reply, err := r.Route(context.Background(), "lx06-1", "救命")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "呼叫急救") {
		t.Fatalf("触发回复应含“呼叫急救”，实际 %q", reply)
	}
	if *notified != 1 {
		t.Fatalf("触发后 Notify 应调 1 次，实际 %d", *notified)
	}
	if !r.EmergencyActive("lx06-1") {
		t.Fatal("触发后 EmergencyActive 应为 true")
	}
	// 取消 → 回复确认取消，Notify 不再增加，状态清理。
	reply, err = r.Route(context.Background(), "lx06-1", "取消呼叫")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "已经取消了本次紧急呼叫") {
		t.Fatalf("取消回复不对: %q", reply)
	}
	if *notified != 1 {
		t.Fatalf("取消不应再调 Notify，实际 %d 次", *notified)
	}
	if r.EmergencyActive("lx06-1") {
		t.Fatal("取消后 EmergencyActive 应为 false")
	}
	// 无进行中呼叫时再取消 → 明说没有，不报错、不 Notify。
	reply, err = r.Route(context.Background(), "lx06-1", "取消呼叫")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "没有正在进行的紧急呼叫") {
		t.Fatalf("无呼叫时取消应明说，实际 %q", reply)
	}
	if *notified != 1 {
		t.Fatalf("无呼叫取消不应调 Notify，实际 %d 次", *notified)
	}
	// 取消后可重新触发 → Notify 再次调用。
	reply, err = r.Route(context.Background(), "lx06-1", "打 120")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "呼叫急救") {
		t.Fatalf("取消后重新触发回复不对: %q", reply)
	}
	if *notified != 2 {
		t.Fatalf("重新触发后 Notify 应为 2 次，实际 %d", *notified)
	}
}

func TestEmergencyTimeout(t *testing.T) {
	deps, notified := countingNotifyDeps(t)
	r := New(deps, nil)
	if _, err := r.Route(context.Background(), "lx06-1", "救命"); err != nil {
		t.Fatal(err)
	}
	if *notified != 1 || !r.EmergencyActive("lx06-1") {
		t.Fatalf("触发后应有 1 次 Notify 且状态进行中，实际 notified=%d active=%v", *notified, r.EmergencyActive("lx06-1"))
	}
	// 30s 内扫荡（窗口 60s）：不应清理。
	if n := r.SweepEmergency(time.Now().Add(30 * time.Second)); n != 0 {
		t.Fatalf("30s 内扫荡不应清理，实际清了 %d 个", n)
	}
	if !r.EmergencyActive("lx06-1") {
		t.Fatal("30s 内呼叫应仍进行中")
	}
	// 61s 后扫荡：清理掉。
	if n := r.SweepEmergency(time.Now().Add(61 * time.Second)); n != 1 {
		t.Fatalf("61s 后扫荡应清理 1 个，实际 %d", n)
	}
	if r.EmergencyActive("lx06-1") {
		t.Fatal("超时后 EmergencyActive 应为 false")
	}
	// 超时清理后重新触发 → 允许再次 Notify。
	if _, err := r.Route(context.Background(), "lx06-1", "救命"); err != nil {
		t.Fatal(err)
	}
	if *notified != 2 {
		t.Fatalf("超时后重新触发 Notify 应为 2 次，实际 %d", *notified)
	}
}

func TestEmergencyRepeatDoesNotRenotify(t *testing.T) {
	deps, notified := countingNotifyDeps(t)
	r := New(deps, nil)
	if _, err := r.Route(context.Background(), "lx06-1", "救命"); err != nil {
		t.Fatal(err)
	}
	if *notified != 1 {
		t.Fatalf("首次触发 Notify 应为 1 次，实际 %d", *notified)
	}
	// 同设备重复触发：不重复 Notify，回复说明已经在呼叫。
	reply, err := r.Route(context.Background(), "lx06-1", "打 120")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "已经在呼叫急救了") {
		t.Fatalf("重复触发回复应说明已经在呼叫，实际 %q", reply)
	}
	if *notified != 1 {
		t.Fatalf("重复触发不应再调 Notify，实际 %d 次", *notified)
	}
	// 不同设备触发：各自独立，Notify 再调一次。
	if _, err := r.Route(context.Background(), "oh2p-1", "叫救护车"); err != nil {
		t.Fatal(err)
	}
	if *notified != 2 {
		t.Fatalf("另一设备触发 Notify 应为 2 次，实际 %d", *notified)
	}
	// 别台设备取消：只能取消自己的，lx06-1 的呼叫不受影响。
	reply, err = r.Route(context.Background(), "oh2p-1", "取消呼叫")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "已经取消了本次紧急呼叫") {
		t.Fatalf("oh2p-1 取消回复不对: %q", reply)
	}
	if !r.EmergencyActive("lx06-1") {
		t.Fatal("oh2p-1 取消后 lx06-1 的呼叫应仍进行中")
	}
	if r.EmergencyActive("oh2p-1") {
		t.Fatal("oh2p-1 取消后自己的呼叫应已清理")
	}
}

func TestReminderDailyAndCancel(t *testing.T) {
	deps, rem, _, _ := testDeps(t)
	r := New(deps, nil)
	reply, err := r.Route(context.Background(), "lx06-1", "每天 8 点提醒我吃药")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "每天 08:00 提醒你吃药") {
		t.Fatalf("每日提醒回复不对: %q", reply)
	}
	if len(rem.List()) != 1 || rem.List()[0].Text != "吃药" {
		t.Fatalf("提醒应存 1 条“吃药”，实际 %+v", rem.List())
	}
	reply, err = r.Route(context.Background(), "lx06-1", "取消吃药提醒")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "已取消 1 条") {
		t.Fatalf("取消回复不对: %q", reply)
	}
	if len(rem.List()) != 0 {
		t.Fatalf("取消后应无提醒，实际 %+v", rem.List())
	}
}

func TestReminderInterval(t *testing.T) {
	deps, rem, _, _ := testDeps(t)
	r := New(deps, nil)
	reply, err := r.Route(context.Background(), "lx06-1", "每 30 分钟提醒我喝水")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "每 30 分钟提醒你喝水") {
		t.Fatalf("间隔提醒回复不对: %q", reply)
	}
	if len(rem.List()) != 1 || rem.List()[0].Every != 30 {
		t.Fatalf("间隔提醒应存 Every=30，实际 %+v", rem.List())
	}
}

func TestMemory(t *testing.T) {
	deps, _, mem, _ := testDeps(t)
	r := New(deps, nil)
	reply, err := r.Route(context.Background(), "lx06-1", "记住空调 26 度")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "记住了：空调 26 度") {
		t.Fatalf("记忆回复不对: %q", reply)
	}
	if got := mem.All("lx06-1"); len(got) != 1 || got[0] != "空调 26 度" {
		t.Fatalf("记忆应存 1 条，实际 %v", got)
	}
}

func TestHomeAssistantTurnOnAndAlreadyOn(t *testing.T) {
	deps, _, _, f := testDeps(t)
	entities := map[string]string{"客厅灯": "light.living_room", "卧室灯": "light.bedroom"}
	r := New(deps, entities)
	// 客厅灯当前 off → 开灯应调 turn_on。
	reply, err := r.Route(context.Background(), "lx06-1", "开客厅灯")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "客厅灯开了") {
		t.Fatalf("开灯回复不对: %q", reply)
	}
	f.mu.Lock()
	if len(f.calls) != 1 || f.calls[0] != "light/turn_on/light.living_room" {
		t.Fatalf("应调 1 次 light/turn_on/light.living_room，实际 %v", f.calls)
	}
	f.mu.Unlock()
	// 卧室灯当前 on → 再开应直接说已经开着，不重复调服务。
	reply, err = r.Route(context.Background(), "lx06-1", "开卧室灯")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "已经是开着的了") {
		t.Fatalf("已开的灯应直接告知，实际 %q", reply)
	}
	f.mu.Lock()
	if len(f.calls) != 1 {
		t.Fatalf("已开的灯不应再调服务，实际调用 %v", f.calls)
	}
	f.mu.Unlock()
	// 关卧室灯 → turn_off。
	reply, err = r.Route(context.Background(), "lx06-1", "关卧室灯")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "卧室灯关了") {
		t.Fatalf("关灯回复不对: %q", reply)
	}
}

func TestHomeAssistantRefusesLock(t *testing.T) {
	deps, _, _, f := testDeps(t)
	entities := map[string]string{"门锁": "lock.front_door"}
	r := New(deps, entities)
	reply, err := r.Route(context.Background(), "lx06-1", "开门锁")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "不能语音直接控制") {
		t.Fatalf("门锁应被拒绝，实际 %q", reply)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != 0 {
		t.Fatalf("门锁被拒时不应调任何服务，实际 %v", f.calls)
	}
}

func TestMusicViaMA(t *testing.T) {
	deps, _, _, _ := testDeps(t)
	// 假 MA：track_by_name 成功。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"message_id":1,"result":{"uri":"library://track/1"}}`)
	}))
	defer srv.Close()
	deps.MA = music.NewMA(srv.URL, "")
	deps.QueueID = "p1"
	r := New(deps, nil)
	reply, err := r.Route(context.Background(), "lx06-1", "播放青花瓷")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "正在播放青花瓷") {
		t.Fatalf("MA 点歌回复不对: %q", reply)
	}
}

func TestMusicFallbackNeteaseMPD(t *testing.T) {
	deps, _, _, _ := testDeps(t)
	// MA 不可用 → 网易云搜歌 + MPD 直播 URL。
	ne := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/search") {
			fmt.Fprint(w, `{"code":200,"result":{"songs":[{"id":12345,"name":"青花瓷","artists":[{"name":"周杰伦"}]}]}}`)
			return
		}
		fmt.Fprint(w, `{"code":200,"data":[{"id":12345,"url":"http://music.example.com/12345.mp3"}]}`)
	}))
	defer ne.Close()
	deps.Netease = music.NewNetease(ne.URL)

	mpdLn := startFakeMPD(t)
	deps.MPD = mpdLn.client(t)

	r := New(deps, nil)
	reply, err := r.Route(context.Background(), "lx06-1", "播放青花瓷")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "正在播放周杰伦的青花瓷") {
		t.Fatalf("降级点歌回复不对: %q", reply)
	}
	if got := mpdLn.commands(); len(got) != 3 || got[1] != "add http://music.example.com/12345.mp3" {
		t.Fatalf("MPD 应收到 clear/add/play，实际 %v", got)
	}
}

func TestAgentFallback(t *testing.T) {
	deps, _, _, _ := testDeps(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"今天晴，22 度。"}}]}`)
	}))
	defer srv.Close()
	deps.Agent = agent.New(srv.URL+"/v1", "m", "")
	r := New(deps, nil)
	reply, err := r.Route(context.Background(), "lx06-1", "今天天气怎么样")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "今天晴，22 度。" {
		t.Fatalf("Agent 兜底回复不对: %q", reply)
	}
}

func TestNoAgentMessage(t *testing.T) {
	deps, _, _, _ := testDeps(t)
	r := New(deps, nil)
	reply, err := r.Route(context.Background(), "lx06-1", "随便聊聊")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "Agent 未配置") {
		t.Fatalf("无 Agent 时应明说，实际 %q", reply)
	}
}
