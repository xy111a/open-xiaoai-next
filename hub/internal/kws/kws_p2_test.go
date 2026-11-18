package kws

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// N-P2-2：Close 后再 SendPCM 不得新建流、不泄漏拨号 goroutine。
func TestCloseThenSendPCMNoLeakP2(t *testing.T) {
	f := newFakeSidecar(t)
	c, err := New(f.url(), testOpts())
	if err != nil {
		t.Fatal(err)
	}
	sendFrames(c, "lx06-1", 2)
	f.waitHello(t, "lx06-1", 3*time.Second)
	before := c.Status().Streams
	if before != 1 {
		t.Fatalf("关闭前应有 1 条流，实际 %d", before)
	}
	c.Close()
	c.Close()                  // 幂等
	sendFrames(c, "lx06-1", 5) // Close 后误调：应直接丢弃，不建流
	time.Sleep(300 * time.Millisecond)
	if got := c.Status().Streams; got != before {
		t.Fatalf("Close 后 SendPCM 不得新建流，流数 %d → %d", before, got)
	}
}

// N-P2-3：重连后断线期积压的陈旧 PCM 被排空，不发给边车；
// 重连后的新帧正常转发。
func TestReconnectDrainsStalePCMP2(t *testing.T) {
	f := newFakeSidecar(t)
	c, err := New(f.url(), testOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sendFrames(c, "lx06-1", 2)
	f.waitHello(t, "lx06-1", 3*time.Second)
	f.waitPCM(t, "lx06-1", 1280, 3*time.Second)
	// 模拟边车断线。
	f.closeConn("lx06-1")
	// 等客户端观测到断线（Connected 归零）再发“断线期”音频。
	deadline := time.Now().Add(3 * time.Second)
	for c.Status().Connected != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if c.Status().Connected != 0 {
		t.Fatal("断线后客户端仍显示已连接")
	}
	f.mu.Lock()
	base := f.pcmBytes["lx06-1"]
	f.mu.Unlock()
	sendFrames(c, "lx06-1", 10) // 断线期积压的 10 帧陈旧音频
	// 等重连成功。
	f.waitHello(t, "lx06-1", 3*time.Second)
	time.Sleep(500 * time.Millisecond)
	f.mu.Lock()
	afterReconnect := f.pcmBytes["lx06-1"]
	f.mu.Unlock()
	if afterReconnect != base {
		t.Fatalf("重连后陈旧音频应被排空，边车收到字节 %d → %d（应不变）", base, afterReconnect)
	}
	// 重连后的新帧正常转发。
	sendFrames(c, "lx06-1", 2)
	f.waitPCM(t, "lx06-1", base+1280, 3*time.Second)
}

// N-P2-4：wake 事件的 device_id 以流 ID 为准，
// 边车回传别的 device_id 也不采信。
func TestWakeDeviceIDNotTrustedP2(t *testing.T) {
	f := newFakeSidecar(t)
	c, err := New(f.url(), testOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sendFrames(c, "lx06-1", 1)
	f.waitHello(t, "lx06-1", 3*time.Second)
	// 假边车故意回传错误的 device_id。
	f.mu.Lock()
	conn, ok := f.conns["lx06-1"]
	f.mu.Unlock()
	if !ok {
		t.Fatal("假边车没有 lx06-1 的连接")
	}
	if err := f.writeJSON(conn, map[string]any{
		"type": "wake", "device_id": "other-device", "keyword": "你好小李",
		"rms": 0.42, "timestamp_ms": time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("发 wake: %v", err)
	}
	select {
	case ev := <-c.Events():
		if ev.DeviceID != "lx06-1" {
			t.Fatalf("wake 事件 device_id 应为流 ID lx06-1，实际 %q（轻信了边车回传值）", ev.DeviceID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("未收到 wake 事件")
	}
}

// N-P2-5：Start 后立即做一次健康检查，不等第一个 tick——
// /health 404 的边车应在 3 秒内就被标 Down，而不是等 HealthInterval。
func TestHealthCheckedImmediatelyOnStartP2(t *testing.T) {
	// 只有 WS、没有 /health 的假服务（/health 走 404）。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/kws" {
			up := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
			conn, err := up.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/kws"
	opts := testOpts()
	opts.HealthInterval = time.Hour // 间隔拉到 1 小时：不用立即检查就测不出来
	c, err := New(wsURL, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Start()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c.Status().Down {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("Start 后 3 秒内 kws_status 应已是 Down（立即健康检查未生效）")
}

// N-P2-12：重连退避带 jitter——同一起点多次计算的值应在一个
// ±25% 的带里、且不全相等（旧代码纯翻倍，两台流脉冲同步）。
func TestNextBackoffJitterP2(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		got := nextBackoff(500*time.Millisecond, 30*time.Second)
		// 基数 1000ms ±25% → [750ms, 1250ms]。
		if got < 750*time.Millisecond || got > 1250*time.Millisecond {
			t.Fatalf("退避应在 750ms~1250ms 带内，实际 %v", got)
		}
		seen[got] = true
	}
	if len(seen) < 2 {
		t.Fatal("退避 200 次应有多个不同值（无 jitter）")
	}
	// 上限钳制：jitter 后也不得超过 max。
	for i := 0; i < 50; i++ {
		if got := nextBackoff(30*time.Second, 30*time.Second); got > 30*time.Second {
			t.Fatalf("退避不得超过 max 30s，实际 %v", got)
		}
	}
}

// N-P2-10：wss + 自签证书的边车——配了指纹 pin 才能连上，
// 健康检查与拨号共用同一 TLS 配置；不配指纹时校验失败。
func TestWSSFingerprintP2(t *testing.T) {
	mux := http.NewServeMux()
	hellos := make(chan string, 4)
	mux.HandleFunc("/v1/kws", func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, first, err := conn.ReadMessage()
		if err != nil {
			return
		}
		hellos <- string(first)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	cert := srv.Certificate()
	if cert == nil {
		t.Fatal("TLS 测试服务没有证书")
	}
	sum := sha256.Sum256(cert.Raw)
	fp := hex.EncodeToString(sum[:])
	wsURL := "wss" + strings.TrimPrefix(srv.URL, "https") + "/v1/kws"

	// 配了指纹：健康检查通过，拨号 + hello 成功。
	opts := testOpts()
	opts.TLSFingerprint = fp
	c, err := New(wsURL, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.CheckHealth(context.Background()); err != nil {
		t.Fatalf("配了指纹时健康检查应通过，实际 %v", err)
	}
	sendFrames(c, "lx06-1", 1)
	select {
	case h := <-hellos:
		if !strings.Contains(h, "lx06-1") {
			t.Fatalf("hello 应含设备 ID，实际 %s", h)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("配了指纹时拨号 + hello 应成功")
	}

	// 不配指纹：自签证书校验失败，健康检查报错。
	c2, err := New(wsURL, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if err := c2.CheckHealth(context.Background()); err == nil {
		t.Fatal("不配指纹时自签证书的健康检查应失败")
	} else if !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("健康检查失败原因应与证书有关，实际 %v", err)
	}
	// 指纹格式非法：New 即报错。
	badOpts := testOpts()
	badOpts.TLSFingerprint = "xyz"
	if _, err := New(wsURL, badOpts); err == nil {
		t.Fatal("非法指纹格式应被 New 拒绝")
	}
}
