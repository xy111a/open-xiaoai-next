package discovery

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/huajun/open-xiaoai-next/hub/internal/tlscert"
)

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

// H-13：ProbeModel 及其 6 处测试调用已删（P2-29 死代码，生产无调用方）。
// 下面只保留 ProbeFingerprint 的测试。

// P0-4：明文（TLS=false）拒绝探测，永不发 Token。
func TestPlaintextRefused(t *testing.T) {
	f, port, fp := startRecordingFake(t)
	sc := New(port, "secret")
	sc.Timeout = 3 * time.Second
	sc.ExpectedFingerprints = []string{fp}
	if _, err := sc.ProbeFingerprint(context.Background(), "127.0.0.1"); err == nil {
		t.Fatal("明文探测应被拒绝")
	}
	if n, _ := f.counts(); n != 0 {
		t.Fatalf("明文模式不得建 WS 连接，实际 %d 次", n)
	}
}
