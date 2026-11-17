package wyoming

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"testing"
)

// fakeWyoming 是按 Wyoming 协议（JSON 头行 + 可选二进制负载）回话的假服务器，
// 不是 mock 库：它真实走 TCP，客户端真实读写协议帧。
type fakeWyoming struct {
	t    *testing.T
	ln   net.Listener
	done chan struct{}
}

func startFake(t *testing.T) *fakeWyoming {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeWyoming{t: t, ln: ln, done: make(chan struct{})}
	go f.serve()
	return f
}

func (f *fakeWyoming) addr() string { return f.ln.Addr().String() }

func (f *fakeWyoming) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *fakeWyoming) handle(conn net.Conn) {
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
		case "describe":
			f.write(conn, map[string]any{"type": "info", "asr": []any{map[string]any{"name": "whisper", "languages": []string{"zh"}}}, "tts": []any{map[string]any{"name": "piper"}}}, nil)
		case "transcribe":
			// 读掉后续的 audio-start/chunk/stop 再回 transcript。
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
			f.write(conn, map[string]any{"type": "transcript", "text": "打开客厅灯"}, nil)
		case "synthesize":
			f.write(conn, map[string]any{"type": "audio-start", "rate": 16000, "width": 2, "channels": 1}, nil)
			pcm := make([]byte, 320*2) // 20ms 静音 PCM
			for i := range pcm {
				pcm[i] = byte(i % 251)
			}
			f.write(conn, map[string]any{"type": "audio-chunk"}, pcm)
			f.write(conn, map[string]any{"type": "audio-stop"}, nil)
		default:
			f.write(conn, map[string]any{"type": "error", "text": "未知类型"}, nil)
		}
	}
}

func (f *fakeWyoming) write(conn net.Conn, header map[string]any, payload []byte) {
	if payload != nil {
		header["payload_length"] = len(payload)
	}
	b, _ := json.Marshal(header)
	_, _ = conn.Write(append(b, '\n'))
	if payload != nil {
		_, _ = conn.Write(payload)
	}
}

func TestDescribeTranscribeSynthesize(t *testing.T) {
	f := startFake(t)
	defer f.ln.Close()
	c, err := Dial(f.addr(), 0)
	if err != nil {
		t.Fatalf("Dial 假 Wyoming 失败: %v", err)
	}
	defer c.Close()

	info, err := c.Describe()
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if info["type"] != "info" {
		t.Fatalf("Describe 应返回 info，实际 %v", info["type"])
	}

	pcm := make([]byte, 16000*2) // 1 秒 16kHz PCM
	text, err := c.Transcribe(pcm, "zh")
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if text != "打开客厅灯" {
		t.Fatalf("Transcribe 应返回“打开客厅灯”，实际 %q", text)
	}

	out, err := c.Synthesize("好的", "zh_CN-huayan-medium")
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if len(out) != 640 {
		t.Fatalf("Synthesize 应返回 640 字节 PCM，实际 %d", len(out))
	}
}

func TestPCMConversion(t *testing.T) {
	orig := []int16{0, 1, -1, 32767, -32768, 12345}
	b := Int16ToPCM(orig)
	back := PCMToInt16(b)
	if len(back) != len(orig) {
		t.Fatalf("往返长度不对: %d vs %d", len(back), len(orig))
	}
	for i, v := range orig {
		if back[i] != v {
			t.Fatalf("第 %d 个采样往返不对: %d vs %d", i, back[i], v)
		}
	}
	if got := PCMToInt16([]byte{0x01}); len(got) != 0 {
		t.Fatalf("奇数长度字节流应丢弃尾字节，实际 %v", got)
	}
}
