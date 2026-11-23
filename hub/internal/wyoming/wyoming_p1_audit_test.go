package wyoming

// P1-8 回归测试（2026-11-23 审查）：Dialer“失败重试一次”只针对建连失败/
// 立即失败；已耗时很久的读写失败不再重试，否则单次 STT/TTS 最坏耗时翻倍。

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// countingListener 记连接数；mode 决定服务端行为：
//   - "close"：接后立即断开（建连后立即失败，快失败）；
//   - "delayed-error"：读完一轮 transcribe 请求后等 delay 再回 error 事件
//     （慢失败：首次调用耗时 > retryFastWindow，不应重试）。
type countingListener struct {
	ln    net.Listener
	mu    sync.Mutex
	conns int
	mode  string
	delay time.Duration
}

func startCountingListener(t *testing.T, mode string, delay time.Duration) *countingListener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &countingListener{ln: ln, mode: mode, delay: delay}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.conns++
			f.mu.Unlock()
			go f.handle(conn)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func (f *countingListener) handle(conn net.Conn) {
	defer conn.Close()
	if f.mode == "close" {
		return // 立即断开。
	}
	// delayed-error：读完 transcribe 一轮（到 audio-stop），再等 delay 回 error。
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var header map[string]any
		if err := json.Unmarshal(line, &header); err != nil {
			return
		}
		if n, ok := header["payload_length"].(float64); ok && n > 0 {
			if _, err := io.CopyN(io.Discard, r, int64(n)); err != nil {
				return
			}
		}
		if header["type"] == "audio-stop" {
			break
		}
	}
	time.Sleep(f.delay)
	b, _ := json.Marshal(map[string]any{"type": "error", "text": "slow boom"})
	_, _ = conn.Write(append(b, '\n'))
}

func (f *countingListener) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conns
}

// 立即失败（服务端接后即断）：应重试一次，共 2 次连接。
func TestDialerRetriesFastFailure(t *testing.T) {
	oldWindow := retryFastWindow
	retryFastWindow = 10 * time.Second
	defer func() { retryFastWindow = oldWindow }()

	f := startCountingListener(t, "close", 0)
	d := NewDialer(f.ln.Addr().String(), 3*time.Second)
	defer d.Close()
	if _, err := d.Transcribe(make([]byte, 640), "zh"); err == nil {
		t.Fatal("服务端立即断线时 Transcribe 应报错")
	}
	if got := f.count(); got != 2 {
		t.Fatalf("快失败应重试一次、共 2 次连接，实际 %d", got)
	}
}

// 慢失败（首次调用 200ms 才失败，窗口 50ms）：不应重试，只 1 次连接。
func TestDialerNoRetryOnSlowFailure(t *testing.T) {
	oldWindow := retryFastWindow
	retryFastWindow = 50 * time.Millisecond
	defer func() { retryFastWindow = oldWindow }()

	f := startCountingListener(t, "delayed-error", 200*time.Millisecond)
	d := NewDialer(f.ln.Addr().String(), 3*time.Second)
	defer d.Close()
	start := time.Now()
	if _, err := d.Transcribe(make([]byte, 640), "zh"); err == nil {
		t.Fatal("服务端回 error 时 Transcribe 应报错")
	}
	elapsed := time.Since(start)
	if got := f.count(); got != 1 {
		t.Fatalf("慢失败不应重试、只 1 次连接，实际 %d", got)
	}
	// 首次调用约 200ms 即回；若偷偷重试一次，耗时会接近 400ms。
	if elapsed > 350*time.Millisecond {
		t.Fatalf("慢失败应 200ms 左右即回，实际 %v", elapsed)
	}
}
