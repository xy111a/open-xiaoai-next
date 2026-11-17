// Package router 里的 Memory：简单的键值记忆（FR-09）。
// “记住空调 26 度”存在 JSON 文件里，换对话仍有效。
package router

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

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

// Set 记一条（key 用时间戳，value 存原文；查的时候按原文匹配）。
func (m *Memory) Set(deviceID, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data[deviceID] == nil {
		m.data[deviceID] = map[string]string{}
	}
	m.data[deviceID][time.Now().Format(time.RFC3339)] = text
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
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}
