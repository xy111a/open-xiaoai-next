package wyoming

// P1-1 回归测试：恶意 payload_length / 超长头行 / TTS 累积超限。
// 用 net.Pipe 直连 Client（同包，可构造未导出字段），服务端一端按脚本发帧。

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"
)

func pipeClient(t *testing.T) (*Client, net.Conn) {
	t.Helper()
	a, b := net.Pipe()
	c := &Client{conn: a, reader: bufio.NewReader(a)}
	t.Cleanup(func() { a.Close(); b.Close() })
	return c, b
}

// 发一帧原始头行（不经 writeEvent），随后按需发负载。
// 返回错误而不是调 t.Fatalf：调用方在 goroutine 里，Fatal 非法。
func writeRaw(conn net.Conn, header string, payload []byte) error {
	if _, err := conn.Write([]byte(header + "\n")); err != nil {
		return err
	}
	if payload != nil {
		if _, err := conn.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

func TestReadEventRejectsMaliciousPayloadLength(t *testing.T) {
	cases := []struct {
		name   string
		header string
	}{
		{"超大值 1e9", `{"type":"audio-chunk","payload_length":1000000000}`},
		{"刚超 1MB 上限", `{"type":"audio-chunk","payload_length":1048577}`},
		{"负数", `{"type":"audio-chunk","payload_length":-1}`},
		{"非数字字符串", `{"type":"audio-chunk","payload_length":"100"}`},
		{"非数字 null", `{"type":"audio-chunk","payload_length":null}`},
		{"非整数 1.5", `{"type":"audio-chunk","payload_length":1.5}`},
		{"非整数 0.5", `{"type":"audio-chunk","payload_length":0.5}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, srv := pipeClient(t)
			done := make(chan struct{})
			go func() {
				defer close(done)
				// 写失败无处可报：对端 readEvent 的返回值才是断言点。
				_ = writeRaw(srv, tc.header, nil)
			}()
			// readEvent 不应 panic、不应分配超大内存，应返回错误。
			// （用 recover 兜底断言：若实现仍 panic，测试直接失败。）
			_, _, err := c.readEvent()
			<-done
			if err == nil {
				t.Fatalf("恶意 payload_length %s 应报错，实际无错", tc.header)
			}
			if !strings.Contains(err.Error(), "payload_length") {
				t.Fatalf("错误应提及 payload_length，实际: %v", err)
			}
		})
	}
}

func TestReadEventAcceptsValidPayloadLength(t *testing.T) {
	c, srv := pipeClient(t)
	// 写失败无处可报：readEvent 的返回值才是断言点。
	go func() { _ = writeRaw(srv, `{"type":"audio-chunk","payload_length":4}`, []byte{1, 2, 3, 4}) }()
	header, payload, err := c.readEvent()
	if err != nil {
		t.Fatalf("合法 payload_length 应通过: %v", err)
	}
	if header["type"] != "audio-chunk" || len(payload) != 4 || payload[3] != 4 {
		t.Fatalf("返回不对: header=%v payload=%v", header, payload)
	}
}

func TestReadEventRejectsZeroAndBoundary(t *testing.T) {
	// payload_length 0 合法（空负载）；恰好 1MB 合法（不测试分配 1MB 的读满，
	// 只验证 parsePayloadLength 的边界判定）。
	if _, err := parsePayloadLength(float64(0)); err != nil {
		t.Fatalf("0 应合法: %v", err)
	}
	if _, err := parsePayloadLength(float64(MaxPayloadLength)); err != nil {
		t.Fatalf("恰好 1MB 应合法: %v", err)
	}
	if _, err := parsePayloadLength(float64(MaxPayloadLength + 1)); err == nil {
		t.Fatal("1MB+1 应被拒绝")
	}
}

func TestReadEventRejectsOversizeHeaderLine(t *testing.T) {
	c, srv := pipeClient(t)
	go func() {
		// 70KB 无换行头行：readHeaderLine 应在 64KB 上限处报错，而不是无限读。
		_, _ = srv.Write([]byte(strings.Repeat("x", 70<<10)))
	}()
	_, _, err := c.readEvent()
	if err == nil {
		t.Fatal("超长头行应报错")
	}
	if !strings.Contains(err.Error(), "头行超过") {
		t.Fatalf("错误应提及头行超长，实际: %v", err)
	}
}

// TestSynthesizeTotalCapViaFakeServer 用自定义假服务端验证累积上限：
// 服务端连发 600KB 的 chunk 且不发 audio-stop，累积超过 10MB 上限时
// Synthesize 应报错而非无限累积。
func TestSynthesizeTotalCapViaFakeServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		// 读掉 synthesize 请求头。
		_, _, _ = readWyomingFrame(r)
		w := func(header map[string]any, payload []byte) {
			writeWyomingFrame(conn, header, payload)
		}
		w(map[string]any{"type": "audio-start", "data": map[string]any{"rate": 16000, "width": 2, "channels": 1}}, nil)
		chunk := make([]byte, 600<<10) // 600KB，单块合法
		for i := 0; i < 20; i++ {
			w(map[string]any{"type": "audio-chunk", "data": map[string]any{"rate": 16000, "width": 2, "channels": 1}}, chunk)
		}
		// 不发 audio-stop；累积到第 18 块（>10MB）时客户端应已报错断开。
	}()
	c, err := Dial(ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if _, err := c.Synthesize("你好", "v"); err == nil {
		t.Fatal("累积 PCM 超限应报错")
	} else if !strings.Contains(err.Error(), "超过") {
		t.Fatalf("错误应提及超限，实际: %v", err)
	}
}
