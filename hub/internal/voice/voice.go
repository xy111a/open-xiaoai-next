// Package voice 是语音管线：VAD → 流式 STT → 路由 → 流式 TTS。
//
// 链路：Client Opus 音频 → Hub 解码 → FunASR Paraformer-large（Wyoming :10300，zh）
//
//	→ 文本 → 路由：家电命令先走 HA 本地意图（<1s，不经过 LLM）；
//	  音乐走 Music Assistant；闲聊/复杂问答走 Agent（OpenAI 兼容，首字 ≤800ms）
//	→ Piper（Wyoming :10200）流式合成 → Opus → 赢得仲裁的那台播放。
//
// 连续对话（Phase 3）：唤醒一次连聊 20s；说话中喊唤醒词可打断；
//
//	打断走 OwnDing 的子带 AEC 思路（播放时采回音抵消），目标打断成功率 ≥90%。
//
// 打断的状态机在 cmd/hub/main.go 的 DeviceState：放音中（Speaking）
// 检出唤醒词 → Hub 发 interrupt 停播 → 设备进 Listening 开新一轮听；
// 本管线只负责回答的合成与 onReply 播报，不参与打断判决。
// AEC 消回声本身仍是真机验证项（见打断任务的硬件验证清单）。
package voice

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"regexp"
	"strings"
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

// errAnswerCancelled 是回答被取消的哨兵错误（打断后续：新唤醒到达时
// Cancel 取消在途回答）。与超时（answerTimeout 触发的 DeadlineExceeded）
// 区分开——超时是故障要记失败日志，取消是正常打断只记一行提示。
var errAnswerCancelled = errors.New("voice: 回答已取消")

// flight 是一次在途回答：gen 是代际（防旧 goroutine 的 defer 误删
// 新一轮的 inFlight 条目），cancel 取消该回答的 ctx。
type flight struct {
	gen    uint64
	cancel context.CancelFunc
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

	// inFlight 是每设备正在回答的在途记录（P1-29 + 打断后续）：
	// 同一设备上一句还没答完时新结算先判撤退词（P1-14：是撤退词就
	// Cancel 在途回答并重新结算，不是则丢弃、只记日志），不让回答
	// 在后台无限排队；Cancel 用它找到并取消在途回答的 ctx。
	inFlight map[string]*flight

	// genSeq 是 inFlight 的代际计数器（每次 settle +1）：旧回答的
	// goroutine 退出时只删自己那一代的条目，不误删 Cancel 后新一轮
	// 已写入的条目。
	genSeq uint64

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
		inFlight:   map[string]*flight{},
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

// Cancel 取消某设备正在进行的回答（打断后续，决定 6：新唤醒到达时
// Hub 的 OnWake 调）。取消在途的 STT→Router→TTS，旧答案不再播出；
// 无在途回答时是空操作。取消后 inFlight 立即释放，新一轮结算可马上
// 进行，不用等旧 goroutine 退出（旧 goroutine 的 defer 按代际删条目，
// 不会误删新一轮的）。只取消本设备，不碰其他设备。
func (p *Pipeline) Cancel(deviceID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if f, ok := p.inFlight[deviceID]; ok {
		f.cancel()
		delete(p.inFlight, deviceID)
	}
}

// retreatRe 是撤退词表（P1-14：与 router.retreatRe 同步，改词表时两边
// 一起改）：整句就是这些词才命中，“停车”“停一下，我想想”不命中。
var retreatRe = regexp.MustCompile(`^(?:停|停止|停下|别说了|别播了|不要说了)\s*$`)

// settle 异步结算一句话（P1-29）：FramePCM/End 只把 PCM 交出来就返回，
// 回答（STT→Router→TTS→编码）在后台 goroutine 跑，不再阻塞 transport
// 读循环；带 120s 超时 ctx；同一设备上一句未答完时新结算先判撤退词
// （P1-14：是撤退词就 Cancel 在途回答并重新结算，不是则丢弃）。
func (p *Pipeline) settle(ctx context.Context, deviceID string, pcm []int16) error {
	p.mu.Lock()
	if _, ok := p.inFlight[deviceID]; ok {
		p.mu.Unlock()
		// P1-14：上一句还在合成中，本句不能直接丢——可能是撤退词
		// （“停”）：异步转写一次判撤退词（不阻塞 transport 读循环），
		// 命中即 Cancel 在途回答并重新结算本句（见 maybeRetreat）。
		log.Printf("voice: 设备 %s 上一句还没答完，本句先判撤退词", deviceID)
		go p.maybeRetreat(ctx, deviceID, pcm)
		return nil
	}
	p.genSeq++
	gen := p.genSeq
	ctx, cancel := context.WithTimeout(ctx, answerTimeout)
	p.inFlight[deviceID] = &flight{gen: gen, cancel: cancel}
	p.mu.Unlock()
	go func() {
		defer cancel()
		defer func() {
			p.mu.Lock()
			// 只删自己这一代的条目：Cancel 后新一轮可能已写入同设备
			// 的新 flight（代际不同），不能误删。
			if f, ok := p.inFlight[deviceID]; ok && f.gen == gen {
				delete(p.inFlight, deviceID)
			}
			p.mu.Unlock()
		}()
		if err := p.answer(ctx, deviceID, pcm); err != nil {
			if errors.Is(err, errAnswerCancelled) || errors.Is(err, context.Canceled) {
				log.Printf("voice: 设备 %s 回答已取消（新唤醒打断）", deviceID)
			} else {
				log.Printf("voice: 设备 %s 回答失败: %v", deviceID, err)
			}
		}
	}()
	return nil
}

// maybeRetreat 异步处理被 inFlight 挡下的本句（P1-14）：转写后只看是不是
// 撤退词。是撤退词 → Cancel 在途回答（停合成、旧答案不再播出），再结算
// 本句走路由器的撤退快路径（停 MPD 音乐 + StopAudio 停客户端播报）；
// 不是撤退词 → 丢弃（与旧行为一致），只记一行日志。
// 注意：命中撤退词时会转写两次（这里一次、结算后 answer 里一次），
// 多一次本地 STT（约 0.4s），换来“停”在合成中也能立即生效；
// 非撤退词只多一次 STT 就丢弃，不进路由、不调 LLM。
// ctx 是 OnAudioFrame 传进来的 context.Background（不取消），
// 异步 goroutine 里继续用它结算没有问题。
func (p *Pipeline) maybeRetreat(ctx context.Context, deviceID string, pcm []int16) {
	if p.stt == nil {
		log.Printf("voice: 设备 %s STT 不可用，丢弃的本句无法判撤退词", deviceID)
		return
	}
	pcmBytes := make([]byte, 0, len(pcm)*2)
	for _, v := range pcm {
		pcmBytes = append(pcmBytes, byte(v), byte(v>>8))
	}
	text, err := p.stt.Transcribe(pcmBytes, "zh")
	if err != nil {
		log.Printf("voice: 设备 %s 丢弃的本句转写失败，不处理: %v", deviceID, err)
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if !retreatRe.MatchString(text) {
		log.Printf("voice: 设备 %s 上一句还没答完，本句丢弃（不是撤退词）", deviceID)
		return
	}
	log.Printf("voice: 设备 %s 丢弃的本句是撤退词 %q，取消在途回答并重新结算", deviceID, text)
	p.Cancel(deviceID)
	if err := p.settle(ctx, deviceID, pcm); err != nil {
		log.Printf("voice: 设备 %s 撤退词重新结算失败: %v", deviceID, err)
	}
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
// 打断后续（决定 6）：每段之间都查 ctx——Cancel 取消后，已取消的回答
// 在任何一段结束时都直接返回 errAnswerCancelled，不再往下走，尤其
// 不调 onReply（旧答案绝不播出）。STT/TTS 本身不接 ctx，所以显式
// 查 ctx.Err() 是唯一的取消保证，不能只靠 Router 的 ctx。
func (p *Pipeline) answer(ctx context.Context, deviceID string, pcm []int16) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("voice: answer panic 已兜底: %v", r)
		}
	}()
	// Cancel 可能发生在 settle 与 goroutine 启动之间：先查一次。
	if ctx.Err() != nil {
		return errAnswerCancelled
	}
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
	if ctx.Err() != nil {
		return errAnswerCancelled
	}
	if text == "" {
		return nil
	}
	reply := ""
	if p.router != nil {
		reply, err = p.router.Route(ctx, deviceID, text)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return errAnswerCancelled
			}
			return err
		}
	}
	if ctx.Err() != nil {
		return errAnswerCancelled
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
	if ctx.Err() != nil {
		return errAnswerCancelled
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
	// 播出前的最后一道关：取消的回答即使已合成好也不播。
	if ctx.Err() != nil {
		return errAnswerCancelled
	}
	if p.onReply != nil {
		p.onReply(deviceID, frames)
	}
	return nil
}
