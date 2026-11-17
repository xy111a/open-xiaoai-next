// Package ha 是 Home Assistant WebSocket 客户端。
// Hub 只做三件事：查状态、调服务、订阅事件（Phase 4 主动播报）。
// 场景/联动/定时由 HA 自己跑，不依赖 LLM 在线。
//
// 规则（PLAN.md §4.4）：
//   - 家电命令先查 HA 再答，不编数字；
//   - 高风险设备（门锁/燃气/热水器）不进场景，必须当面直接说；
//   - 空调/窗帘进联动时必须播报一声。
//
// 并发模型：Dial 后启动唯一读循环，按 id 把 result 分发给等待的 call，
// 把 event 分发给订阅者。调用方绝不要自己读连接。
package ha

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// resultMsg 是一条 result 响应。
type resultMsg struct {
	Success bool            `json:"success"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Client 是一个 HA 连接。
type Client struct {
	url   string
	token string
	conn  *websocket.Conn

	mu       sync.Mutex
	seq      int
	pending  map[int]chan resultMsg
	handlers []func(event map[string]any)
	done     chan struct{}
}

// Dial 连上 HA（ws://host:8123/api/websocket），完成 auth 握手，并启动读循环。
func Dial(url, token string, timeout time.Duration) (*Client, error) {
	dialer := websocket.Dialer{HandshakeTimeout: timeout}
	conn, _, err := dialer.Dial(url, nil)
	if err != nil {
		return nil, err
	}
	c := &Client{url: url, token: token, conn: conn, pending: map[int]chan resultMsg{}, done: make(chan struct{})}
	// 等 auth_required，再发 auth。
	_, msg, err := conn.ReadMessage()
	if err != nil {
		conn.Close()
		return nil, err
	}
	var hello struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(msg, &hello); err != nil || hello.Type != "auth_required" {
		conn.Close()
		return nil, fmt.Errorf("ha: 期望 auth_required，实际 %s", string(msg))
	}
	if err := conn.WriteJSON(map[string]any{"type": "auth", "access_token": token}); err != nil {
		conn.Close()
		return nil, err
	}
	_, msg, err = conn.ReadMessage()
	if err != nil {
		conn.Close()
		return nil, err
	}
	var authResp struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(msg, &authResp); err != nil {
		conn.Close()
		return nil, err
	}
	if authResp.Type != "auth_ok" {
		conn.Close()
		return nil, fmt.Errorf("ha: 认证失败: %s", authResp.Message)
	}
	go c.readLoop()
	return c, nil
}

// Close 关闭连接。
func (c *Client) Close() error {
	select {
	case <-c.done:
	default:
		close(c.done)
	}
	return c.conn.Close()
}

func (c *Client) readLoop() {
	for {
		_, msg, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		var base struct {
			ID    int             `json:"id"`
			Type  string          `json:"type"`
			Event json.RawMessage `json:"event"`
		}
		if err := json.Unmarshal(msg, &base); err != nil {
			continue
		}
		switch base.Type {
		case "result":
			var r resultMsg
			if err := json.Unmarshal(msg, &r); err != nil {
				continue
			}
			c.mu.Lock()
			ch, ok := c.pending[base.ID]
			if ok {
				delete(c.pending, base.ID)
			}
			c.mu.Unlock()
			if ok {
				select {
				case ch <- r:
				default:
				}
			}
		case "event":
			var ev map[string]any
			if err := json.Unmarshal(base.Event, &ev); err != nil {
				continue
			}
			c.mu.Lock()
			handlers := append([]func(map[string]any){}, c.handlers...)
			c.mu.Unlock()
			for _, h := range handlers {
				h(ev)
			}
		}
	}
}

// SubscribeEvents 订阅 HA 事件（event_type 如 "state_changed"），handler 在读循环里被调。
func (c *Client) SubscribeEvents(eventType string, handler func(event map[string]any)) error {
	c.mu.Lock()
	c.seq++
	id := c.seq
	c.mu.Unlock()
	if err := c.conn.WriteJSON(map[string]any{
		"id": id, "type": "subscribe_events", "event_type": eventType,
	}); err != nil {
		return err
	}
	c.mu.Lock()
	c.handlers = append(c.handlers, handler)
	c.mu.Unlock()
	return nil
}

// call 发一条请求并等 result。
func (c *Client) call(msgType string, extra map[string]any) (json.RawMessage, error) {
	c.mu.Lock()
	c.seq++
	id := c.seq
	ch := make(chan resultMsg, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	m := map[string]any{"id": id, "type": msgType}
	for k, v := range extra {
		m[k] = v
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := c.conn.WriteJSON(m); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		if !r.Success {
			if r.Error != nil {
				return nil, fmt.Errorf("ha: %s: %s", r.Error.Code, r.Error.Message)
			}
			return nil, fmt.Errorf("ha: 调用失败")
		}
		return r.Result, nil
	case <-time.After(10 * time.Second):
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("ha: 调用超时")
	}
}

// State 是一条实体状态。
type State struct {
	EntityID string `json:"entity_id"`
	State    string `json:"state"`
}

// GetStates 取全部实体状态。
func (c *Client) GetStates() ([]State, error) {
	raw, err := c.call("get_states", nil)
	if err != nil {
		return nil, err
	}
	var states []State
	if err := json.Unmarshal(raw, &states); err != nil {
		return nil, err
	}
	return states, nil
}

// GetEntityState 查一个实体的 state，例如 light.living_room → "on"。
func (c *Client) GetEntityState(entityID string) (string, error) {
	states, err := c.GetStates()
	if err != nil {
		return "", err
	}
	for _, s := range states {
		if s.EntityID == entityID {
			return s.State, nil
		}
	}
	return "", fmt.Errorf("ha: 实体 %s 不存在", entityID)
}

// CallService 调一个服务，例如 light.turn_off + entity_id。
func (c *Client) CallService(domain, service, entityID string) error {
	_, err := c.call("call_service", map[string]any{
		"domain":  domain,
		"service": service,
		"target":  map[string]any{"entity_id": entityID},
	})
	return err
}

// FireEvent 触发一个 HA 事件（紧急呼叫通知等用）。
func (c *Client) FireEvent(eventType string, data map[string]any) error {
	_, err := c.call("fire_event", map[string]any{
		"event_type": eventType,
		"event_data": data,
	})
	return err
}
