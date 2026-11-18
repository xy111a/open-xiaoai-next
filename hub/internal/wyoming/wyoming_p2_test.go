package wyoming

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// weirdFake 是行为异常的假 Wyoming 服务：
//   - mode "unknown"：transcribe 后连发未知类型事件（P2-17）；
//   - mode "rate8000"：synthesize 的 audio-start 带 rate 8000（P2-48）。
type weirdFake struct {
	t    *testing.T
	ln   net.Listener
	mode string
}

func startWeirdFake(t *testing.T, mode string) *weirdFake {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &weirdFake{t: t, ln: ln, mode: mode}
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

func (f *weirdFake) write(conn net.Conn, header map[string]any, payload []byte) {
	if payload != nil {
		header["payload_length"] = len(payload)
	}
	b, _ := json.Marshal(header)
	_, _ = conn.Write(append(b, '\n'))
	if payload != nil {
		_, _ = conn.Write(payload)
	}
}

func (f *weirdFake) handle(conn net.Conn) {
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
		switch header["type"] {
		case "transcribe":
			// 读掉 audio-start/chunk/stop。
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
			if f.mode == "unknown" {
				for i := 0; i < 20; i++ {
					f.write(conn, map[string]any{"type": "mystery-event", "n": i}, nil)
				}
				return
			}
			f.write(conn, map[string]any{"type": "transcript", "text": "你好"}, nil)
		case "synthesize":
			rate := 16000
			if f.mode == "rate8000" {
				rate = 8000
			}
			f.write(conn, map[string]any{"type": "audio-start", "rate": rate, "width": 2, "channels": 1}, nil)
			f.write(conn, map[string]any{"type": "audio-chunk"}, make([]byte, 640))
			f.write(conn, map[string]any{"type": "audio-stop"}, nil)
		default:
			f.write(conn, map[string]any{"type": "error", "text": "未知类型"}, nil)
		}
	}
}

// P2-17：连续未知事件不再静默死循环，10 个即报错（不用等 120s 超时）。
func TestUnknownEventsAbortP2(t *testing.T) {
	f := startWeirdFake(t, "unknown")
	defer f.ln.Close()
	c, err := Dial(f.ln.Addr().String(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	_, err = c.Transcribe(make([]byte, 3200), "zh")
	if err == nil {
		t.Fatal("连续未知事件应报错")
	}
	if !strings.Contains(err.Error(), "未知事件") {
		t.Fatalf("错误应说明未知事件，实际 %v", err)
	}
	if time.Since(start) > 30*time.Second {
		t.Fatalf("未知事件应在 120s 超时前就报错，实际耗时 %v", time.Since(start))
	}
}

// P2-48：合成采样率不是 16kHz 即报错，不播变调语音。
func TestSynthesizeWrongRateRejectedP2(t *testing.T) {
	f := startWeirdFake(t, "rate8000")
	defer f.ln.Close()
	c, err := Dial(f.ln.Addr().String(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Synthesize("你好", "v"); err == nil {
		t.Fatal("rate 8000 的合成应报错")
	} else if !strings.Contains(err.Error(), "采样率") {
		t.Fatalf("错误应说明采样率，实际 %v", err)
	}
}
