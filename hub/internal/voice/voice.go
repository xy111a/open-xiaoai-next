// Package voice 是语音管线：VAD → 流式 STT → 路由 → 流式 TTS。
//
// 链路：Client Opus 音频 → Hub 解码 → faster-whisper（Wyoming :10300，small-int8，zh）
//
//	→ 文本 → 路由：家电命令先走 HA 本地意图（<1s，不经过 LLM）；
//	  音乐走 Music Assistant；闲聊/复杂问答走 Agent（OpenAI 兼容，首字 ≤800ms）
//	→ Piper（Wyoming :10200）流式合成 → Opus → 赢得仲裁的那台播放。
//
// 连续对话（Phase 3）：唤醒一次连聊 20s；说话中喊唤醒词可打断；
//
//	打断走 OwnDing 的子带 AEC 思路（播放时采回音抵消），目标打断成功率 ≥90%。
package voice

import (
	"context"
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"github.com/huajun/open-xiaoai-next/hub/internal/observability"
	"github.com/huajun/open-xiaoai-next/hub/internal/opus"
)

// P1-2 上限：单句最长 30 秒（16kHz 单声道 int16：16000×2×30 = 960KB，
// 即 480000 采样点），超限强制结算；全管线最多 4 路并发 answer。
const (
	// MaxUtteranceSamples 是单句累积 PCM 的采样点上限（30 秒）。
	MaxUtteranceSamples = 480000
	// MaxConcurrentAnswers 是 Pipeline 同时跑的 answer 路数上限。
	MaxConcurrentAnswers = 4
)

// STT 是语音识别接口（生产用 Wyoming，测试用 fake）。
type STT interface {
	Transcribe(pcm []byte, language string) (string, error)
}

// TTS 是语音合成接口（生产用 Wyoming Piper，测试用 fake）。
type TTS interface {
	Synthesize(text, voice string) ([]byte, error)
}

// Router 是文本路由接口：收到识别文本后决定怎么回。
// 返回的 reply 是要播报的文本；空表示不播报（例如已直接执行完家电命令但不需要播报时也返回确认文本）。
type Router interface {
	Route(ctx context.Context, deviceID, text string) (reply string, err error)
}

// utterance 是一句话的累积音频。
type utterance struct {
	pcm       []int16
	speech    bool // 是否检测到过语音
	silenceMs int  // 当前连续静音毫秒数
}

// Pipeline 是语音管线。
type Pipeline struct {
	stt    STT
	tts    TTS
	router Router
	voice  string

	// VAD 参数。
	silenceThreshold float64 // 静音判定 RMS 阈值（默认 200，约 -50dBFS）
	silenceEndMs     int     // 静音多久算说完（默认 800ms）

	mu         sync.Mutex
	utterances map[string]*utterance
	onReply    func(deviceID string, opusFrames [][]byte)

	// inFlight 是每设备正在回答的标记（P1-29）：同一设备上一句还没
	// 答完时新结算直接丢弃（只记日志），不让回答在后台无限排队。
	inFlight map[string]bool

	// sem 是 answer 的总并发上限（P1-2）：满了直接报“繁忙”，不排队阻塞
	// transport 读循环。
	sem chan struct{}

	// Latency 是可选的分段延迟记录器（可观测性）：main 注入
	// observability.Recorder；为 nil 时不记录，不影响管线。
	// answer 入口 Begin 一条 voice 交互（消费 main 经 NoteWake 记下的
	// KWS 延迟），STT/TTS 各记一段；LLM 与点歌由 Router 在 Route 里
	// 经 RecordStageForDevice/RecordMusic 写进同一条交互。
	Latency *observability.Recorder
}

// answerTimeout 是单次回答的总超时（P1-29）：STT→Router→TTS→编码全程
// 120s，超时则 answer 的 ctx 取消，不让后台 goroutine 永久挂住。
const answerTimeout = 120 * time.Second

// New 新建 Pipeline。onReply 在合成好回答后被调用，参数是 Opus 帧列表。
func New(stt STT, tts TTS, router Router, voice string, onReply func(deviceID string, opusFrames [][]byte)) *Pipeline {
	return &Pipeline{
		stt: stt, tts: tts, router: router, voice: voice,
		silenceThreshold: 200, silenceEndMs: 800,
		utterances: map[string]*utterance{},
		inFlight:   map[string]bool{},
		onReply:    onReply,
		sem:        make(chan struct{}, MaxConcurrentAnswers),
	}
}

// Discard 丢弃某设备的未结算 utterance（P1-28：OnDisconnect 调，
// 断线后半句话不再残留，下次重连不被旧音频污染）。已在回答中的
// inFlight 不受影响（由 answerTimeout 兜底）。
func (p *Pipeline) Discard(deviceID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.utterances, deviceID)
}

// settle 异步结算一句话（P1-29）：FramePCM/End 只把 PCM 交出来就返回，
// 回答（STT→Router→TTS→编码）在后台 goroutine 跑，不再阻塞 transport
// 读循环；带 120s 超时 ctx；同一设备上一句未答完时新结算直接丢弃。
func (p *Pipeline) settle(ctx context.Context, deviceID string, pcm []int16) error {
	p.mu.Lock()
	if p.inFlight[deviceID] {
		p.mu.Unlock()
		log.Printf("voice: 设备 %s 上一句还没答完，本句丢弃", deviceID)
		return nil
	}
	p.inFlight[deviceID] = true
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, answerTimeout)
	go func() {
		defer cancel()
		defer func() {
			p.mu.Lock()
			delete(p.inFlight, deviceID)
			p.mu.Unlock()
		}()
		if err := p.answer(ctx, deviceID, pcm); err != nil {
			log.Printf("voice: 设备 %s 回答失败: %v", deviceID, err)
		}
	}()
	return nil
}

// RMS 算一帧 PCM 的均方根（供 VAD 与测试用）。
func RMS(pcm []int16) float64 {
	if len(pcm) == 0 {
		return 0
	}
	var sum float64
	for _, v := range pcm {
		f := float64(v)
		sum += f * f
	}
	return math.Sqrt(sum / float64(len(pcm)))
}

// Frame 处理一帧 Opus 音频：解码 → VAD → 说完则转写并回答。
func (p *Pipeline) Frame(ctx context.Context, deviceID string, dec *opus.Decoder, data []byte) error {
	pcm, err := dec.Decode(data)
	if err != nil {
		return err
	}
	return p.FramePCM(ctx, deviceID, pcm)
}

// FramePCM 处理一帧已解码的 PCM（16kHz 单声道 int16，正常 20ms 一帧 320 点；
// 长度不固定也接受，按实际点数计 VAD/累积）。
// 供 Hub 的 OnAudioFrame 用：Opus 只解一次，解出的 PCM 同时喂 KWS 边车与本管线，
// 不再为 KWS 单独解一次。
func (p *Pipeline) FramePCM(ctx context.Context, deviceID string, pcm []int16) error {
	rms := RMS(pcm)
	p.mu.Lock()
	u, ok := p.utterances[deviceID]
	if !ok {
		u = &utterance{}
		p.utterances[deviceID] = u
	}
	u.pcm = append(u.pcm, pcm...)
	if rms > p.silenceThreshold {
		u.speech = true
		u.silenceMs = 0
	} else {
		// 按实际点数折算毫秒（320 点 = 20ms；非整帧也按比例计）。
		u.silenceMs += len(pcm) * 1000 / opus.SampleRate
	}
	finished := u.speech && u.silenceMs >= p.silenceEndMs
	// P1-2 单句上限：持续语音（或持续底噪）不再无限累积。
	if len(u.pcm) > MaxUtteranceSamples {
		if u.speech {
			log.Printf("voice: 设备 %s 单句超过 %d 采样点（约 30s），强制结算", deviceID, MaxUtteranceSamples)
			u.pcm = u.pcm[:MaxUtteranceSamples]
			finished = true
		} else {
			// 从未检出语音：纯静音/底噪累积无意义，直接丢弃，避免 OOM。
			log.Printf("voice: 设备 %s 未检出语音的累积音频超过 %d 采样点，已丢弃", deviceID, MaxUtteranceSamples)
			u.pcm = u.pcm[:0]
			u.silenceMs = 0
		}
	}
	var full []int16
	if finished {
		full = u.pcm
		delete(p.utterances, deviceID)
	}
	p.mu.Unlock()
	if !finished {
		return nil
	}
	// P1-29：异步结算，不阻塞 transport 读循环。
	return p.settle(ctx, deviceID, full)
}

// End 强制结束当前一句话（收到 audio_end 时调）。
func (p *Pipeline) End(ctx context.Context, deviceID string) error {
	p.mu.Lock()
	u, ok := p.utterances[deviceID]
	if !ok || !u.speech {
		if ok {
			delete(p.utterances, deviceID)
		}
		p.mu.Unlock()
		return nil
	}
	full := u.pcm
	delete(p.utterances, deviceID)
	p.mu.Unlock()
	// P1-29：异步结算，不阻塞 transport 读循环。
	return p.settle(ctx, deviceID, full)
}

// answer 转写 → 路由 → 合成 → 回 Opus 帧。
// P1-2：STT/TTS 为 nil 时返回明确错误（不 panic，由调用方播报降级回复）；
// 总并发超上限时直接报“繁忙”；panic 转错误。
func (p *Pipeline) answer(ctx context.Context, deviceID string, pcm []int16) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("voice: answer panic 已兜底: %v", r)
		}
	}()
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	default:
		return fmt.Errorf("语音管线繁忙，请稍后再试")
	}
	if p.stt == nil {
		return fmt.Errorf("STT 不可用（Whisper 未连接），请稍后再试")
	}
	if p.tts == nil {
		return fmt.Errorf("TTS 不可用（Piper 未连接），请稍后再试")
	}
	pcmBytes := make([]byte, 0, len(pcm)*2)
	for _, v := range pcm {
		pcmBytes = append(pcmBytes, byte(v), byte(v>>8))
	}
	// 可观测性：本句问答开始记延迟；STT/TTS 各记一段，
	// 失败也 Finish（ok=false），不在半路丢记录。
	inter := p.Latency.Begin(deviceID, "voice")
	defer func() { p.Latency.Finish(inter, err) }()
	sttStart := time.Now()
	text, err := p.stt.Transcribe(pcmBytes, "zh")
	p.Latency.RecordStage(inter, observability.StageSTT, time.Since(sttStart))
	if err != nil {
		return err
	}
	if text == "" {
		return nil
	}
	reply := ""
	if p.router != nil {
		reply, err = p.router.Route(ctx, deviceID, text)
		if err != nil {
			return err
		}
	}
	if reply == "" {
		return nil
	}
	ttsStart := time.Now()
	audio, err := p.tts.Synthesize(reply, p.voice)
	p.Latency.RecordStage(inter, observability.StageTTS, time.Since(ttsStart))
	if err != nil {
		return err
	}
	enc, err := opus.NewEncoder()
	if err != nil {
		return err
	}
	samples := make([]int16, 0, len(audio)/2)
	for i := 0; i+1 < len(audio); i += 2 {
		samples = append(samples, int16(audio[i])|int16(audio[i+1])<<8)
	}
	var frames [][]byte
	// P2-48：尾部不足一帧（320 点）的采样补零到整帧再编码——
	// 旧代码直接丢弃，TTS 尾音（常 <20ms，短回复可能整句 <320 点
	// 即零帧）会丢失；短于一帧的回复也不再发空帧列表。
	if rem := len(samples) % opus.FrameSize; rem != 0 {
		for i := 0; i < opus.FrameSize-rem; i++ {
			samples = append(samples, 0)
		}
	}
	for i := 0; i+opus.FrameSize <= len(samples); i += opus.FrameSize {
		f, err := enc.Encode(samples[i : i+opus.FrameSize])
		if err != nil {
			return err
		}
		frames = append(frames, f)
	}
	if p.onReply != nil {
		p.onReply(deviceID, frames)
	}
	return nil
}
