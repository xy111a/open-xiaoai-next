package wyoming

import (
	"bufio"
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
	writeWyomingFrame(conn, header, payload)
}

func (f *weirdFake) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	for {
		_, header, err := readWyomingFrame(r)
		if err != nil {
			return
		}
		switch header["type"] {
		case "transcribe":
			// 读掉 audio-start/chunk/stop。
			for {
				_, h2, err := readWyomingFrame(r)
				if err != nil {
					return
				}
				if h2["type"] == "audio-stop" {
					break
				}
			}
			if f.mode == "unknown" {
				for i := 0; i < 20; i++ {
					f.write(conn, map[string]any{"type": "mystery-event", "data": map[string]any{"n": i}}, nil)
				}
				return
			}
			f.write(conn, map[string]any{"type": "transcript", "data": map[string]any{"text": "你好"}}, nil)
		case "synthesize":
			rate := 16000
			if f.mode == "rate8000" {
				rate = 8000
			}
			if f.mode == "rate192k" {
				rate = 192000
			}
			f.write(conn, map[string]any{"type": "audio-start", "data": map[string]any{"rate": rate, "width": 2, "channels": 1}}, nil)
			f.write(conn, map[string]any{"type": "audio-chunk", "data": map[string]any{"rate": rate, "width": 2, "channels": 1}}, make([]byte, 640))
			f.write(conn, map[string]any{"type": "audio-stop", "data": map[string]any{}}, nil)
		default:
			f.write(conn, map[string]any{"type": "error", "data": map[string]any{"text": "未知类型"}}, nil)
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

// P2-48：合成采样率不是 16kHz 时重采样到 16kHz 再返回，不播变调语音。
// （旧行为是直接报错；2026-11-23 真实服务端端到端发现 Piper
// zh_CN-huayan-medium 原生就是 22050Hz，报错等于 TTS 全灭。）
// rate8000 的 640 字节 PCM（320 采样 @8kHz = 40ms）应返回 1280 字节
// （640 采样 @16kHz = 40ms），时长不变。
func TestSynthesizeResamplesNon16kRateP2(t *testing.T) {
	f := startWeirdFake(t, "rate8000")
	defer f.ln.Close()
	c, err := Dial(f.ln.Addr().String(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	out, err := c.Synthesize("你好", "v")
	if err != nil {
		t.Fatalf("rate 8000 应重采样后返回，不应报错: %v", err)
	}
	if len(out) != 1280 {
		t.Fatalf("8000Hz 640 字节应重采样为 16kHz 1280 字节，实际 %d", len(out))
	}
}

// 采样率荒谬（0 或超 96kHz）时仍报错，不重采样。
func TestSynthesizeRejectsAbsurdRateP2(t *testing.T) {
	f := startWeirdFake(t, "rate192k")
	defer f.ln.Close()
	c, err := Dial(f.ln.Addr().String(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Synthesize("你好", "v"); err == nil {
		t.Fatal("rate 192000 的合成应报错")
	} else if !strings.Contains(err.Error(), "采样率") {
		t.Fatalf("错误应说明采样率，实际 %v", err)
	}
}
