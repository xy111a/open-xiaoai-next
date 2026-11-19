// Package observability 是 Hub 的可观测性：语音分段延迟记录与 NAS
// 7 个容器的健康检查。只依赖标准库，不依赖 voice/router/web，
// 避免导入环；上层（voice 管线、Router、Web 面板、main）
// 把它当作可选依赖注入——为 nil 时各调用点一律跳过，不影响主流程。
//
// 延迟 JSON 契约（GET /api/latency，见 hub/internal/web/web.go）：
//
//	{"interactions": [Interaction...], "count": N}
//
// Interaction:
//
//	{
//	  "id": "lx06-192-168-1-2-1732000000000000000", // deviceID + 起始纳秒
//	  "device_id": "lx06-192-168-1-2",
//	  "kind": "voice",              // voice=一次语音问答，music=一次点歌播放
//	  "started_at": "2026-11-19T17:00:00+08:00", // RFC3339
//	  "finished_at": "2026-11-19T17:00:03+08:00",
//	  "total_ms": 3120,             // Begin→Finish 的墙钟耗时
//	  "stages": {                   // 只含实际测到的分段，缺失=该段没走/没测到
//	    "kws_ms": 120,              // 边车唤醒事件时间戳→Hub 收到事件的延迟
//	    "stt_ms": 900,              // Wyoming STT 转写耗时
//	    "llm_ms": 800,              // Router 里真正跑 LLM 对话（Agent Chat）的耗时；
//	                                // 走 HA/音乐/提醒等本地快路时本段不存在
//	    "tts_ms": 600,              // Wyoming TTS 合成耗时
//	    "music_ms": 1500            // 点歌：网易云搜歌+取地址+MPD 起播的耗时
//	  },
//	  "ok": true,                   // false=本次问答中途出错
//	  "error": "..."                // ok=false 时的错误摘要（截断 120 字，不含语音原文）
//	}
//
// 列表按开始时间倒序，只保留最近 DefaultCapacity（20）条。
// 隐私：这里不存识别文本、问答内容、歌名，只存耗时与设备 ID。
package observability

import (
	"fmt"
	"sync"
	"time"
)

// DefaultCapacity 是延迟环形缓冲保留的最近交互条数。
const DefaultCapacity = 20

// StageKWS / StageSTT / StageLLM / StageTTS / StageMusic 是
// Interaction.Stages 的键。
const (
	StageKWS   = "kws_ms"
	StageSTT   = "stt_ms"
	StageLLM   = "llm_ms"
	StageTTS   = "tts_ms"
	StageMusic = "music_ms"
)

// pendingWakeTTL 是 NoteWake 记下的唤醒延迟的有效期：唤醒后要先等
// 用户说完一句话（VAD 静音 800ms 结算，单句最长 30s），
// 120s 与 voice.answerTimeout 对齐，过期未被 Begin 消费即丢弃，
// 避免一次旧唤醒的延迟被算到下一句不相关的问答头上。
const pendingWakeTTL = 120 * time.Second

// Interaction 是一次语音问答或一次点歌播放的分段延迟记录。
type Interaction struct {
	ID         string           `json:"id"`
	DeviceID   string           `json:"device_id"`
	Kind       string           `json:"kind"` // "voice" | "music"
	StartedAt  time.Time        `json:"started_at"`
	FinishedAt time.Time        `json:"finished_at,omitempty"`
	Stages     map[string]int64 `json:"stages"`
	TotalMs    int64            `json:"total_ms"`
	OK         bool             `json:"ok"`
	Error      string           `json:"error,omitempty"`

	started time.Time // 墙钟起点（StartedAt 的副本，供 TotalMs 计算）
}

type pendingWake struct {
	ms int64
	at time.Time
}

// Recorder 记分段延迟：环形缓冲 + 每设备的进行中交互。
// 零值不可用，用 NewRecorder 建；所有方法并发安全，
// Recorder 为 nil 时各方法（除 Recent/Latest 返回空外）一律空操作，
// 调用方不用每次判空——但 Begin/Finish 在 nil 上返回 nil，
// 调用方拿到 nil Interaction 后续 RecordStage 也同样空操作。
type Recorder struct {
	mu       sync.Mutex
	capacity int
	buf      []*Interaction // 倒序：最新在前
	active   map[string]*Interaction
	pending  map[string]pendingWake
}

// NewRecorder 新建 Recorder；capacity <= 0 时用 DefaultCapacity。
func NewRecorder(capacity int) *Recorder {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	return &Recorder{
		capacity: capacity,
		active:   map[string]*Interaction{},
		pending:  map[string]pendingWake{},
	}
}

// NoteWake 记一次唤醒的 KWS 延迟（毫秒）：边车 wake 事件自带时间戳，
// Hub 收到时算出的 lag。只暂存，等同设备的下一次 Begin 消费；
// 多次唤醒以最近一次为准。KWS 未启用/客户端直连唤醒时不调它，
// 该次问答就没有 kws_ms。
func (r *Recorder) NoteWake(deviceID string, kwsMs int64) {
	if r == nil || deviceID == "" {
		return
	}
	r.mu.Lock()
	r.pending[deviceID] = pendingWake{ms: kwsMs, at: time.Now()}
	r.mu.Unlock()
}

// Begin 开始一次交互（voice 管线 answer 入口调）：
// 消费同设备未过期的 pending KWS 延迟（有则写入 stages），
// 并登记为该设备的进行中交互，供 Router 的 RecordStageForDevice /
// RecordMusic 在问答中途把 llm_ms / music_ms 写进同一条记录。
// 同设备已有进行中交互时，旧的先按未完成（ok=false）归档，
// 不让它永久占着 active。
func (r *Recorder) Begin(deviceID, kind string) *Interaction {
	if r == nil {
		return nil
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.active[deviceID]; ok {
		r.finishLocked(old, fmt.Errorf("被同设备的新交互顶掉"))
	}
	inter := &Interaction{
		ID:        fmt.Sprintf("%s-%d", deviceID, now.UnixNano()),
		DeviceID:  deviceID,
		Kind:      kind,
		StartedAt: now,
		Stages:    map[string]int64{},
		started:   now,
	}
	if pw, ok := r.pending[deviceID]; ok {
		delete(r.pending, deviceID)
		if now.Sub(pw.at) <= pendingWakeTTL {
			inter.Stages[StageKWS] = pw.ms
		}
	}
	r.active[deviceID] = inter
	return inter
}

// RecordStage 把一段耗时写进某条交互（nil 交互空操作）。
func (r *Recorder) RecordStage(inter *Interaction, stage string, d time.Duration) {
	if r == nil || inter == nil || stage == "" {
		return
	}
	r.mu.Lock()
	inter.Stages[stage] = d.Milliseconds()
	r.mu.Unlock()
}

// RecordStageForDevice 把一段耗时写进某设备当前进行中的交互；
// 没有进行中交互时静默丢弃（例如提醒触发的 Router.Route，
// 本来就不属于一次语音问答）。
func (r *Recorder) RecordStageForDevice(deviceID, stage string, d time.Duration) {
	if r == nil || deviceID == "" || stage == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if inter, ok := r.active[deviceID]; ok {
		inter.Stages[stage] = d.Milliseconds()
	}
}

// RecordMusic 记一次点歌播放耗时：问答进行中时写进该条语音交互的
// music_ms；不在问答里时（例如以后 Web 面板直接点歌）
// 单独立一条 kind=music 的记录并立即归档。
func (r *Recorder) RecordMusic(deviceID string, d time.Duration) {
	if r == nil || deviceID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if inter, ok := r.active[deviceID]; ok {
		inter.Stages[StageMusic] = d.Milliseconds()
		return
	}
	now := time.Now()
	inter := &Interaction{
		ID:         fmt.Sprintf("%s-%d", deviceID, now.UnixNano()),
		DeviceID:   deviceID,
		Kind:       "music",
		StartedAt:  now,
		FinishedAt: now,
		Stages:     map[string]int64{StageMusic: d.Milliseconds()},
		TotalMs:    d.Milliseconds(),
		OK:         true,
		started:    now,
	}
	r.pushLocked(inter)
}

// Finish 结束一条交互：算 total、记 ok/error，推进环形缓冲，
// 并解除该设备的进行中登记。err 非 nil 时 ok=false，
// error 只留首行、截断 120 字（不含语音原文）。
func (r *Recorder) Finish(inter *Interaction, err error) {
	if r == nil || inter == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finishLocked(inter, err)
}

// finishLocked 是 Finish 的持锁实现。
func (r *Recorder) finishLocked(inter *Interaction, err error) {
	now := time.Now()
	inter.FinishedAt = now
	inter.TotalMs = now.Sub(inter.started).Milliseconds()
	inter.OK = err == nil
	if err != nil {
		msg := err.Error()
		// 只留首行。
		for i, c := range msg {
			if c == '\n' || c == '\r' {
				msg = msg[:i]
				break
			}
		}
		if len(msg) > 120 {
			msg = msg[:120]
		}
		inter.Error = msg
	}
	if cur, ok := r.active[inter.DeviceID]; ok && cur == inter {
		delete(r.active, inter.DeviceID)
	}
	r.pushLocked(inter)
}

// pushLocked 把一条已完成的交互放进环形缓冲头部，超容丢最旧的。
// 调用方须持锁。
func (r *Recorder) pushLocked(inter *Interaction) {
	r.buf = append([]*Interaction{inter}, r.buf...)
	if len(r.buf) > r.capacity {
		r.buf = r.buf[:r.capacity]
	}
}

// Recent 返回最近交互的快照（倒序：最新在前），是深拷贝，
// 调用方改它不影响 Recorder 内部。
func (r *Recorder) Recent() []Interaction {
	if r == nil {
		return []Interaction{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Interaction, 0, len(r.buf))
	for _, inter := range r.buf {
		out = append(out, inter.clone())
	}
	return out
}

// Latest 返回最新一条交互的快照；没有时返回 nil。
func (r *Recorder) Latest() *Interaction {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.buf) == 0 {
		return nil
	}
	c := r.buf[0].clone()
	return &c
}

// clone 深拷贝一条交互（Stages map 另起一份）。
func (in *Interaction) clone() Interaction {
	out := *in
	if in.Stages != nil {
		out.Stages = make(map[string]int64, len(in.Stages))
		for k, v := range in.Stages {
			out.Stages[k] = v
		}
	}
	return out
}
