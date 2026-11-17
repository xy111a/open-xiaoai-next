package wyoming

// P1-20 / P1-27 回归测试：并发写锁、可重连 Dialer。

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestConcurrentUseNoRace(t *testing.T) {
	// P1-20：多 goroutine 并发调同一 Client 的 Transcribe/Synthesize/
	// Describe，不得读写交错（-race 下验证；回复与请求错配时文本会对不上）。
	f := startFake(t)
	defer f.ln.Close()
	c, err := Dial(f.addr(), 3*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	pcm := make([]byte, 16000*2)
	var wg sync.WaitGroup
	errs := make(chan error, 24)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			text, err := c.Transcribe(pcm, "zh")
			if err != nil {
				errs <- err
				return
			}
			if text != "打开客厅灯" {
				errs <- errMismatch(text)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := c.Synthesize("好的", "zh_CN-huayan-medium")
			if err != nil {
				errs <- err
				return
			}
			if len(out) != 640 {
				errs <- errMismatch("synthesize len")
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Describe(); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发调用出错: %v", err)
	}
}

func errMismatch(s string) error {
	return &mismatchError{s}
}

type mismatchError struct{ s string }

func (e *mismatchError) Error() string { return "结果错配: " + e.s }

// oneShotWyoming 是“每连接只服务一次转写就断开”的假服务器：
// 用来模拟 Wyoming 服务端断线后 Hub 侧的恢复。
type oneShotWyoming struct {
	ln net.Listener
}

func startOneShot(t *testing.T) *oneShotWyoming {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &oneShotWyoming{ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.handle(conn)
		}
	}()
	return f
}

func (f *oneShotWyoming) handle(conn net.Conn) {
	defer conn.Close()
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
		if header["type"] == "transcribe" {
			for {
				l2, err := r.ReadBytes('\n')
				if err != nil {
					return
				}
				var h2 map[string]any
				_ = json.Unmarshal(l2, &h2)
				if n, ok := h2["payload_length"].(float64); ok && n > 0 {
					_, _ = io.CopyN(io.Discard, r, int64(n))
				}
				if h2["type"] == "audio-stop" {
					break
				}
			}
			b, _ := json.Marshal(map[string]any{"type": "transcript", "text": "打开客厅灯"})
			_, _ = conn.Write(append(b, '\n'))
			return // 回完即断开：模拟服务端断线
		}
		b, _ := json.Marshal(map[string]any{"type": "error", "text": "未知类型"})
		_, _ = conn.Write(append(b, '\n'))
	}
}

func TestDialerReconnectsAfterDrop(t *testing.T) {
	f := startOneShot(t)
	defer f.ln.Close()
	d := NewDialer(f.ln.Addr().String(), 3*time.Second)
	defer d.Close()
	pcm := make([]byte, 16000*2)
	// 第一次成功（服务端随即断开连接）。
	if text, err := d.Transcribe(pcm, "zh"); err != nil || text != "打开客厅灯" {
		t.Fatalf("第一次 Transcribe 应成功，实际 text=%q err=%v", text, err)
	}
	// 第二次：旧连接已断，Dialer 应自动重拨重试一次，仍成功。
	if text, err := d.Transcribe(pcm, "zh"); err != nil || text != "打开客厅灯" {
		t.Fatalf("断线后 Transcribe 应重拨恢复，实际 text=%q err=%v", text, err)
	}
}

func TestDialerLazyDialAndClose(t *testing.T) {
	// 指向死地址：首次调用才拨号、报错；Close 后可再调（仍报错，不 panic）。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := ln.Addr().String()
	_ = ln.Close()
	d := NewDialer(deadAddr, 200*time.Millisecond)
	if _, err := d.Transcribe(make([]byte, 640), "zh"); err == nil {
		t.Fatal("死地址 Transcribe 应报错")
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close 不应报错: %v", err)
	}
	if _, err := d.Synthesize("你好", "v"); err == nil {
		t.Fatal("Close 后 Synthesize 死地址仍应报错")
	}
}
