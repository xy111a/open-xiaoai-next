// Command hub 是 open-xiaoai-next 的入口：NAS 上唯一常驻的服务。
// 启动顺序：读配置 → 建注册表 → 接 HA/MA/网易云（失败不致命，记日志继续跑）
// → 建语音管线 → 起 wss :4399 → 起 Web 面板 → 自动发现扫描 → 提醒调度 → 摄像头监视。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/huajun/open-xiaoai-next/hub/internal/agent"
	"github.com/huajun/open-xiaoai-next/hub/internal/alerts"
	"github.com/huajun/open-xiaoai-next/hub/internal/arbitration"
	"github.com/huajun/open-xiaoai-next/hub/internal/config"
	"github.com/huajun/open-xiaoai-next/hub/internal/discovery"
	"github.com/huajun/open-xiaoai-next/hub/internal/ha"
	"github.com/huajun/open-xiaoai-next/hub/internal/intercom"
	"github.com/huajun/open-xiaoai-next/hub/internal/kws"
	"github.com/huajun/open-xiaoai-next/hub/internal/music"
	"github.com/huajun/open-xiaoai-next/hub/internal/opus"
	"github.com/huajun/open-xiaoai-next/hub/internal/ota"
	"github.com/huajun/open-xiaoai-next/hub/internal/registry"
	"github.com/huajun/open-xiaoai-next/hub/internal/reminders"
	"github.com/huajun/open-xiaoai-next/hub/internal/router"
	"github.com/huajun/open-xiaoai-next/hub/internal/tlscert"
	"github.com/huajun/open-xiaoai-next/hub/internal/transport"
	"github.com/huajun/open-xiaoai-next/hub/internal/voice"
	"github.com/huajun/open-xiaoai-next/hub/internal/web"
	"github.com/huajun/open-xiaoai-next/hub/internal/wyoming"
)

// hubHandler 把 transport 的事件接到语音管线、仲裁、对讲、认领上。
type hubHandler struct {
	cfg       *config.Config
	cfgPath   string
	reg       *registry.Registry
	server    *transport.Server
	arbiter   *arbitration.Arbiter
	pipeline  *voice.Pipeline
	decoders  map[string]*opus.Decoder
	decMu     sync.Mutex
	intercom  *intercom.Manager
	otaMgr    *ota.Manager
	reminders *reminders.Store
	kws       *kws.Client // 服务端 KWS 边车客户端；nil 表示 KWS 未启用

	// 连续对话会话：deviceID → 截止时间（唤醒后 20s 内不用再喊唤醒词）。
	sessions  map[string]time.Time
	sessMu    sync.Mutex
	playing   map[string]bool // 正在放 TTS 的设备（打断用）
	playMu    sync.Mutex
	speakText func(deviceID, text string) // TTS 播报（提醒/主动播报用）
}

func (h *hubHandler) decoderFor(deviceID string) (*opus.Decoder, error) {
	h.decMu.Lock()
	defer h.decMu.Unlock()
	if d, ok := h.decoders[deviceID]; ok {
		return d, nil
	}
	d, err := opus.NewDecoder()
	if err != nil {
		return nil, err
	}
	h.decoders[deviceID] = d
	return d, nil
}

func (h *hubHandler) OnWake(deviceID string, w transport.Wake) {
	// 播放中收到唤醒 = 打断：停播，重新听。
	h.playMu.Lock()
	playing := h.playing[deviceID]
	h.playMu.Unlock()
	if playing {
		if c, ok := h.server.Conn(deviceID); ok {
			_ = c.SendInterrupt()
		}
		h.playMu.Lock()
		h.playing[deviceID] = false
		h.playMu.Unlock()
	}
	h.arbiter.Submit(arbitration.WakeEvent{
		DeviceID: deviceID, WakeWord: w.WakeWord, RMSdB: w.RMSdB,
		Timestamp: time.Now(),
	})
}

func (h *hubHandler) OnAudioStart(deviceID string) {}

func (h *hubHandler) OnAudioFrame(deviceID string, data []byte) {
	// P1-1/P1-2：transport 读循环顶层兜底——语音管线里任何 panic 都只记
	// 日志，不崩 Hub 进程。
	defer func() {
		if r := recover(); r != nil {
			log.Printf("voice frame panic 已兜底: %v", r)
		}
	}()
	// 对讲会话中：音频直接转发，不进语音管线与 KWS。
	if to, isBroadcast, err := h.intercom.Forward(deviceID, data); err == nil {
		if to != "" || isBroadcast {
			if isBroadcast {
				for _, d := range h.reg.Online() {
					if d.ID == deviceID {
						continue
					}
					if c, ok := h.server.Conn(d.ID); ok {
						_ = c.SendBinary(data)
					}
				}
			}
			return
		}
	}
	// KWS 全量转发（唤醒检测需要连续音频），语音管线只处理仲裁赢了 /
	// 会话期内的音频。KWS 未启用且不在会话期时直接返回（与旧行为一致，
	// 不做无用解码）。
	h.sessMu.Lock()
	_, inSession := h.sessions[deviceID]
	h.sessMu.Unlock()
	if h.kws == nil && !inSession {
		return
	}
	// Opus 只解一次：解出的 PCM 同时喂 KWS 边车与（会话期内）语音管线，
	// 不再为 KWS 单独解一次。
	dec, err := h.decoderFor(deviceID)
	if err != nil {
		log.Printf("opus decoder: %v", err)
		return
	}
	pcm, err := dec.Decode(data)
	if err != nil {
		log.Printf("opus decode: %v", err)
		return
	}
	if h.kws != nil {
		// 非阻塞：边车断线/消费慢时 SendPCM 丢最旧帧并记日志，
		// 不堵 transport 读循环。
		h.kws.SendPCM(deviceID, pcm)
	}
	if !inSession {
		return
	}
	if err := h.pipeline.FramePCM(context.Background(), deviceID, pcm); err != nil {
		log.Printf("voice frame: %v", err)
		// P1-2：STT/TTS 不可用或管线繁忙时，播一句降级回复而不是静默。
		// speakText 内部会在 TTS 为 nil 时只记日志，不 panic。
		if isVoiceServiceError(err) && h.speakText != nil {
			h.speakText(deviceID, "语音服务暂时没连上，请稍后再试")
		}
	}
}

// OnKWSWake 把边车 wake 事件走与本地 wake 同一条仲裁链路
// （OnWake → 250ms 仲裁 → wake_win/wake_lose → 赢家进语音管线）。
// 边车 rms 是线性值（0..1，如 0.42）；仲裁只比大小，换算取 20*log10(rms)，
// rms<=0 记 -100dB。timestamp_ms 只用于记边车延迟日志，
// 仲裁窗口以 Hub 收到事件的时间为准（边车与 Hub 时钟可能不同步）。
func (h *hubHandler) OnKWSWake(ev kws.WakeEvent) {
	rmsdB := -100.0
	if ev.RMS > 0 {
		rmsdB = 20 * math.Log10(ev.RMS)
	}
	if ev.TimestampMs > 0 {
		if lag := time.Now().UnixMilli() - ev.TimestampMs; lag >= 0 && lag < 60000 {
			log.Printf("kws: 设备 %s 唤醒 %q（边车上报延迟约 %dms）", ev.DeviceID, ev.Keyword, lag)
		}
	}
	h.OnWake(ev.DeviceID, transport.Wake{WakeWord: ev.Keyword, RMSdB: rmsdB, TsMs: ev.TimestampMs})
}

// isVoiceServiceError 判断语音错误是否属于“下游服务不可用”类（P1-2 降级
// 回复只针对这类错误；识别/路由等普通错误不重复播报）。
func isVoiceServiceError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "STT 不可用") ||
		strings.Contains(msg, "TTS 不可用") ||
		strings.Contains(msg, "语音管线繁忙")
}

func (h *hubHandler) OnAudioEnd(deviceID string) {
	// 对讲会话结束。
	h.intercom.End(deviceID)
	if err := h.pipeline.End(context.Background(), deviceID); err != nil {
		log.Printf("voice end: %v", err)
		if isVoiceServiceError(err) && h.speakText != nil {
			h.speakText(deviceID, "语音服务暂时没连上，请稍后再试")
		}
	}
}

func (h *hubHandler) OnRoomClaim(deviceID string, room string) {
	room = strings.TrimSpace(room)
	if room == "" {
		return
	}
	if !h.reg.SetRoom(deviceID, room) {
		return
	}
	// 写回 config.yaml 持久化。P1-32：Save 失败必须记日志，不许吞掉
	//（只读挂载下房间认领重启后会丢失，操作员要能从日志看到）。
	h.cfg.Devices[deviceID] = config.Device{
		Model: mustModel(h.reg, deviceID), IP: mustIP(h.reg, deviceID), Room: room,
	}
	if err := h.cfg.Save(h.cfgPath); err != nil {
		log.Printf("房间认领保存失败（重启后认领会丢失）: %v", err)
	}
	if c, ok := h.server.Conn(deviceID); ok {
		_ = c.SendJSON(map[string]string{"type": "room_ok", "room": room})
	}
	log.Printf("房间认领：%s → %s", deviceID, room)
}

func mustModel(r *registry.Registry, id string) string {
	if d, ok := r.Get(id); ok {
		return d.Model
	}
	return ""
}

func mustIP(r *registry.Registry, id string) string {
	if d, ok := r.Get(id); ok {
		return d.IP
	}
	return ""
}

func (h *hubHandler) OnStatus(deviceID string, s transport.Status) {
	h.reg.Upsert(registry.Device{ID: deviceID, Version: s.Version})
	h.reg.SetOnline(deviceID, true)
}

func (h *hubHandler) OnRPCResult(deviceID string, r transport.RPCResult) {
	if !r.OK {
		log.Printf("rpc %s 在 %s 失败: %s", r.ID, deviceID, r.Error)
	}
}

func (h *hubHandler) OnDisconnect(deviceID string) {
	h.sessMu.Lock()
	delete(h.sessions, deviceID)
	h.sessMu.Unlock()
	h.intercom.End(deviceID)
	// 单设备断开即清理其 KWS 流状态（不断流的连接与队列不残留）。
	if h.kws != nil {
		h.kws.RemoveDevice(deviceID)
	}
}

// openSession 仲裁赢了之后开 20s 连续对话会话。
func (h *hubHandler) openSession(deviceID string) {
	h.sessMu.Lock()
	h.sessions[deviceID] = time.Now().Add(20 * time.Second)
	h.sessMu.Unlock()
}

// sweepSessions 清理过期会话（每秒一次）。
func (h *hubHandler) sweepSessions() {
	for range time.Tick(time.Second) {
		now := time.Now()
		h.sessMu.Lock()
		for id, until := range h.sessions {
			if now.After(until) {
				delete(h.sessions, id)
			}
		}
		h.sessMu.Unlock()
	}
}

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config.yaml")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		log.Fatalf("config 无效: %v", err)
	}
	token := config.Token()
	// P0-3：启动时拒绝弱 Token（空 / <16 位 / CHANGE_ME 等常见弱值），
	// 失败即 Fatal，错误里给出生成命令（openssl rand -hex 16）。
	// 注意：ValidateToken 只能放在这里（生产启动处），绝不能放进
	// transport.New/Serve——hub/integration_test.go 与 discovery 测试
	// 仍用短 Token "secret"，放进去会破坏它们。
	if err := transport.ValidateToken(token); err != nil {
		log.Fatalf("OPEN_XIAOAI_TOKEN 无效: %v", err)
	}
	if err := os.MkdirAll(cfg.Hub.DataDir, 0755); err != nil {
		log.Fatalf("建数据目录失败: %v", err)
	}

	reg := registry.New()
	for id, d := range cfg.Devices {
		reg.Upsert(registry.Device{ID: id, Model: d.Model, IP: d.IP, Room: d.Room})
	}

	// --- 下游服务（失败不致命：Hub 先跑起来，缺的服务在回复里明说）---
	var haClient *ha.Client
	if cfg.HomeAssistant.URL != "" {
		if c, err := ha.Dial(cfg.HomeAssistant.URL, config.HASSToken(), 5*time.Second); err != nil {
			log.Printf("HA 连接失败（稍后重试，不阻塞启动）: %v", err)
		} else {
			haClient = c
			log.Printf("HA 已连接: %s", cfg.HomeAssistant.URL)
		}
	}
	var maClient *music.MAClient
	if cfg.Netease.MusicAssistant != "" {
		maClient = music.NewMA(cfg.Netease.MusicAssistant, os.Getenv("MA_TOKEN"))
	}
	netease := music.NewNetease(cfg.Netease.API)

	// Agent：按唤醒词路由选默认（你好小李 → hermes）。
	var agentClient *agent.Client
	if name, ok := cfg.WakeWordRoutes[cfg.WakeWord]; ok {
		if a, ok := cfg.Agents[name]; ok {
			agentClient = agent.New(a.BaseURL, a.Model, config.LLMAPIKey())
		}
	}
	if agentClient == nil {
		for _, a := range cfg.Agents {
			agentClient = agent.New(a.BaseURL, a.Model, config.LLMAPIKey())
			break
		}
	}

	// 提醒与记忆。
	remStore := reminders.New(cfg.Hub.DataDir+"/reminders.json", nil)
	_ = remStore.Load()
	memory := router.NewMemory(cfg.Hub.DataDir + "/memory.json")
	_ = memory.Load()

	// 语音管线（STT/TTS 走 Wyoming；连不上时 Pipeline 报错并记日志）。
	var stt voice.STT
	var tts voice.TTS
	if c, err := wyoming.Dial(cfg.Hub.STT, 3*time.Second); err != nil {
		log.Printf("Wyoming STT 连接失败（语音识别不可用）: %v", err)
	} else {
		stt = &wyomingSTT{c: c}
	}
	if c, err := wyoming.Dial(cfg.Hub.TTS, 3*time.Second); err != nil {
		log.Printf("Wyoming TTS 连接失败（语音合成不可用）: %v", err)
	} else {
		tts = &wyomingTTS{c: c}
	}

	// 对讲与 OTA。
	otaMgr := ota.New()

	// Handler 与 Server 先建（Pipeline 的 onReply 需要 server 引用，用闭包晚绑定）。
	h := &hubHandler{
		cfg: cfg, cfgPath: *cfgPath, reg: reg,
		decoders: map[string]*opus.Decoder{},
		otaMgr:   otaMgr, reminders: remStore,
		sessions: map[string]time.Time{},
		playing:  map[string]bool{},
	}
	server := transport.New(token, reg, h)
	h.server = server
	h.intercom = intercom.New(func(deviceID string, opusFrame []byte) error {
		c, ok := server.Conn(deviceID)
		if !ok {
			return fmt.Errorf("设备 %s 不在线", deviceID)
		}
		return c.SendBinary(opusFrame)
	})

	// 路由器。P1-26：启动时查 MA Players() 取默认 player_id 作 QueueID；
	// 查不到记日志，点歌走 MPD 降级（MA 恢复后需重启 Hub，见 P1-27）。
	// MPD 与 Snapcast 在启动时 Dial，失败记日志并后台每 30s 重试，晚到
	// 时经 rt.SetMPD/SetSnap 补接。
	queueID := ""
	if maClient != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		q, err := router.DefaultQueueID(ctx, maClient)
		cancel()
		if err != nil {
			log.Printf("MA 播放器未就绪（点歌将走 MPD 降级，MA 恢复后需重启 Hub）: %v", err)
		} else {
			queueID = q
			log.Printf("MA 默认播放器：%s", q)
		}
	}
	var mpdClient *music.MPDClient
	if cfg.Hub.MPD != "" {
		if c, err := music.DialMPD(cfg.Hub.MPD, 3*time.Second); err != nil {
			log.Printf("MPD 连接失败（点歌降级路径不可用，后台重试）: %v", err)
		} else {
			mpdClient = c
		}
	}
	var snapClient *music.SnapClient
	if cfg.Hub.Snapcast != "" {
		if c, err := music.DialSnap(cfg.Hub.Snapcast, 3*time.Second); err != nil {
			log.Printf("Snapcast 连接失败（多房间同步延迟校准不可用，后台重试）: %v", err)
		} else {
			snapClient = c
		}
	}
	rt := router.New(router.Deps{
		HA: haClient, MA: maClient, Netease: netease, Agent: agentClient,
		Reminders: remStore, Memory: memory, QueueID: queueID,
		MPD: mpdClient, Snap: snapClient,
		SnapLatencies: cfg.Hub.SnapcastLatencies,
		SnapGroupID:   cfg.Hub.SnapcastGroup,
		SnapStreamID:  cfg.Hub.SnapcastStream,
		RoomOfDevice: func(deviceID string) string {
			if d, ok := reg.Get(deviceID); ok {
				return d.Room
			}
			return ""
		},
		Notify: func(ctx context.Context, text string) error {
			if haClient == nil {
				return fmt.Errorf("HA 未连接，无法通知")
			}
			return haClient.FireEvent("open_xiaoai_emergency", map[string]any{"text": text})
		},
	}, cfg.HAEntities)

	// P1-26 后台重试：MPD / Snapcast 启动时没连上时每 30s 重试一次，
	// 连上即补接到 Router（SetMPD/SetSnap），Hub 不用重启。
	if mpdClient == nil && cfg.Hub.MPD != "" {
		go func() {
			for range time.Tick(30 * time.Second) {
				c, err := music.DialMPD(cfg.Hub.MPD, 3*time.Second)
				if err != nil {
					log.Printf("MPD 重试连接失败: %v", err)
					continue
				}
				rt.SetMPD(c)
				log.Printf("MPD 已连上（后台重试）: %s", cfg.Hub.MPD)
				return
			}
		}()
	}
	if snapClient == nil && cfg.Hub.Snapcast != "" {
		go func() {
			for range time.Tick(30 * time.Second) {
				c, err := music.DialSnap(cfg.Hub.Snapcast, 3*time.Second)
				if err != nil {
					log.Printf("Snapcast 重试连接失败: %v", err)
					continue
				}
				rt.SetSnap(c)
				log.Printf("Snapcast 已连上（后台重试）: %s", cfg.Hub.Snapcast)
				return
			}
		}()
	}

	// Pipeline 的 onReply：把回答的 Opus 帧发给对应设备播放。
	h.pipeline = voice.New(stt, tts, rt, cfg.Hub.TTSVoice, func(deviceID string, frames [][]byte) {
		c, ok := server.Conn(deviceID)
		if !ok {
			return
		}
		h.playMu.Lock()
		h.playing[deviceID] = true
		h.playMu.Unlock()
		_ = c.SendRPC("play-"+time.Now().Format("150405.000"), "start_play", nil)
		for _, f := range frames {
			_ = c.SendBinary(f)
		}
		h.playMu.Lock()
		h.playing[deviceID] = false
		h.playMu.Unlock()
	})
	// 提醒到点：TTS 播报到对应房间（或全屋）。
	remStore.SetOnFire(func(r reminders.Reminder) {
		text := "提醒你" + r.Text
		if r.Room != "" {
			if d := reg.ByRoom(r.Room); d != nil && h.speakText != nil {
				h.speakText(d.ID, text)
				return
			}
		}
		for _, d := range reg.Online() {
			if h.speakText != nil {
				h.speakText(d.ID, text)
			}
		}
	})
	remStore.Start()
	// speakText：经 Pipeline 的 TTS 合成后发给设备（提醒/主动播报共用）。
	h.speakText = func(deviceID, text string) {
		// P1-2：提醒 goroutine 里 panic 也不能崩 Hub。
		defer func() {
			if r := recover(); r != nil {
				log.Printf("speakText panic 已兜底: %v", r)
			}
		}()
		if tts == nil {
			log.Printf("TTS 不可用，无法播报给 %s: %s", deviceID, text)
			return
		}
		audio, err := tts.Synthesize(text, cfg.Hub.TTSVoice)
		if err != nil {
			log.Printf("TTS 合成失败: %v", err)
			return
		}
		enc, err := opus.NewEncoder()
		if err != nil {
			return
		}
		samples := make([]int16, 0, len(audio)/2)
		for i := 0; i+1 < len(audio); i += 2 {
			samples = append(samples, int16(audio[i])|int16(audio[i+1])<<8)
		}
		c, ok := server.Conn(deviceID)
		if !ok {
			return
		}
		for i := 0; i+opus.FrameSize <= len(samples); i += opus.FrameSize {
			f, err := enc.Encode(samples[i : i+opus.FrameSize])
			if err != nil {
				return
			}
			_ = c.SendBinary(f)
		}
	}

	// 仲裁：窗口关闭后，赢家开会话 + 发 wake_win，输家发 wake_lose。
	h.arbiter = arbitration.New(arbitration.Window, func(winner string, losers []string) {
		if winner == "" {
			return
		}
		h.openSession(winner)
		if c, ok := server.Conn(winner); ok {
			_ = c.SendJSON(map[string]string{"type": "wake_win"})
		}
		for _, l := range losers {
			if c, ok := server.Conn(l); ok {
				_ = c.SendJSON(map[string]string{"type": "wake_lose"})
			}
		}
	})
	go h.sweepSessions()

	// --- 服务端 KWS 边车 ---
	// 配置非法（enabled=true 但 url/keywords 缺配或配错）时 New 即报错，
	// 启动失败并明说原因，不静默用错地址。边车运行时不可用不阻塞启动：
	// 降级为日志 + /api/status 报告 kws_down，音频流重连后自动恢复。
	var kwsClient *kws.Client
	if cfg.KWS.Enabled {
		c, err := kws.New(cfg.KWS.URL, kws.Options{Keywords: cfg.KWS.Keywords})
		if err != nil {
			log.Fatalf("kws 配置无效: %v", err)
		}
		kwsClient = c
		h.kws = kwsClient
		if err := kwsClient.CheckHealth(context.Background()); err != nil {
			log.Printf("唤醒服务暂不可用：边车 %s 健康检查失败（%v），语音唤醒降级，/api/status 将报告 kws_down", cfg.KWS.URL, err)
		} else {
			log.Printf("KWS 边车健康检查通过: %s", cfg.KWS.URL)
		}
		kwsClient.Start()
		go func() {
			for ev := range kwsClient.Events() {
				h.OnKWSWake(ev)
			}
		}()
	} else if cfg.KWS.URL != "" {
		log.Printf("KWS 未启用：kws.url 已配但 kws.enabled 未开启，语音唤醒不可用")
	} else {
		log.Printf("KWS 未启用（kws.enabled 未开启），语音唤醒不可用")
	}

	// --- wss :4399 ---
	ln, err := net.Listen("tcp", cfg.Hub.Listen)
	if err != nil {
		log.Fatalf("监听 %s 失败: %v", cfg.Hub.Listen, err)
	}
	// 传输层：wss :4399（或 tls=false 时纯 ws）。
	go func() {
		if cfg.TLSEnabled() {
			// P0-3：mTLS——CA 签发的服务端证书 + 客户端 CA 池。
			// LoadOrCreateMTLS() 会在旧自签 hub.crt 已存在且不是 CA 签发时
			// 重新签发覆盖（指纹随之变，client.toml 的 hub_ca_fingerprint
			// 需同步更新）；同时签发共享客户端证书（CN open-xiaoai-client，
			// 随固件下发给音箱）。SetClientCAs 后 Serve() 要求客户端出示
			// 该 CA 签发的证书（RequireAndVerifyClientCert），只做 Token
			// 鉴权的旧路径不再走。
			cert, pool, fp, err := tlscert.LoadOrCreateMTLS(cfg.Hub.DataDir)
			if err != nil {
				log.Fatalf("TLS 证书失败: %v", err)
			}
			server.SetClientCAs(pool)
			log.Printf("wss 已启用（mTLS：要求客户端证书），Hub 证书指纹：%s", fp)
			if err := server.Serve(ln, &cert); err != nil {
				log.Fatalf("wss 服务退出: %v", err)
			}
		} else {
			log.Printf("警告：TLS 已关闭，走纯 ws（仅本地测试）")
			if err := server.Serve(ln, nil); err != nil {
				log.Fatalf("ws 服务退出: %v", err)
			}
		}
	}()

	// --- Web 面板（Phase 4）---
	webServer := web.New(*cfgPath, cfg, reg, otaMgr)
	webServer.KWSStatus = func() string {
		if kwsClient == nil {
			return "disabled"
		}
		if kwsClient.Status().Down {
			return "down"
		}
		return "ok"
	}
	webServer.PushOTA = func(deviceID, version, url, sha256 string) error {
		c, ok := server.Conn(deviceID)
		if !ok {
			return fmt.Errorf("设备 %s 不在线", deviceID)
		}
		return c.SendJSON(map[string]string{
			"type": "ota", "version": version, "url": url, "sha256": sha256,
		})
	}
	go func() {
		srv := webServer.NewHTTPServer()
		log.Printf("Web 面板：http://%s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil {
			log.Printf("Web 面板退出: %v", err)
		}
	}()

	// --- 自动发现（P0-4：默认关闭；只有配置显式开启才在启动时扫一次）---
	go func() {
		scanner := newDiscoveryScanner(cfg, token)
		if scanner == nil {
			return // 未显式开启 / 未配指纹白名单 / TLS 未开：本次启动不扫描
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cidr := cfg.Hub.ScanCIDR
		if cidr == "" {
			cidr = localCIDR()
		}
		if cidr == "" {
			log.Printf("自动发现：取不到本机网段，跳过扫描")
			return
		}
		found, err := scanner.Scan(ctx, cidr)
		if err != nil {
			log.Printf("自动发现失败: %v", err)
			return
		}
		for _, d := range found {
			id := transport.DeviceIDFor(d.Model, d.IP)
			reg.Upsert(registry.Device{ID: id, Model: d.Model, IP: d.IP})
			log.Printf("自动发现：%s %s", d.Model, d.IP)
		}
	}()

	// --- 摄像头监视（FR-10，Phase 4）---
	// 判定逻辑在 internal/alerts（ShouldAnnounce，可单测）：
	// 漏水/烟雾/水浸类实体变 on → 向所有在线设备播报，勿扰时段跳过。
	if haClient != nil {
		watcher := alerts.New(haClient, cfg.Hub.DoNotDisturb, func(text string) {
			for _, d := range reg.Online() {
				h.speakText(d.ID, text)
			}
		})
		if err := watcher.Start(); err != nil {
			log.Printf("摄像头监视订阅失败: %v", err)
		}
	}

	log.Printf("open-xiaoai-hub 已启动：唤醒词 %q，监听 %s", cfg.WakeWord, cfg.Hub.Listen)

	// 等退出信号。
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("收到退出信号，关闭中…")
	if kwsClient != nil {
		kwsClient.Close()
	}
	if haClient != nil {
		_ = haClient.Close()
	}
}

// wyomingSTT 把 wyoming.Client 适配成 voice.STT。
type wyomingSTT struct{ c *wyoming.Client }

func (w *wyomingSTT) Transcribe(pcm []byte, language string) (string, error) {
	return w.c.Transcribe(pcm, language)
}

// wyomingTTS 把 wyoming.Client 适配成 voice.TTS。
type wyomingTTS struct{ c *wyoming.Client }

func (w *wyomingTTS) Synthesize(text, voice string) ([]byte, error) {
	return w.c.Synthesize(text, voice)
}

// discoveryPort 从 cfg.Hub.Listen（如 ":4399"）取端口，供 discovery.Scanner 用。
// 端口不写死：Hub 监听端口改了，扫描端口跟着走。Listen 已由 config.Validate
// 校验为合法 host:port，这里只做解析（非法时返回错误，调用方跳过扫描）。
func discoveryPort(listen string) (int, error) {
	_, portStr, err := net.SplitHostPort(listen)
	if err != nil {
		return 0, fmt.Errorf("hub.listen 不是合法 host:port：%q", listen)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return 0, fmt.Errorf("hub.listen 端口非法：%q", listen)
	}
	return port, nil
}

// newDiscoveryScanner 按配置建扫描器（P0-4 接线）：
//   - 端口取 cfg.Hub.Listen，不写死 4399；
//   - Enabled 只有配置显式开启（hub.scan_enabled=true）才为 true，默认关闭；
//   - 指纹白名单 cfg.Hub.DiscoveryFingerprints 原样传入；
//   - TLS 跟随 Hub 配置（TLS 未开时拒绝明文扫描）。
//
// 返回 nil 表示本次启动不扫描：未显式开启、未配指纹白名单、端口非法或
// TLS 未开（已记日志，调用方直接跳过）。白名单为空时也返回 nil——
// discovery 层本就拒绝无白名单发 Token，这里直接 fail closed，
// 避免一次注定失败的 60 秒扫描。
func newDiscoveryScanner(cfg *config.Config, token string) *discovery.Scanner {
	if !cfg.Hub.ScanEnabled {
		return nil
	}
	if len(cfg.Hub.DiscoveryFingerprints) == 0 {
		log.Printf("自动发现：已开启但未配 hub.discovery_fingerprints 白名单，拒绝扫描")
		return nil
	}
	port, err := discoveryPort(cfg.Hub.Listen)
	if err != nil {
		log.Printf("自动发现：%v，跳过扫描", err)
		return nil
	}
	if !cfg.TLSEnabled() {
		log.Printf("自动发现：TLS 未开启，拒绝明文扫描，跳过")
		return nil
	}
	scanner := discovery.New(port, token)
	scanner.Enabled = true
	scanner.TLS = true
	scanner.ExpectedFingerprints = cfg.Hub.DiscoveryFingerprints
	return scanner
}

// localCIDR 取本机第一个非回环 IPv4 网段（/24）。
func localCIDR() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
				if ip := ipnet.IP.To4(); ip != nil {
					return ip.Mask(net.CIDRMask(24, 32)).String() + "/24"
				}
			}
		}
	}
	return ""
}
