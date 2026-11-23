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
	"strings"
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
	// closeOnce 保护 close(done)（H-6）：看门狗探活失败与关停可能
	// 并发调 Close()，旧代码 select-default 双关即 panic
	//（close of closed channel）。
	closeOnce sync.Once

	// writeMu 串行化同一连接上的所有写操作（P1-21）：gorilla websocket
	// 禁止同一连接并发写；call() 与 SubscribeEvents() 分属不同 goroutine
	// 时旧代码直接并发 WriteJSON，-race 下已复现竞态。
	writeMu sync.Mutex
}

// writeJSON 是唯一写入口：持写锁 + 5 秒写 deadline。
func (c *Client) writeJSON(v any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return c.conn.WriteJSON(v)
}

// Dial 连上 HA（ws://host:8123/api/websocket），完成 auth 握手，并启动读循环。
// P1-5：两次 ReadMessage 都带 timeout 上限（无 deadline 时对端只建连不说话
// 即永久挂起）；空 token 直接报错，不发一次 auth 再失败。
func Dial(url, token string, timeout time.Duration) (*Client, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("ha: HASS_TOKEN 为空，拒绝无鉴权连接（HA 须配长期访问令牌）")
	}
	dialer := websocket.Dialer{HandshakeTimeout: timeout}
	conn, _, err := dialer.Dial(url, nil)
	if err != nil {
		return nil, err
	}
	c := &Client{url: url, token: token, conn: conn, pending: map[int]chan resultMsg{}, done: make(chan struct{})}
	// 等 auth_required，再发 auth。timeout<=0 表示不设读 deadline
	//（测试调用 Dial(..., 0) 时沿用旧行为；生产传 5s）。
	setHandshakeDeadline := func() {
		if timeout > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(timeout))
		}
	}
	clearHandshakeDeadline := func() {
		if timeout > 0 {
			_ = conn.SetReadDeadline(time.Time{})
		}
	}
	setHandshakeDeadline()
	_, msg, err := conn.ReadMessage()
	clearHandshakeDeadline()
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
	if err := c.writeJSON(map[string]any{"type": "auth", "access_token": token}); err != nil {
		conn.Close()
		return nil, err
	}
	setHandshakeDeadline()
	_, msg, err = conn.ReadMessage()
	clearHandshakeDeadline()
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

// Close 关闭连接。并发双关安全（H-6）：sync.Once 保证
// close(c.done) 只执行一次；底层连接的 Close 可重复调。
func (c *Client) Close() error {
	c.closeOnce.Do(func() { close(c.done) })
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
	if err := c.writeJSON(map[string]any{
		"id": id, "type": "subscribe_events", "event_type": eventType,
	}); err != nil {
		return err
	}
	c.mu.Lock()
	c.handlers = append(c.handlers, handler)
	c.mu.Unlock()
	return nil
}

// call 发一条请求并等 result（10s 超时）。
func (c *Client) call(msgType string, extra map[string]any) (json.RawMessage, error) {
	return c.callWithTimeout(msgType, extra, 10*time.Second)
}

// callWithTimeout 发一条请求并等 result，超时可配。
// 读回验证（ReadState）用 2s 短超时：HA 挂了时快失败，不拖语音回复。
func (c *Client) callWithTimeout(msgType string, extra map[string]any, timeout time.Duration) (json.RawMessage, error) {
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
	if err := c.writeJSON(m); err != nil {
		// P2-16（顺带）：写失败即删 pending，否则等待者永远等不到
		// result、pending 表永久泄漏。
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
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
	case <-time.After(timeout):
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
	// Attributes 是实体属性（P2-49：场景实体的 attributes.entity_id
	// 列出场景内含的实体，供执行前做 domain allowlist 校验）。
	Attributes map[string]any `json:"attributes,omitempty"`
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

// GetSceneEntityIDs 取一个场景实体内含的实体 ID 列表（P2-49）：
// 读 scene.<name> 的 attributes.entity_id。场景不存在或属性缺失
// 时返回错误——调用方应 fail-closed（拒绝执行），不要猜场景内容。
func (c *Client) GetSceneEntityIDs(sceneEntityID string) ([]string, error) {
	states, err := c.GetStates()
	if err != nil {
		return nil, err
	}
	for _, s := range states {
		if s.EntityID != sceneEntityID {
			continue
		}
		raw, ok := s.Attributes["entity_id"]
		if !ok {
			return nil, fmt.Errorf("ha: 场景 %s 没有 entity_id 属性", sceneEntityID)
		}
		list, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("ha: 场景 %s 的 entity_id 属性格式异常", sceneEntityID)
		}
		out := make([]string, 0, len(list))
		for _, v := range list {
			id, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("ha: 场景 %s 的 entity_id 里有非字符串项", sceneEntityID)
			}
			out = append(out, id)
		}
		return out, nil
	}
	return nil, fmt.Errorf("ha: 实体 %s 不存在", sceneEntityID)
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

// ---- 读回验证（2026-11-23，设计思路借鉴 home-mate 的 tool/home.rs
// set_entity，独立重新实现，未复制其代码）----
//
// CallService 成功只表示 HA 接受了请求（service_accepted ≠ confirmed）。
// 调服务后必须读回状态比对，才知道设备是不是真的动了。三种结果严格区分：
//   - 确认成功：读回与目标一致；
//   - 读回不一致：读到了，但状态没变（设备没执行、HA 没下发）；
//   - 读不到状态：实体不存在或 HA 故障——不谎称失败（动作可能已发生，
//     只是没法确认），也不谎称成功。
//
// 本客户端是 WebSocket，没有 REST /api/states/{entity}：读回走 get_states
// 全量再过滤，等价于 home-mate 的 GET /api/states/{entity}，只是传输层不同。

const (
	// ReadbackAttempts 是读回验证的最多读取次数。
	ReadbackAttempts = 3
	// ReadbackInterval 是两次读取之间的间隔（状态下发需要一点时间）。
	ReadbackInterval = 150 * time.Millisecond
	// ReadbackTimeout 是每次读取的超时（HA 挂了时快失败，不拖语音回复）。
	ReadbackTimeout = 2 * time.Second
)

// ReadbackOutcome 是一次读回验证的结果。
type ReadbackOutcome int

const (
	// ReadbackConfirmed 读回与目标一致（或场景状态已变化）。
	ReadbackConfirmed ReadbackOutcome = iota
	// ReadbackMismatch 读到了状态，但与目标不一致。
	ReadbackMismatch
	// ReadbackUnknown 读不到状态（实体不存在、HA 故障或超时）。
	ReadbackUnknown
)

// ReadState 读一个实体的当前 state（2s 超时；实体不存在时返回错误）。
func (c *Client) ReadState(entityID string) (string, error) {
	raw, err := c.callWithTimeout("get_states", nil, ReadbackTimeout)
	if err != nil {
		return "", err
	}
	var states []State
	if err := json.Unmarshal(raw, &states); err != nil {
		return "", err
	}
	for _, s := range states {
		if s.EntityID == entityID {
			return s.State, nil
		}
	}
	return "", fmt.Errorf("ha: 实体 %s 不存在", entityID)
}

// VerifyState 读回验证：最多读 ReadbackAttempts 次、间隔 ReadbackInterval，
// 比对目标 state。第一次读到目标即确认；读到但 3 次都不符即不一致；
// 读不到（实体不存在/HA 故障/超时）即未知——不等读完 3 次，HA 不可达时
// 重试没有意义，立刻返回。
func (c *Client) VerifyState(entityID, want string) ReadbackOutcome {
	return c.VerifyStatePred(entityID, func(state string) bool { return state == want })
}

// VerifyStatePred 读回验证的判定函数版：不是所有 domain 的 state 都能用
// 精确字符串比对——climate 开着时 state 是运行模式（heat/cool/auto…）
// 而不是 "on"，cover 开着时是 "open"/"opening"。调用方按 domain 给判定，
// 重试/超时语义与 VerifyState 完全相同。
func (c *Client) VerifyStatePred(entityID string, want func(state string) bool) ReadbackOutcome {
	for i := 0; i < ReadbackAttempts; i++ {
		state, err := c.ReadState(entityID)
		if err != nil {
			return ReadbackUnknown
		}
		if want(state) {
			return ReadbackConfirmed
		}
		if i < ReadbackAttempts-1 {
			time.Sleep(ReadbackInterval)
		}
	}
	return ReadbackMismatch
}

// VerifyStateChanged 场景类读回验证：读到状态与调用前不同即确认
// （HA 场景实体的 state 是上次触发时间，触发后即变化）。
// 3 次都相同即不一致；读不到即未知。
func (c *Client) VerifyStateChanged(entityID, before string) ReadbackOutcome {
	for i := 0; i < ReadbackAttempts; i++ {
		state, err := c.ReadState(entityID)
		if err != nil {
			return ReadbackUnknown
		}
		if state != before {
			return ReadbackConfirmed
		}
		if i < ReadbackAttempts-1 {
			time.Sleep(ReadbackInterval)
		}
	}
	return ReadbackMismatch
}

// CallServiceWithReadback 调服务并读回验证：调服务失败时返回错误
// （outcome 为 ReadbackUnknown）；调服务成功时返回读回结果。
// 调用方按 outcome 组织回复，禁止把“HA 接受了”说成“设备动了”。
func (c *Client) CallServiceWithReadback(domain, service, entityID, want string) (ReadbackOutcome, error) {
	if err := c.CallService(domain, service, entityID); err != nil {
		return ReadbackUnknown, err
	}
	return c.VerifyState(entityID, want), nil
}

// CallServiceWithReadbackPred 是 CallServiceWithReadback 的判定函数版，
// 供 state 不能精确比对的 domain 用（climate/cover，见 router 的
// haControlTarget）。重试/超时语义与 CallServiceWithReadback 相同。
func (c *Client) CallServiceWithReadbackPred(domain, service, entityID string, want func(state string) bool) (ReadbackOutcome, error) {
	if err := c.CallService(domain, service, entityID); err != nil {
		return ReadbackUnknown, err
	}
	return c.VerifyStatePred(entityID, want), nil
}
