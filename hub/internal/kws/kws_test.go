package kws

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeSidecar 是测试用边车：WS /v1/kws + HTTP /health。
// 注意：这是假边车，喂的是合成 PCM（正弦波），测的是接线不是真人唤醒率。
type fakeSidecar struct {
	ln  net.Listener
	srv *http.Server

	mu       sync.Mutex
	conns    map[string]*websocket.Conn
	hellos   map[string]map[string]any
	pcmBytes map[string]int // 每设备收到的 PCM 字节数
	pcmFrame map[string]int // 每设备收到的二进制帧数
}

func newFakeSidecar(t *testing.T) *fakeSidecar {
	t.Helper()
	f := &fakeSidecar{
		conns:    map[string]*websocket.Conn{},
		hellos:   map[string]map[string]any{},
		pcmBytes: map[string]int{},
		pcmFrame: map[string]int{},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.ln = ln
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/kws", f.handleWS)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	f.srv = &http.Server{Handler: mux}
	go func() { _ = f.srv.Serve(ln) }()
	t.Cleanup(func() { _ = f.srv.Close() })
	return f
}

func (f *fakeSidecar) url() string {
	return "ws://" + f.ln.Addr().String() + "/v1/kws"
}

func (f *fakeSidecar) handleWS(w http.ResponseWriter, r *http.Request) {
	up := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	// 首条必须是 hello。
	_, first, err := conn.ReadMessage()
	if err != nil {
		_ = conn.Close()
		return
	}
	var hello map[string]any
	if err := json.Unmarshal(first, &hello); err != nil || hello["type"] != "hello" {
		_ = conn.Close()
		return
	}
	deviceID, _ := hello["device_id"].(string)
	f.mu.Lock()
	if old, ok := f.conns[deviceID]; ok {
		_ = old.Close()
	}
	f.conns[deviceID] = conn
	f.hellos[deviceID] = hello
	f.mu.Unlock()
	_ = conn.WriteJSON(map[string]string{"type": "ready"})
	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if mt == websocket.BinaryMessage {
			f.mu.Lock()
			f.pcmBytes[deviceID] += len(data)
			f.pcmFrame[deviceID]++
			f.mu.Unlock()
		}
	}
}

// waitHello 等待某设备完成 hello 握手。
func (f *fakeSidecar) waitHello(t *testing.T, deviceID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		_, ok := f.hellos[deviceID]
		f.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("超时：假边车未收到设备 %s 的 hello", deviceID)
}

// waitPCM 等待某设备累计收到至少 n 字节 PCM。
func (f *fakeSidecar) waitPCM(t *testing.T, deviceID string, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		got := f.pcmBytes[deviceID]
		f.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.mu.Lock()
	got := f.pcmBytes[deviceID]
	f.mu.Unlock()
	t.Fatalf("超时：设备 %s 的 PCM 只收到 %d 字节，想要至少 %d", deviceID, got, n)
}

// sendWake 给某设备发一条 wake 事件。
func (f *fakeSidecar) sendWake(t *testing.T, deviceID, keyword string, rms float64) {
	t.Helper()
	f.mu.Lock()
	conn, ok := f.conns[deviceID]
	f.mu.Unlock()
	if !ok {
		t.Fatalf("假边车没有设备 %s 的连接，无法发 wake", deviceID)
	}
	if err := conn.WriteJSON(map[string]any{
		"type": "wake", "device_id": deviceID, "keyword": keyword,
		"rms": rms, "timestamp_ms": time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("发 wake 失败: %v", err)
	}
}

// closeConn 模拟边车断线：关掉某设备的连接。
func (f *fakeSidecar) closeConn(deviceID string) {
	f.mu.Lock()
	conn, ok := f.conns[deviceID]
	if ok {
		delete(f.conns, deviceID)
		delete(f.hellos, deviceID)
	}
	f.mu.Unlock()
	if ok {
		_ = conn.Close()
	}
}

func testOpts() Options {
	return Options{
		Keywords:         []string{"你好小李"},
		QueueFrames:      50,
		DialTimeout:      2 * time.Second,
		ReconnectInitial: 50 * time.Millisecond,
		ReconnectMax:     200 * time.Millisecond,
		HealthInterval:   30 * time.Second,
	}
}

// 合成一帧 PCM：320 点常量波形（合成语音，非真人唤醒率）。
func synthFrame() []int16 {
	frame := make([]int16, 320)
	for i := range frame {
		frame[i] = 12000
	}
	return frame
}

// sendFrames 连发 n 帧合成 PCM。
func sendFrames(c *Client, deviceID string, n int) {
	for i := 0; i < n; i++ {
		c.SendPCM(deviceID, synthFrame())
	}
}

func TestHelloAndPCMForward(t *testing.T) {
	f := newFakeSidecar(t)
	c, err := New(f.url(), testOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	sendFrames(c, "lx06-1", 10) // 10 帧 × 640 字节
	f.waitHello(t, "lx06-1", 3*time.Second)
	f.waitPCM(t, "lx06-1", 10*640, 3*time.Second)

	f.mu.Lock()
	hello := f.hellos["lx06-1"]
	frames := f.pcmFrame["lx06-1"]
	f.mu.Unlock()
	if hello["type"] != "hello" || hello["device_id"] != "lx06-1" {
		t.Fatalf("hello 内容不对: %v", hello)
	}
	if sr, _ := hello["sample_rate"].(float64); sr != 16000 {
		t.Fatalf("hello sample_rate 应为 16000，实际 %v", hello["sample_rate"])
	}
	if frames != 10 {
		t.Fatalf("应收到 10 帧 PCM，实际 %d 帧", frames)
	}
}

func TestWakeEventDelivery(t *testing.T) {
	f := newFakeSidecar(t)
	c, err := New(f.url(), testOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	sendFrames(c, "lx06-1", 1)
	f.waitHello(t, "lx06-1", 3*time.Second)
	f.sendWake(t, "lx06-1", "你好小李", 0.42)

	select {
	case ev := <-c.Events():
		if ev.DeviceID != "lx06-1" || ev.Keyword != "你好小李" || ev.RMS != 0.42 {
			t.Fatalf("wake 事件内容不对: %+v", ev)
		}
		if ev.TimestampMs <= 0 {
			t.Fatalf("wake 事件应带 timestamp_ms，实际 %+v", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("超时：未收到边车 wake 事件")
	}
}

func TestUnknownKeywordFiltered(t *testing.T) {
	f := newFakeSidecar(t)
	c, err := New(f.url(), testOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	sendFrames(c, "lx06-1", 1)
	f.waitHello(t, "lx06-1", 3*time.Second)
	f.sendWake(t, "lx06-1", "你好龙虾", 0.5)

	select {
	case ev := <-c.Events():
		t.Fatalf("未配置的唤醒词 %q 不应进事件 channel，实际收到 %+v", ev.Keyword, ev)
	case <-time.After(300 * time.Millisecond):
	}
	// 已配置的词仍能通过。
	f.sendWake(t, "lx06-1", "你好小李", 0.42)
	select {
	case ev := <-c.Events():
		if ev.Keyword != "你好小李" {
			t.Fatalf("应收到“你好小李”，实际 %+v", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("超时：未收到已配置唤醒词的 wake 事件")
	}
}

func TestErrorMessageDoesNotBreakStream(t *testing.T) {
	f := newFakeSidecar(t)
	c, err := New(f.url(), testOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	sendFrames(c, "lx06-1", 1)
	f.waitHello(t, "lx06-1", 3*time.Second)
	f.mu.Lock()
	conn := f.conns["lx06-1"]
	f.mu.Unlock()
	if err := conn.WriteJSON(map[string]string{"type": "error", "message": "model not loaded"}); err != nil {
		t.Fatal(err)
	}
	// error 之后流不断：再发 wake 仍能收到。
	time.Sleep(100 * time.Millisecond)
	f.sendWake(t, "lx06-1", "你好小李", 0.42)
	select {
	case <-c.Events():
	case <-time.After(3 * time.Second):
		t.Fatal("error 消息后 wake 事件应仍能收到")
	}
}

func TestReconnectAfterDrop(t *testing.T) {
	f := newFakeSidecar(t)
	c, err := New(f.url(), testOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	sendFrames(c, "lx06-1", 2)
	f.waitHello(t, "lx06-1", 3*time.Second)
	f.waitPCM(t, "lx06-1", 2*640, 3*time.Second)

	// 模拟边车断线。
	f.closeConn("lx06-1")
	// 断线期间发的音频可能丢在途帧（正常现象，不作断言）；关键是重连后
	// 流能恢复：等新 hello，再发新帧，计数必须继续增长。
	sendFrames(c, "lx06-1", 1)
	f.waitHello(t, "lx06-1", 5*time.Second)
	f.mu.Lock()
	before := f.pcmFrame["lx06-1"]
	f.mu.Unlock()
	sendFrames(c, "lx06-1", 2)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		got := f.pcmFrame["lx06-1"]
		f.mu.Unlock()
		if got >= before+2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.mu.Lock()
	got := f.pcmFrame["lx06-1"]
	f.mu.Unlock()
	if got < before+2 {
		t.Fatalf("重连后新帧未恢复：重连时 %d 帧，发 2 帧后 %d 帧", before, got)
	}

	// 重连后 wake 事件仍能走通。
	f.sendWake(t, "lx06-1", "你好小李", 0.42)
	select {
	case <-c.Events():
	case <-time.After(3 * time.Second):
		t.Fatal("重连后未收到 wake 事件")
	}
}

func TestBackpressureDropsOldestWithoutBlocking(t *testing.T) {
	// 指向没人监听的端口：永远连不上，队列必须满后丢最旧帧，不能堵 SendPCM。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := ln.Addr().String()
	_ = ln.Close()

	c, err := New("ws://"+deadAddr+"/v1/kws", Options{
		Keywords:         []string{"你好小李"},
		QueueFrames:      5,
		DialTimeout:      200 * time.Millisecond,
		ReconnectInitial: 50 * time.Millisecond,
		ReconnectMax:     100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	start := time.Now()
	for i := 0; i < 200; i++ {
		sendFrames(c, "lx06-1", 1)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("SendPCM 被阻塞了：200 次调用耗时 %v", elapsed)
	}
	if got := c.DroppedFrames("lx06-1"); got == 0 {
		t.Fatal("边车不可用时应丢帧并计数，实际丢 0 帧")
	}
	st := c.Status()
	if !st.Down {
		t.Fatalf("边车不可用时 Status().Down 应为 true，实际 %+v", st)
	}
	if st.Streams != 1 || st.Connected != 0 {
		t.Fatalf("Status 流计数不对: %+v", st)
	}
}

func TestRemoveDeviceCleansStream(t *testing.T) {
	f := newFakeSidecar(t)
	c, err := New(f.url(), testOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	sendFrames(c, "lx06-1", 1)
	f.waitHello(t, "lx06-1", 3*time.Second)
	if st := c.Status(); st.Streams != 1 {
		t.Fatalf("发音频后应有 1 条流，实际 %+v", st)
	}
	c.RemoveDevice("lx06-1")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st := c.Status(); st.Streams == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("RemoveDevice 后流状态应被清理")
}

func TestCheckHealth(t *testing.T) {
	f := newFakeSidecar(t)
	c, err := New(f.url(), testOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.CheckHealth(context.Background()); err != nil {
		t.Fatalf("假边车 /health 应返回 200：%v", err)
	}
	if st := c.Status(); st.Down {
		t.Fatalf("健康检查通过时 Down 应为 false，实际 %+v", st)
	}

	// 死地址：健康检查应报错且 Down 为 true。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := ln.Addr().String()
	_ = ln.Close()
	c2, err := New("ws://"+deadAddr+"/v1/kws", testOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if err := c2.CheckHealth(context.Background()); err == nil {
		t.Fatal("死地址健康检查应报错")
	}
	if st := c2.Status(); !st.Down {
		t.Fatalf("健康检查失败时 Down 应为 true，实际 %+v", st)
	}
}

func TestHealthURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ws://kws-sidecar:10400/v1/kws", "http://kws-sidecar:10400/health"},
		{"wss://kws-sidecar:10400/v1/kws", "https://kws-sidecar:10400/health"},
		{"ws://127.0.0.1:12345/v1/kws?x=1", "http://127.0.0.1:12345/health"},
	}
	for _, tc := range cases {
		got, err := HealthURL(tc.in)
		if err != nil {
			t.Fatalf("HealthURL(%q) 报错: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("HealthURL(%q) = %q，想要 %q", tc.in, got, tc.want)
		}
	}
	if _, err := HealthURL("http://127.0.0.1:1234/v1/kws"); err == nil {
		t.Fatal("http scheme 的 HealthURL 应报错")
	}
}

func TestNewRejectsBadURL(t *testing.T) {
	bad := []string{
		"",
		"not-a-url",
		"http://127.0.0.1:10400/v1/kws",         // 必须是 ws/wss
		"ws://203.0.113.1:10400/v1/kws",         // 公网 host
		"ws://example.com/v1/kws",               // 公网域名
		"ws://user:pass@127.0.0.1:10400/v1/kws", // 带 userinfo
		"ws://127.0.0.1:10400",                  // 无路径
		"ws://127.0.0.1:10400/",                 // 只有根路径
	}
	for _, u := range bad {
		if _, err := New(u, testOpts()); err == nil {
			t.Fatalf("New(%q) 应报错，实际通过", u)
		}
	}
	// 内网地址应通过。
	for _, u := range []string{
		"ws://127.0.0.1:10400/v1/kws",
		"ws://kws-sidecar:10400/v1/kws",
		"ws://192.168.1.5:10400/v1/kws",
		"wss://kws-sidecar:10400/v1/kws",
	} {
		c, err := New(u, testOpts())
		if err != nil {
			t.Fatalf("New(%q) 应通过，实际报错: %v", u, err)
		}
		c.Close()
	}
}

// TestCloseIsIdempotent：Close 多次不 panic、不死锁。
func TestCloseIsIdempotent(t *testing.T) {
	f := newFakeSidecar(t)
	c, err := New(f.url(), testOpts())
	if err != nil {
		t.Fatal(err)
	}
	sendFrames(c, "lx06-1", 1)
	f.waitHello(t, "lx06-1", 3*time.Second)
	c.Close()
	c.Close()
}
