package ha

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// 假 HA：走真 WebSocket，按 HA 协议做 auth 握手 + get_states/call_service/subscribe_events/fire_event。
type fakeHA struct {
	t       *testing.T
	mu      sync.Mutex
	writeMu sync.Mutex       // gorilla websocket 要求同一连接最多一个并发写者；handle goroutine 与 pushEvent（测试 goroutine）会并发写
	calls   []map[string]any // 收到的 call_service
	fired   []string         // 收到的 fire_event 类型
	states  map[string]string
	conn    *websocket.Conn
	// stubborn 为真时 call_service 只记录、不改状态（读回不一致测试用）。
	stubborn bool
	// hangService 为真时 call_service 永远不回（P1-7 总预算测试用）。
	hangService bool
	// attrs 是 get_states 里随实体一起回的属性（P1-15 ReadEntity 测试用）。
	attrs map[string]map[string]any
	// slowGetStates 让 get_states 回复前拖延（超时测试用）。
	slowGetStates time.Duration
	// flipAfterReads/flipEntity/flipTo：第几次 get_states 后把指定实体
	// 翻成目标状态（读回重试测试用；0 表示不翻转）。
	flipAfterReads int
	flipEntity     string
	flipTo         string
	getStatesCount int
}

// writeJSON 是本替身唯一的写入口（持 writeMu），防并发写 panic。
func (f *fakeHA) writeJSON(v any) error {
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	return f.conn.WriteJSON(v)
}

func startFakeHA(t *testing.T) (*fakeHA, string) {
	f := &fakeHA{
		t:      t,
		states: map[string]string{"light.living_room": "off", "light.bedroom": "on"},
	}
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		f.conn = c
		defer c.Close()
		// 1. 发 auth_required。
		_ = f.writeJSON(map[string]any{"type": "auth_required", "ha_version": "2026.1"})
		// 2. 读 auth。
		var auth map[string]any
		if err := c.ReadJSON(&auth); err != nil {
			return
		}
		if auth["type"] != "auth" || auth["access_token"] != "hass-token" {
			_ = f.writeJSON(map[string]any{"type": "auth_invalid", "message": "bad token"})
			return
		}
		_ = f.writeJSON(map[string]any{"type": "auth_ok", "ha_version": "2026.1"})
		// 3. 处理请求 + 推送事件。
		for {
			var msg map[string]any
			if err := c.ReadJSON(&msg); err != nil {
				return
			}
			f.handle(msg)
		}
	}))
	t.Cleanup(srv.Close)
	return f, "ws" + strings.TrimPrefix(srv.URL, "http")
}

func (f *fakeHA) handle(msg map[string]any) {
	id, _ := msg["id"].(float64)
	reply := func(result any) {
		_ = f.writeJSON(map[string]any{"id": int(id), "type": "result", "success": true, "result": result})
	}
	switch msg["type"] {
	case "get_states":
		f.mu.Lock()
		f.getStatesCount++
		if f.flipAfterReads > 0 && f.getStatesCount >= f.flipAfterReads && f.flipEntity != "" {
			f.states[f.flipEntity] = f.flipTo
		}
		delay := f.slowGetStates
		f.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		f.mu.Lock()
		var out []map[string]any
		for eid, st := range f.states {
			item := map[string]any{"entity_id": eid, "state": st}
			if a := f.attrs[eid]; a != nil {
				item["attributes"] = a
			}
			out = append(out, item)
		}
		f.mu.Unlock()
		reply(out)
	case "call_service":
		f.mu.Lock()
		f.calls = append(f.calls, msg)
		hang := f.hangService
		f.mu.Unlock()
		if hang {
			// 永远不回：客户端应在自己的超时/预算内报错，而不是等服务端。
			time.Sleep(30 * time.Second)
			return
		}
		f.mu.Lock()
		// 真改状态：turn_on → on，turn_off → off；stubborn 时只记录不改。
		eid, _ := msg["target"].(map[string]any)["entity_id"].(string)
		if eid == "" {
			eid, _ = msg["entity_id"].(string)
		}
		if !f.stubborn {
			// 按 domain 建模真实 state：climate 开着是运行模式
			// （heat/cool…）不是 "on"，cover 用 open/close_cover，
			// state 是 "open"/"closed"。拿 "on" 一律写死会测不到
			// M1 的 bug（2026-11-23 审查 M2）。
			switch msg["service"] {
			case "turn_on":
				if strings.HasPrefix(eid, "climate.") {
					f.states[eid] = "heat"
				} else {
					f.states[eid] = "on"
				}
			case "turn_off":
				f.states[eid] = "off"
			case "open_cover":
				f.states[eid] = "open"
			case "close_cover":
				f.states[eid] = "closed"
			}
		}
		f.mu.Unlock()
		reply([]any{})
	case "subscribe_events":
		reply(nil)
	case "fire_event":
		f.mu.Lock()
		f.fired = append(f.fired, msg["event_type"].(string))
		f.mu.Unlock()
		reply(nil)
	default:
		_ = f.writeJSON(map[string]any{"id": int(id), "type": "result", "success": false, "error": map[string]any{"code": "unknown", "message": "unknown type"}})
	}
}

// pushEvent 从服务端主动推一条 state_changed 事件。
func (f *fakeHA) pushEvent() {
	_ = f.writeJSON(map[string]any{
		"type": "event",
		"event": map[string]any{
			"event_type": "state_changed",
			"data": map[string]any{
				"entity_id": "light.living_room",
				"new_state": map[string]any{"state": "on"},
			},
		},
	})
}

func TestAuthAndGetStates(t *testing.T) {
	f, url := startFakeHA(t)
	c, err := Dial(url, "hass-token", 5*time.Second)
	if err != nil {
		t.Fatalf("Dial 假 HA 失败: %v", err)
	}
	defer c.Close()

	states, err := c.GetStates()
	if err != nil {
		t.Fatalf("GetStates: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("GetStates 应返回 2 个实体，实际 %d", len(states))
	}
	st, err := c.GetEntityState("light.living_room")
	if err != nil {
		t.Fatalf("GetEntityState: %v", err)
	}
	if st != "off" {
		t.Fatalf("客厅灯初始应为 off，实际 %q", st)
	}
	_ = f
}

func TestCallServiceChangesState(t *testing.T) {
	_, url := startFakeHA(t)
	c, err := Dial(url, "hass-token", 5*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	if err := c.CallService("light", "turn_on", "light.living_room"); err != nil {
		t.Fatalf("CallService: %v", err)
	}
	st, err := c.GetEntityState("light.living_room")
	if err != nil {
		t.Fatalf("GetEntityState: %v", err)
	}
	if st != "on" {
		t.Fatalf("turn_on 后客厅灯应为 on，实际 %q", st)
	}
}

func TestSubscribeEventsReceivesPush(t *testing.T) {
	f, url := startFakeHA(t)
	c, err := Dial(url, "hass-token", 5*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	got := make(chan string, 1)
	if err := c.SubscribeEvents("state_changed", func(ev map[string]any) {
		data, _ := ev["data"].(map[string]any)
		eid, _ := data["entity_id"].(string)
		got <- eid
	}); err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	f.pushEvent()
	select {
	case eid := <-got:
		if eid != "light.living_room" {
			t.Fatalf("事件 entity_id 应为 light.living_room，实际 %q", eid)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("3s 内未收到服务端推送的 state_changed 事件")
	}
}

func TestFireEvent(t *testing.T) {
	f, url := startFakeHA(t)
	c, err := Dial(url, "hass-token", 5*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	if err := c.FireEvent("open_xiaoai_test", map[string]any{"device": "lx06-1"}); err != nil {
		t.Fatalf("FireEvent: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.fired) != 1 || f.fired[0] != "open_xiaoai_test" {
		t.Fatalf("服务端应收到 1 次 fire_event，实际 %v", f.fired)
	}
}

func TestBadTokenRejected(t *testing.T) {
	_, url := startFakeHA(t)
	if _, err := Dial(url, "wrong-token", 5*time.Second); err == nil {
		t.Fatal("错误 token 应被 HA 拒绝")
	}
}
