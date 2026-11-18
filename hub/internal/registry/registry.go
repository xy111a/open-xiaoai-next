// Package registry 是设备注册表：Hub 在内存里维护的设备表。
// 来源有三个：config.yaml 的 devices、自动发现、Client 连接时的 hello。
// 房间认领写回 config.yaml（Save 时持久化）。
package registry

import (
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// MaxRoomRunes 是房间名长度上限（P2-3）：16 个字符，够写“主卧卫生间”
// 这类名字，超长名字进不了注册表、配置文件与日志。
const MaxRoomRunes = 16

// MaxFieldLen 是 hello/status 里 Model/Version 类自由文本的长度上限
// （P2-3）：超长直接丢弃该字段，不进注册表。
const MaxFieldLen = 64

// ValidateRoomName 校验房间名（P2-3）：非空、≤16 字符、只允许中文/
// 字母/数字/常用标点，拒绝控制字符与换行（防日志伪造与配置污染）。
func ValidateRoomName(room string) error {
	if strings.TrimSpace(room) == "" {
		return errEmpty
	}
	if utf8.RuneCountInString(room) > MaxRoomRunes {
		return errTooLong
	}
	for _, r := range room {
		if unicode.IsControl(r) {
			return errControl
		}
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) ||
			strings.ContainsRune(" -_·.,，。、（）()「」『』", r)) {
			return errBadChar
		}
	}
	return nil
}

type roomErr string

func (e roomErr) Error() string { return string(e) }

const (
	errEmpty   = roomErr("房间名为空")
	errTooLong = roomErr("房间名超过 16 个字符")
	errControl = roomErr("房间名含控制字符")
	errBadChar = roomErr("房间名含不允许的字符")
)

// SanitizeField 截断/丢弃超长自由文本字段（P2-3）：超过 MaxFieldLen
// 即返回 ""（调用方当字段缺失处理），不让超长 Version/Model 进注册表。
func SanitizeField(s string) string {
	if len(s) > MaxFieldLen {
		return ""
	}
	return s
}

// Device 是一台音箱的运行态。
type Device struct {
	ID       string
	Model    string // lx06 / oh2p
	IP       string
	Room     string // 语音认领写入；未认领为空
	Online   bool
	Version  string
	LastSeen time.Time
}

// Registry 是线程安全的设备表。
type Registry struct {
	mu      sync.RWMutex
	devices map[string]*Device
}

// New 新建空注册表。
func New() *Registry {
	return &Registry{devices: map[string]*Device{}}
}

// Upsert 新增或更新设备（按 ID），返回该设备。
// 合并语义：只覆盖调用方给出的非空字段（Model/IP/Room/Version），不碰 Online
// （在线状态只由 SetOnline 改，避免一次缺字段的 Upsert 把已连设备标离线或抹掉型号/IP）。
// 每次 Upsert 都刷新 LastSeen。
func (r *Registry) Upsert(d Device) *Device {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.devices[d.ID]; ok {
		if d.Model != "" {
			old.Model = d.Model
		}
		if d.IP != "" {
			old.IP = d.IP
		}
		if d.Room != "" {
			old.Room = d.Room
		}
		if d.Version != "" {
			old.Version = d.Version
		}
		old.LastSeen = time.Now()
		return old
	}
	d.LastSeen = time.Now()
	r.devices[d.ID] = &d
	return &d
}

// SetOnline 标记在线/离线。
func (r *Registry) SetOnline(id string, online bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d, ok := r.devices[id]; ok {
		d.Online = online
		d.LastSeen = time.Now()
	}
}

// SetRoom 写入房间认领；设备不存在时返回 false。
// room 须先过 ValidateRoomName（P2-3），非法房间名不写入。
func (r *Registry) SetRoom(id, room string) bool {
	if err := ValidateRoomName(room); err != nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.devices[id]
	if !ok {
		return false
	}
	d.Room = room
	d.LastSeen = time.Now()
	return true
}

// ClearRoom 清空一台设备的房间（P2-51：Upsert 的“只覆盖非空字段”
// 语义让房间一旦写入便无法清空；清空是显式操作，走这里）。
func (r *Registry) ClearRoom(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.devices[id]
	if !ok {
		return false
	}
	d.Room = ""
	d.LastSeen = time.Now()
	return true
}

// FillRoomIfEmpty 只在设备当前无房间时写入 room（P2-51：hello 上报的
// 静态 room 不得覆盖语音认领的房间；已认领时 hello 的 room 被忽略）。
// room 为空或非法时什么都不做，返回 false。
func (r *Registry) FillRoomIfEmpty(id, room string) bool {
	if err := ValidateRoomName(room); err != nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.devices[id]
	if !ok || d.Room != "" {
		return false
	}
	d.Room = room
	d.LastSeen = time.Now()
	return true
}

// Get 取一台设备。
func (r *Registry) Get(id string) (*Device, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.devices[id]
	if !ok {
		return nil, false
	}
	cp := *d
	return &cp, true
}

// All 返回全部设备的快照。
func (r *Registry) All() []Device {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Device, 0, len(r.devices))
	for _, d := range r.devices {
		out = append(out, *d)
	}
	return out
}

// ByRoom 按房间名找在线设备；没找到返回 nil。
// 同房间多设备时按设备 ID 排序取第一台（P2-27：旧代码遍历 map 取
// 第一个在线，Go map 迭代顺序随机，每次可能挑不同设备；
// 需要全房间播报时用 ByRoomAll）。
func (r *Registry) ByRoom(room string) *Device {
	all := r.ByRoomAll(room)
	if len(all) == 0 {
		return nil
	}
	cp := all[0]
	return &cp
}

// ByRoomAll 按房间名返回全部在线设备，按设备 ID 排序（P2-27：
// 房间定向提醒应对同房间全部设备播报，而不是随机挑一台）。
func (r *Registry) ByRoomAll(room string) []Device {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Device
	for _, d := range r.devices {
		if d.Room == room && d.Online {
			out = append(out, *d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Online 返回所有在线设备。
func (r *Registry) Online() []Device {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Device
	for _, d := range r.devices {
		if d.Online {
			out = append(out, *d)
		}
	}
	return out
}
