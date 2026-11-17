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
	t      *testing.T
	mu     sync.Mutex
	calls  []map[string]any // 收到的 call_service
	fired  []string         // 收到的 fire_event 类型
	states map[string]string
	conn   *websocket.Conn
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
		_ = c.WriteJSON(map[string]any{"type": "auth_required", "ha_version": "2026.1"})
		// 2. 读 auth。
		var auth map[string]any
		if err := c.ReadJSON(&auth); err != nil {
			return
		}
		if auth["type"] != "auth" || auth["access_token"] != "hass-token" {
			_ = c.WriteJSON(map[string]any{"type": "auth_invalid", "message": "bad token"})
			return
		}
		_ = c.WriteJSON(map[string]any{"type": "auth_ok", "ha_version": "2026.1"})
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
		_ = f.conn.WriteJSON(map[string]any{"id": int(id), "type": "result", "success": true, "result": result})
	}
	switch msg["type"] {
	case "get_states":
		var out []map[string]any
		f.mu.Lock()
		for eid, st := range f.states {
			out = append(out, map[string]any{"entity_id": eid, "state": st})
		}
		f.mu.Unlock()
		reply(out)
	case "call_service":
		f.mu.Lock()
		f.calls = append(f.calls, msg)
		// 真改状态：turn_on → on，turn_off → off。
		eid, _ := msg["target"].(map[string]any)["entity_id"].(string)
		if eid == "" {
			eid, _ = msg["entity_id"].(string)
		}
		switch msg["service"] {
		case "turn_on":
			f.states[eid] = "on"
		case "turn_off":
			f.states[eid] = "off"
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
		_ = f.conn.WriteJSON(map[string]any{"id": int(id), "type": "result", "success": false, "error": map[string]any{"code": "unknown", "message": "unknown type"}})
	}
}

// pushEvent 从服务端主动推一条 state_changed 事件。
func (f *fakeHA) pushEvent() {
	_ = f.conn.WriteJSON(map[string]any{
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

	if err := c.FireEvent("open_xiaoai_emergency", map[string]any{"device": "lx06-1"}); err != nil {
		t.Fatalf("FireEvent: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.fired) != 1 || f.fired[0] != "open_xiaoai_emergency" {
		t.Fatalf("服务端应收到 1 次 fire_event，实际 %v", f.fired)
	}
}

func TestBadTokenRejected(t *testing.T) {
	_, url := startFakeHA(t)
	if _, err := Dial(url, "wrong-token", 5*time.Second); err == nil {
		t.Fatal("错误 token 应被 HA 拒绝")
	}
}
