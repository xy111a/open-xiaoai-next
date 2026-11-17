// Package reminders 是循环提醒（FR-14）：
// “每天 8 点提醒吃药”“每 30 分钟提醒一次”；到点播报，内容含事由；
// 可语音取消/改时间。提醒由 HA 自动化 + TTS 落地时，本包只做调度与持久化；
// Hub 内置调度器直接调 TTS 播报（不依赖 HA 在线）。
package reminders

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// idSeq 是 Add 生成 ID 的进程内自增序号（与纳秒时间戳组合，保证唯一）。
var idSeq atomic.Uint64

// Kind 是提醒类型。
type Kind string

const (
	// KindOnce 一次性提醒。
	KindOnce Kind = "once"
	// KindDaily 每天固定时间提醒。
	KindDaily Kind = "daily"
	// KindInterval 每隔 N 分钟提醒。
	KindInterval Kind = "interval"
)

// Reminder 是一条提醒。
type Reminder struct {
	ID      string    `json:"id"`
	Kind    Kind      `json:"kind"`
	Text    string    `json:"text"`  // 提醒内容（含事由），例如“吃药”
	Room    string    `json:"room"`  // 播报房间；空表示全屋
	At      string    `json:"at"`    // daily 用，格式 "08:00"
	Every   int       `json:"every"` // interval 用，分钟数
	Next    time.Time `json:"next"`  // 下次触发时间
	Enabled bool      `json:"enabled"`
}

// Store 是提醒的存储 + 调度器。
type Store struct {
	mu      sync.Mutex
	items   map[string]*Reminder
	path    string // 持久化文件（JSON）
	onFire  func(r Reminder)
	stopCh  chan struct{}
	stopped bool
}

// New 新建 Store。path 为空表示不持久化。onFire 在提醒到点时被调用。
func New(path string, onFire func(r Reminder)) *Store {
	return &Store{
		items:  map[string]*Reminder{},
		path:   path,
		onFire: onFire,
		stopCh: make(chan struct{}),
	}
}

// SetOnFire 设置到点回调（启动调度前调）。
func (s *Store) SetOnFire(fn func(r Reminder)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onFire = fn
}
func (s *Store) Load() error {
	if s.path == "" {
		return nil
	}
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var items []*Reminder
	if err := json.Unmarshal(b, &items); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range items {
		s.items[it.ID] = it
	}
	return nil
}

func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	var items []*Reminder
	for _, it := range s.items {
		items = append(items, it)
	}
	b, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Add 新增一条提醒，返回 ID。Next 由调用方算好传入。
// ID 用纳秒时间戳 + 进程内自增序号：同一毫秒内连加两条也不能撞 ID。
func (s *Store) Add(r Reminder) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.ID == "" {
		r.ID = fmt.Sprintf("%d-%d", time.Now().UnixNano(), idSeq.Add(1))
	}
	r.Enabled = true
	cp := r
	s.items[r.ID] = &cp
	return r.ID, s.saveLocked()
}

// Cancel 取消一条提醒（按 ID）；不存在返回 false。
func (s *Store) Cancel(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[id]; !ok {
		return false
	}
	delete(s.items, id)
	_ = s.saveLocked()
	return true
}

// CancelByText 按内容取消（语音取消用：“取消吃药提醒”）。
func (s *Store) CancelByText(text string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, it := range s.items {
		if it.Text == text {
			delete(s.items, id)
			n++
		}
	}
	_ = s.saveLocked()
	return n
}

// List 返回全部提醒的快照。
func (s *Store) List() []Reminder {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Reminder, 0, len(s.items))
	for _, it := range s.items {
		out = append(out, *it)
	}
	return out
}

// NextDaily 算下一个每天固定时间的触发点（今天已过则明天）。
func NextDaily(at string, now time.Time) (time.Time, error) {
	t, err := time.Parse("15:04", at)
	if err != nil {
		return time.Time{}, err
	}
	next := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next, nil
}

// Start 启动调度循环（每秒扫一次）。
func (s *Store) Start() {
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-s.stopCh:
				return
			case now := <-ticker.C:
				s.tick(now)
			}
		}
	}()
}

// Stop 停止调度循环。
func (s *Store) Stop() {
	s.mu.Lock()
	if !s.stopped {
		s.stopped = true
		close(s.stopCh)
	}
	s.mu.Unlock()
}

func (s *Store) tick(now time.Time) {
	s.mu.Lock()
	var due []*Reminder
	for _, it := range s.items {
		if !it.Enabled || it.Next.After(now) {
			continue
		}
		cp := *it
		due = append(due, &cp)
		// 算下一次。
		switch it.Kind {
		case KindOnce:
			delete(s.items, it.ID)
		case KindDaily:
			next, err := NextDaily(it.At, now)
			if err == nil {
				it.Next = next
			} else {
				delete(s.items, it.ID)
			}
		case KindInterval:
			if it.Every > 0 {
				it.Next = now.Add(time.Duration(it.Every) * time.Minute)
			} else {
				delete(s.items, it.ID)
			}
		}
	}
	_ = s.saveLocked()
	s.mu.Unlock()
	for _, r := range due {
		if s.onFire != nil {
			s.onFire(*r)
		}
	}
}
