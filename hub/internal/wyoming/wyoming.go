// Package wyoming 是 Wyoming 协议客户端（HA 官方语音协议）。
// Wire 格式（Python 参考库 wyoming 1.11.0 的 async_write_event /
// async_read_event，一行 JSON 头 + 分离的 data JSON + 可选二进制负载）：
//
//	{"type": ..., "version": "1.11.0", "data_length": N, "payload_length": M}\n
//	<N 字节 data JSON><M 字节 payload>
//
// data 不在头行里：写侧把 header["data"] 序列化后作为 data_length 字节
// 紧跟头行；读侧按 data_length 读回 data JSON 再 merge 进头（头行里若
// 也有内联 data，分离字节优先）。data 为空时不发 data_length。
// 2026-11-23 彩排 P0-1：旧代码把字段写在头行顶层（无 data_length），
// whisper/piper 的 from_event 读 event.data["rate"]/["text"] 失败，
// STT/TTS 全灭；且读侧不消费 data_length 字节，后续帧全部错位。
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

// wyomingVersion 是写事件时头行带的协议版本号（与 Python 参考库
// async_write_event 一致；服务端读侧忽略它，带上只为与参考实现同形）。
const wyomingVersion = "1.11.0"

// maxUnknownEvents 是 Transcribe/Synthesize 读循环里连续未知事件的上限
// （P2-17）：旧代码遇未知类型只静默循环，全靠 120s 整体超时兜底；
// 现连续 10 个未知事件即报错（服务端行为异常时早失败、不空转）。
const maxUnknownEvents = 10

// expectedSampleRate 是语音管线假定的采样率（P2-48）：
// 下游 Opus 编码固定 16kHz；服务端以其他采样率合成时，Synthesize
// 内部重采样到 16kHz 再返回（Piper zh_CN-huayan-medium 实测 22050Hz）。
const expectedSampleRate = 16000

// maxSampleRate 是服务端 audio-start 采样率的上界：超过即视为异常，
// Synthesize 报错而不重采样（防垃圾 rate 导致超大输出）。
const maxSampleRate = 96000

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

// retryFastWindow 是 Dialer 重试的“快失败”窗口（P1-8，2026-11-23 审查）：
// 首次调用在 10s 内失败（建连失败/立即失败）才重试一次；首次调用已耗时
// 很久（读写超时 120s）时不再重试——否则单次 STT/TTS 最坏耗时从 120s
// 翻倍到约 240s，远超语音管线 120s 的 answerTimeout。10s 的分界：建连
// 失败通常在毫秒~3s 内返回；超过 10s 的失败基本是服务端已在处理中途挂掉，
// 立即重拨大概率同样慢，不如让调用方快速失败、语音管线播报降级回复。
// var 而非 const，供测试缩短（与 transcribeTimeout 同款）。
var retryFastWindow = 10 * time.Second

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

// readEvent 读一条事件：JSON 头行 + 分离的 data JSON + 可选负载。
// 返回的头 map 已把 data merge 进 header["data"]（参考库
// async_read_event 的行为：头行内联 data 与 data_length 分离字节
// 合并，分离字节优先）。
// P1-1：全程设 10s 读写 deadline；头行限长读取；data_length /
// payload_length 经 parseLength 校验（非数字/负数/非整数/超 1MB
// 一律报错，调用方收到错误后应断开连接——本函数只返回错误，不自行 Close）。
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
	// data_length 分离字节：读回 data JSON，merge 进 header["data"]。
	if l, ok := header["data_length"]; ok {
		n, err := parseLength(l, "data_length", MaxPayloadLength)
		if err != nil {
			return nil, nil, err
		}
		dataBytes := make([]byte, n)
		if _, err := io.ReadFull(c.reader, dataBytes); err != nil {
			return nil, nil, err
		}
		var data map[string]any
		if err := json.Unmarshal(dataBytes, &data); err != nil {
			return nil, nil, fmt.Errorf("wyoming: data 解析失败: %w", err)
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

// parseLength 校验 Wyoming 头里的长度字段（data_length / payload_length）：
// 必须是 JSON 数字、非负、整数，且 ≤ max。
func parseLength(v any, field string, max int) (int, error) {
	f, ok := v.(float64)
	if !ok {
		return 0, fmt.Errorf("wyoming: %s 不是数字: %T", field, v)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("wyoming: %s 不是有限数字: %v", field, f)
	}
	if f < 0 {
		return 0, fmt.Errorf("wyoming: %s 为负: %v", field, f)
	}
	if f != math.Trunc(f) {
		return 0, fmt.Errorf("wyoming: %s 不是整数: %v", field, f)
	}
	if f > float64(max) {
		return 0, fmt.Errorf("wyoming: %s 超限: %v > %d", field, f, max)
	}
	return int(f), nil
}

// parsePayloadLength 校验 Wyoming 头里的 payload_length：
// 必须是 JSON 数字、非负、整数，且 ≤ MaxPayloadLength。
func parsePayloadLength(v any) (int, error) {
	return parseLength(v, "payload_length", MaxPayloadLength)
}

// writeEvent 发一条事件。P1-1：每次写设 10s deadline，写死等不再无限挂起。
// Wire 格式（参考库 async_write_event）：调用方传
// {"type": ..., "data": {...}}，这里把 data 拆出头行——头行只含
// type/version/data_length/payload_length，data JSON 紧跟头行，
// payload 最后；data 为空时不发 data_length。
func (c *Client) writeEvent(header map[string]any, payload []byte) error {
	if err := c.conn.SetWriteDeadline(time.Now().Add(readEventTimeout)); err != nil {
		return err
	}
	// 清写 deadline：best-effort，失败无处可报（函数已在返回；
	// 连接关闭时 deadline 随之失效），故显式丢弃错误。
	defer func() { _ = c.conn.SetWriteDeadline(time.Time{}) }()
	var dataBytes []byte
	if data, ok := header["data"].(map[string]any); ok && len(data) > 0 {
		b, err := json.Marshal(data)
		if err != nil {
			return err
		}
		dataBytes = b
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
	b, err := json.Marshal(line)
	if err != nil {
		return err
	}
	if _, err := c.conn.Write(append(b, '\n')); err != nil {
		return err
	}
	if dataBytes != nil {
		if _, err := c.conn.Write(dataBytes); err != nil {
			return err
		}
	}
	if payload != nil {
		if _, err := c.conn.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// eventData 取事件头的 data 对象（Wyoming 协议：除 type 与
// payload_length 外所有字段都在 data 里）。缺失或不是对象时返回 nil，
// 调用方按字段缺失处理（空串/放行），不 panic。
func eventData(header map[string]any) map[string]any {
	data, _ := header["data"].(map[string]any)
	return data
}

// Describe 拿服务信息（模型、语言等），用于连通性检查。
// 返回 info 事件的 data（如 {"asr": [...], "tts": [...]})，
// 不是整个头——type 字段不在 data 里。
func (c *Client) Describe() (map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.describeLocked()
}

func (c *Client) describeLocked() (map[string]any, error) {
	if err := writeWithDeadline(c.conn, 5*time.Second, func() error {
		return c.writeEvent(map[string]any{"type": "describe", "data": map[string]any{}}, nil)
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
	return eventData(header), nil
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
		"type": "transcribe", "data": map[string]any{"name": "whisper", "language": language},
	}, nil); err != nil {
		return "", err
	}
	// 音频帧：每条 audio-start/audio-chunk/audio-stop；这里一次发完。
	// 参考库 AudioStart/AudioChunk 的 event() 都把 rate/width/channels
	// 放进 data（AudioChunk.from_event 读 event.data["rate"]，缺了即 KeyError）。
	audioData := map[string]any{"rate": 16000, "width": 2, "channels": 1}
	if err := c.writeEvent(map[string]any{"type": "audio-start", "data": audioData}, nil); err != nil {
		return "", err
	}
	const chunk = 4096
	for len(pcm) > 0 {
		n := chunk
		if len(pcm) < n {
			n = len(pcm)
		}
		if err := c.writeEvent(map[string]any{"type": "audio-chunk", "data": audioData}, pcm[:n]); err != nil {
			return "", err
		}
		pcm = pcm[n:]
	}
	if err := c.writeEvent(map[string]any{"type": "audio-stop", "data": map[string]any{}}, nil); err != nil {
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
			text, _ := eventData(header)["text"].(string)
			return text, nil
		case "error":
			return "", fmt.Errorf("wyoming stt error: %v", eventData(header)["text"])
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
// 服务端若以非 16kHz 合成（如 Piper zh_CN-huayan-medium 是 22050Hz，
// 2026-11-23 真实服务端端到端实测），内部线性插值重采样到 16kHz 再返回，
// 保证下游 Opus 编码（固定 16kHz）拿到的永远是 16kHz。
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
		"type": "synthesize", "data": map[string]any{"text": text, "voice": map[string]any{"name": voice}},
	}, nil); err != nil {
		return nil, err
	}
	sampleRate := expectedSampleRate // audio-start 未带 rate 时按默认 16kHz
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
			// 服务端合成采样率：记下来，结束时重采样到 16kHz。
			// 旧版 Piper 不发 rate，按默认 16kHz 处理。
			if rate, ok := eventData(header)["rate"].(float64); ok {
				sampleRate = int(rate)
			}
		case "audio-chunk":
			if len(pcm)+len(payload) > MaxSynthesizeTotal {
				return nil, fmt.Errorf("wyoming: tts 音频累积超过 %d 字节上限", MaxSynthesizeTotal)
			}
			pcm = append(pcm, payload...)
		case "audio-stop":
			if sampleRate <= 0 || sampleRate > maxSampleRate {
				return nil, fmt.Errorf("wyoming: tts 采样率 %d 非法，拒绝播放", sampleRate)
			}
			if sampleRate != expectedSampleRate {
				pcm = resamplePCM16(pcm, sampleRate, expectedSampleRate)
			}
			return pcm, nil
		case "error":
			return nil, fmt.Errorf("wyoming tts error: %v", eventData(header)["text"])
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

// resamplePCM16 把单声道 int16 PCM 从 fromRate 线性插值重采样到 toRate。
// 语音播报够用（Piper 22050→16000 实测）；fromRate==toRate 时原样返回。
func resamplePCM16(pcm []byte, fromRate, toRate int) []byte {
	if fromRate == toRate || len(pcm) == 0 {
		return pcm
	}
	in := PCMToInt16(pcm)
	outLen := len(in) * toRate / fromRate
	if outLen == 0 {
		return nil
	}
	out := make([]int16, outLen)
	for i := range out {
		pos := float64(i) * float64(fromRate) / float64(toRate)
		idx := int(pos)
		frac := pos - float64(idx)
		if idx+1 < len(in) {
			out[i] = int16(float64(in[idx])*(1-frac) + float64(in[idx+1])*frac)
		} else {
			out[i] = in[len(in)-1]
		}
	}
	return Int16ToPCM(out)
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

// Transcribe 识别：快失败时丢旧连接、重拨重试一次；
// 首次调用已耗时很久才失败时不重试（P1-8：见 retryFastWindow）。
// 失败时错误带“STT 不可用”前缀（P1-27：语音管线的降级播报靠此识别）。
func (d *Dialer) Transcribe(pcm []byte, language string) (string, error) {
	c, err := d.get()
	if err != nil {
		return "", fmt.Errorf("STT 不可用（Wyoming 未连接）: %w", err)
	}
	if c == nil {
		return "", fmt.Errorf("STT 不可用（Wyoming 未连接）: 连接为空")
	}
	start := time.Now()
	text, err := c.Transcribe(pcm, language)
	if err == nil {
		return text, nil
	}
	// P1-8：首次调用已耗时很久才失败（读写超时）时不重试——
	// 重试一次会把最坏耗时翻倍到约 240s。
	if time.Since(start) > retryFastWindow {
		return "", fmt.Errorf("STT 不可用: %w", err)
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

// Synthesize 合成：快失败时丢旧连接、重拨重试一次；
// 首次调用已耗时很久才失败时不重试（P1-8：见 retryFastWindow）。
// 失败时错误带“TTS 不可用”前缀（P1-27：语音管线的降级播报靠此识别）。
func (d *Dialer) Synthesize(text, voice string) ([]byte, error) {
	c, err := d.get()
	if err != nil {
		return nil, fmt.Errorf("TTS 不可用（Wyoming 未连接）: %w", err)
	}
	if c == nil {
		return nil, fmt.Errorf("TTS 不可用（Wyoming 未连接）: 连接为空")
	}
	start := time.Now()
	out, err := c.Synthesize(text, voice)
	if err == nil {
		return out, nil
	}
	// P1-8：首次调用已耗时很久才失败（读写超时）时不重试——
	// 重试一次会把最坏耗时翻倍到约 240s。
	if time.Since(start) > retryFastWindow {
		return nil, fmt.Errorf("TTS 不可用: %w", err)
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
