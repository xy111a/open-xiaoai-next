// Package ota 是 Hub OTA：一键升级 Client / Hub，不拆机。
//
// 流程：Hub 下发 ota 消息（version + url + sha256）→ Client 下载 →
// 验 sha256 → 写备用分区 → 校验 → 切换启动分区 → 重启。
// 失败自动回滚 boot1（原小米系统分区）：Client 新版本启动后必须在
// 30s 内重新 hello，超时 Hub 标记失败，设备下次启动仍走旧分区。
//
// 红线（已验证，见 firmware/REA_ANALYSIS.md）：
//   - 免拆机伪造小米官方 OTA 不可能（RSA + SHA1，私钥在小米手里），
//     所以第一次必须拆机刷补丁固件；之后全部走 Hub 自有 OTA。
//   - boot 切换错了会变砖：切换前必须校验新镜像 sha256 + 保留 boot1 回滚路径。
//   - 固件总盘 40MB 上限（0x02800000），Client 二进制目标 < 5MB。
package ota

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// Status 是 OTA 状态。
type Status string

const (
	// StatusIdle 无进行中的 OTA。
	StatusIdle Status = "idle"
	// StatusPushing 已下发，等待设备下载校验。
	StatusPushing Status = "pushing"
	// StatusVerifying 设备已重启，等待新版本 hello。
	StatusVerifying Status = "verifying"
	// StatusOK 设备已用新版本 hello，升级成功。
	StatusOK Status = "ok"
	// StatusFailed 超时未 hello 或设备上报失败，已回滚。
	StatusFailed Status = "failed"
)

// Job 是一次 OTA 任务。
type Job struct {
	DeviceID string
	Version  string
	URL      string
	SHA256   string
	Status   Status
	Started  time.Time
	Updated  time.Time
	VerifyBy time.Time // 等待新版本 hello 的截止时间
}

// Manager 管理 OTA 任务。
type Manager struct {
	mu   sync.Mutex
	jobs map[string]*Job // key 是设备 ID
	// VerifyTimeout 是等新版本 hello 的超时（默认 30s）。
	VerifyTimeout time.Duration
	// PushTimeout 是 pushing 状态的最长停留（默认 10 分钟，P1-9）：
	// 推送后设备一直不重启、不 hello，任务不能永远停在 pushing，
	// Sweep 到期即标失败（回滚由设备端 boot1 保证，Hub 只记状态）。
	PushTimeout time.Duration
}

// New 新建 Manager。
func New() *Manager {
	return &Manager{jobs: map[string]*Job{}, VerifyTimeout: 30 * time.Second, PushTimeout: 10 * time.Minute}
}

func (m *Manager) pushTimeout() time.Duration {
	if m.PushTimeout <= 0 {
		return 10 * time.Minute
	}
	return m.PushTimeout
}

// Push 开始一次 OTA：校验参数并记录任务。
func (m *Manager) Push(deviceID, version, url, sha256hex string) (*Job, error) {
	if deviceID == "" || version == "" || url == "" {
		return nil, fmt.Errorf("ota: device/version/url 不能为空")
	}
	if len(sha256hex) != 64 {
		return nil, fmt.Errorf("ota: sha256 必须是 64 位 hex，实际 %d 位", len(sha256hex))
	}
	if _, err := hex.DecodeString(sha256hex); err != nil {
		return nil, fmt.Errorf("ota: sha256 不是合法 hex: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if j, ok := m.jobs[deviceID]; ok && (j.Status == StatusPushing || j.Status == StatusVerifying) {
		return nil, fmt.Errorf("ota: 设备 %s 已有进行中的任务（%s）", deviceID, j.Status)
	}
	now := time.Now()
	j := &Job{
		DeviceID: deviceID, Version: version, URL: url, SHA256: sha256hex,
		Status: StatusPushing, Started: now, Updated: now,
	}
	m.jobs[deviceID] = j
	return j, nil
}

// MarkDownloading 设备已开始下载（Client 上报后调，暂未用协议字段，预留）。
func (m *Manager) MarkDownloading(deviceID string) {}

// MarkRebooted 设备已重启进新版本等待验证。
func (m *Manager) MarkRebooted(deviceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j, ok := m.jobs[deviceID]; ok && j.Status == StatusPushing {
		j.Status = StatusVerifying
		j.VerifyBy = time.Now().Add(m.VerifyTimeout)
		j.Updated = time.Now()
	}
}

// ConfirmHello 设备用新版本重新 hello，确认成功。
func (m *Manager) ConfirmHello(deviceID, version string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[deviceID]
	if !ok || j.Status != StatusVerifying {
		return false
	}
	if j.Version != version {
		return false
	}
	j.Status = StatusOK
	j.Updated = time.Now()
	return true
}

// Sweep 超时未验证的任务标记失败（回滚由设备端 boot1 保证，Hub 只记状态）。
// P1-9：除 verifying 超时外，pushing 超过 PushTimeout 仍无进展的任务
// 也标失败，不再永远停在 pushing。
func (m *Manager) Sweep(now time.Time) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var failed []string
	for id, j := range m.jobs {
		if j.Status == StatusVerifying && now.After(j.VerifyBy) {
			j.Status = StatusFailed
			j.Updated = now
			failed = append(failed, id)
			continue
		}
		if j.Status == StatusPushing && now.Sub(j.Started) > m.pushTimeout() {
			j.Status = StatusFailed
			j.Updated = now
			failed = append(failed, id)
		}
	}
	return failed
}

// List 返回全部任务的快照（P1-9：Web 面板 GET /api/ota 用）。
func (m *Manager) List() []Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		out = append(out, *j)
	}
	return out
}

// Get 取任务状态。
func (m *Manager) Get(deviceID string) (*Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[deviceID]
	if !ok {
		return nil, false
	}
	cp := *j
	return &cp, true
}

// SHA256Hex 算字节流的 sha256 hex（供 Hub 生成 ota 消息前校验镜像文件）。
func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
