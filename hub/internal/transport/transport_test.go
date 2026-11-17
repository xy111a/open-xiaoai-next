package transport

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/huajun/open-xiaoai-next/hub/internal/registry"
	"github.com/huajun/open-xiaoai-next/hub/internal/tlscert"
)

// stubHandler 记录收到的事件，供断言。
type stubHandler struct {
	wakes  chan Wake
	claims chan RoomClaim
	status chan Status
	disc   chan string
}

func (h *stubHandler) OnWake(deviceID string, w Wake)            { h.wakes <- w }
func (h *stubHandler) OnAudioStart(deviceID string)              {}
func (h *stubHandler) OnAudioFrame(deviceID string, opus []byte) {}
func (h *stubHandler) OnAudioEnd(deviceID string)                {}
func (h *stubHandler) OnRoomClaim(deviceID string, room string) {
	h.claims <- RoomClaim{Room: room}
}
func (h *stubHandler) OnStatus(deviceID string, s Status)       { h.status <- s }
func (h *stubHandler) OnRPCResult(deviceID string, r RPCResult) {}
func (h *stubHandler) OnDisconnect(deviceID string)             { h.disc <- deviceID }

func startServer(t *testing.T, token string) (*Server, string, *stubHandler) {
	reg := registry.New()
	h := &stubHandler{
		wakes:  make(chan Wake, 4),
		claims: make(chan RoomClaim, 4),
		status: make(chan Status, 4),
		disc:   make(chan string, 4),
	}
	srv := New(token, reg, h)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln, nil) }()
	t.Cleanup(func() { ln.Close() })
	return srv, "ws://" + ln.Addr().String() + "/ws", h
}

func dial(t *testing.T, url string) *websocket.Conn {
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("Dial %s: %v", url, err)
	}
	return c
}

func TestHelloOKAndWake(t *testing.T) {
	srv, url, h := startServer(t, "secret")
	_ = srv
	c := dial(t, url)
	defer c.Close()
	// 正确 Token + lx06 → hello_ok，device_id 由 model + IP 生成。
	if err := c.WriteJSON(map[string]any{"type": "hello", "model": "lx06", "ram_mb": 256, "version": "1.94.13", "token": "secret"}); err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := c.ReadJSON(&resp); err != nil {
		t.Fatalf("读 hello_ok: %v", err)
	}
	if resp["type"] != "hello_ok" {
		t.Fatalf("应收到 hello_ok，实际 %v", resp)
	}
	if resp["device_id"] != "lx06-127-0-0-1" {
		t.Fatalf("device_id 应为 lx06-127-0-0-1，实际 %v", resp["device_id"])
	}
	// 发 wake → handler 收到。
	if err := c.WriteJSON(map[string]any{"type": "wake", "wake_word": "你好小李", "rms_db": -18.5, "ts_ms": 123}); err != nil {
		t.Fatal(err)
	}
	select {
	case w := <-h.wakes:
		if w.WakeWord != "你好小李" || w.RMSdB != -18.5 {
			t.Fatalf("OnWake 收到不对: %+v", w)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("2s 内未收到 OnWake")
	}
}

func TestHelloBadTokenRejected(t *testing.T) {
	_, url, _ := startServer(t, "secret")
	c := dial(t, url)
	defer c.Close()
	if err := c.WriteJSON(map[string]any{"type": "hello", "model": "lx06", "ram_mb": 256, "version": "1.94.13", "token": "wrong"}); err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := c.ReadJSON(&resp); err != nil {
		t.Fatalf("读 hello_reject: %v", err)
	}
	if resp["type"] != "hello_reject" {
		t.Fatalf("错误 Token 应收到 hello_reject，实际 %v", resp)
	}
	// 拒绝后连接应关闭：再读应出错。
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := c.ReadJSON(&resp); err == nil {
		t.Fatal("hello_reject 后连接应已关闭")
	}
}

func TestHelloBadModelRejected(t *testing.T) {
	_, url, _ := startServer(t, "secret")
	c := dial(t, url)
	defer c.Close()
	if err := c.WriteJSON(map[string]any{"type": "hello", "model": "lx07", "ram_mb": 256, "version": "1.0", "token": "secret"}); err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := c.ReadJSON(&resp); err != nil {
		t.Fatalf("读 hello_reject: %v", err)
	}
	if resp["type"] != "hello_reject" {
		t.Fatalf("未知型号应收到 hello_reject，实际 %v", resp)
	}
}

func TestRoomClaimAndRPC(t *testing.T) {
	srv, url, h := startServer(t, "secret")
	c := dial(t, url)
	defer c.Close()
	if err := c.WriteJSON(map[string]any{"type": "hello", "model": "oh2p", "ram_mb": 256, "version": "1.58.6", "token": "secret"}); err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := c.ReadJSON(&resp); err != nil {
		t.Fatal(err)
	}
	if resp["type"] != "hello_ok" {
		t.Fatalf("hello 应 ok，实际 %v", resp)
	}
	// 房间认领。
	if err := c.WriteJSON(map[string]any{"type": "room_claim", "room": "卧室"}); err != nil {
		t.Fatal(err)
	}
	select {
	case cl := <-h.claims:
		if cl.Room != "卧室" {
			t.Fatalf("OnRoomClaim 房间不对: %+v", cl)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("2s 内未收到 OnRoomClaim")
	}
	// 服务端主动下发 RPC，客户端应收到 start_play。
	conn, ok := srv.Conn("oh2p-127-0-0-1")
	if !ok {
		t.Fatal("服务端 Conn 找不到 oh2p-127-0-0-1")
	}
	if err := conn.SendRPC("r1", "start_play", map[string]any{"text": "你好"}); err != nil {
		t.Fatalf("SendRPC: %v", err)
	}
	var rpc map[string]any
	if err := c.ReadJSON(&rpc); err != nil {
		t.Fatalf("读 rpc: %v", err)
	}
	if rpc["type"] != "rpc" || rpc["method"] != "start_play" {
		t.Fatalf("客户端应收到 start_play rpc，实际 %v", rpc)
	}
	// 二进制音频帧：首字节 0x01。
	if err := conn.SendBinary([]byte{0x10, 0x20, 0x30}); err != nil {
		t.Fatalf("SendBinary: %v", err)
	}
	mt, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("读 binary: %v", err)
	}
	if mt != websocket.BinaryMessage || len(data) != 4 || data[0] != 0x01 || data[1] != 0x10 {
		t.Fatalf("二进制帧应为 0x01 前缀 + 原数据，实际 mt=%d data=%v", mt, data)
	}
}

func TestDeviceIDFor(t *testing.T) {
	if got := DeviceIDFor("lx06", "192.168.1.2"); got != "lx06-192-168-1-2" {
		t.Fatalf("DeviceIDFor 不对: %q", got)
	}
}

// --- P0-3：弱 Token 校验 ---

func TestValidateToken(t *testing.T) {
	strong := "0123456789abcdef0123456789abcdef" // openssl rand -hex 16 的长度
	if err := ValidateToken(strong); err != nil {
		t.Fatalf("32 位 hex 强 Token 应通过：%v", err)
	}
	if err := ValidateToken("0123456789abcdef"); err != nil {
		t.Fatalf("16 位 Token 应通过：%v", err)
	}
	weak := []string{
		"",
		"short",
		"0123456789abcde", // 15 位
		"CHANGE_ME",
		"change_me",
		"Change_Me",
		"secret",
		"password",
		"123456",
		"open-xiaoai",
		"test",
	}
	for _, w := range weak {
		if err := ValidateToken(w); err == nil {
			t.Fatalf("弱 Token %q 应被拒绝", w)
		} else if !strings.Contains(err.Error(), "openssl rand -hex 16") {
			t.Fatalf("弱 Token %q 的错误应给出生成命令，实际：%v", w, err)
		}
	}
}

func TestTokenEqual(t *testing.T) {
	if !tokenEqual("abc", "abc") {
		t.Fatal("相同 Token 应相等")
	}
	if tokenEqual("abc", "abd") {
		t.Fatal("不同 Token 应不等")
	}
	if tokenEqual("abc", "abcd") {
		t.Fatal("不同长度 Token 应不等")
	}
	if !tokenEqual("", "") {
		t.Fatal("空串与空串的 SHA-256 相同，应为真（调用方仍用 s.token != \"\" 把关）")
	}
}

// --- P0-3：mTLS 端到端 ---

// startMTLSServer 用 tlscert 的 CA 材料起一个要求客户端证书的 wss 服务，
// 返回 wss 地址、CA 材料目录、客户端 CA 池。
func startMTLSServer(t *testing.T, token string) (string, string, *x509.CertPool, func()) {
	t.Helper()
	dir := t.TempDir()
	serverCert, pool, _, err := tlscert.LoadOrCreateMTLS(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateMTLS: %v", err)
	}
	reg := registry.New()
	h := &stubHandler{
		wakes:  make(chan Wake, 4),
		claims: make(chan RoomClaim, 4),
		status: make(chan Status, 4),
		disc:   make(chan string, 4),
	}
	srv := New(token, reg, h)
	srv.SetClientCAs(pool)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln, &serverCert) }()
	addr := ln.Addr().(*net.TCPAddr)
	return "wss://localhost:" + strconv.Itoa(addr.Port) + "/ws", dir, pool,
		func() { ln.Close() }
}

// mtlsDialer 返回带 CA 根 + 可选客户端证书的 websocket Dialer。
func mtlsDialer(t *testing.T, pool *x509.CertPool, clientCertFile, clientKeyFile string) *websocket.Dialer {
	t.Helper()
	tlsConf := &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12}
	if clientCertFile != "" {
		cert, err := tls.LoadX509KeyPair(clientCertFile, clientKeyFile)
		if err != nil {
			t.Fatalf("读客户端证书: %v", err)
		}
		tlsConf.Certificates = []tls.Certificate{cert}
	}
	return &websocket.Dialer{TLSClientConfig: tlsConf, HandshakeTimeout: 5 * time.Second}
}

func TestServeMTLSRequiresClientCert(t *testing.T) {
	token := "0123456789abcdef0123456789abcdef"
	url, dir, pool, cleanup := startMTLSServer(t, token)
	defer cleanup()
	// LoadOrCreateMTLS 已签发共享客户端证书（CN open-xiaoai-client）。
	clientCert := dir + "/tls/client-open-xiaoai-client.crt"
	clientKey := dir + "/tls/client-open-xiaoai-client.key"
	if _, err := os.Stat(clientCert); err != nil {
		t.Fatalf("共享客户端证书不存在: %v", err)
	}

	// 有正确客户端证书 + 正确 Token → hello_ok。
	d := mtlsDialer(t, pool, clientCert, clientKey)
	c, _, err := d.Dial(url, nil)
	if err != nil {
		t.Fatalf("带客户端证书应能建连: %v", err)
	}
	defer c.Close()
	if err := c.WriteJSON(map[string]any{"type": "hello", "model": "lx06", "ram_mb": 256, "version": "1.94.13", "token": token}); err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := c.ReadJSON(&resp); err != nil {
		t.Fatalf("读 hello_ok: %v", err)
	}
	if resp["type"] != "hello_ok" {
		t.Fatalf("应收到 hello_ok，实际 %v", resp)
	}

	// 无客户端证书 → TLS 握手即失败（连不上）。
	d2 := mtlsDialer(t, pool, "", "")
	if _, _, err := d2.Dial(url, nil); err == nil {
		t.Fatal("无客户端证书应被 mTLS 拒绝")
	}

	// 另一个 CA 签发的客户端证书 → 同样握手失败。
	otherDir := t.TempDir()
	otherCA, err := tlscert.LoadOrCreateCA(otherDir)
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	otherCert, otherKey, err := otherCA.IssueClientCert(otherDir, "attacker")
	if err != nil {
		t.Fatalf("IssueClientCert: %v", err)
	}
	d3 := mtlsDialer(t, pool, otherCert, otherKey)
	if _, _, err := d3.Dial(url, nil); err == nil {
		t.Fatal("非本 CA 签发的客户端证书应被 mTLS 拒绝")
	}
}
