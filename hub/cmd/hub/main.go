// Command hub 是 open-xiaoai-next 的入口：NAS 上唯一常驻的服务。
// 启动顺序：读配置 → 建注册表 → 接 HA/网易云（失败不致命，记日志继续跑）
// → 建语音管线 → 起 wss :4399 → 起 Web 面板 → 自动发现扫描 → 提醒调度 → 摄像头监视。
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/huajun/open-xiaoai-next/hub/internal/agent"
	"github.com/huajun/open-xiaoai-next/hub/internal/alerts"
	"github.com/huajun/open-xiaoai-next/hub/internal/arbitration"
	"github.com/huajun/open-xiaoai-next/hub/internal/config"
	"github.com/huajun/open-xiaoai-next/hub/internal/ha"
	"github.com/huajun/open-xiaoai-next/hub/internal/intercom"
	"github.com/huajun/open-xiaoai-next/hub/internal/kws"
	"github.com/huajun/open-xiaoai-next/hub/internal/music"
	"github.com/huajun/open-xiaoai-next/hub/internal/observability"
	"github.com/huajun/open-xiaoai-next/hub/internal/opus"
	"github.com/huajun/open-xiaoai-next/hub/internal/ota"
	"github.com/huajun/open-xiaoai-next/hub/internal/registry"
	"github.com/huajun/open-xiaoai-next/hub/internal/reminders"
	"github.com/huajun/open-xiaoai-next/hub/internal/router"
	"github.com/huajun/open-xiaoai-next/hub/internal/session"
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
	cfgMu     *sync.Mutex // 与 web.Server.CfgLock 共享的配置锁（P1-22）
	reg       *registry.Registry
	server    *transport.Server
	arbiter   *arbitration.Arbiter
	pipeline  *voice.Pipeline
	router    *router.Router // P1-38：OnWake 在此记唤醒词；nil 时跳过（测试用）
	decoders  map[string]*opus.Decoder
	decMu     sync.Mutex
	intercom  *intercom.Manager
	otaMgr    *ota.Manager
	reminders *reminders.Store
	kws       *kws.Client // 服务端 KWS 边车客户端；nil 表示 KWS 未启用

	// latency 是分段延迟记录器（可观测性）：语音管线与 Router 共用，
	// Web 面板 /api/latency 读它。由 main 建好后分发给各处。
	latency *observability.Recorder

	// 连续对话会话：deviceID → 截止时间（唤醒后 20s 内不用再喊唤醒词）。
	sessions map[string]time.Time
	sessMu   sync.Mutex
	// playing 是正在放 TTS 的设备（打断用）；playGen 是每设备的播报
	// 代际计数（P1-16：旧播放的定时清除不得清除新播放）。
	playing map[string]bool
	playGen map[string]uint64
	playMu  sync.Mutex
	// playLocks 是每设备的播报串行锁（N-P1-3）：speakText 与 onReply
	// 走同一把锁，Opus 帧不再交错。
	playLocks   map[string]*sync.Mutex
	playLocksMu sync.Mutex
	speakText   func(deviceID, text string) // TTS 播报（提醒/主动播报用）
}

// playLock 取某设备的播报串行锁（不存在即新建）。
func (h *hubHandler) playLock(deviceID string) *sync.Mutex {
	h.playLocksMu.Lock()
	defer h.playLocksMu.Unlock()
	if h.playLocks == nil {
		h.playLocks = map[string]*sync.Mutex{}
	}
	if mu, ok := h.playLocks[deviceID]; ok {
		return mu
	}
	mu := &sync.Mutex{}
	h.playLocks[deviceID] = mu
	return mu
}

// playFrames 统一播放协议（P1-16）：start_play → 帧 → stop_play，
// 按帧数×20ms 估算时长维护 playing；空帧列表不发 start_play；
// 同设备播报经 playLock 串行化（N-P1-3）。
func (h *hubHandler) playFrames(deviceID string, frames [][]byte) {
	if len(frames) == 0 {
		return
	}
	mu := h.playLock(deviceID)
	mu.Lock()
	defer mu.Unlock()
	c, ok := h.server.Conn(deviceID)
	if !ok {
		return
	}
	h.playMu.Lock()
	if h.playGen == nil {
		h.playGen = map[string]uint64{}
	}
	h.playGen[deviceID]++
	gen := h.playGen[deviceID]
	h.playing[deviceID] = true
	h.playMu.Unlock()
	clearPlaying := func() {
		h.playMu.Lock()
		if h.playGen[deviceID] == gen {
			delete(h.playing, deviceID)
		}
		h.playMu.Unlock()
	}
	// 按音频时长定时清除 playing（只清本代）。
	dur := time.Duration(len(frames)) * 20 * time.Millisecond
	time.AfterFunc(dur, clearPlaying)
	if err := c.SendRPC("play-"+time.Now().Format("150405.000"), "start_play", nil); err != nil {
		log.Printf("播报 %s：start_play 失败（帧未发）: %v", deviceID, err)
		clearPlaying()
		return
	}
	for _, f := range frames {
		if err := c.SendBinary(f); err != nil {
			log.Printf("播报 %s：发帧失败（%v），停止本次播报", deviceID, err)
			break
		}
	}
	if err := c.SendRPC("stop-"+time.Now().Format("150405.000"), "stop_play", nil); err != nil {
		log.Printf("播报 %s：stop_play 失败: %v", deviceID, err)
	}
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
	// P1-38：记下本次唤醒词，供 Router 按唤醒词选 Agent。
	if h.router != nil {
		h.router.SetWakeWord(deviceID, w.WakeWord)
	}
	h.arbiter.Submit(arbitration.WakeEvent{
		DeviceID: deviceID, WakeWord: w.WakeWord, RMSdB: w.RMSdB,
		// P1-13：仲裁用事件自带时间戳（边车/客户端上报的 TsMs），
		// 为零时才补 Hub 收到时间；不再一律 time.Now() 覆盖。
		Timestamp: wakeTimestamp(w),
	})
}

// wakeTimestamp 取唤醒事件时间戳：TsMs > 0 时用它（毫秒时间戳），
// 否则用 Hub 收到时间。
func wakeTimestamp(w transport.Wake) time.Time {
	if w.TsMs > 0 {
		return time.UnixMilli(w.TsMs)
	}
	return time.Now()
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
	// P1-17：Forward 出错（目标掉线等）即记日志并 return，
	// 不再继续走语音管线（否则对讲断线后对方的话会被当成对本机的命令）。
	to, isBroadcast, err := h.intercom.Forward(deviceID, data)
	if err != nil {
		log.Printf("对讲转发失败（%s）: %v", deviceID, err)
		return
	}
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
// S-1：仲裁优先用边车上报的 rms_peak（1s 窗口峰值，不被 trailing 静音
// 稀释）；旧边车不发 rms_peak 时回退 rms 并记一条 warn。
func (h *hubHandler) OnKWSWake(ev kws.WakeEvent) {
	if ev.RMSPeak <= 0 {
		log.Printf("kws: 设备 %s 唤醒 %q 缺少 rms_peak，回退用 rms %v 仲裁", ev.DeviceID, ev.Keyword, ev.RMS)
	}
	rmsdB := -100.0
	if rms := ev.EffectiveRMS(); rms > 0 {
		rmsdB = 20 * math.Log10(rms)
	}
	if ev.TimestampMs > 0 {
		if lag := time.Now().UnixMilli() - ev.TimestampMs; lag >= 0 && lag < 60000 {
			log.Printf("kws: 设备 %s 唤醒 %q（边车上报延迟约 %dms）", ev.DeviceID, ev.Keyword, lag)
			// 可观测性：把这次唤醒的边车延迟暂存给延迟记录器，
			// 同设备下一次语音问答 Begin 时消费为 kws_ms。
			h.latency.NoteWake(ev.DeviceID, lag)
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
	// P2-3：房间名先校验——超长/含控制字符直接拒绝，不进注册表、
	// 配置文件与日志（SetRoom 内部也会再拦一道，这里先拦是为
	// 了给设备回一个明确的失败，而不是静默无事发生）。
	if err := registry.ValidateRoomName(room); err != nil {
		log.Printf("房间认领被拒（%s）：%v", deviceID, err)
		if c, ok := h.server.Conn(deviceID); ok {
			_ = c.SendJSON(map[string]string{"type": "room_reject", "reason": err.Error()})
		}
		return
	}
	if !h.reg.SetRoom(deviceID, room) {
		return
	}
	// 写回 config.yaml 持久化。P1-32：Save 失败必须记日志，不许吞掉
	//（只读挂载下房间认领重启后会丢失，操作员要能从日志看到）。
	// P1-22：与 Web PUT 共用 cfgMu（P1-23 的唯一临时文件只防写坏，
	// 不防两边同时写丢字段），持锁期间写配置 + 落盘。
	if h.cfgMu != nil {
		h.cfgMu.Lock()
		defer h.cfgMu.Unlock()
	}
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
	// P1-15：双向结束对讲（只 End(from) 会漏掉“对端发起、本设备是
	// 目标方”的会话，断线后对方的帧还会往已死的连接发）。
	h.intercom.EndFor(deviceID)
	// P1-28：断线清 Pipeline 残留 utterance 与 decoder，
	// 半句话不再残留，下次重连不被旧音频污染。
	if h.pipeline != nil {
		h.pipeline.Discard(deviceID)
	}
	h.decMu.Lock()
	delete(h.decoders, deviceID)
	h.decMu.Unlock()
	// P1-9：断线即视为设备重启（OTA 推送后设备重启重连走此路）：
	// 有 pushing 任务时转 verifying，30s 内等不到新版本 hello 即失败。
	if h.otaMgr != nil {
		h.otaMgr.MarkRebooted(deviceID)
	}
	h.playMu.Lock()
	delete(h.playing, deviceID)
	h.playMu.Unlock()
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
// P2-53：time.Tick 改 NewTicker + done 退出（旧代码 ticker 永不 Stop，
// 且 goroutine 在进程退出前永不返回）。
func (h *hubHandler) sweepSessions(done <-chan struct{}) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
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
}

// tightenFilePerm 把已存在文件的权限收紧到 perm（P2-4）：
// 文件不存在时什么都不做；收紧失败只记日志，不阻塞启动。
func tightenFilePerm(path string, perm os.FileMode) {
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	if fi.Mode().Perm() == perm {
		return
	}
	if err := os.Chmod(path, perm); err != nil {
		log.Printf("收紧 %s 权限到 %o 失败: %v", path, perm, err)
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
	// P1-45：启动检查 music_folder（融合 B：不存在只警告，不再 Fatal；
	// 本地曲库兜底不可用，点歌仍走网易云直连）。
	if err := cfg.ValidateStartup(); err != nil {
		log.Fatalf("config 启动检查失败: %v", err)
	}
	if w := cfg.MusicFolderWarning(); w != "" {
		log.Printf("警告：%s", w)
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
	// P2-4：数据目录 0700（旧代码 0755，同 NAS 多用户可列目录）；
	// 已有的数据文件（提醒/记忆含用户语音原文）在启动时收紧到 0600。
	if err := os.MkdirAll(cfg.Hub.DataDir, 0700); err != nil {
		log.Fatalf("建数据目录失败: %v", err)
	}
	tightenFilePerm(filepath.Join(cfg.Hub.DataDir, "reminders.json"), 0600)
	tightenFilePerm(filepath.Join(cfg.Hub.DataDir, "memory.json"), 0600)
	tightenFilePerm(filepath.Join(cfg.Hub.DataDir, "netease-cookie.json"), 0600)
	tightenFilePerm(*cfgPath, 0600)

	reg := registry.New()
	for id, d := range cfg.Devices {
		// P1-45：种子设备空 IP 不报错（出厂 config.yaml 本身含空 IP
		// 占位），这里跳过并记日志，等设备主动注册后再认领。
		if strings.TrimSpace(d.IP) == "" {
			log.Printf("种子设备 %s（%s）IP 为空，跳过注册，等设备主动注册", id, d.Model)
			continue
		}
		// P2-51：种子键归一化为真实 ID（model-IP，IP 里的 "." 换 "-"）——
		// 旧代码按 config.yaml 里的键原样注册，种子键
		// livingroom-speaker 与真实 ID lx06-<ip> 永远对不上，
		// 注册表里新旧条目并存、越积越多。
		realID := transport.DeviceIDFor(d.Model, d.IP)
		if id != realID {
			log.Printf("种子设备键 %q 已归一化为真实 ID %q", id, realID)
			id = realID
		}
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
	netease := music.NewNetease(cfg.Netease.API)
	// 网易云扫码登录（融合 B）：cookie 存在 Hub 数据卷
	// <data_dir>/netease-cookie.json（0600），只供 SongURL 取 VIP 歌用；
	// 没登录也能放免费歌。文件不存在表示从未登录过，不报错。
	neteaseAuth := music.NewNeteaseAuth(cfg.Netease.API, filepath.Join(cfg.Hub.DataDir, "netease-cookie.json"))
	if ok, err := neteaseAuth.Load(); err != nil {
		log.Printf("网易云登录 cookie 加载失败（VIP 歌可能放不了，需在 Web 面板重新扫码）: %v", err)
	} else if ok {
		netease.SetCookie(neteaseAuth.Cookie())
		log.Printf("网易云登录 cookie 已加载（VIP 歌可用）")
	}

	// Agent：全量建表供 Router 按唤醒词路由（P1-38）；默认大脑取
	// 唤醒词路由指向的 Agent，未配时按 Agent 名排序取首个（确定性，
	// 旧代码 Go map 随机迭代 break 是假路由）。
	agentClients := map[string]*agent.Client{}
	for name, a := range cfg.Agents {
		agentClients[name] = agent.New(a.BaseURL, a.Model, config.LLMAPIKey())
	}
	var agentClient *agent.Client
	if name, ok := cfg.WakeWordRoutes[cfg.WakeWord]; ok {
		agentClient = agentClients[name]
	}
	if agentClient == nil && len(agentClients) > 0 {
		names := make([]string, 0, len(agentClients))
		for name := range agentClients {
			names = append(names, name)
		}
		sort.Strings(names)
		agentClient = agentClients[names[0]]
	}

	// 提醒与记忆。
	remStore := reminders.New(cfg.Hub.DataDir+"/reminders.json", nil)
	// P2-44：Load 失败不再静默吞掉（损坏文件已由 Store 备份，
	// 这里记日志让运维知道今天的提醒可能丢了）。
	if err := remStore.Load(); err != nil {
		log.Printf("提醒存储加载失败: %v", err)
	}
	memory := router.NewMemory(cfg.Hub.DataDir + "/memory.json")
	if err := memory.Load(); err != nil {
		log.Printf("场景记忆加载失败: %v", err)
	}

	// 语音管线（STT/TTS 走 Wyoming Dialer，P1-27：懒拨号，断线后
	// 下次调用自动重拨重试一次；失败时错误带“STT/TTS 不可用”前缀，
	// 语音管线的降级播报靠此识别）。
	stt := wyoming.NewDialer(cfg.Hub.STT, 3*time.Second)
	tts := wyoming.NewDialer(cfg.Hub.TTS, 3*time.Second)

	// 对讲与 OTA。
	otaMgr := ota.New()

	// 可观测性：分段延迟记录器（最近 20 次交互），main 建一份，
	// 分给 hubHandler（记 KWS 唤醒延迟）、语音管线与 Router
	// （记 STT/LLM/TTS/点歌）、Web 面板（/api/latency 读）。
	latencyRec := observability.NewRecorder(observability.DefaultCapacity)

	// Handler 与 Server 先建（Pipeline 的 onReply 需要 server 引用，用闭包晚绑定）。
	// cfgMu 是 Web PUT 与房间认领共用的配置锁（P1-22），建好后传给两边。
	cfgMu := &sync.Mutex{}
	h := &hubHandler{
		cfg: cfg, cfgPath: *cfgPath, cfgMu: cfgMu, reg: reg,
		decoders: map[string]*opus.Decoder{},
		otaMgr:   otaMgr, reminders: remStore,
		sessions: map[string]time.Time{},
		playing:  map[string]bool{},
		playGen:  map[string]uint64{},
		latency:  latencyRec,
	}
	server := transport.New(token, reg, h)
	h.server = server
	// P1-44：注册制——设备在 WS 上发 register 主动注册，Hub 按指纹
	// 白名单校验；白名单为空时注册一律拒绝（transport 层 fail closed）。
	server.SetFingerprints(cfg.Hub.DiscoveryFingerprints)
	if len(cfg.Hub.DiscoveryFingerprints) == 0 {
		log.Printf("设备注册制：指纹白名单为空，register 一律拒绝（需在 config.yaml 配 hub.discovery_fingerprints）")
	} else {
		log.Printf("设备注册制：已加载 %d 个设备证书指纹白名单", len(cfg.Hub.DiscoveryFingerprints))
	}
	// P1-9：hello 成功即调 OTA ConfirmHello（verifying 的任务版本
	// 对上即成功；无任务时 ConfirmHello 返回 false，什么都不做）。
	server.OnHello = func(deviceID, version string) {
		if otaMgr.ConfirmHello(deviceID, version) {
			log.Printf("OTA 确认成功：%s → %s", deviceID, version)
		}
	}
	h.intercom = intercom.New(func(deviceID string, opusFrame []byte) error {
		c, ok := server.Conn(deviceID)
		if !ok {
			return fmt.Errorf("设备 %s 不在线", deviceID)
		}
		return c.SendBinary(opusFrame)
	})

	// 路由器（融合 B：点歌直连网易云 → MPD，不再经 MA；MPD 与 Snapcast
	// 在启动时 Dial，失败记日志并后台每 30s 重试，晚到时经
	// rt.SetMPD/SetSnap 补接）。
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
	// 多轮对话会话存根（内存 only，不落盘）：同一设备/房间在空闲超时
	// （默认 60s，hub.session_timeout_seconds）内连续对话，历史全量
	// 发给 LLM；过期由下面的 10s 扫荡清理。注意它与 hubHandler.sessions
	// 的 20s 音频会话是两回事：那个是“唤醒后 20s 内不用再喊唤醒词”，
	// 这个是“60s 内跟进式问题能对上前文”。
	convSessions := session.New(cfg.SessionTimeout())
	// Router 的 Notify 闭包要读看门狗热换后的最新 HA（P1-27），
	// 而 rt 在下面才赋值：先声明变量，闭包调用时再读。
	var rt *router.Router
	rt = router.New(router.Deps{
		HA: haClient, Netease: netease, Agent: agentClient,
		Agents: agentClients, WakeWordRoutes: cfg.WakeWordRoutes,
		Reminders: remStore, Memory: memory,
		Sessions:    convSessions,
		MusicFolder: cfg.MusicFolder,
		MPD:         mpdClient, Snap: snapClient,
		SnapLatencies: cfg.Hub.SnapcastLatencies,
		SnapGroupID:   cfg.Hub.SnapcastGroup,
		SnapStreamID:  cfg.Hub.SnapcastStream,
		// P1-15：对讲触发——语音“呼叫卧室”由此进 intercom；
		// P1-39：语音房间认领由此写注册表。
		Intercom: h.intercom,
		DeviceByRoom: func(room string) (string, bool) {
			d := reg.ByRoom(room)
			if d == nil {
				return "", false
			}
			return d.ID, true
		},
		RoomClaim: h.OnRoomClaim,
		RoomOfDevice: func(deviceID string) string {
			if d, ok := reg.Get(deviceID); ok {
				return d.Room
			}
			return ""
		},
		Notify: func(ctx context.Context, text string) error {
			// P1-27：读当前 HA（看门狗热换后是最新的），不是启动时那个。
			c := rt.HA()
			if c == nil {
				return fmt.Errorf("HA 未连接，无法通知")
			}
			return c.FireEvent("open_xiaoai_emergency", map[string]any{"text": text})
		},
	}, cfg.HAEntities)
	// P1-38：OnWake 在 hubHandler 里记唤醒词，供 Router 选 Agent。
	h.router = rt
	// 可观测性：Router 记 LLM 对话耗时（llm_ms）与点歌耗时（music_ms）。
	rt.Latency = latencyRec

	// P1-27 看门狗：每 30s 探活下游，断线重连并热更新到 Router，
	// 不再需要重启 Hub；接管原有的 MPD/Snap 启动重试 goroutine。
	// （Wyoming 走 Dialer 懒重拨，不用看门狗管；HA 重连后要给
	// alerts Watcher 重订，见下面的 watcher 变量。）
	// P2-53：关停通道——后台 goroutine（看门狗、会话清扫、
	// OTA/对讲/紧急扫荡）收到 done 后退出，不再靠 time.Tick
	// 泄漏到进程结束。
	done := make(chan struct{})

	var watcher *alerts.Watcher
	go func() {
		// P2-53：time.Tick 改 NewTicker + done 退出。
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
			}
			// MPD：Ping 失败即断开、Router 清空，下轮重拨。
			if cfg.Hub.MPD != "" {
				if mpdClient == nil {
					if c, err := music.DialMPD(cfg.Hub.MPD, 3*time.Second); err != nil {
						log.Printf("看门狗：MPD 重连失败: %v", err)
					} else {
						mpdClient = c
						rt.SetMPD(c)
						log.Printf("看门狗：MPD 已连上: %s", cfg.Hub.MPD)
					}
				} else if err := mpdClient.Ping(); err != nil {
					log.Printf("看门狗：MPD 探活失败，断开重连: %v", err)
					mpdClient.Close()
					mpdClient = nil
					rt.SetMPD(nil)
				}
			}
			// Snapcast：同 MPD（GetGroups 探活）。
			if cfg.Hub.Snapcast != "" {
				if snapClient == nil {
					if c, err := music.DialSnap(cfg.Hub.Snapcast, 3*time.Second); err != nil {
						log.Printf("看门狗：Snapcast 重连失败: %v", err)
					} else {
						snapClient = c
						rt.SetSnap(c)
						log.Printf("看门狗：Snapcast 已连上: %s", cfg.Hub.Snapcast)
					}
				} else if _, err := snapClient.GetGroups(); err != nil {
					log.Printf("看门狗：Snapcast 探活失败，断开重连: %v", err)
					snapClient.Close()
					snapClient = nil
					rt.SetSnap(nil)
				}
			}
			// HA：GetStates 探活；失败即断开、Router 清空；重连成功后
			// 热换并给 alerts Watcher 重订。
			if cfg.HomeAssistant.URL != "" {
				if haClient == nil {
					if c, err := ha.Dial(cfg.HomeAssistant.URL, config.HASSToken(), 5*time.Second); err != nil {
						log.Printf("看门狗：HA 重连失败: %v", err)
					} else {
						haClient = c
						rt.SetHA(c)
						log.Printf("看门狗：HA 已重连: %s", cfg.HomeAssistant.URL)
						if watcher != nil {
							if err := watcher.Rebind(c); err != nil {
								log.Printf("看门狗：HA 监视重订失败: %v", err)
							}
						}
					}
				} else if _, err := haClient.GetStates(); err != nil {
					log.Printf("看门狗：HA 探活失败，断开重连: %v", err)
					_ = haClient.Close()
					haClient = nil
					rt.SetHA(nil)
				}
			}
		}
	}()

	// Pipeline 的 onReply：把回答的 Opus 帧发给对应设备播放
	//（P1-16/N-P1-3：统一走 playFrames——start_play → 帧 → stop_play，
	// 按时长维护 playing，同设备播报串行化）。
	h.pipeline = voice.New(stt, tts, rt, cfg.Hub.TTSVoice, func(deviceID string, frames [][]byte) {
		h.playFrames(deviceID, frames)
	})
	// 可观测性：语音管线记 KWS/STT/TTS 分段耗时（见 voice.Pipeline.Latency）。
	h.pipeline.Latency = latencyRec
	// 提醒到点：KindAction 是定时动作（走 Route 执行命令），其余是播报。
	remStore.SetOnFire(func(r reminders.Reminder) {
		if r.Kind == reminders.KindAction {
			// P1-41：定时动作——把 Text 当命令走一遍 Route 执行，
			// 回复播报到对应房间的全部设备（P2-27：旧代码只取
			// ByRoom 随机一台，同房间的另一台收不到）。
			// 无房间时播到第一台在线设备。
			targets := []string{}
			if r.Room != "" {
				for _, d := range reg.ByRoomAll(r.Room) {
					targets = append(targets, d.ID)
				}
			}
			if len(targets) == 0 {
				if online := reg.Online(); len(online) > 0 {
					targets = append(targets, online[0].ID)
				}
			}
			if len(targets) == 0 {
				log.Printf("定时动作无在线设备，跳过：%s", r.Text)
				return
			}
			reply, err := rt.Route(context.Background(), targets[0], r.Text)
			if err != nil {
				log.Printf("定时动作执行失败（%s）: %v", r.Text, err)
				return
			}
			if reply != "" && h.speakText != nil {
				for _, devID := range targets {
					h.speakText(devID, reply)
				}
			}
			return
		}
		text := "提醒你" + r.Text
		if r.Room != "" {
			// P2-27：房间播报给该房间全部在线设备，不再随机一台。
			if devs := reg.ByRoomAll(r.Room); len(devs) > 0 && h.speakText != nil {
				for _, d := range devs {
					h.speakText(d.ID, text)
				}
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
	// P1-16/N-P1-3：合成后统一走 playFrames（start_play → 帧 → stop_play，
	// 同设备串行化）；Encode 中途出错记 error 日志并停止，不发半截音频。
	h.speakText = func(deviceID, text string) {
		// P1-2：提醒 goroutine 里 panic 也不能崩 Hub。
		defer func() {
			if r := recover(); r != nil {
				log.Printf("speakText panic 已兜底: %v", r)
			}
		}()
		if tts == nil {
			log.Printf("TTS 不可用（给 %s）：合成器为空，跳过播报", deviceID)
			return
		}
		audio, err := tts.Synthesize(text, cfg.Hub.TTSVoice)
		if err != nil {
			// P2-43 顺带：日志只记文本长度，不记全文（提醒内容可能含隐私）。
			log.Printf("TTS 合成失败（给 %s，%d 字）: %v", deviceID, len([]rune(text)), err)
			return
		}
		enc, err := opus.NewEncoder()
		if err != nil {
			log.Printf("speakText 编码器创建失败: %v", err)
			return
		}
		samples := make([]int16, 0, len(audio)/2)
		for i := 0; i+1 < len(audio); i += 2 {
			samples = append(samples, int16(audio[i])|int16(audio[i+1])<<8)
		}
		var frames [][]byte
		for i := 0; i+opus.FrameSize <= len(samples); i += opus.FrameSize {
			f, err := enc.Encode(samples[i : i+opus.FrameSize])
			if err != nil {
				log.Printf("speakText 编码失败（给 %s，第 %d 帧）: %v", deviceID, len(frames), err)
				return
			}
			frames = append(frames, f)
		}
		// P2-48：尾部不足一帧的采样补零到整帧再编码（与 voice.answer
		// 同款）——旧代码直接丢掉，TTS 尾音（常是句尾词）被吞。
		if rest := len(samples) % opus.FrameSize; rest != 0 {
			tail := make([]int16, opus.FrameSize)
			copy(tail, samples[len(samples)-rest:])
			f, err := enc.Encode(tail)
			if err != nil {
				log.Printf("speakText 尾帧编码失败（给 %s）: %v", deviceID, err)
				return
			}
			frames = append(frames, f)
		}
		h.playFrames(deviceID, frames)
	}

	// 仲裁：窗口关闭后，赢家开会话 + 发 wake_win，输家发 wake_lose。
	// P1-13：赢家失败补位——赢家已断线时按 RMS 降序试 losers，
	// 取第一个仍在线的当赢家；都掉线则本轮无人回。
	h.arbiter = arbitration.New(arbitration.Window, func(winner string, losers []string) {
		if winner == "" {
			return
		}
		chosen := winner
		if _, ok := server.Conn(winner); !ok {
			chosen = ""
			for _, l := range losers {
				if _, ok := server.Conn(l); ok {
					chosen = l
					break
				}
			}
			if chosen == "" {
				log.Printf("仲裁赢家 %s 已断线，输家也都不在线，本轮无人回", winner)
				return
			}
			log.Printf("仲裁赢家 %s 已断线，补位 %s", winner, chosen)
		}
		h.openSession(chosen)
		if c, ok := server.Conn(chosen); ok {
			_ = c.SendJSON(map[string]string{"type": "wake_win"})
		}
		for _, id := range append([]string{winner}, losers...) {
			if id == chosen {
				continue
			}
			if c, ok := server.Conn(id); ok {
				_ = c.SendJSON(map[string]string{"type": "wake_lose"})
			}
		}
	})
	go h.sweepSessions(done)

	// P1-9/P1-15：每 10s 扫一次——OTA 超时任务标失败（pushing 超 10 分钟
	// 或 verifying 超 30s）、对讲超时会话挂断、紧急呼叫过期清理。
	// P2-53：time.Tick 改 NewTicker + done 退出。
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
			}
			if failed := otaMgr.Sweep(time.Now()); len(failed) > 0 {
				log.Printf("OTA 扫荡：任务失败（超时未验证）: %v", failed)
			}
			if ended := h.intercom.Sweep(time.Now()); len(ended) > 0 {
				log.Printf("对讲扫荡：超时挂断: %v", ended)
			}
			rt.SweepEmergency(time.Now())
			// 多轮对话会话过期清理（内存释放；不调也行，取用时惰性清）。
			rt.SweepSessions(time.Now())
		}
	}()

	// --- 服务端 KWS 边车 ---
	// 配置非法（enabled=true 但 url/keywords 缺配或配错）时 New 即报错，
	// 启动失败并明说原因，不静默用错地址。边车运行时不可用不阻塞启动：
	// 降级为日志 + /api/status 报告 kws_down，音频流重连后自动恢复。
	var kwsClient *kws.Client
	if cfg.KWS.Enabled {
		// N-P2-10：边车走 wss + 自签证书时，把 config.yaml 配的
		// 指纹交给 Client（拨号与健康检查共用 pin 配置）。
		c, err := kws.New(cfg.KWS.URL, kws.Options{Keywords: cfg.KWS.Keywords, Token: config.Token(), TLSFingerprint: cfg.KWS.Fingerprint})
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
	// P0-3：mTLS——CA 签发的服务端证书 + 客户端 CA 池。
	// 证书在启动 goroutine 前签发（P1-10：指纹要同步交给 Web 面板，
	// 供刷机前取回写进 client.toml）。
	// LoadOrCreateMTLS() 会在旧自签 hub.crt 已存在且不是 CA 签发时
	// 重新签发覆盖（指纹随之变，client.toml 的 hub_ca_fingerprint
	// 需同步更新）；同时签发共享客户端证书（CN open-xiaoai-client，
	// 随固件下发给音箱）。SetClientCAs 后 Serve() 要求客户端出示
	// 该 CA 签发的证书（RequireAndVerifyClientCert），只做 Token
	// 鉴权的旧路径不再走。
	var serverCert *tls.Certificate
	hubFingerprint := ""
	if cfg.TLSEnabled() {
		cert, pool, fp, err := tlscert.LoadOrCreateMTLS(cfg.Hub.DataDir)
		if err != nil {
			log.Fatalf("TLS 证书失败: %v", err)
		}
		server.SetClientCAs(pool)
		serverCert = &cert
		hubFingerprint = fp
		log.Printf("wss 已启用（mTLS：要求客户端证书），Hub 证书指纹：%s（刷机前写进 client.toml 的 hub_ca_fingerprint；也可经 Web 面板 /api/fingerprint 取回）", fp)
	}
	// 传输层：wss :4399（或 tls=false 时纯 ws）。
	// P2-53：服务 goroutine 里不再 log.Fatalf（旧代码服务一崩
	// 整个 Hub 直接 exit，且提醒/记忆来不及落盘）——错误走
	// serveErr 通道，由主流程统一关停。
	serveErr := make(chan error, 1)
	go func() {
		if cfg.TLSEnabled() {
			if err := server.Serve(ln, serverCert); err != nil {
				serveErr <- fmt.Errorf("wss 服务退出: %w", err)
			}
		} else {
			log.Printf("警告：TLS 已关闭，走纯 ws（仅本地测试）")
			if err := server.Serve(ln, nil); err != nil {
				serveErr <- fmt.Errorf("ws 服务退出: %w", err)
			}
		}
	}()

	// --- Web 面板（Phase 4）---
	webServer := web.New(*cfgPath, cfg, reg, otaMgr)
	// P1-22：PUT 与房间认领共用 cfgMu；P1-10：指纹交给面板，
	// 供刷机前经 /api/fingerprint 取回。
	webServer.CfgLock = cfgMu
	webServer.HubFingerprint = hubFingerprint
	webServer.KWSStatus = func() string {
		if kwsClient == nil {
			return "disabled"
		}
		if kwsClient.Status().Down {
			return "down"
		}
		return "ok"
	}
	// 融合 B：音乐端点与网易云登录端点的后端——Router（MPD 队列 +
	// Hub 内存当前曲）、网易云客户端、扫码登录管理器。
	webServer.Music = rt
	webServer.Netease = netease
	webServer.NeteaseAuth = neteaseAuth
	// 可观测性：/api/latency 读延迟记录器；/api/health 用下面的
	// Checker 对 7 个容器做协议级探活（地址全取自 config.yaml
	// 已有字段，不新增配置项）。
	webServer.Latency = latencyRec
	webServer.Health = observability.NewChecker(observability.DefaultServices(observability.HealthAddrs{
		STT: cfg.Hub.STT, TTS: cfg.Hub.TTS,
		KWSWSURL: cfg.KWS.URL, KWSEnabled: cfg.KWS.Enabled,
		Snapcast: cfg.Hub.Snapcast, MPD: cfg.Hub.MPD,
		NeteaseAPI: cfg.Netease.API,
	}), observability.DefaultHealthTimeout)
	webServer.PushOTA = func(deviceID, version, url, sha256 string) error {
		c, ok := server.Conn(deviceID)
		if !ok {
			return fmt.Errorf("设备 %s 不在线", deviceID)
		}
		return c.SendJSON(map[string]string{
			"type": "ota", "version": version, "url": url, "sha256": sha256,
		})
	}
	// P2-53：Web 服务句柄留给关停流程做 Shutdown（旧代码只记
	// 日志、进程退出时不优雅关闭）。
	webSrv := webServer.NewHTTPServer()
	go func() {
		log.Printf("Web 面板：http://%s", webSrv.Addr)
		if err := webSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("Web 面板退出: %v", err)
		}
	}()

	// --- 摄像头监视（FR-10，Phase 4）---
	// 判定逻辑在 internal/alerts（ShouldAnnounce，可单测）：
	// 漏水/烟雾/水浸类实体变 on → 向所有在线设备播报，勿扰时段跳过。
	// watcher 变量在 P1-27 看门狗处已声明（HA 重连后由看门狗 Rebind）。
	if haClient != nil {
		watcher = alerts.New(haClient, cfg.Hub.DoNotDisturb, func(text string) {
			for _, d := range reg.Online() {
				h.speakText(d.ID, text)
			}
		})
		if err := watcher.Start(); err != nil {
			log.Printf("摄像头监视订阅失败: %v", err)
		}
	}

	log.Printf("open-xiaoai-hub 已启动：唤醒词 %q，监听 %s", cfg.WakeWord, cfg.Hub.Listen)

	// 等退出信号或 wss 服务崩溃（二者任一都走同一关停路径）。
	// P2-53：旧代码只等信号、不调 remStore.Stop()、不关 Wyoming
	// Dialer/MPD/Snap/Web——提醒可能丢、连接悬着。
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sig:
		log.Printf("收到退出信号，关闭中…")
	case err := <-serveErr:
		log.Printf("收到服务退出信号，关闭中…: %v", err)
	}
	// 先停后台 goroutine（看门狗/清扫/扫荡），再停各客户端；
	// 提醒存储最后 Stop（落盘正在进行的提醒）。
	close(done)
	if remStore != nil {
		remStore.Stop()
	}
	if kwsClient != nil {
		kwsClient.Close()
	}
	if stt != nil {
		_ = stt.Close()
	}
	if tts != nil {
		_ = tts.Close()
	}
	if mpdClient != nil {
		mpdClient.Close()
	}
	if snapClient != nil {
		snapClient.Close()
	}
	if haClient != nil {
		_ = haClient.Close()
	}
	// Web 面板优雅关闭（5s 内未处理完的请求直接断开）。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = webSrv.Shutdown(ctx)
	// alerts.Watcher 没有 Stop：订阅挂在 HA 客户端连接上，
	// 上面 haClient.Close() 已断开 SSE，监视随之结束。
}
