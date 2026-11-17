// Package intercom 是对讲（FR-04）：点对点呼叫 + 全屋广播。
// 半双工（Phase 3）：发起方说完一句话（audio_end）即挂断，
// 音频帧原样转发，不经过 STT/TTS。触发可复用就近仲裁（Hub 知道哪间房发起的）。
// 全双工（Phase 4）：会话可在半双工与全双工之间切换；全双工下双向同时推流，
// 麦克风信号走 aec 单带 NLMS 回声消除（播放信号作参考，输出残余回声抑制后的信号）。
// 回声路径是 PCM 级的：Opus 帧需先解码成 PCM 才能进 NLMS，
// 真机回声抑制率需在音箱上实测（BLOCKED）。
package intercom

import (
	"sync"
	"time"

	"github.com/huajun/open-xiaoai-next/hub/internal/aec"
)

// Duplex 是会话的双工模式。
type Duplex int

const (
	// HalfDuplex 半双工：只有发起方 → 目标方单向推流。
	HalfDuplex Duplex = iota
	// FullDuplex 全双工：双向同时推流，麦克风信号过 AEC。
	FullDuplex
)

// String 返回模式名。
func (d Duplex) String() string {
	if d == FullDuplex {
		return "full"
	}
	return "half"
}

// Relay 是音频帧转发函数：把一帧 Opus 发给目标设备。
// 由 transport 层注入（Server.Conn(deviceID).SendBinary）。
type Relay func(deviceID string, opus []byte) error

// Session 是一次对讲会话。
type Session struct {
	From      string // 发起设备
	To        string // 目标设备；广播时为 ""，ToAll 为 true
	ToAll     bool
	Mode      Duplex // 半双工 / 全双工（点对点会话可切换，广播固定半双工）
	Started   time.Time
	LastFrame time.Time
	aec       *aec.NLMS // 全双工时的回声消除器（半双工为 nil）
}

// Manager 管理对讲会话。
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session // key 是发起设备 ID
	relay    Relay
	// IdleTimeout 是多久没收到帧算挂断（半双工默认 5s）。
	IdleTimeout time.Duration
}

// New 新建 Manager。
func New(relay Relay) *Manager {
	return &Manager{
		sessions:    map[string]*Session{},
		relay:       relay,
		IdleTimeout: 5 * time.Second,
	}
}

// Start 开始一次点对点呼叫。已在通话中则返回 false。
func (m *Manager) Start(from, to string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.participantBusy(from) || m.participantBusy(to) {
		return false
	}
	m.sessions[from] = &Session{From: from, To: to, Started: time.Now(), LastFrame: time.Now()}
	return true
}

// StartBroadcast 开始一次全屋广播。
func (m *Manager) StartBroadcast(from string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.participantBusy(from) {
		return false
	}
	m.sessions[from] = &Session{From: from, ToAll: true, Started: time.Now(), LastFrame: time.Now()}
	return true
}

// SetFullDuplex 把一次点对点会话切到全双工：新建 AEC（256 抽头 ≈ 16ms 回声尾长），
// 会话本身（发起方/目标方/开始时间/末帧时间）原样保留。
// 广播会话没有回声路径可接（单向），返回 false；不存在的会话返回 false。
func (m *Manager) SetFullDuplex(from string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[from]
	if !ok || s.ToAll {
		return false
	}
	if s.Mode == FullDuplex {
		return true
	}
	s.Mode = FullDuplex
	s.aec = aec.New(256)
	return true
}

// SetHalfDuplex 把会话切回半双工：丢掉 AEC 状态，会话本身原样保留。
func (m *Manager) SetHalfDuplex(from string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[from]
	if !ok {
		return false
	}
	s.Mode = HalfDuplex
	s.aec = nil
	return true
}

// ModeOf 返回会话的双工模式；会话不存在时 ok 为 false。
func (m *Manager) ModeOf(from string) (Duplex, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[from]
	if !ok {
		return HalfDuplex, false
	}
	return s.Mode, true
}

// EchoCancel 回声路径：ref 是 Hub 正在给该设备播放的信号（参考），
// mic 是该设备麦克风信号（输入），返回残余回声抑制后的信号。
// 只有全双工会话才走 AEC；半双工/无会话时原样返回 mic 的拷贝
// （半双工同一时刻只有一方说话，不需要回声消除）。
func (m *Manager) EchoCancel(from string, ref, mic []float64) []float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[from]
	// NLMS 不是线程安全的，全程持 Manager 锁跑完一帧
	//（320 点 × 256 抽头 < 1ms，不会堵住 Start/Forward）。
	if !ok || s.Mode != FullDuplex || s.aec == nil {
		out := make([]float64, len(mic))
		copy(out, mic)
		return out
	}
	return s.aec.ProcessFrame(ref, mic)
}
func (m *Manager) participantBusy(id string) bool {
	for _, s := range m.sessions {
		if s.From == id || s.To == id {
			return true
		}
	}
	return false
}

// Forward 转发一帧音频：只在会话进行中转发；返回目标列表（点对点 1 个，广播由调用方展开）。
// 半双工：只有发起方 → 目标方；全双工：双向都转发（目标方发来的帧中继回发起方）。
func (m *Manager) Forward(from string, opus []byte) (string, bool, error) {
	m.mu.Lock()
	s, ok := m.sessions[from]
	var to string
	var toAll bool
	if ok {
		to, toAll = s.To, s.ToAll
		s.LastFrame = time.Now()
	}
	if !ok && !toAll {
		// 全双工下目标方也可能发音频：找以 from 为目标方的全双工会话。
		for _, s2 := range m.sessions {
			if s2.Mode == FullDuplex && !s2.ToAll && s2.To == from {
				to = s2.From
				s2.LastFrame = time.Now()
				ok = true
				break
			}
		}
	}
	m.mu.Unlock()
	if !ok {
		return "", false, nil
	}
	if toAll {
		return "", true, nil
	}
	if m.relay == nil {
		return "", false, nil
	}
	if err := m.relay(to, opus); err != nil {
		return "", false, err
	}
	return to, false, nil
}

// End 结束一次会话（收到 audio_end 或超时时调）。
func (m *Manager) End(from string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[from]; !ok {
		return false
	}
	delete(m.sessions, from)
	return true
}

// Active 返回进行中的会话数。
func (m *Manager) Active() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// Sweep 清理超时未收帧的会话，返回清理掉的发起设备列表。
func (m *Manager) Sweep(now time.Time) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for from, s := range m.sessions {
		if now.Sub(s.LastFrame) > m.IdleTimeout {
			delete(m.sessions, from)
			out = append(out, from)
		}
	}
	return out
}
