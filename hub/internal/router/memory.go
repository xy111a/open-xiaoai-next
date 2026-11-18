// Package router 里的 Memory：简单的键值记忆（FR-09）。
// “记住空调 26 度”存在 JSON 文件里，换对话仍有效。
package router

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// memSeq 是 Set 生成 key 的进程内自增序号（与纳秒时间戳组合，
// P2-15：纳秒时间戳 + 序号，同一纳秒内两次“记住”也不会撞 key）。
var memSeq atomic.Uint64

// Memory 是一台设备（或全屋）的记忆。
type Memory struct {
	mu   sync.Mutex
	path string
	data map[string]map[string]string // deviceID → key → value（value 存原文）
}

// NewMemory 新建 Memory。path 为空表示不持久化。
func NewMemory(path string) *Memory {
	return &Memory{path: path, data: map[string]map[string]string{}}
}

// Load 从文件读回。
func (m *Memory) Load() error {
	if m.path == "" {
		return nil
	}
	b, err := os.ReadFile(m.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return json.Unmarshal(b, &m.data)
}

// Set 记一条（key 用纳秒时间戳 + 自增序号，value 存原文；查的时候按原文匹配）。
// key 必须用纳秒级 + 序号：秒级 RFC3339 会让同一秒内的两条“记住”互相覆盖
// （P2-15；P1-41 顺带改的纳秒 key 已闭合秒级碰撞，序号再兜底同一纳秒）。
func (m *Memory) Set(deviceID, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data[deviceID] == nil {
		m.data[deviceID] = map[string]string{}
	}
	m.data[deviceID][fmt.Sprintf("%d-%d", time.Now().UnixNano(), memSeq.Add(1))] = text
	return m.saveLocked()
}

// All 取一台设备的全部记忆原文。
func (m *Memory) All(deviceID string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, v := range m.data[deviceID] {
		out = append(out, v)
	}
	return out
}

func (m *Memory) saveLocked() error {
	if m.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(m.data, "", "  ")
	if err != nil {
		return err
	}
	// P2-4：记忆文件含用户语音原文，落盘 0600；唯一临时文件。
	dir := filepath.Dir(m.path)
	f, err := os.CreateTemp(dir, ".memory-*.tmp")
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
	if err := os.Rename(tmp, m.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
