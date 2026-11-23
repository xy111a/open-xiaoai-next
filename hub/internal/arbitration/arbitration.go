// Package arbitration 是就近唤醒仲裁：250ms 窗口内比唤醒语音量（RMS 信号强度），
// 响的回，静的静默。只保证永远只回一台、且是能听清的那台。
//
// 已定决策（2026-11-17）：
//   - 不依赖房间信息，不追求百分百选最近。偶尔远处那台赢了也能接受，
//     失败模式只有两种：两台都回，或两台都不回。
//   - OH2P +6dB 补偿是可选微调：近处通常响 20–40dB，6dB 差距可忽略，
//     先不加，实测有问题再加。
//   - 协议抄 LVA 的 UDP 仲裁思路，但本项目走 Hub 内的 wss 事件，不另开 UDP。
//
// 2026-11-23 升级（设计思路借鉴 home-mate 的 arbitration.rs，独立重新实现，
// 未复制其代码）：纯“谁大谁赢”升级为三级裁决——响度门限胜出 → 门限内按
// 到达时间 → 完全平局按固定设备序；会话粘性加分；每次裁决留审计记录。
// 刻意没做的事（数据不存在，不编造）：
//   - 没有 SNR/削波/回声质量门：唤醒事件里只有 RMSdB 一个响度字段，
//     边车 rms_peak 已由 kws.EffectiveRMS 折进 RMSdB（S-1），
//     客户端本地唤醒报的是单帧 RMS（client/src/kws.rs 的 wake_rms_db）。
//     没有峰值/SNR/回声字段可供裁决，就不设质量门。
//   - 没有新增 PeakdB 字段：客户端要提供峰值需改 Rust 客户端并重刷机，
//     改动不小；边车峰值已在 RMSdB 里，够用。
//   - 归并窗仍是 250ms，不拉到 home-mate 的 2400ms：2400ms 覆盖的是
//     1.3–1.4s 跨房间触发间隔，但回答要等窗口关闭才开始，2400ms 会让
//     每次唤醒都多等 2 秒以上；本项目以首声速度为先，250ms 窗口 +
//     500ms 迟到容忍 + 1s 冷却已能覆盖局域网内同句唤醒的正常 spread，
//     超过 250ms 的 spread 仍可能两台各回一次（2026-11-17 已接受的
//     失败模式），部署日实测 spread 再定要不要动窗口。
package arbitration

import (
	"log"
	"sort"
	"sync"
	"time"
)

// WakeEvent 是 Client 上报的唤醒事件。
type WakeEvent struct {
	DeviceID  string
	WakeWord  string
	RMSdB     float64 // 唤醒语音量，单位 dB（边车事件已是峰值优先的 EffectiveRMS，见 kws.S-1；本地唤醒是单帧 RMS）
	Timestamp time.Time
}

// Window 是仲裁窗口，默认 250ms（可配 100–500ms）。
const Window = 250 * time.Millisecond

const (
	// dedupWindow 是同一设备重复唤醒的去重窗口：双通道（本地 wake +
	// 边车 wake）同一声唤醒的时间戳相差一般 <1s，视为一次。
	dedupWindow = 1000 * time.Millisecond
	// staleGrace 是陈旧事件的容忍：窗口已开后，比窗口起点早 500ms
	// 以上的事件视为迟到，丢弃（否则迟到 250ms 以上会开第二窗口各赢一次）。
	staleGrace = 500 * time.Millisecond
	// cooldown 是裁决后的冷却期：冷却期内时间戳不晚于上一轮关闭的
	// 事件视为迟到重复，直接丢弃。
	cooldown = 1000 * time.Millisecond
	// minLoudnessDBFS 是响度门限胜出的下限：赢家原始响度低于 -50dBFS
	// 时不认响度胜出，改按到达时间裁决（数值取自 home-mate 的
	// MIN_LOUDNESS_EVIDENCE_DBFS；-50dBFS 以下基本是底噪/远处串音，
	// 响度已不可信）。
	minLoudnessDBFS = -50.0
	// louderMarginDB 是响度翻盘阈值：赢家有效响度要比第二名响至少
	// 3dB 才算响度胜出，否则进到达时间裁决（home-mate LOUDER_DB）。
	louderMarginDB = 3.0
	// stickyBonusDB 是会话粘性加分：上一轮赢家在 20s 内再进裁决时，
	// 有效响度 +4dB（home-mate SESSION_STICKY_BONUS_DB），防对话中途
	// 回答在两台音箱之间来回跳。
	stickyBonusDB = 4.0
	// stickyWindow 是粘性加分的有效期：与 main.go 的 20s 连续对话
	// 会话同窗——同一会话里重复唤醒才谈得上“粘性”，过期即新会话。
	stickyWindow = 20 * time.Second
)

// Reason 是一次裁决的胜出原因（审计用）。
type Reason string

const (
	// ReasonSolo 只有一台候选，直接赢。
	ReasonSolo Reason = "solo"
	// ReasonLoudness 响度门限胜出：赢家原始响度 ≥-50dBFS 且有效响度
	// 比第二名响 ≥3dB。
	ReasonLoudness Reason = "loudness"
	// ReasonSticky 靠会话粘性加分才赢：不加分时响度差 <3dB，
	// 加分后才满足门限。
	ReasonSticky Reason = "sticky"
	// ReasonArrival 门限内按到达时间：响度分不出（差 <3dB 或赢家
	// 低于 -50dBFS），在可信范围（与最响者有效值差距 <3dB）内最早到者赢。
	ReasonArrival Reason = "arrival"
	// ReasonTieBreak 完全平局：可信范围内最早到的多台有效响度与
	// 时间戳都相同，按固定设备顺序（deviceID 字典序）裁决。
	ReasonTieBreak Reason = "tiebreak"
)

// Decision 是一次裁决的结果（审计用，部署日排查看日志里的同款字段）。
type Decision struct {
	Winner string // 赢家设备 ID；空事件时为 ""
	// Losers 是失败补位顺序（按有效响度降序，不含赢家）：
	// 调用方做赢家失败补位时按此顺序尝试。
	Losers []string
	Reason Reason
	// MarginDB 是赢家与有效响度第二名的有效响度差（dB）：
	// 到达时间/平局胜出时可能为负（赢家比最响者还轻），如实记。
	MarginDB float64
	// MarginMS 是赢家比有效响度第二名早到的毫秒数：
	// 负值 = 赢家反而晚到；单台/空事件时为 0。
	MarginMS int64
	// Considered 是参与裁决的设备数（去重后）。
	Considered int
	// Sticky 是赢家是否吃了会话粘性加分。
	Sticky bool
}

// effectiveLoudness 是某事件的有效响度：原始响度 + 粘性加分
// （上一轮赢家在粘性窗口内再进裁决时 +4dB）。
func effectiveLoudness(e WakeEvent, lastWinner string, lastWinAt time.Time, now time.Time) (eff float64, sticky bool) {
	eff = e.RMSdB
	if lastWinner != "" && e.DeviceID == lastWinner && !lastWinAt.IsZero() && now.Sub(lastWinAt) <= stickyWindow {
		eff += stickyBonusDB
		sticky = true
	}
	return eff, sticky
}

// Decide 对一组唤醒事件做一次裁决（纯函数，可单测；Arbiter.close 用它）。
// 前提：同一设备至多一条事件（Submit 的 1s 去重保证；多余的按时间戳
// 最早一条保留，防御性处理）。
// 永不静默：非空事件必有赢家；空事件返回 Winner ""。
func Decide(events []WakeEvent, lastWinner string, lastWinAt time.Time, now time.Time) Decision {
	if len(events) == 0 {
		return Decision{}
	}
	// 同设备去重：只留时间戳最早的一条（与 Submit“先到者为准”一致）。
	byDevice := map[string]WakeEvent{}
	for _, e := range events {
		if old, ok := byDevice[e.DeviceID]; !ok || e.Timestamp.Before(old.Timestamp) {
			byDevice[e.DeviceID] = e
		}
	}
	uniq := make([]WakeEvent, 0, len(byDevice))
	for _, e := range byDevice {
		uniq = append(uniq, e)
	}
	// 按有效响度降序；同值按到达时间升序；还同按设备 ID 字典序
	// （全确定性排序，裁决结果不依赖输入顺序）。
	type scored struct {
		ev     WakeEvent
		eff    float64
		sticky bool
	}
	scoredList := make([]scored, 0, len(uniq))
	for _, e := range uniq {
		eff, sticky := effectiveLoudness(e, lastWinner, lastWinAt, now)
		scoredList = append(scoredList, scored{ev: e, eff: eff, sticky: sticky})
	}
	sort.Slice(scoredList, func(i, j int) bool {
		if scoredList[i].eff != scoredList[j].eff {
			return scoredList[i].eff > scoredList[j].eff
		}
		if !scoredList[i].ev.Timestamp.Equal(scoredList[j].ev.Timestamp) {
			return scoredList[i].ev.Timestamp.Before(scoredList[j].ev.Timestamp)
		}
		return scoredList[i].ev.DeviceID < scoredList[j].ev.DeviceID
	})
	top := scoredList[0]
	losers := make([]string, 0, len(scoredList)-1)
	for _, s := range scoredList[1:] {
		losers = append(losers, s.ev.DeviceID)
	}
	d := Decision{
		Winner:     top.ev.DeviceID,
		Losers:     losers,
		Considered: len(scoredList),
		Sticky:     top.sticky,
	}
	if len(scoredList) == 1 {
		d.Reason = ReasonSolo
		return d
	}
	second := scoredList[1]
	d.MarginDB = top.eff - second.eff
	d.MarginMS = second.ev.Timestamp.Sub(top.ev.Timestamp).Milliseconds()
	// 一级：响度门限胜出。
	if top.ev.RMSdB >= minLoudnessDBFS && d.MarginDB >= louderMarginDB {
		if top.sticky && top.ev.RMSdB-second.eff < louderMarginDB {
			// 不加分就赢不了：记 sticky，审计时一眼看出是粘性在起作用。
			d.Reason = ReasonSticky
		} else {
			d.Reason = ReasonLoudness
		}
		return d
	}
	// 二级：门限内按到达时间。
	// 可信范围的取法分两种（与 home-mate 的 tier 规则同义）：
	//   - 最响者达到下限（响度证据可用）：可信 = 原始响度 ≥-50dBFS
	//     且与最响者有效值差距 <3dB 的 claim；
	//   - 最响者低于下限（响度证据整体不可信）：全体参与，
	//     不再用响度筛人——否则最早到的那台可能被 3dB 圈子不合理排除。
	var credible []scored
	if top.ev.RMSdB >= minLoudnessDBFS {
		for _, s := range scoredList {
			if s.ev.RMSdB >= minLoudnessDBFS && top.eff-s.eff < louderMarginDB {
				credible = append(credible, s)
			}
		}
	} else {
		credible = scoredList
	}
	// 可信范围内最早到者赢；最早时间戳有多台且有效响度相同 → 固定设备序。
	earliest := credible[0].ev.Timestamp
	for _, s := range credible {
		if s.ev.Timestamp.Before(earliest) {
			earliest = s.ev.Timestamp
		}
	}
	var tied []scored
	for _, s := range credible {
		if s.ev.Timestamp.Equal(earliest) {
			tied = append(tied, s)
		}
	}
	winner := tied[0]
	reason := ReasonArrival
	if len(tied) > 1 {
		// 时间戳相同：有效响度最高者赢；还相同 → 设备 ID 字典序（tied
		// 已按 eff 降序、deviceID 升序排过，取首个即固定顺序）。
		best := tied[0]
		allEqual := true
		for _, s := range tied[1:] {
			if s.eff != best.eff {
				allEqual = false
				break
			}
		}
		if allEqual {
			reason = ReasonTieBreak
		}
		winner = best
	}
	// 赢家可能不是响度第一：重算审计字段（MarginDB/MarginMS 统一口径为
	// “赢家 vs 有效响度第二名”，第二名指除赢家外有效响度最高者）。
	d.Winner = winner.ev.DeviceID
	d.Reason = reason
	d.Sticky = winner.sticky
	var runnerUp scored
	for _, s := range scoredList {
		if s.ev.DeviceID != winner.ev.DeviceID {
			runnerUp = s
			break
		}
	}
	d.MarginDB = winner.eff - runnerUp.eff
	d.MarginMS = runnerUp.ev.Timestamp.Sub(winner.ev.Timestamp).Milliseconds()
	// 输家顺序重排：除赢家外按有效响度降序（失败补位顺序）。
	d.Losers = d.Losers[:0]
	for _, s := range scoredList {
		if s.ev.DeviceID != winner.ev.DeviceID {
			d.Losers = append(d.Losers, s.ev.DeviceID)
		}
	}
	return d
}

// Arbitrate 在窗口内收到的一组唤醒事件里选一台（旧接口，保留给
// 单测与简单调用方；无粘性上下文，等于 Decide(..., "", ...).Winner）。
// 只有一台时直接赢；窗口内没收到任何事件时返回 ""（都不回）。
func Arbitrate(events []WakeEvent) string {
	return Decide(events, "", time.Time{}, time.Now()).Winner
}

// Arbiter 是带窗口计时的仲裁器：收到第一条 wake 后开窗口，
// 窗口关闭时选出赢家并调 onResult（赢家 + 按有效响度降序的输家列表，
// 调用方按此顺序做赢家失败补位）。
type Arbiter struct {
	window   time.Duration
	onResult func(winner string, losers []string)

	mu          sync.Mutex
	pending     []WakeEvent
	timer       *time.Timer
	windowStart time.Time // 本轮窗口开启的到达时间
	lastClose   time.Time // 上一轮窗口关闭的时间（冷却期用）
	lastWinner  string    // 上一轮赢家（会话粘性加分用）
	lastWinAt   time.Time // 上一轮裁决时间（粘性窗口用）
}

// New 新建 Arbiter。
func New(window time.Duration, onResult func(winner string, losers []string)) *Arbiter {
	if window <= 0 {
		window = Window
	}
	return &Arbiter{window: window, onResult: onResult}
}

// Submit 提交一条唤醒事件。第一条事件开窗口，后续事件进窗口收集。
// P1-13：用事件自带 Timestamp 做去重/去陈旧/冷却，不再只认到达时间——
// 同设备 1s 内的重复提交（本地 + 边车双通道）只留一条；窗口已开后
// 比窗口起点早 500ms 以上的迟到事件丢弃；裁决后 1s 冷却期内时间戳
// 不晚于上一轮关闭的迟到事件丢弃。
func (a *Arbiter) Submit(ev WakeEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now()
	}
	now := time.Now()
	if !a.lastClose.IsZero() && now.Sub(a.lastClose) < cooldown && !ev.Timestamp.After(a.lastClose) {
		return
	}
	for _, p := range a.pending {
		if p.DeviceID == ev.DeviceID {
			diff := ev.Timestamp.Sub(p.Timestamp)
			if diff < 0 {
				diff = -diff
			}
			if diff < dedupWindow {
				return
			}
		}
	}
	if a.timer != nil && ev.Timestamp.Before(a.windowStart.Add(-staleGrace)) {
		return
	}
	a.pending = append(a.pending, ev)
	if a.timer == nil {
		a.windowStart = now
		a.timer = time.AfterFunc(a.window, a.close)
	}
}

func (a *Arbiter) close() {
	a.mu.Lock()
	events := a.pending
	a.pending = nil
	a.timer = nil
	now := time.Now()
	a.lastClose = now
	lastWinner, lastWinAt := a.lastWinner, a.lastWinAt
	a.mu.Unlock()
	decision := Decide(events, lastWinner, lastWinAt, now)
	if decision.Winner != "" {
		a.mu.Lock()
		a.lastWinner = decision.Winner
		a.lastWinAt = now
		a.mu.Unlock()
	}
	// 审计记录：每次裁决都留一行，供部署日排查“为什么是这台回”。
	log.Printf("arbitration: 赢家 %s（原因 %s，有效响度差 %.1fdB，到达差 %dms，参与 %d 台，粘性加分 %v），输家 %v",
		decision.Winner, decision.Reason, decision.MarginDB, decision.MarginMS, decision.Considered, decision.Sticky, decision.Losers)
	if a.onResult != nil {
		a.onResult(decision.Winner, decision.Losers)
	}
}
