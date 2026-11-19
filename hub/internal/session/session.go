// Package session 是多轮对话的会话存根：按设备/房间 key 存消息历史，
// 下一轮问答把完整历史发给 LLM，跟进式问题（“那明天呢”）才能
// 对上前文（例如“北京天气”）。
//
// 只在内存里，不落盘、不记音频、不记原始语音转写以外的任何东西：
// 每条只存 role + 文本；会话 60s 空闲即过期清空，Hub 重启即清空。
// Key 由调用方定：Router 用 "room:<房间名>"（已认领）或
// "device:<设备 ID>"（未认领），所以同房间的两台音箱共享一次对话，
// 不同房间、不同未认领设备各自独立。
package session

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// DefaultTimeout 是会话空闲过期的默认值（60s）：60s 内没新一轮
// 对话，会话过期清空，下次从头聊。
const DefaultTimeout = 60 * time.Second

// DefaultMaxMessages 是单会话保留的消息条数上限（20 条 = 10 轮问答）：
// 防长对话把内存撑大；超了丢最旧的轮次，只留最近的。
const DefaultMaxMessages = 20

// Message 是一条对话消息（只存 role + 文本，不存音频）。
type Message struct {
	Role    string // "user" 或 "assistant"
	Content string
}

// conversation 是一次会话：ID + 消息历史 + 上次活跃时间。
type conversation struct {
	id         string
	messages   []Message
	lastActive time.Time
}

// Store 是会话存根：key → conversation，全部在内存里。
type Store struct {
	mu          sync.Mutex
	sessions    map[string]*conversation
	timeout     time.Duration // <=0 时用 DefaultTimeout
	maxMessages int           // <=0 时用 DefaultMaxMessages
	now         func() time.Time
}

// New 新建 Store。timeout <= 0 时用 DefaultTimeout（60s）。
func New(timeout time.Duration) *Store {
	return &Store{
		sessions:    map[string]*conversation{},
		timeout:     timeout,
		maxMessages: DefaultMaxMessages,
		now:         time.Now,
	}
}

// effectiveTimeout 返回生效的超时。
func (s *Store) effectiveTimeout() time.Duration {
	if s.timeout <= 0 {
		return DefaultTimeout
	}
	return s.timeout
}

// effectiveMaxMessages 返回生效的条数上限。
func (s *Store) effectiveMaxMessages() int {
	if s.maxMessages <= 0 {
		return DefaultMaxMessages
	}
	return s.maxMessages
}

// newID 生成会话 ID：16 字节随机 hex（只用于区分“同一会话继续”
// 还是“过期后新开”，不含任何设备或用户信息）。
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 随机源极小概率失败时退回纳秒时间戳，保证不撞同 key 的旧 ID。
		return hex.EncodeToString(b[:]) + time.Now().Format("150405.000000000")
	}
	return hex.EncodeToString(b[:])
}

// getLocked 取 key 的会话；不存在或已过期返回 nil（过期即删）。
func (s *Store) getLocked(key string, now time.Time) *conversation {
	c, ok := s.sessions[key]
	if !ok {
		return nil
	}
	if now.Sub(c.lastActive) > s.effectiveTimeout() {
		delete(s.sessions, key)
		return nil
	}
	return c
}

// History 取 key 的完整消息历史（按轮次顺序的拷贝；不存在或已过期
// 返回 nil）。调用方发给 LLM 前把本轮 user 再拼在后面——History
// 本身不含本轮，只含已完成的前文。
func (s *Store) History(key string) []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.getLocked(key, s.now())
	if c == nil {
		return nil
	}
	out := make([]Message, len(c.messages))
	copy(out, c.messages)
	return out
}

// AppendTurn 把一轮已完成的问答追加进会话（不存在或已过期则新开
// 一个会话），返回会话 ID。user/assistant 任一为空串时该条不追加，
// 但另一条仍追加、活跃时间仍刷新；两条都空时只刷新活跃时间。
// LLM 调用失败时不要调它（问答没完成，进历史会留半截），调用方
// 只在拿到 assistant 回复后追加——失败的那轮不会污染下次历史。
func (s *Store) AppendTurn(key, user, assistant string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	c := s.getLocked(key, now)
	if c == nil {
		c = &conversation{id: newID()}
		s.sessions[key] = c
	}
	if user != "" {
		c.messages = append(c.messages, Message{Role: "user", Content: user})
	}
	if assistant != "" {
		c.messages = append(c.messages, Message{Role: "assistant", Content: assistant})
	}
	// 超上限丢最旧的轮次，只留最近的；拷贝出新 slice，不让旧的
	// 底层数组继续占着已丢消息的内存。
	if max := s.effectiveMaxMessages(); len(c.messages) > max {
		kept := make([]Message, max)
		copy(kept, c.messages[len(c.messages)-max:])
		c.messages = kept
	}
	c.lastActive = now
	return c.id
}

// SessionID 取 key 当前会话的 ID；不存在或已过期返回 ""。
func (s *Store) SessionID(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.getLocked(key, s.now())
	if c == nil {
		return ""
	}
	return c.id
}

// Active 返回当前未过期的会话数。
func (s *Store) Active() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	n := 0
	for key, c := range s.sessions {
		if now.Sub(c.lastActive) > s.effectiveTimeout() {
			delete(s.sessions, key)
			continue
		}
		n++
	}
	return n
}

// Sweep 清理已过期的会话，返回清理掉的会话数。
// main.go 的定时扫荡调它；不调也行——History/AppendTurn 取用时
// 会惰性清过期会话，Sweep 只是让闲置会话的内存早点释放。
func (s *Store) Sweep(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for key, c := range s.sessions {
		if now.Sub(c.lastActive) > s.effectiveTimeout() {
			delete(s.sessions, key)
			n++
		}
	}
	return n
}
