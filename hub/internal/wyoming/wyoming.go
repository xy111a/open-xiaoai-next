// Package wyoming 是 Wyoming 协议客户端（HA 官方语音协议）。
// 协议格式：每条消息 = 一行 JSON 头（\n 结尾）+ 可选的二进制负载。
// 头里 type 区分消息；payload 长度由头里的字段决定（音频帧直接跟在头后面）。
// STT：transcribe → transcript；TTS：synthesize → audio-start/audio-chunk/audio-stop。
//
// HA 融合（probe.go）：HA 的 Wyoming 集成就是这样一个外部客户端——
// 先 describe 拿服务信息，再发 transcribe / synthesize。ProbeSTT /
// ProbeTTS 就是“假 HA”探针：用与 HA 相同的调用顺序去验证
// Whisper（:10300）与 Piper（:10200）这两个容器；探针不连真 HA、
// 不改容器，只在 NAS 上对 127.0.0.1:10300/10200 跑一次即可。
package wyoming

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"sync"
	"time"
)

// maxUnknownEvents 是 Transcribe/Synthesize 读循环里连续未知事件的上限
// （P2-17）：旧代码遇未知类型只静默循环，全靠 120s 整体超时兜底；
// 现连续 10 个未知事件即报错（服务端行为异常时早失败、不空转）。
const maxUnknownEvents = 10

// expectedSampleRate 是语音管线假定的采样率（P2-48）：
// Piper 应返回 16kHz 单声道 PCM；audio-start 带的 rate 不是 16000
// 即报错，不让变调/变速的语音悄悄播出去。
const expectedSampleRate = 16000

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
// P1-20：同一连接上的读写必须串行——两台设备 + 提醒 goroutine 并发调
// 同一 STT/TTS 连接时，旧代码读写交错（写事件 A 头 + 写事件 B 头再读
// 回复，回复与请求错配）。mu 串行化整轮 Transcribe/Synthesize/Describe。
type Client struct {
	conn   net.Conn
	reader *bufio.Reader
	mu     sync.Mutex
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
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.describeLocked()
}

func (c *Client) describeLocked() (map[string]any, error) {
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
	c.mu.Lock()
	defer c.mu.Unlock()
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
	unknown := 0
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
		default:
			// P2-17：未知事件类型计数，连续超限即报错，不静默空转。
			unknown++
			if unknown == 1 || unknown%maxUnknownEvents == 0 {
				log.Printf("wyoming: transcribe 收到未知事件类型 %v（连续 %d 个）", header["type"], unknown)
			}
			if unknown >= maxUnknownEvents {
				return "", fmt.Errorf("wyoming: transcribe 连续收到 %d 个未知事件类型，服务端行为异常", maxUnknownEvents)
			}
		}
	}
}

// Synthesize 把文本发去 TTS，返回 16kHz 单声道 PCM（小端 int16 字节流）。
// P1-1：整轮设 synthesizeTimeout 上限；累积 PCM 超 MaxSynthesizeTotal 即
// 报错（防服务端无限发 chunk 打满内存）；panic 转错误。
func (c *Client) Synthesize(text, voice string) (pcm []byte, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
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
	unknown := 0
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
			// P2-48：校验合成采样率——audio-start 带 rate 且不是 16kHz
			// 即报错（变调/变速的语音不播）；不带 rate 字段时放行
			// （旧版 Piper 不发 rate，按默认 16kHz 处理）。
			if rate, ok := header["rate"].(float64); ok && int(rate) != expectedSampleRate {
				return nil, fmt.Errorf("wyoming: tts 采样率 %d 不是 %d，拒绝播放", int(rate), expectedSampleRate)
			}
		case "audio-chunk":
			if len(pcm)+len(payload) > MaxSynthesizeTotal {
				return nil, fmt.Errorf("wyoming: tts 音频累积超过 %d 字节上限", MaxSynthesizeTotal)
			}
			pcm = append(pcm, payload...)
		case "audio-stop":
			return pcm, nil
		case "error":
			return nil, fmt.Errorf("wyoming tts error: %v", header["text"])
		default:
			// P2-17：未知事件类型计数，连续超限即报错，不静默空转。
			unknown++
			if unknown == 1 || unknown%maxUnknownEvents == 0 {
				log.Printf("wyoming: synthesize 收到未知事件类型 %v（连续 %d 个）", header["type"], unknown)
			}
			if unknown >= maxUnknownEvents {
				return nil, fmt.Errorf("wyoming: synthesize 连续收到 %d 个未知事件类型，服务端行为异常", maxUnknownEvents)
			}
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

// Dialer 是可重连的 Wyoming 客户端（P1-27）：
// STT/TTS 断线后不再永久 nil——下次调用自动重拨，出错时丢弃旧连接
// 再重拨重试一次。实现 voice.STT 与 voice.TTS 两个接口，main.go 用它
// 代替直连的 *Client，Pipeline 与 speakText 不用改。
// 注意：Dialer 不缓存“服务不可用”状态；下游一直挂时每次调用都试拨
// 一次（拨号超时由 timeout 控制，默认 3s）。
type Dialer struct {
	addr    string
	timeout time.Duration
	mu      sync.Mutex
	client  *Client
}

// NewDialer 新建 Dialer（只记地址，不拨号；首次调用时懒拨号）。
func NewDialer(addr string, timeout time.Duration) *Dialer {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &Dialer{addr: addr, timeout: timeout}
}

func (d *Dialer) get() (*Client, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.client != nil {
		return d.client, nil
	}
	c, err := Dial(d.addr, d.timeout)
	if err != nil {
		return nil, err
	}
	d.client = c
	return c, nil
}

// drop 丢弃指定的旧连接（只在它仍是当前连接时），下次调用重拨。
func (d *Dialer) drop(c *Client) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.client == c {
		_ = c.Close()
		d.client = nil
	}
}

// Close 关闭当前连接（下次调用重拨）。
func (d *Dialer) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.client == nil {
		return nil
	}
	err := d.client.Close()
	d.client = nil
	return err
}

// Transcribe 识别：出错时丢旧连接、重拨重试一次。
// 失败时错误带“STT 不可用”前缀（P1-27：语音管线的降级播报靠此识别）。
func (d *Dialer) Transcribe(pcm []byte, language string) (string, error) {
	c, err := d.get()
	if err != nil {
		return "", fmt.Errorf("STT 不可用（Wyoming 未连接）: %w", err)
	}
	if c == nil {
		return "", fmt.Errorf("STT 不可用（Wyoming 未连接）: 连接为空")
	}
	text, err := c.Transcribe(pcm, language)
	if err == nil {
		return text, nil
	}
	d.drop(c)
	c2, err := d.get()
	if err != nil {
		return "", fmt.Errorf("STT 不可用（Wyoming 未连接）: %w", err)
	}
	if c2 == nil {
		return "", fmt.Errorf("STT 不可用（Wyoming 未连接）: 连接为空")
	}
	text, err = c2.Transcribe(pcm, language)
	if err != nil {
		return "", fmt.Errorf("STT 不可用: %w", err)
	}
	return text, nil
}

// Synthesize 合成：出错时丢旧连接、重拨重试一次。
// 失败时错误带“TTS 不可用”前缀（P1-27：语音管线的降级播报靠此识别）。
func (d *Dialer) Synthesize(text, voice string) ([]byte, error) {
	c, err := d.get()
	if err != nil {
		return nil, fmt.Errorf("TTS 不可用（Wyoming 未连接）: %w", err)
	}
	if c == nil {
		return nil, fmt.Errorf("TTS 不可用（Wyoming 未连接）: 连接为空")
	}
	out, err := c.Synthesize(text, voice)
	if err == nil {
		return out, nil
	}
	d.drop(c)
	c2, err := d.get()
	if err != nil {
		return nil, fmt.Errorf("TTS 不可用（Wyoming 未连接）: %w", err)
	}
	if c2 == nil {
		return nil, fmt.Errorf("TTS 不可用（Wyoming 未连接）: 连接为空")
	}
	out, err = c2.Synthesize(text, voice)
	if err != nil {
		return nil, fmt.Errorf("TTS 不可用: %w", err)
	}
	return out, nil
}

func writeWithDeadline(conn net.Conn, d time.Duration, fn func() error) error {
	_ = conn.SetWriteDeadline(time.Now().Add(d))
	// 清写 deadline：best-effort，失败无处可报（函数已在返回），故显式丢弃错误。
	defer func() { _ = conn.SetWriteDeadline(time.Time{}) }()
	return fn()
}
