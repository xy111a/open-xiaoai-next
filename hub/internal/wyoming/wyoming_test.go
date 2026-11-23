package wyoming

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"sync"
	"testing"
)

// writeWyomingFrame 按真实 wire 格式（参考库 async_write_event）写一帧：
// 头行只含 type/version/data_length/payload_length，data JSON 紧跟头行，
// payload 最后。测试假服务器用它发帧，与客户端的 writeEvent 各自独立实现，
// 避免两侧共用同一段错误逻辑互相掩护（最终以真实服务端端到端为准）。
func writeWyomingFrame(conn net.Conn, header map[string]any, payload []byte) {
	var dataBytes []byte
	if data, ok := header["data"].(map[string]any); ok && len(data) > 0 {
		dataBytes, _ = json.Marshal(data)
	}
	line := make(map[string]any, len(header)+2)
	for k, v := range header {
		if k == "data" {
			continue
		}
		line[k] = v
	}
	line["version"] = wyomingVersion
	if dataBytes != nil {
		line["data_length"] = len(dataBytes)
	}
	if payload != nil {
		line["payload_length"] = len(payload)
	}
	b, _ := json.Marshal(line)
	_, _ = conn.Write(append(b, '\n'))
	if dataBytes != nil {
		_, _ = conn.Write(dataBytes)
	}
	if payload != nil {
		_, _ = conn.Write(payload)
	}
}

// readWyomingFrame 按真实 wire 格式读一帧：头行 + data_length 分离字节
// （merge 进 header["data"]，分离字节优先）+ 丢弃 payload。
// 返回原始头行（wire 格式断言用）与 merge 后的头。
func readWyomingFrame(r *bufio.Reader) (string, map[string]any, error) {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return "", nil, err
	}
	var header map[string]any
	if err := json.Unmarshal(line, &header); err != nil {
		return "", nil, err
	}
	if n, ok := header["data_length"].(float64); ok && n > 0 {
		dataBytes := make([]byte, int(n))
		if _, err := io.ReadFull(r, dataBytes); err != nil {
			return "", nil, err
		}
		var data map[string]any
		if err := json.Unmarshal(dataBytes, &data); err != nil {
			return "", nil, err
		}
		merged, _ := header["data"].(map[string]any)
		if merged == nil {
			merged = make(map[string]any, len(data))
		}
		for k, v := range data {
			merged[k] = v
		}
		header["data"] = merged
	}
	if n, ok := header["payload_length"].(float64); ok && n > 0 {
		if _, err := io.CopyN(io.Discard, r, int64(n)); err != nil {
			return "", nil, err
		}
	}
	return string(line), header, nil
}

// fakeWyoming 是按 Wyoming 协议（JSON 头行 + 可选二进制负载）回话的假服务器，
// 不是 mock 库：它真实走 TCP，客户端真实读写协议帧。
// 关键：它必须说正确的 wire 方言——头行只含 type/version/data_length/
// payload_length，data 以分离 JSON 字节紧跟头行（与 Python 参考库一致）。
// 2026-11-23 彩排 P0-1 的教训：旧假服务器说的是顶层字段的错误方言，
// 单测全过、真服务端全灭；回归测试必须在假服务器上钉死 wire 格式。
type fakeWyoming struct {
	t        *testing.T
	ln       net.Listener
	done     chan struct{}
	mu       sync.Mutex
	received []map[string]any // 客户端发来的全部头（data 已 merge，服务端逻辑视角）
	rawLines []string         // 客户端发来的原始头行（wire 格式断言用）
}

// receivedHeaders 返回客户端发来的头快照（按收到顺序，data 已 merge）。
func (f *fakeWyoming) receivedHeaders() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, len(f.received))
	copy(out, f.received)
	return out
}

// receivedRawLines 返回客户端发来的原始头行快照（按收到顺序）。
func (f *fakeWyoming) receivedRawLines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.rawLines))
	copy(out, f.rawLines)
	return out
}

func (f *fakeWyoming) record(raw string, header map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rawLines = append(f.rawLines, raw)
	f.received = append(f.received, header)
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
		raw, header, err := readWyomingFrame(r)
		if err != nil {
			return
		}
		f.record(raw, header)
		switch header["type"] {
		case "describe":
			f.write(conn, map[string]any{"type": "info", "data": map[string]any{"asr": []any{map[string]any{"name": "whisper", "languages": []string{"zh"}}}, "tts": []any{map[string]any{"name": "piper"}}}}, nil)
		case "transcribe":
			// 读掉后续的 audio-start/chunk/stop 再回 transcript。
			for {
				raw2, h2, err := readWyomingFrame(r)
				if err != nil {
					return
				}
				f.record(raw2, h2)
				if h2["type"] == "audio-stop" {
					break
				}
			}
			f.write(conn, map[string]any{"type": "transcript", "data": map[string]any{"text": "打开客厅灯"}}, nil)
		case "synthesize":
			f.write(conn, map[string]any{"type": "audio-start", "data": map[string]any{"rate": 16000, "width": 2, "channels": 1}}, nil)
			pcm := make([]byte, 320*2) // 20ms 静音 PCM
			for i := range pcm {
				pcm[i] = byte(i % 251)
			}
			f.write(conn, map[string]any{"type": "audio-chunk", "data": map[string]any{"rate": 16000, "width": 2, "channels": 1}}, pcm)
			f.write(conn, map[string]any{"type": "audio-stop", "data": map[string]any{}}, nil)
		default:
			f.write(conn, map[string]any{"type": "error", "data": map[string]any{"text": "未知类型"}}, nil)
		}
	}
}

func (f *fakeWyoming) write(conn net.Conn, header map[string]any, payload []byte) {
	writeWyomingFrame(conn, header, payload)
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
	// Describe 返回 info 事件的 data，不再含 type 顶层字段。
	if _, ok := info["asr"]; !ok {
		t.Fatalf("Describe 应返回含 asr 的 info data，实际 %v", info)
	}
	if _, ok := info["type"]; ok {
		t.Fatalf("Describe 返回的应是 data，不应含 type，实际 %v", info)
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
