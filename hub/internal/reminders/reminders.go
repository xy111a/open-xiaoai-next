// Package reminders 是循环提醒（FR-14）：
// “每天 8 点提醒吃药”“每 30 分钟提醒一次”；到点播报，内容含事由；
// 可语音取消/改时间。提醒由 HA 自动化 + TTS 落地时，本包只做调度与持久化；
// Hub 内置调度器直接调 TTS 播报（不依赖 HA 在线）。
package reminders

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
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
	// KindAction 定时动作（P1-41）：“2 小时后关灯”——触发时执行一次命令
	// 而不是播报，触发后删除，与 KindOnce 同调度语义。
	KindAction Kind = "action"
)

// validKind 是合法 Kind 白名单（P1-24：Add/Load 拒绝未知 Kind）。
func validKind(k Kind) bool {
	switch k {
	case KindOnce, KindDaily, KindInterval, KindAction:
		return true
	}
	return false
}

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
	onFire  func(r Reminder) error
	stopCh  chan struct{}
	started bool
	// dirty 是“内存已改、落盘失败待重试”标记（P2-14）：saveLocked
	// 失败时置位，下次 tick 重试保存，不让一次磁盘错误丢掉整批变更。
	dirty bool
	// attempts 是执行失败的重试计数（P1-12）：once/action 执行失败时
	// 保留并至多重试 1 次；计数只放内存（重启后清零，可接受——
	// 重启后提醒按 Next 正常触发，失败计数重新算）。
	attempts map[string]int
}

// reminderRetryDelay 是一次性提醒执行失败后的重试间隔（P1-12）：1 分钟。
const reminderRetryDelay = time.Minute

// New 新建 Store。path 为空表示不持久化。onFire 在提醒到点时被调用，
// 返回 nil 表示执行成功（once/action 随后删除），返回 error 表示
// 执行失败（once/action 保留并至多重试 1 次）。
func New(path string, onFire func(r Reminder) error) *Store {
	return &Store{
		items:    map[string]*Reminder{},
		path:     path,
		onFire:   onFire,
		stopCh:   make(chan struct{}),
		attempts: map[string]int{},
	}
}

// SetOnFire 设置到点回调（启动调度前调）。
func (s *Store) SetOnFire(fn func(r Reminder) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onFire = fn
}

// Load 从文件读回。文件损坏（JSON 解析失败）时把损坏文件改名备份
// （path + ".corrupt.<时间戳>"），并返回错误——P2-44：
// 旧代码静默丢掉全部提醒（main.go 里错误还被 `_ =` 吞掉），
// 现在损坏有备份、启动有日志，提醒可从备份人工恢复。
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
		backup := s.path + ".corrupt." + time.Now().Format("20060102-150405")
		if rerr := os.Rename(s.path, backup); rerr != nil {
			return fmt.Errorf("提醒文件损坏（JSON 解析失败），备份也失败: %w（原错误: %v）", rerr, err)
		}
		log.Printf("reminders: 提醒文件损坏，已备份到 %s，本次启动提醒为空", backup)
		return fmt.Errorf("提醒文件损坏（已备份到 %s）: %w", backup, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range items {
		// P1-24：磁盘里未知 Kind 的提醒直接丢弃并记日志，不再进表。
		if !validKind(it.Kind) {
			log.Printf("reminders: 丢弃未知类型提醒 %s（kind=%q）", it.ID, it.Kind)
			continue
		}
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
	// P2-4：提醒文件含用户语音原文，落盘 0600；唯一临时文件
	// （CreateTemp），不与其它写路径共用固定 .tmp 名。
	dir := filepath.Dir(s.path)
	f, err := os.CreateTemp(dir, ".reminders-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Add 新增一条提醒，返回 ID。Next 由调用方算好传入。
// ID 用纳秒时间戳 + 进程内自增序号：同一毫秒内连加两条也不能撞 ID。
func (s *Store) Add(r Reminder) (string, error) {
	// P1-24：未知 Kind 拒绝入库，不再靠 tick 的无 default 分支兜底。
	if !validKind(r.Kind) {
		return "", fmt.Errorf("未知提醒类型 %q", r.Kind)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.ID == "" {
		r.ID = fmt.Sprintf("%d-%d", time.Now().UnixNano(), idSeq.Add(1))
	}
	r.Enabled = true
	cp := r
	s.items[r.ID] = &cp
	if err := s.saveLocked(); err != nil {
		// P2-14：落盘失败置 dirty，下次 tick 重试保存；
		// 错误仍返回给调用方（Router 会明说记不下来）。
		s.dirty = true
		return r.ID, err
	}
	return r.ID, nil
}

// Cancel 取消一条提醒（按 ID）；不存在返回 false。
// 落盘失败时返回 error——调用方须告诉用户“取消没存住”，
// 不能谎报已取消（P2-14：旧代码 `_ = saveLocked()` 吞掉错误，
// 重启后提醒复活重播）。
func (s *Store) Cancel(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[id]; !ok {
		return false, nil
	}
	delete(s.items, id)
	delete(s.attempts, id)
	if err := s.saveLocked(); err != nil {
		s.dirty = true
		log.Printf("reminders: 取消 %s 后落盘失败（重启后可能复活）: %v", id, err)
		return true, err
	}
	return true, nil
}

// CancelByText 按内容取消（语音取消用：“取消吃药提醒”）。
// 落盘失败时返回 error（同 Cancel）。
func (s *Store) CancelByText(text string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, it := range s.items {
		if it.Text == text {
			delete(s.items, id)
			delete(s.attempts, id)
			n++
		}
	}
	if n == 0 {
		return 0, nil
	}
	if err := s.saveLocked(); err != nil {
		s.dirty = true
		log.Printf("reminders: 按文本取消 %q 后落盘失败（重启后可能复活）: %v", text, err)
		return n, err
	}
	return n, nil
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
// P2-13：重复 Start 是 no-op，不再每调一次多一个 ticker goroutine
// （双倍触发）；Stop 后可再次 Start（每次用新的 stopCh，
// 旧代码 Stop 关掉 stopCh 后新 goroutine 一启动就读到已关闭的 channel
// 而立即退出，调度器再也起不来）。
func (s *Store) Start() {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	stopCh := make(chan struct{})
	s.stopCh = stopCh
	s.mu.Unlock()
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case now := <-ticker.C:
				s.tick(now)
			}
		}
	}()
}

// Stop 停止调度循环；可再次 Start。
func (s *Store) Stop() {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return
	}
	s.started = false
	close(s.stopCh)
	s.mu.Unlock()
}

func (s *Store) tick(now time.Time) {
	s.mu.Lock()
	var due []*Reminder
	changed := false
	for _, it := range s.items {
		if !it.Enabled || it.Next.After(now) {
			continue
		}
		cp := *it
		due = append(due, &cp)
		// 算下一次。
		switch it.Kind {
		case KindOnce, KindAction:
			// P1-12（2026-11-23 审查）：一次性提醒不再先删后执行——
			// 执行失败（Route 报错、无在线设备）时提醒就静默丢了。
			// 改为先把 Next 顺延一个重试间隔（防执行期间下一轮 tick
			// 重复触发），执行成功后 fireAsync 再删；失败保留、
			// 1 分钟后重试（见 fireAsync）。
			changed = true
			it.Next = now.Add(reminderRetryDelay)
		case KindDaily:
			changed = true
			next, err := NextDaily(it.At, now)
			if err == nil {
				it.Next = next
			} else {
				delete(s.items, it.ID)
			}
		case KindInterval:
			changed = true
			if it.Every > 0 {
				it.Next = now.Add(time.Duration(it.Every) * time.Minute)
			} else {
				delete(s.items, it.ID)
			}
		default:
			// P1-24：未知 Kind（旧数据/手写文件）删除并记日志——
			// 否则 Next 恒 ≤ now，每秒触发一次 onFire 永不停。
			changed = true
			log.Printf("reminders: 删除未知类型提醒 %s（kind=%q）", it.ID, it.Kind)
			delete(s.items, it.ID)
		}
	}
	// P2-44：只有增删改（或上次落盘失败待重试）时才写盘——旧代码
	// 每秒全量写盘，无论有无变化。落盘失败记 error 日志并置 dirty，
	// 下次 tick 重试；内存里的变更已生效，不因写盘失败回滚。
	if changed || s.dirty {
		if err := s.saveLocked(); err != nil {
			s.dirty = true
			log.Printf("reminders: 提醒落盘失败（下次 tick 重试）: %v", err)
		} else {
			s.dirty = false
		}
	}
	onFire := s.onFire
	s.mu.Unlock()
	// P2-44：到点回调不再同步阻塞调度——每条提醒起独立 goroutine
	// 执行（TTS 合成 + 网络发送慢也不拖住 1s tick）。
	for _, r := range due {
		if onFire != nil {
			go s.fireAsync(*r, onFire)
		}
	}
}

// fireAsync 执行一条到点提醒并按结果处理 once/action 的删除/重试：
// onFire 返回 nil 即成功，执行完删除；失败时保留并至多重试 1 次
// （Next 已在 tick 里顺延 1 分钟）；第二次失败即删除并记 error 日志，
// 不再无限重试，也不再静默丢失。daily/interval 不走删除逻辑
// （tick 里已重算 Next）。
// 注意：删除是异步的（onFire 返回后才删）——调用方不要假设回调
// 到达时表里已删；测试应轮询 List() 等删除完成。
func (s *Store) fireAsync(r Reminder, onFire func(r Reminder) error) {
	err := onFire(r)
	if r.Kind != KindOnce && r.Kind != KindAction {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	deleted := false
	if err == nil {
		if _, ok := s.items[r.ID]; ok {
			delete(s.items, r.ID)
			deleted = true
		}
		delete(s.attempts, r.ID)
	} else {
		if s.attempts == nil {
			s.attempts = map[string]int{}
		}
		s.attempts[r.ID]++
		if s.attempts[r.ID] >= 2 {
			log.Printf("reminders: 提醒 %s 执行失败，已重试 1 次后放弃: %v", r.ID, err)
			if _, ok := s.items[r.ID]; ok {
				delete(s.items, r.ID)
				deleted = true
			}
			delete(s.attempts, r.ID)
		} else {
			// 保留：Next 已在 tick 里顺延 1 分钟，1 分钟后重试。
			// 只记 ID 与错误，不记提醒文本（提醒内容可能含隐私，见 P2-43）。
			log.Printf("reminders: 提醒 %s 执行失败，1 分钟后重试: %v", r.ID, err)
		}
	}
	if deleted {
		if serr := s.saveLocked(); serr != nil {
			s.dirty = true
			log.Printf("reminders: 提醒落盘失败（下次 tick 重试）: %v", serr)
		}
	}
}
