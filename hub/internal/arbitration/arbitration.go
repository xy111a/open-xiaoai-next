// Package arbitration 是就近唤醒仲裁：250ms 窗口内比唤醒语音量（RMS 信号强度），
// 响的回，静的静默。只保证永远只回一台、且是能听清的那台。
//
// 已定决策（2026-11-17）：
//   - 不依赖房间信息，不追求百分百选最近。偶尔远处那台赢了也能接受，
//     失败模式只有两种：两台都回，或两台都不回。
//   - OH2P +6dB 补偿是可选微调：近处通常响 20–40dB，6dB 差距可忽略，
//     先不加，实测有问题再加。
//   - 协议抄 LVA 的 UDP 仲裁思路，但本项目走 Hub 内的 wss 事件，不另开 UDP。
package arbitration

import (
	"sync"
	"time"
)

// WakeEvent 是 Client 上报的唤醒事件。
type WakeEvent struct {
	DeviceID  string
	WakeWord  string
	RMSdB     float64 // 唤醒语音量，单位 dB
	Timestamp time.Time
}

// Window 是仲裁窗口，默认 250ms（可配 100–500ms）。
const Window = 250 * time.Millisecond

// Arbitrate 在窗口内收到的一组唤醒事件里选一台：RMS 最大的赢。
// 只有一台时直接赢；窗口内没收到任何事件时返回 ""（都不回）。
func Arbitrate(events []WakeEvent) string {
	winner := ""
	best := -1e9
	for _, e := range events {
		if e.RMSdB > best {
			best = e.RMSdB
			winner = e.DeviceID
		}
	}
	return winner
}

// Arbiter 是带窗口计时的仲裁器：收到第一条 wake 后开窗口，
// 窗口关闭时选出赢家并调 onResult（赢家 + 输家列表）。
type Arbiter struct {
	window   time.Duration
	onResult func(winner string, losers []string)

	mu      sync.Mutex
	pending []WakeEvent
	timer   *time.Timer
}

// New 新建 Arbiter。
func New(window time.Duration, onResult func(winner string, losers []string)) *Arbiter {
	if window <= 0 {
		window = Window
	}
	return &Arbiter{window: window, onResult: onResult}
}

// Submit 提交一条唤醒事件。第一条事件开窗口，后续事件进窗口收集。
func (a *Arbiter) Submit(ev WakeEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now()
	}
	a.pending = append(a.pending, ev)
	if a.timer == nil {
		a.timer = time.AfterFunc(a.window, a.close)
	}
}

func (a *Arbiter) close() {
	a.mu.Lock()
	events := a.pending
	a.pending = nil
	a.timer = nil
	a.mu.Unlock()
	winner := Arbitrate(events)
	var losers []string
	for _, e := range events {
		if e.DeviceID != winner {
			losers = append(losers, e.DeviceID)
		}
	}
	if a.onResult != nil {
		a.onResult(winner, losers)
	}
}
