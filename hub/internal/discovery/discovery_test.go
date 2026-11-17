package discovery

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/huajun/open-xiaoai-next/hub/internal/registry"
	"github.com/huajun/open-xiaoai-next/hub/internal/tlscert"
	"github.com/huajun/open-xiaoai-next/hub/internal/transport"
)

// stubHandler 满足 transport.Handler（ProbeModel 只需要 hello 握手）。
type stubHandler struct{}

func (stubHandler) OnWake(deviceID string, w transport.Wake)           {}
func (stubHandler) OnAudioStart(deviceID string)                       {}
func (stubHandler) OnAudioFrame(deviceID string, opus []byte)          {}
func (stubHandler) OnAudioEnd(deviceID string)                         {}
func (stubHandler) OnRoomClaim(deviceID string, room string)           {}
func (stubHandler) OnStatus(deviceID string, s transport.Status)       {}
func (stubHandler) OnRPCResult(deviceID string, r transport.RPCResult) {}
func (stubHandler) OnDisconnect(deviceID string)                       {}

// startFakeHubTLS 在 127.0.0.1 上起一个真 wss transport 服务，
// 返回端口与该服务的证书指纹（供 Scanner 白名单用）。
func startFakeHubTLS(t *testing.T, token string) (int, string) {
	cert, fp, err := tlscert.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	srv := transport.New(token, reg, stubHandler{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln, &cert) }()
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().(*net.TCPAddr).Port, fp
}

// recordingFake 是记录型假音箱：只记 /ws 上收到的第一条消息与连接数，
// 回 hello_ok。它用自签证书跑 wss（与真音箱一致）。
type recordingFake struct {
	mu       sync.Mutex
	wsConns  int
	messages []map[string]any
}

func startRecordingFake(t *testing.T) (*recordingFake, int, string) {
	cert, fp, err := tlscert.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &recordingFake{}
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.wsConns++
		f.mu.Unlock()
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		var msg map[string]any
		if err := ws.ReadJSON(&msg); err != nil {
			return
		}
		f.mu.Lock()
		f.messages = append(f.messages, msg)
		f.mu.Unlock()
		_ = ws.WriteJSON(map[string]any{"type": "hello_ok", "device_id": "lx06-127-0-0-1"})
	})
	srv := &http.Server{Handler: mux, TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { ln.Close(); _ = srv.Close() })
	return f, ln.Addr().(*net.TCPAddr).Port, fp
}

func (f *recordingFake) counts() (int, []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.wsConns, append([]map[string]any(nil), f.messages...)
}

func newScanner(port int, token, fp string) *Scanner {
	sc := New(port, token)
	sc.TLS = true
	sc.Timeout = 3 * time.Second
	if fp != "" {
		sc.ExpectedFingerprints = []string{fp}
	}
	return sc
}

// P0-4：匿名探测不发 Token——Token 为空时仍能拿到指纹，且探测不产生任何 WS 连接。
func TestProbeAnonymousNoToken(t *testing.T) {
	f, port, fp := startRecordingFake(t)
	sc := newScanner(port, "", fp) // Token 为空：探测本就不该需要它
	got, err := sc.ProbeFingerprint(context.Background(), "127.0.0.1")
	if err != nil {
		t.Fatalf("ProbeFingerprint: %v", err)
	}
	if normalizeFingerprint(got) != normalizeFingerprint(fp) {
		t.Fatalf("指纹应为 %s，实际 %s", fp, got)
	}
	if n, _ := f.counts(); n != 0 {
		t.Fatalf("匿名探测不应产生 WS 连接，实际 %d 次", n)
	}
}

// P0-4：指纹不匹配不发 Token——假音箱应看不到任何 WS 连接/消息，
// 且错误信息里不含 Token。
func TestProbeModelFingerprintMismatch(t *testing.T) {
	f, port, _ := startRecordingFake(t)
	sc := newScanner(port, "secret", "00deadbeef00deadbeef00deadbeef00deadbeef00deadbeef00deadbeef00deadbeef")
	if _, err := sc.ProbeModel(context.Background(), "127.0.0.1", "lx06"); err == nil {
		t.Fatal("指纹不匹配的 ProbeModel 应失败")
	} else if strings.Contains(err.Error(), "secret") {
		t.Fatalf("错误信息不得含 Token：%v", err)
	}
	if n, msgs := f.counts(); n != 0 || len(msgs) != 0 {
		t.Fatalf("指纹不匹配时不得发 Token：WS 连接 %d 次，消息 %v", n, msgs)
	}
}

// P0-4：未配置指纹白名单时不发 Token。
func TestProbeModelNoFingerprintConfigured(t *testing.T) {
	f, port, _ := startRecordingFake(t)
	sc := newScanner(port, "secret", "")
	if _, err := sc.ProbeModel(context.Background(), "127.0.0.1", "lx06"); err == nil {
		t.Fatal("未配置指纹白名单的 ProbeModel 应失败")
	}
	if n, _ := f.counts(); n != 0 {
		t.Fatalf("未配置白名单时不得建 WS 连接，实际 %d 次", n)
	}
}

// P0-4：指纹匹配后才发 Token 做 hello，且 hello 里确实带 Token。
func TestProbeModelSuccess(t *testing.T) {
	f, port, fp := startRecordingFake(t)
	sc := newScanner(port, "secret", fp)
	id, err := sc.ProbeModel(context.Background(), "127.0.0.1", "lx06")
	if err != nil {
		t.Fatalf("ProbeModel lx06: %v", err)
	}
	if id != "lx06-127-0-0-1" {
		t.Fatalf("ProbeModel 应返回 lx06-127-0-0-1，实际 %q", id)
	}
	if n, msgs := f.counts(); n != 1 || len(msgs) != 1 {
		t.Fatalf("指纹匹配后应恰好 1 次 WS 连接 + 1 条 hello，实际 %d 次、%v", n, msgs)
	} else if msgs[0]["token"] != "secret" || msgs[0]["type"] != "hello" {
		t.Fatalf("hello 应带 Token，实际 %v", msgs[0])
	}
}

// P0-4：明文（TLS=false）拒绝探测与握手，永不发 Token。
func TestPlaintextRefused(t *testing.T) {
	f, port, fp := startRecordingFake(t)
	sc := New(port, "secret")
	sc.Timeout = 3 * time.Second
	sc.ExpectedFingerprints = []string{fp}
	if _, err := sc.ProbeFingerprint(context.Background(), "127.0.0.1"); err == nil {
		t.Fatal("明文探测应被拒绝")
	}
	if _, err := sc.ProbeModel(context.Background(), "127.0.0.1", "lx06"); err == nil {
		t.Fatal("明文握手应被拒绝")
	}
	if n, _ := f.counts(); n != 0 {
		t.Fatalf("明文模式不得建 WS 连接，实际 %d 次", n)
	}
}

// 旧的明文 Probe 行为已删除：错误 Token 在指纹匹配后仍被服务端拒绝。
func TestProbeModelWrongTokenRejected(t *testing.T) {
	port, fp := startFakeHubTLS(t, "secret")
	scBad := newScanner(port, "wrong", fp)
	scBad.Timeout = 3 * time.Second
	if _, err := scBad.ProbeModel(context.Background(), "127.0.0.1", "lx06"); err == nil {
		t.Fatal("错误 Token 的 Probe 应失败")
	}
	// 正确 Token 通过。
	sc := newScanner(port, "secret", fp)
	sc.Timeout = 3 * time.Second
	id, err := sc.ProbeModel(context.Background(), "127.0.0.1", "lx06")
	if err != nil {
		t.Fatalf("ProbeModel lx06: %v", err)
	}
	if id != "lx06-127-0-0-1" {
		t.Fatalf("ProbeModel 应返回 lx06-127-0-0-1，实际 %q", id)
	}
}
