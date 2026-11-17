// Package registry 是设备注册表：Hub 在内存里维护的设备表。
// 来源有三个：config.yaml 的 devices、自动发现、Client 连接时的 hello。
// 房间认领写回 config.yaml（Save 时持久化）。
package registry

import (
	"sync"
	"time"
)

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
func (r *Registry) SetRoom(id, room string) bool {
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
func (r *Registry) ByRoom(room string) *Device {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, d := range r.devices {
		if d.Room == room && d.Online {
			cp := *d
			return &cp
		}
	}
	return nil
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
