package transport

// P1-3 / P1-11 / P1-12 / P1-44 回归测试。

import (
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/huajun/open-xiaoai-next/hub/internal/registry"
)

func helloConn(t *testing.T, url, model, token string) *websocket.Conn {
	t.Helper()
	c := dial(t, url)
	if err := c.WriteJSON(map[string]any{"type": "hello", "model": model, "ram_mb": 256, "version": "1.0", "token": token}); err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := c.ReadJSON(&resp); err != nil {
		t.Fatalf("读 hello_ok: %v", err)
	}
	if resp["type"] != "hello_ok" {
		t.Fatalf("应收到 hello_ok，实际 %v", resp)
	}
	return c
}

// TestPingKeepsIdleConnectionAlive：Hub 主动 Ping + 客户端回 Pong 时，
// 空闲连接在 3 倍 PongWait 后仍活着（旧代码 60 秒一次性 deadline 必断）。
func TestPingKeepsIdleConnectionAlive(t *testing.T) {
	reg := registry.New()
	h := &stubHandler{wakes: make(chan Wake, 4), claims: make(chan RoomClaim, 4), status: make(chan Status, 4), disc: make(chan string, 4)}
	srv := New("secret", reg, h)
	srv.PingInterval = 200 * time.Millisecond
	srv.PongWait = 500 * time.Millisecond
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { _ = srv.Serve(ln, nil) }()
	url := "ws://" + ln.Addr().String() + "/ws"

	c := helloConn(t, url, "lx06", "secret")
	defer c.Close()
	// 客户端读循环（gorilla 默认 PingHandler 在读到 Ping 时自动回 Pong）。
	readErr := make(chan error, 1)
	go func() {
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				readErr <- err
				return
			}
		}
	}()
	time.Sleep(1600 * time.Millisecond) // 3 倍 PongWait，旧代码早已断线
	if _, ok := srv.Conn("lx06-127-0-0-1"); !ok {
		t.Fatal("Ping 保活下空闲连接 1.6s 后仍应在 map 里")
	}
	select {
	case err := <-readErr:
		t.Fatalf("客户端读循环不应出错: %v", err)
	default:
	}
}

// TestIdleConnectionWithoutPingDies：Hub 不发 Ping 时，
// 空闲连接在 PongWait 后被读 deadline 关闭（对照组）。
func TestIdleConnectionWithoutPingDies(t *testing.T) {
	reg := registry.New()
	h := &stubHandler{wakes: make(chan Wake, 4), claims: make(chan RoomClaim, 4), status: make(chan Status, 4), disc: make(chan string, 4)}
	srv := New("secret", reg, h)
	srv.PingInterval = 10 * time.Second // 本轮不发 Ping
	srv.PongWait = 300 * time.Millisecond
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { _ = srv.Serve(ln, nil) }()
	url := "ws://" + ln.Addr().String() + "/ws"

	c := helloConn(t, url, "lx06", "secret")
	defer c.Close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := srv.Conn("lx06-127-0-0-1"); !ok {
			return // 已按预期断开
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("不发 Ping 的空闲连接应在 PongWait 后断开，实际仍在")
}

// TestReconnectDoesNotDeleteNewConn：同设备重连顶掉旧连接后，
// 旧连接的清理不得误删新连接、不得把设备标离线（P1-12）。
func TestReconnectDoesNotDeleteNewConn(t *testing.T) {
	reg := registry.New()
	h := &stubHandler{wakes: make(chan Wake, 4), claims: make(chan RoomClaim, 4), status: make(chan Status, 4), disc: make(chan string, 4)}
	srv := New("secret", reg, h)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { _ = srv.Serve(ln, nil) }()
	url := "ws://" + ln.Addr().String() + "/ws"

	c1 := helloConn(t, url, "lx06", "secret")
	defer c1.Close()
	c2 := helloConn(t, url, "lx06", "secret")
	defer c2.Close()
	// 等旧连接的清理跑完：新连接必须仍在，且服务端下发能到达新连接。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, ok := srv.Conn("lx06-127-0-0-1")
		if !ok {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		_ = conn.SendJSON(map[string]string{"type": "ping-test"})
		_ = c2.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		_, data, err := c2.ReadMessage()
		if err == nil && strings.Contains(string(data), "ping-test") {
			if d, ok := reg.Get("lx06-127-0-0-1"); !ok || !d.Online {
				t.Fatalf("新连接活着时设备应标在线，实际 %+v", d)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("重连后新连接应仍在且能收到服务端下发")
}

// TestRegisterFingerprint：指纹白名单内的 register 通过（记在线 +
// register_ok），白名单外/不一致的被拒并断开（P1-44）。
func TestRegisterFingerprint(t *testing.T) {
	reg := registry.New()
	h := &stubHandler{wakes: make(chan Wake, 4), claims: make(chan RoomClaim, 4), status: make(chan Status, 4), disc: make(chan string, 4)}
	srv := New("secret", reg, h)
	fp := "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	srv.SetFingerprints([]string{fp})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { _ = srv.Serve(ln, nil) }()
	url := "ws://" + ln.Addr().String() + "/ws"

	// 白名单内（大小写/冒号归一化后匹配）→ register_ok。
	c := helloConn(t, url, "lx06", "secret")
	defer c.Close()
	if err := c.WriteJSON(map[string]any{"type": "register", "device_id": "lx06-127-0-0-1",
		"model": "lx06", "room": "客厅", "fingerprint": "AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99"}); err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := c.ReadJSON(&resp); err != nil {
		t.Fatalf("读 register_ok: %v", err)
	}
	if resp["type"] != "register_ok" {
		t.Fatalf("白名单内 register 应回 register_ok，实际 %v", resp)
	}
	if !srv.IsRegistered("lx06-127-0-0-1") {
		t.Fatal("register 通过后 IsRegistered 应为 true")
	}
	before := srv.LastHeartbeat("lx06-127-0-0-1")
	if before.IsZero() {
		t.Fatal("register 通过后应记心跳时间")
	}
	// heartbeat 续期。
	time.Sleep(10 * time.Millisecond)
	if err := c.WriteJSON(map[string]any{"type": "heartbeat"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if !srv.LastHeartbeat("lx06-127-0-0-1").After(before) {
		t.Fatal("heartbeat 后 LastHeartbeat 应更新")
	}

	// 白名单外 → register_reject 并断开（用 oh2p 拿另一个 deviceID）。
	c2 := helloConn(t, url, "oh2p", "secret")
	defer c2.Close()
	if err := c2.WriteJSON(map[string]any{"type": "register", "device_id": "oh2p-127-0-0-1",
		"model": "oh2p", "room": "卧室", "fingerprint": "00deadbeef"}); err != nil {
		t.Fatal(err)
	}
	if err := c2.ReadJSON(&resp); err != nil {
		t.Fatalf("读 register_reject: %v", err)
	}
	if resp["type"] != "register_reject" {
		t.Fatalf("白名单外 register 应回 register_reject，实际 %v", resp)
	}
	if srv.IsRegistered("oh2p-127-0-0-1") {
		t.Fatal("白名单外 register 不应记为已注册")
	}
	_ = c2.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := c2.ReadMessage(); err == nil {
		t.Fatal("register 被拒后连接应已关闭")
	}

	// device_id 与连接不一致 → 拒绝。
	c3 := helloConn(t, url, "lx06", "secret")
	defer c3.Close()
	// c3 与 c 是同一 deviceID，后连的会顶掉前面的；这里只验拒绝文案。
	_ = c3.WriteJSON(map[string]any{"type": "register", "device_id": "lx06-9-9-9-9",
		"model": "lx06", "room": "客厅", "fingerprint": fp})
	if err := c3.ReadJSON(&resp); err != nil {
		t.Fatalf("读 register_reject: %v", err)
	}
	if resp["type"] != "register_reject" {
		t.Fatalf("device_id 不一致应回 register_reject，实际 %v", resp)
	}
}

// TestRegisterWithoutWhitelist：未配指纹白名单时注册一律拒绝（fail closed）。
func TestRegisterWithoutWhitelist(t *testing.T) {
	_, url, _ := startServer(t, "secret")
	c := helloConn(t, url, "lx06", "secret")
	defer c.Close()
	_ = c.WriteJSON(map[string]any{"type": "register", "device_id": "lx06-127-0-0-1",
		"model": "lx06", "room": "客厅", "fingerprint": "aabbcc"})
	var resp map[string]any
	if err := c.ReadJSON(&resp); err != nil {
		t.Fatalf("读 register_reject: %v", err)
	}
	if resp["type"] != "register_reject" {
		t.Fatalf("未配白名单时 register 应被拒，实际 %v", resp)
	}
}

// TestHelloRateLimit：同一 IP 连续 5 次 hello 失败后，第 6 次建连即被拒（429）。
func TestHelloRateLimit(t *testing.T) {
	_, url, _ := startServer(t, "secret")
	for i := 0; i < 5; i++ {
		c := dial(t, url)
		_ = c.WriteJSON(map[string]any{"type": "hello", "model": "lx06", "ram_mb": 256, "version": "1.0", "token": "wrong"})
		var resp map[string]any
		if err := c.ReadJSON(&resp); err != nil || resp["type"] != "hello_reject" {
			c.Close()
			t.Fatalf("第 %d 次错误 hello 应收到 hello_reject，实际 %v / %v", i+1, resp, err)
		}
		c.Close()
	}
	// 第 6 次：握手即被 429 拒绝，Dial 失败。
	d := &websocket.Dialer{HandshakeTimeout: 2 * time.Second}
	if _, _, err := d.Dial(url, nil); err == nil {
		t.Fatal("5 次 hello 失败后第 6 次建连应被 429 拒绝")
	}
}

// TestReadLimit：超 64KB 的单条消息即断线（P1-3）。
func TestReadLimit(t *testing.T) {
	srv, url, _ := startServer(t, "secret")
	c := helloConn(t, url, "lx06", "secret")
	defer c.Close()
	if err := c.WriteJSON(map[string]any{"type": "status", "volume": 50, "playing": false, "version": strings.Repeat("x", 70*1024)}); err != nil {
		// 服务器可能在写完成前就因超限断开了连接（RST），
		// 这同样是“超限即断线”的表现，不算失败；下面仍会验证连接已断。
		if !strings.Contains(err.Error(), "connection reset") && !strings.Contains(err.Error(), "broken pipe") {
			t.Fatalf("写超大消息: %v", err)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := srv.Conn("lx06-127-0-0-1"); !ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("超 64KB 消息后连接应被断开")
}

// TestOriginRejected：公网 Origin 的握手应被拒绝（P1-3）。
func TestOriginRejected(t *testing.T) {
	_, url, _ := startServer(t, "secret")
	d := &websocket.Dialer{HandshakeTimeout: 2 * time.Second}
	hdr := http.Header{"Origin": []string{"http://evil.example.com"}}
	if _, _, err := d.Dial(url, hdr); err == nil {
		t.Fatal("公网 Origin 的握手应被拒绝")
	}
	// 空 Origin（音箱客户端）仍放行。
	if _, _, err := d.Dial(url, nil); err != nil {
		t.Fatalf("空 Origin 应放行，实际: %v", err)
	}
}
