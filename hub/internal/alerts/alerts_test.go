package alerts

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/huajun/open-xiaoai-next/hub/internal/ha"
)

// ---- 播报文本：含事件类型与位置 ----

func TestAnnouncementText(t *testing.T) {
	cases := []struct {
		entity string
		want   string
	}{
		{"binary_sensor.kitchen_leak", "注意，厨房漏水报警了，请检查一下。"},
		{"binary_sensor.living_room_smoke", "注意，客厅烟雾报警了，请检查一下。"},
		{"binary_sensor.bathroom_water", "注意，浴室水浸报警了，请检查一下。"},
		{"binary_sensor.basement_water_sensor", "注意，地下室水浸报警了，请检查一下。"},
	}
	for _, c := range cases {
		got, ok := AnnouncementText(c.entity)
		if !ok {
			t.Fatalf("%s 应生成播报文本", c.entity)
		}
		if got != c.want {
			t.Fatalf("%s 播报文本应为 %q，实际 %q", c.entity, c.want, got)
		}
	}
	// 非安全类实体：不播报。
	if _, ok := AnnouncementText("light.living_room"); ok {
		t.Fatal("light.living_room 不应生成播报文本")
	}
	if _, ok := AnnouncementText("binary_sensor.front_door"); ok {
		t.Fatal("binary_sensor.front_door（门磁）不应生成播报文本")
	}
}

// ---- ShouldAnnounce 判定：真 HA 信封形状 + 扁平形状 ----

// nestedEvent 造真 HA 的 state_changed 信封：event.data.entity_id / new_state。
func nestedEvent(entityID, state string) map[string]any {
	return map[string]any{
		"data": map[string]any{
			"entity_id": entityID,
			"new_state": map[string]any{"state": state},
		},
	}
}

// flatEvent 造扁平形状（旧 main.go 接线用的形状，兼容用）。
func flatEvent(entityID, state string) map[string]any {
	return map[string]any{
		"entity_id": entityID,
		"new_state": map[string]any{"state": state},
	}
}

func TestShouldAnnounceSafetyOn(t *testing.T) {
	day := time.Date(2026, 11, 17, 14, 0, 0, 0, time.Local)
	for _, ev := range []map[string]any{
		nestedEvent("binary_sensor.kitchen_leak", "on"),
		flatEvent("binary_sensor.kitchen_leak", "on"),
	} {
		text, ok := ShouldAnnounce(ev, "23:00-07:00", day)
		if !ok {
			t.Fatalf("漏水变 on、白天应播报，事件 %+v", ev)
		}
		if !strings.Contains(text, "漏水") || !strings.Contains(text, "厨房") {
			t.Fatalf("播报内容应含事件类型“漏水”与位置“厨房”，实际 %q", text)
		}
	}
	// 烟雾 / 水浸同样播报，文本含各自类型与位置。
	text, ok := ShouldAnnounce(nestedEvent("binary_sensor.living_room_smoke", "on"), "23:00-07:00", day)
	if !ok || !strings.Contains(text, "烟雾") || !strings.Contains(text, "客厅") {
		t.Fatalf("烟雾播报应含“烟雾”与“客厅”，实际 ok=%v text=%q", ok, text)
	}
	text, ok = ShouldAnnounce(nestedEvent("binary_sensor.bathroom_water", "on"), "23:00-07:00", day)
	if !ok || !strings.Contains(text, "水浸") || !strings.Contains(text, "浴室") {
		t.Fatalf("水浸播报应含“水浸”与“浴室”，实际 ok=%v text=%q", ok, text)
	}
}

func TestShouldAnnounceSkipsDoNotDisturb(t *testing.T) {
	night := time.Date(2026, 11, 17, 2, 0, 0, 0, time.Local)
	ev := nestedEvent("binary_sensor.kitchen_leak", "on")
	if text, ok := ShouldAnnounce(ev, "23:00-07:00", night); ok {
		t.Fatalf("勿扰时段内不应播报，实际 %q", text)
	}
	// 勿扰边界：07:00 整点已出勿扰，应播报；23:00 整点进入勿扰，不播报。
	seven := time.Date(2026, 11, 17, 7, 0, 0, 0, time.Local)
	if _, ok := ShouldAnnounce(ev, "23:00-07:00", seven); !ok {
		t.Fatal("07:00 已出勿扰时段，应播报")
	}
	twentyThree := time.Date(2026, 11, 17, 23, 0, 0, 0, time.Local)
	if _, ok := ShouldAnnounce(ev, "23:00-07:00", twentyThree); ok {
		t.Fatal("23:00 已进入勿扰时段，不应播报")
	}
}

func TestShouldAnnounceIgnoresNonSafety(t *testing.T) {
	day := time.Date(2026, 11, 17, 14, 0, 0, 0, time.Local)
	cases := []map[string]any{
		nestedEvent("light.living_room", "on"),           // 非安全类实体变 on
		nestedEvent("binary_sensor.front_door", "on"),    // 门磁，不是漏水/烟雾/水浸
		nestedEvent("binary_sensor.kitchen_leak", "off"), // 安全类但变 off（恢复）
		nestedEvent("binary_sensor.kitchen_leak", "unavailable"),
		{"data": map[string]any{"entity_id": "binary_sensor.kitchen_leak"}}, // 无 new_state
		{}, // 空事件
	}
	for i, ev := range cases {
		if text, ok := ShouldAnnounce(ev, "23:00-07:00", day); ok {
			t.Fatalf("用例 %d 不应播报，实际 %q（事件 %+v）", i, text, ev)
		}
	}
}

func TestInDoNotDisturb(t *testing.T) {
	at := func(h, m int) time.Time {
		return time.Date(2026, 11, 17, h, m, 0, 0, time.Local)
	}
	cases := []struct {
		spec string
		now  time.Time
		want bool
	}{
		{"23:00-07:00", at(23, 30), true},  // 跨午夜：后半段
		{"23:00-07:00", at(2, 0), true},    // 跨午夜：前半段
		{"23:00-07:00", at(6, 59), true},   // 结束前一分钟
		{"23:00-07:00", at(7, 0), false},   // 结束整点（左闭右开）
		{"23:00-07:00", at(12, 0), false},  // 白天
		{"23:00-07:00", at(22, 59), false}, // 开始前一分钟
		{"09:00-18:00", at(12, 0), true},   // 不跨午夜：中间
		{"09:00-18:00", at(18, 0), false},  // 不跨午夜：结束整点
		{"09:00-18:00", at(8, 59), false},  // 不跨午夜：开始前
		{"", at(2, 0), false},              // 未配勿扰：永不跳过
		{"bad", at(2, 0), false},           // 格式错：按不在算
		{"23:00", at(2, 0), false},         // 格式错：按不在算
	}
	for _, c := range cases {
		if got := InDoNotDisturb(c.spec, c.now); got != c.want {
			t.Fatalf("InDoNotDisturb(%q, %s) 应为 %v，实际 %v", c.spec, c.now.Format("15:04"), c.want, got)
		}
	}
}

// ---- Watcher + 假 HA WebSocket 服务：推真 state_changed 事件 ----

// fakeHA 是最小假 HA：auth 握手 + 订阅回包 + 服务端可主动推事件。
// 写操作统一加锁（服务端回包与测试推送可能并发写同一连接）。
type fakeHA struct {
	t       *testing.T
	conn    *websocket.Conn
	url     string
	writeMu sync.Mutex
}

func startFakeHA(t *testing.T) *fakeHA {
	f := &fakeHA{t: t}
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		f.writeMu.Lock()
		f.conn = c
		f.writeMu.Unlock()
		defer c.Close()
		f.write(map[string]any{"type": "auth_required"})
		var auth map[string]any
		if err := c.ReadJSON(&auth); err != nil {
			return
		}
		f.write(map[string]any{"type": "auth_ok"})
		for {
			var msg map[string]any
			if err := c.ReadJSON(&msg); err != nil {
				return
			}
			id, _ := msg["id"].(float64)
			// 只回包，不做业务。
			f.write(map[string]any{"id": int(id), "type": "result", "success": true, "result": nil})
		}
	}))
	t.Cleanup(srv.Close)
	f.url = "ws" + strings.TrimPrefix(srv.URL, "http")
	return f
}

func (f *fakeHA) write(msg map[string]any) {
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	_ = f.conn.WriteJSON(msg)
}

// pushStateChanged 按真 HA 信封推一条 state_changed 事件。
func (f *fakeHA) pushStateChanged(entityID, state string) {
	f.write(map[string]any{
		"type": "event",
		"event": map[string]any{
			"event_type": "state_changed",
			"data": map[string]any{
				"entity_id": entityID,
				"new_state": map[string]any{"state": state},
			},
		},
	})
}

func TestWatcherWithFakeHA(t *testing.T) {
	f := startFakeHA(t)
	c, err := ha.Dial(f.url, "tok", 5*time.Second)
	if err != nil {
		t.Fatalf("Dial 假 HA: %v", err)
	}
	defer c.Close()

	spoken := make(chan string, 8)
	w := New(c, "23:00-07:00", func(text string) { spoken <- text })
	// 时钟注入：白天 14:00。
	day := time.Date(2026, 11, 17, 14, 0, 0, 0, time.Local)
	w.SetNow(func() time.Time { return day })
	if err := w.Start(); err != nil {
		t.Fatalf("Watcher.Start: %v", err)
	}

	expectSpeak := func(wantSub ...string) {
		t.Helper()
		select {
		case text := <-spoken:
			for _, s := range wantSub {
				if !strings.Contains(text, s) {
					t.Fatalf("播报内容应含 %q，实际 %q", s, text)
				}
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("3s 内应收到播报（期望含 %v）", wantSub)
		}
	}
	expectSilence := func() {
		t.Helper()
		select {
		case text := <-spoken:
			t.Fatalf("不应播报，却收到 %q", text)
		case <-time.After(300 * time.Millisecond):
		}
	}

	// 1. 厨房漏水变 on（白天）→ 播报，含类型与位置。
	f.pushStateChanged("binary_sensor.kitchen_leak", "on")
	expectSpeak("漏水", "厨房")
	// 2. 客厅烟雾变 on（白天）→ 播报。
	f.pushStateChanged("binary_sensor.living_room_smoke", "on")
	expectSpeak("烟雾", "客厅")
	// 3. 非安全类实体变 on → 不播报。
	f.pushStateChanged("light.living_room", "on")
	expectSilence()
	// 4. 安全类变 off（恢复）→ 不播报。
	f.pushStateChanged("binary_sensor.kitchen_leak", "off")
	expectSilence()
	// 5. 勿扰时段（切时钟到 02:00）漏水变 on → 不播报。
	w.SetNow(func() time.Time { return time.Date(2026, 11, 17, 2, 0, 0, 0, time.Local) })
	f.pushStateChanged("binary_sensor.bathroom_water", "on")
	expectSilence()
	// 6. 出勿扰（时钟回 14:00）再推 → 播报。
	w.SetNow(func() time.Time { return day })
	f.pushStateChanged("binary_sensor.bathroom_water", "on")
	expectSpeak("水浸", "浴室")
}
