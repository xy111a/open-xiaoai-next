package wyoming

// Wire 格式回归测试（2026-11-23 彩排 P0-1）：
// 客户端发出的帧必须与 Python 参考库 wyoming 1.11.0 的 wire 格式一致——
// 头行只含 type/version/data_length/payload_length，data 以 data_length
// 分离 JSON 字节紧跟头行，payload 最后；audio-start/audio-chunk 的 data
// 必须含 rate/width/channels（服务端 AudioChunk.from_event 读
// event.data["rate"]，缺了即 KeyError）。
// 读侧必须按 data_length 消费分离字节并从 data 取值；旧的顶层字段格式
// 不再被读取。

import (
	"bufio"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// dataOf 模拟 Python 参考库 Event.from_dict：取 header["data"]。
func dataOf(header map[string]any) map[string]any {
	data, _ := header["data"].(map[string]any)
	return data
}

func TestWireFormatTranscribe(t *testing.T) {
	f := startFake(t)
	defer f.ln.Close()
	c, err := Dial(f.addr(), 3*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	// 先 describe：请求头也必须带 data（空对象）。
	if _, err := c.Describe(); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	// 1 秒 PCM = 8 个 4096 字节 chunk；audio-start + 8 chunk + audio-stop。
	pcm := make([]byte, 16000*2)
	text, err := c.Transcribe(pcm, "zh")
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if text != "打开客厅灯" {
		t.Fatalf("Transcribe 应返回“打开客厅灯”，实际 %q", text)
	}

	got := f.receivedHeaders()
	// describe + transcribe + audio-start + 8 chunk + audio-stop。
	if len(got) != 12 {
		t.Fatalf("应收到 12 条头行，实际 %d: %v", len(got), got)
	}
	// 像 Python from_dict 一样逐条校验 data 嵌套。
	assertData := func(i int, typ string, want map[string]any) {
		t.Helper()
		h := got[i]
		if h["type"] != typ {
			t.Fatalf("第 %d 条应为 %s，实际 %v", i, typ, h)
		}
		data := dataOf(h)
		if len(want) > 0 && data == nil {
			t.Fatalf("第 %d 条 %s 缺 data 对象（旧顶层字段格式），实际 %v", i, typ, h)
		}
		// data 为空时参考库不发 data_length，data 缺失与空对象等价。
		for k, v := range want {
			if data[k] != v {
				t.Fatalf("第 %d 条 %s 的 data[%q] 应为 %v，实际 %v（完整头 %v）", i, typ, k, v, data[k], h)
			}
		}
		// data 之外顶层只允许 type、version、data_length、payload_length。
		for k := range h {
			if k != "type" && k != "data" && k != "payload_length" && k != "data_length" && k != "version" {
				t.Fatalf("第 %d 条 %s 顶层不应有字段 %q（应进 data），实际 %v", i, typ, k, h)
			}
		}
	}
	assertData(0, "describe", map[string]any{})
	assertData(1, "transcribe", map[string]any{"name": "whisper", "language": "zh"})
	assertData(2, "audio-start", map[string]any{"rate": float64(16000), "width": float64(2), "channels": float64(1)})
	for i := 3; i < 11; i++ {
		assertData(i, "audio-chunk", map[string]any{"rate": float64(16000), "width": float64(2), "channels": float64(1)})
		if _, ok := got[i]["payload_length"]; !ok {
			t.Fatalf("第 %d 条 audio-chunk 顶层应有 payload_length，实际 %v", i, got[i])
		}
	}
	assertData(11, "audio-stop", map[string]any{})

	// 原始头行必须是真实 wire 形：data 以 data_length 分离字节发送，
	// 头行里没有内联 "data"；data 为空的帧（describe/audio-stop）不带 data_length。
	raw := f.receivedRawLines()
	if len(raw) != 12 {
		t.Fatalf("应收到 12 条原始头行，实际 %d", len(raw))
	}
	assertRaw := func(i int, wantDataLength bool) {
		t.Helper()
		hasDL := strings.Contains(raw[i], `"data_length"`)
		hasInlineData := strings.Contains(raw[i], `"data":`)
		if hasDL != wantDataLength {
			t.Fatalf("第 %d 条原始头行 data_length 应为 %v，实际 %q", i, wantDataLength, raw[i])
		}
		if hasInlineData {
			t.Fatalf("第 %d 条原始头行不应含内联 data，实际 %q", i, raw[i])
		}
	}
	assertRaw(0, false) // describe：data 为空
	assertRaw(1, true)  // transcribe
	assertRaw(2, true)  // audio-start
	for i := 3; i < 11; i++ {
		assertRaw(i, true) // audio-chunk
		if !strings.Contains(raw[i], `"payload_length"`) {
			t.Fatalf("第 %d 条原始头行应含 payload_length，实际 %q", i, raw[i])
		}
	}
	assertRaw(11, false) // audio-stop：data 为空
}

func TestWireFormatSynthesize(t *testing.T) {
	f := startFake(t)
	defer f.ln.Close()
	c, err := Dial(f.addr(), 3*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	out, err := c.Synthesize("你好", "zh_CN-huayan-medium")
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if len(out) != 640 {
		t.Fatalf("Synthesize 应返回 640 字节 PCM，实际 %d", len(out))
	}

	got := f.receivedHeaders()
	if len(got) != 1 {
		t.Fatalf("应只收到 1 条 synthesize 请求头，实际 %v", got)
	}
	h := got[0]
	if h["type"] != "synthesize" {
		t.Fatalf("请求头 type 应为 synthesize，实际 %v", h)
	}
	data := dataOf(h)
	if data == nil {
		t.Fatalf("synthesize 请求头缺 data 对象，实际 %v", h)
	}
	if data["text"] != "你好" {
		t.Fatalf("synthesize 的 data.text 应为“你好”，实际 %v", data["text"])
	}
	voice, _ := data["voice"].(map[string]any)
	if voice == nil || voice["name"] != "zh_CN-huayan-medium" {
		t.Fatalf("synthesize 的 data.voice.name 应为 zh_CN-huayan-medium，实际 %v", data["voice"])
	}
}

// replyOnceServer 是“读一轮请求、回一帧”的最小服务端：
// 读到 stopType 后按真实 wire 格式回 reply 头，用于断言读侧行为。
func replyOnceServer(t *testing.T, stopType string, reply map[string]any) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		for {
			_, header, err := readWyomingFrame(r)
			if err != nil {
				return
			}
			if header["type"] == stopType {
				break
			}
		}
		writeWyomingFrame(conn, reply, nil)
	}()
	return ln.Addr().String()
}

// replyOnceServerRaw 与 replyOnceServer 相同，但把 reply 原样写成
// 一行头（无 data_length 分离字节），用于模拟旧格式服务端。
func replyOnceServerRaw(t *testing.T, stopType string, reply map[string]any) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		for {
			_, header, err := readWyomingFrame(r)
			if err != nil {
				return
			}
			if header["type"] == stopType {
				break
			}
		}
		b, _ := json.Marshal(reply)
		_, _ = conn.Write(append(b, '\n'))
	}()
	return ln.Addr().String()
}

// TestWireFormatErrorTextFromData：error 事件的 text 在 data 里；
// 旧代码读顶层 text 得到 nil，报出“wyoming tts error: <nil>”（彩排实测）。
func TestWireFormatErrorTextFromData(t *testing.T) {
	addr := replyOnceServer(t, "synthesize", map[string]any{
		"type": "error", "data": map[string]any{"text": "voice not found"},
	})
	c, err := Dial(addr, 3*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	_, err = c.Synthesize("你好", "no-such-voice")
	if err == nil {
		t.Fatal("error 事件应让 Synthesize 报错")
	}
	if !strings.Contains(err.Error(), "voice not found") {
		t.Fatalf("错误应含服务端 data.text，实际: %v", err)
	}
	if strings.Contains(err.Error(), "<nil>") {
		t.Fatalf("错误不应再出现 <nil>（旧顶层读法），实际: %v", err)
	}
}

// TestWireFormatTopLevelFieldsIgnored：旧格式（字段在顶层、无 data）
// 不再被读取——transcript 顶层 text 应被忽略，返回空串。
// 这是协议变更的钉子：服务端只认 data，客户端也只读 data。
func TestWireFormatTopLevelFieldsIgnored(t *testing.T) {
	addr := replyOnceServerRaw(t, "audio-stop", map[string]any{
		"type": "transcript", "text": "顶层旧格式",
	})
	c, err := Dial(addr, 3*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	text, err := c.Transcribe(make([]byte, 3200), "zh")
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if text != "" {
		t.Fatalf("顶层 text 应被忽略、返回空串，实际 %q", text)
	}
}

// TestWireFormatDescribeRequest：describe 请求头带空 data 对象。
func TestWireFormatDescribeRequest(t *testing.T) {
	f := startFake(t)
	defer f.ln.Close()
	c, err := Dial(f.addr(), 3*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if _, err := c.Describe(); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	got := f.receivedHeaders()
	if len(got) != 1 || got[0]["type"] != "describe" {
		t.Fatalf("应只收到 1 条 describe 头，实际 %v", got)
	}
	// describe 的 data 为空对象：参考库不发 data_length，data 缺失与空对象等价。
	if len(dataOf(got[0])) != 0 {
		t.Fatalf("describe 请求头的 data 应为空，实际 %v", got[0])
	}
}

// TestReadEventDataLength：readEvent 按 data_length 消费分离字节并 merge。
func TestReadEventDataLength(t *testing.T) {
	// 分离 data 正常 merge，且分离字节优先于头行内联 data。
	c, srv := pipeClient(t)
	dataJSON := `{"text":"分离优先","extra":1}`
	header := `{"type":"transcript","data":{"text":"内联"},"data_length":` + strconv.Itoa(len(dataJSON)) + `}`
	go func() {
		_, _ = srv.Write([]byte(header + "\n" + dataJSON))
	}()
	h, _, err := c.readEvent()
	if err != nil {
		t.Fatalf("readEvent: %v", err)
	}
	if dataOf(h)["text"] != "分离优先" {
		t.Fatalf("分离 data 应优先于内联 data，实际 %v", h)
	}
	if dataOf(h)["extra"] != float64(1) {
		t.Fatalf("分离 data 应完整 merge，实际 %v", h)
	}

	// 只有分离 data、头行无内联 data 时同样 merge。
	c2, srv2 := pipeClient(t)
	go func() {
		_, _ = srv2.Write([]byte(`{"type":"info","data_length":22}` + "\n" + `{"asr":[{"name":"w"}]}`))
	}()
	h2, _, err := c2.readEvent()
	if err != nil {
		t.Fatalf("readEvent: %v", err)
	}
	asr, _ := dataOf(h2)["asr"].([]any)
	if len(asr) != 1 {
		t.Fatalf("分离 data 应 merge 进 header，实际 %v", h2)
	}
}

// TestReadEventRejectsMaliciousDataLength：恶意 data_length 应报错。
func TestReadEventRejectsMaliciousDataLength(t *testing.T) {
	cases := []struct {
		name   string
		header string
	}{
		{"超大值 1e9", `{"type":"info","data_length":1000000000}`},
		{"刚超 1MB 上限", `{"type":"info","data_length":1048577}`},
		{"负数", `{"type":"info","data_length":-1}`},
		{"非数字字符串", `{"type":"info","data_length":"100"}`},
		{"非整数 1.5", `{"type":"info","data_length":1.5}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, srv := pipeClient(t)
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = writeRaw(srv, tc.header, nil)
			}()
			_, _, err := c.readEvent()
			<-done
			if err == nil {
				t.Fatalf("恶意 data_length %s 应报错，实际无错", tc.header)
			}
			if !strings.Contains(err.Error(), "data_length") {
				t.Fatalf("错误应提及 data_length，实际: %v", err)
			}
		})
	}
}

// TestReadEventRejectsBadDataJSON：data_length 指向的字节不是合法 JSON 时报错。
func TestReadEventRejectsBadDataJSON(t *testing.T) {
	c, srv := pipeClient(t)
	go func() {
		_, _ = srv.Write([]byte("{\"type\":\"info\",\"data_length\":5}\nnot-json"))
	}()
	if _, _, err := c.readEvent(); err == nil {
		t.Fatal("非法 data JSON 应报错")
	} else if !strings.Contains(err.Error(), "data 解析失败") {
		t.Fatalf("错误应提及 data 解析失败，实际: %v", err)
	}
}

// TestResamplePCM16：线性插值重采样（22050→16000 是真实 Piper 的情况）。
func TestResamplePCM16(t *testing.T) {
	// 同采样率原样返回（同一底层数组，不复制）。
	pcm := Int16ToPCM([]int16{100, 200, 300})
	if got := resamplePCM16(pcm, 16000, 16000); string(got) != string(pcm) {
		t.Fatalf("同采样率应原样返回，实际 %v", got)
	}
	// 8000→16000：采样数翻倍，时长不变；线性插值中点。
	in := Int16ToPCM([]int16{0, 1000})
	out := PCMToInt16(resamplePCM16(in, 8000, 16000))
	if len(out) != 4 {
		t.Fatalf("8000→16000 应 2 采样变 4 采样，实际 %d", len(out))
	}
	if out[0] != 0 || out[1] != 500 || out[2] != 1000 || out[3] != 1000 {
		t.Fatalf("线性插值不对，实际 %v", out)
	}
	// 22050→16000：2205 采样 → 1600 采样。
	in22050 := make([]byte, 2205*2)
	if got := resamplePCM16(in22050, 22050, 16000); len(got) != 1600*2 {
		t.Fatalf("22050→16000 应 2205 采样变 1600 采样，实际 %d 字节", len(got))
	}
	// 空输入返回空/ nil，不 panic。
	if got := resamplePCM16(nil, 22050, 16000); len(got) != 0 {
		t.Fatalf("空输入应返回空，实际 %d 字节", len(got))
	}
}

// TestEventDataHelper：data 缺失/类型错误时不 panic，返回 nil。
func TestEventDataHelper(t *testing.T) {
	if got := eventData(map[string]any{"type": "transcript"}); got != nil {
		t.Fatalf("无 data 时应返回 nil，实际 %v", got)
	}
	if got := eventData(map[string]any{"type": "transcript", "data": "not-a-map"}); got != nil {
		t.Fatalf("data 不是对象时应返回 nil，实际 %v", got)
	}
	if got := eventData(map[string]any{"type": "transcript", "data": map[string]any{"text": "x"}}); got["text"] != "x" {
		t.Fatalf("data 应原样返回，实际 %v", got)
	}
	// nil map 索引不 panic（读侧对缺 data 的容错依赖此行为）。
	var nilData map[string]any
	if _, ok := nilData["text"]; ok {
		t.Fatal("nil map 不应有字段")
	}
}
