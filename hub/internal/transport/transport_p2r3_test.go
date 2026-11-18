package transport

// 第三轮审计 P2 回归测试（Hub 面）：
// H-5 register 拒绝原因统一、H-8 transport TLS 最低 1.2。

import (
	"crypto/tls"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// H-5：register 的四种拒绝原因统一回 "rejected"，与 hello 看齐
// （P2-2）；具体原因只记服务端日志，不回显给对端。
func TestRegisterRejectReasonUnifiedP2R3(t *testing.T) {
	fp := "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	srv, url, _ := startServer(t, "secret")
	srv.SetFingerprints([]string{fp})

	rejectReason := func(reg map[string]any) string {
		c := helloConn(t, url, "lx06", "secret")
		defer c.Close()
		if err := c.WriteJSON(reg); err != nil {
			t.Fatalf("发 register: %v", err)
		}
		var resp map[string]string
		if err := c.ReadJSON(&resp); err != nil {
			t.Fatalf("读 register_reject: %v", err)
		}
		if resp["type"] != "register_reject" {
			t.Fatalf("应收到 register_reject，实际 %v", resp)
		}
		return resp["reason"]
	}
	cases := map[string]map[string]any{
		"device_id 与连接不一致":   {"type": "register", "device_id": "lx06-9-9-9-9", "model": "lx06", "room": "客厅", "fingerprint": fp},
		"model 与 hello 不一致":  {"type": "register", "device_id": "lx06-127-0-0-1", "model": "oh2p", "room": "客厅", "fingerprint": fp},
		"fingerprint 不在白名单内": {"type": "register", "device_id": "lx06-127-0-0-1", "model": "lx06", "room": "客厅", "fingerprint": "00deadbeef"},
	}
	for name, reg := range cases {
		if got := rejectReason(reg); got != "rejected" {
			t.Fatalf("%s 时拒绝原因应统一为 \"rejected\"，实际 %q", name, got)
		}
	}

	// 未配置指纹白名单（另起一个 server）→ 同样只回 "rejected"。
	_, url2, _ := startServer(t, "secret")
	c := helloConn(t, url2, "lx06", "secret")
	defer c.Close()
	if err := c.WriteJSON(map[string]any{"type": "register", "device_id": "lx06-127-0-0-1",
		"model": "lx06", "room": "客厅", "fingerprint": fp}); err != nil {
		t.Fatal(err)
	}
	var resp map[string]string
	if err := c.ReadJSON(&resp); err != nil {
		t.Fatalf("读 register_reject: %v", err)
	}
	if resp["type"] != "register_reject" || resp["reason"] != "rejected" {
		t.Fatalf("未配白名单时应回 register_reject/rejected，实际 %v", resp)
	}
}

// H-8：wss 服务端最低 TLS 1.2——TLS 1.1 客户端握手即失败，
// TLS 1.2 客户端正常建连（客户端是自家 Rust 代码，支持 TLS 1.2+）。
func TestServeTLSMinVersion12P2R3(t *testing.T) {
	token := "0123456789abcdef0123456789abcdef"
	url, dir, pool, cleanup := startMTLSServer(t, token)
	defer cleanup()
	clientCert := dir + "/tls/client-open-xiaoai-client.crt"
	clientKey := dir + "/tls/client-open-xiaoai-client.key"
	cert, err := tls.LoadX509KeyPair(clientCert, clientKey)
	if err != nil {
		t.Fatalf("读客户端证书: %v", err)
	}

	// TLS 1.1（最高 1.1）→ 握手失败。
	d11 := &websocket.Dialer{
		HandshakeTimeout: 5 * time.Second,
		TLSClientConfig: &tls.Config{
			RootCAs: pool, ServerName: "localhost",
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS10, MaxVersion: tls.VersionTLS11,
		},
	}
	if _, _, err := d11.Dial(url, nil); err == nil {
		t.Fatal("TLS 1.1 客户端应被服务端拒绝")
	}

	// TLS 1.2（只许 1.2）→ 握手成功，hello_ok。
	d12 := &websocket.Dialer{
		HandshakeTimeout: 5 * time.Second,
		TLSClientConfig: &tls.Config{
			RootCAs: pool, ServerName: "localhost",
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12, MaxVersion: tls.VersionTLS12,
		},
	}
	c, _, err := d12.Dial(url, nil)
	if err != nil {
		t.Fatalf("TLS 1.2 客户端应能建连: %v", err)
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
		t.Fatalf("TLS 1.2 建连后应收到 hello_ok，实际 %v", resp)
	}
}
