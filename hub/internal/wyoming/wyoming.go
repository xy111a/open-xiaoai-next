// Package wyoming 是 Wyoming 协议客户端（HA 官方语音协议）。
// 协议格式：每条消息 = 一行 JSON 头（\n 结尾）+ 可选的二进制负载。
// 头里 type 区分消息；payload 长度由头里的字段决定（音频帧直接跟在头后面）。
// STT：transcribe → transcript；TTS：synthesize → audio-start/audio-chunk/audio-stop。
package wyoming

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"time"
)

// P1-1 防护上限：单条 Wyoming 负载（音频 chunk）最大 1MB；
// 头行（JSON）最大 64KB；读写各设 10s deadline。
const (
	// MaxPayloadLength 是单条 Wyoming 事件负载的上限，超过即断开并报错。
	MaxPayloadLength = 1 << 20
	// MaxHeaderLineLength 是 Wyoming 头行（JSON + \n）的上限。
	MaxHeaderLineLength = 64 << 10
	// MaxSynthesizeTotal 是单次 Synthesize 累积 PCM 的上限（约 5 分钟语音）。
	MaxSynthesizeTotal = 10 << 20
)

const readEventTimeout = 10 * time.Second

// 整体超时（var 而非 const，供测试缩短；生产值见注释）。
var (
	transcribeTimeout = 120 * time.Second // Transcribe 整轮上限
	synthesizeTimeout = 120 * time.Second // Synthesize 整轮上限
)

// Client 是一个 Wyoming 连接。
type Client struct {
	conn   net.Conn
	reader *bufio.Reader
}

// Dial 连上 Wyoming 服务（STT :10300 / TTS :10200）。
func Dial(addr string, timeout time.Duration) (*Client, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn, reader: bufio.NewReader(conn)}, nil
}

// Close 关闭连接。
func (c *Client) Close() error {
	return c.conn.Close()
}

// readEvent 读一条事件：JSON 头 + 可选负载。
// 返回头 map 与负载 bytes。音频负载是原始 PCM（小端 int16）。
// P1-1：全程设 10s 读写 deadline；头行限长读取；payload_length 经
// parsePayloadLength 校验（非数字/负数/非整数/超 1MB 一律报错，调用方
// 收到错误后应断开连接——本函数只返回错误，不自行 Close）。
func (c *Client) readEvent() (map[string]any, []byte, error) {
	if err := c.conn.SetReadDeadline(time.Now().Add(readEventTimeout)); err != nil {
		return nil, nil, err
	}
	// 清读 deadline：best-effort，失败无处可报（函数已在返回；
	// 连接关闭时 deadline 随之失效），故显式丢弃错误。
	defer func() { _ = c.conn.SetReadDeadline(time.Time{}) }()
	line, err := c.readHeaderLine()
	if err != nil {
		return nil, nil, err
	}
	var header map[string]any
	if err := json.Unmarshal(line, &header); err != nil {
		return nil, nil, fmt.Errorf("wyoming: 头解析失败: %w", err)
	}
	var payload []byte
	if l, ok := header["payload_length"]; ok {
		n, err := parsePayloadLength(l)
		if err != nil {
			return nil, nil, err
		}
		payload = make([]byte, n)
		if _, err := io.ReadFull(c.reader, payload); err != nil {
			return nil, nil, err
		}
	}
	return header, payload, nil
}

// readHeaderLine 限长读一行（JSON 头 + \n），超过 MaxHeaderLineLength 即报错。
func (c *Client) readHeaderLine() ([]byte, error) {
	var line []byte
	for {
		b, err := c.reader.ReadByte()
		if err != nil {
			return nil, err
		}
		if len(line) >= MaxHeaderLineLength {
			return nil, fmt.Errorf("wyoming: 头行超过 %d 字节上限", MaxHeaderLineLength)
		}
		line = append(line, b)
		if b == '\n' {
			return line, nil
		}
	}
}

// parsePayloadLength 校验 Wyoming 头里的 payload_length：
// 必须是 JSON 数字、非负、整数，且 ≤ MaxPayloadLength。
func parsePayloadLength(v any) (int, error) {
	f, ok := v.(float64)
	if !ok {
		return 0, fmt.Errorf("wyoming: payload_length 不是数字: %T", v)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("wyoming: payload_length 不是有限数字: %v", f)
	}
	if f < 0 {
		return 0, fmt.Errorf("wyoming: payload_length 为负: %v", f)
	}
	if f != math.Trunc(f) {
		return 0, fmt.Errorf("wyoming: payload_length 不是整数: %v", f)
	}
	if f > MaxPayloadLength {
		return 0, fmt.Errorf("wyoming: payload_length 超限: %v > %d", f, MaxPayloadLength)
	}
	return int(f), nil
}

// writeEvent 发一条事件。P1-1：每次写设 10s deadline，写死等不再无限挂起。
func (c *Client) writeEvent(header map[string]any, payload []byte) error {
	if err := c.conn.SetWriteDeadline(time.Now().Add(readEventTimeout)); err != nil {
		return err
	}
	// 清写 deadline：best-effort，失败无处可报（函数已在返回；
	// 连接关闭时 deadline 随之失效），故显式丢弃错误。
	defer func() { _ = c.conn.SetWriteDeadline(time.Time{}) }()
	b, err := json.Marshal(header)
	if err != nil {
		return err
	}
	if payload != nil {
		header["payload_length"] = len(payload)
		b, err = json.Marshal(header)
		if err != nil {
			return err
		}
	}
	if _, err := c.conn.Write(append(b, '\n')); err != nil {
		return err
	}
	if payload != nil {
		if _, err := c.conn.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// Describe 拿服务信息（模型、语言等），用于连通性检查。
func (c *Client) Describe() (map[string]any, error) {
	if err := writeWithDeadline(c.conn, 5*time.Second, func() error {
		return c.writeEvent(map[string]any{"type": "describe"}, nil)
	}); err != nil {
		return nil, err
	}
	header, _, err := c.readEvent()
	if err != nil {
		return nil, err
	}
	if header["type"] != "info" {
		return nil, fmt.Errorf("wyoming: describe 期望 info，实际 %v", header["type"])
	}
	return header, nil
}

// Transcribe 把一段 16kHz 单声道 PCM 发去 STT，返回识别文本。
// pcm 是小端 int16 字节流；name/language 按 faster-whisper 约定。
// P1-1：整轮设 transcribeTimeout 上限；panic 转错误（不崩 Hub）。
func (c *Client) Transcribe(pcm []byte, language string) (text string, err error) {
	defer func() {
		if r := recover(); r != nil {
			text = ""
			err = fmt.Errorf("wyoming: Transcribe panic 已兜底: %v", r)
		}
	}()
	deadline := time.Now().Add(transcribeTimeout)
	if err := c.writeEvent(map[string]any{
		"type": "transcribe", "name": "whisper", "language": language,
	}, nil); err != nil {
		return "", err
	}
	// 音频帧：每条 audio-start/audio-chunk/audio-stop；这里一次发完。
	if err := c.writeEvent(map[string]any{"type": "audio-start", "rate": 16000, "width": 2, "channels": 1}, nil); err != nil {
		return "", err
	}
	const chunk = 4096
	for len(pcm) > 0 {
		n := chunk
		if len(pcm) < n {
			n = len(pcm)
		}
		if err := c.writeEvent(map[string]any{"type": "audio-chunk"}, pcm[:n]); err != nil {
			return "", err
		}
		pcm = pcm[n:]
	}
	if err := c.writeEvent(map[string]any{"type": "audio-stop"}, nil); err != nil {
		return "", err
	}
	for {
		if time.Now().After(deadline) {
			return "", fmt.Errorf("wyoming: transcribe 超过 %v 未返回 transcript", transcribeTimeout)
		}
		header, _, err := c.readEvent()
		if err != nil {
			return "", err
		}
		switch header["type"] {
		case "transcript":
			text, _ := header["text"].(string)
			return text, nil
		case "error":
			return "", fmt.Errorf("wyoming stt error: %v", header["text"])
		}
	}
}

// Synthesize 把文本发去 TTS，返回 16kHz 单声道 PCM（小端 int16 字节流）。
// P1-1：整轮设 synthesizeTimeout 上限；累积 PCM 超 MaxSynthesizeTotal 即
// 报错（防服务端无限发 chunk 打满内存）；panic 转错误。
func (c *Client) Synthesize(text, voice string) (pcm []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			pcm = nil
			err = fmt.Errorf("wyoming: Synthesize panic 已兜底: %v", r)
		}
	}()
	deadline := time.Now().Add(synthesizeTimeout)
	if err := c.writeEvent(map[string]any{
		"type": "synthesize", "text": text, "voice": map[string]any{"name": voice},
	}, nil); err != nil {
		return nil, err
	}
	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("wyoming: synthesize 超过 %v 未返回 audio-stop", synthesizeTimeout)
		}
		header, payload, err := c.readEvent()
		if err != nil {
			return nil, err
		}
		switch header["type"] {
		case "audio-start":
			// 忽略，继续收 chunk。
		case "audio-chunk":
			if len(pcm)+len(payload) > MaxSynthesizeTotal {
				return nil, fmt.Errorf("wyoming: tts 音频累积超过 %d 字节上限", MaxSynthesizeTotal)
			}
			pcm = append(pcm, payload...)
		case "audio-stop":
			return pcm, nil
		case "error":
			return nil, fmt.Errorf("wyoming tts error: %v", header["text"])
		}
	}
}

// PCMToInt16 把小端 int16 字节流转成采样点（用 binary.LittleEndian，避免手写位运算出错）。
func PCMToInt16(b []byte) []int16 {
	out := make([]int16, len(b)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
	}
	return out
}

// Int16ToPCM 把采样点转成小端 int16 字节流。
func Int16ToPCM(pcm []int16) []byte {
	out := make([]byte, len(pcm)*2)
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(out[2*i:], uint16(v))
	}
	return out
}

func writeWithDeadline(conn net.Conn, d time.Duration, fn func() error) error {
	_ = conn.SetWriteDeadline(time.Now().Add(d))
	// 清写 deadline：best-effort，失败无处可报（函数已在返回），故显式丢弃错误。
	defer func() { _ = conn.SetWriteDeadline(time.Time{}) }()
	return fn()
}
