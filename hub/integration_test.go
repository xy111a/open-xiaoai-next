// Package hub_test 是端到端集成测试：真 transport 服务 + 真仲裁器 + 两个模拟 Client。
// 不用 mock 代替被测对象：wss/ws 握手、Token 鉴权、250ms 仲裁窗口都在真实 socket 上跑。
package hub_test

import (
	"net"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/huajun/open-xiaoai-next/hub/internal/arbitration"
	"github.com/huajun/open-xiaoai-next/hub/internal/registry"
	"github.com/huajun/open-xiaoai-next/hub/internal/transport"
)

// arbHandler 是 main.go 里 hubHandler 的仲裁部分（精简复刻：OnWake → Arbiter → 发 wake_win/wake_lose）。
type arbHandler struct {
	srv     *transport.Server
	arbiter *arbitration.Arbiter
}

func (h *arbHandler) OnWake(deviceID string, w transport.Wake) {
	h.arbiter.Submit(arbitration.WakeEvent{DeviceID: deviceID, WakeWord: w.WakeWord, RMSdB: w.RMSdB})
}
func (h *arbHandler) OnAudioStart(deviceID string)                       {}
func (h *arbHandler) OnAudioFrame(deviceID string, opus []byte)          {}
func (h *arbHandler) OnAudioEnd(deviceID string)                         {}
func (h *arbHandler) OnRoomClaim(deviceID string, room string)           {}
func (h *arbHandler) OnStatus(deviceID string, s transport.Status)       {}
func (h *arbHandler) OnRPCResult(deviceID string, r transport.RPCResult) {}
func (h *arbHandler) OnDisconnect(deviceID string)                       {}

// dialClient 连上 Hub 并完成 hello 握手，返回连接（调用方负责 Close）。
func dialClient(t *testing.T, url, model, token string) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("Dial %s: %v", url, err)
	}
	if err := c.WriteJSON(map[string]any{"type": "hello", "model": model, "ram_mb": 256, "version": "test", "token": token}); err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := c.ReadJSON(&resp); err != nil {
		t.Fatalf("读 hello 响应: %v", err)
	}
	if resp["type"] != "hello_ok" {
		t.Fatalf("hello 应 ok，实际 %v", resp)
	}
	return c
}

// TestTwoClientsArbitration：两台同时唤醒，RMS 大的赢，赢家收到 wake_win，输家收到 wake_lose。
func TestTwoClientsArbitration(t *testing.T) {
	reg := registry.New()
	h := &arbHandler{}
	srv := transport.New("secret", reg, h)
	h.srv = srv
	h.arbiter = arbitration.New(250*time.Millisecond, func(winner string, losers []string) {
		if c, ok := srv.Conn(winner); ok {
			_ = c.SendJSON(map[string]string{"type": "wake_win"})
		}
		for _, l := range losers {
			if c, ok := srv.Conn(l); ok {
				_ = c.SendJSON(map[string]string{"type": "wake_lose"})
			}
		}
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { _ = srv.Serve(ln, nil) }()
	url := "ws://" + ln.Addr().String() + "/ws"

	lx := dialClient(t, url, "lx06", "secret")
	defer lx.Close()
	oh := dialClient(t, url, "oh2p", "secret")
	defer oh.Close()

	// 两台几乎同时唤醒：lx06 RMS -30（远/轻），oh2p RMS -15（近/响）。
	if err := lx.WriteJSON(map[string]any{"type": "wake", "wake_word": "你好小李", "rms_db": -30.0, "ts_ms": 1000}); err != nil {
		t.Fatal(err)
	}
	if err := oh.WriteJSON(map[string]any{"type": "wake", "wake_word": "你好小李", "rms_db": -15.0, "ts_ms": 1001}); err != nil {
		t.Fatal(err)
	}
	// 250ms 窗口关闭后，各自应收到唯一裁决。
	_ = lx.SetReadDeadline(time.Now().Add(3 * time.Second))
	_ = oh.SetReadDeadline(time.Now().Add(3 * time.Second))
	var lxResp, ohResp map[string]any
	if err := lx.ReadJSON(&lxResp); err != nil {
		t.Fatalf("lx06 读裁决: %v", err)
	}
	if err := oh.ReadJSON(&ohResp); err != nil {
		t.Fatalf("oh2p 读裁决: %v", err)
	}
	if lxResp["type"] != "wake_lose" {
		t.Fatalf("RMS 小的 lx06 应收到 wake_lose，实际 %v", lxResp)
	}
	if ohResp["type"] != "wake_win" {
		t.Fatalf("RMS 大的 oh2p 应收到 wake_win，实际 %v", ohResp)
	}
}

// TestSingleClientWinsDirectly：一台唤醒直接赢，不等第二台。
func TestSingleClientWinsDirectly(t *testing.T) {
	reg := registry.New()
	h := &arbHandler{}
	srv := transport.New("secret", reg, h)
	h.srv = srv
	h.arbiter = arbitration.New(250*time.Millisecond, func(winner string, losers []string) {
		if c, ok := srv.Conn(winner); ok {
			_ = c.SendJSON(map[string]string{"type": "wake_win"})
		}
		for _, l := range losers {
			if c, ok := srv.Conn(l); ok {
				_ = c.SendJSON(map[string]string{"type": "wake_lose"})
			}
		}
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { _ = srv.Serve(ln, nil) }()
	url := "ws://" + ln.Addr().String() + "/ws"

	lx := dialClient(t, url, "lx06", "secret")
	defer lx.Close()
	if err := lx.WriteJSON(map[string]any{"type": "wake", "wake_word": "你好小李", "rms_db": -25.0, "ts_ms": 1000}); err != nil {
		t.Fatal(err)
	}
	_ = lx.SetReadDeadline(time.Now().Add(3 * time.Second))
	var resp map[string]any
	if err := lx.ReadJSON(&resp); err != nil {
		t.Fatalf("单台读裁决: %v", err)
	}
	if resp["type"] != "wake_win" {
		t.Fatalf("单台唤醒应收到 wake_win，实际 %v", resp)
	}
}

// TestBadTokenClientRejected：错误 Token 的 Client 连不上 Hub。
func TestBadTokenClientRejected(t *testing.T) {
	reg := registry.New()
	srv := transport.New("secret", reg, &arbHandler{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { _ = srv.Serve(ln, nil) }()
	url := "ws://" + ln.Addr().String() + "/ws"

	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if err := c.WriteJSON(map[string]any{"type": "hello", "model": "lx06", "ram_mb": 256, "version": "test", "token": "wrong"}); err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := c.ReadJSON(&resp); err != nil {
		t.Fatalf("读 hello_reject: %v", err)
	}
	if resp["type"] != "hello_reject" {
		t.Fatalf("错误 Token 应收到 hello_reject，实际 %v", resp)
	}
}
