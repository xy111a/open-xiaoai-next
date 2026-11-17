package reminders

// P1-24 回归测试：未知 Kind 拒绝入库、tick 删除不重排、Load 丢弃；
// P1-41：KindAction 触发一次后删除。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAddRejectsUnknownKind(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "r.json"), nil)
	if _, err := s.Add(Reminder{Kind: "bogus", Text: "x", Next: time.Now()}); err == nil {
		t.Fatal("未知 Kind 的 Add 应返回错误")
	}
	if len(s.List()) != 0 {
		t.Fatalf("未知 Kind 不应入库，实际 %+v", s.List())
	}
}

func TestTickDeletesUnknownKindOnce(t *testing.T) {
	s := New("", nil)
	fired := 0
	s.SetOnFire(func(r Reminder) { fired++ })
	// Add 拦不住手写/旧数据：直接进表模拟磁盘里的未知 Kind。
	s.mu.Lock()
	s.items["u1"] = &Reminder{ID: "u1", Kind: "bogus", Text: "x", Enabled: true, Next: time.Now().Add(-time.Second)}
	s.mu.Unlock()
	s.tick(time.Now())
	if fired != 1 {
		t.Fatalf("未知 Kind 当轮应触发一次后删除，实际触发 %d 次", fired)
	}
	// 再扫一次：不应再触发（P1-24 的核心 bug：每秒触发永不停）。
	s.tick(time.Now())
	if fired != 1 {
		t.Fatalf("未知 Kind 删除后不应再触发，实际 %d 次", fired)
	}
	if len(s.List()) != 0 {
		t.Fatalf("未知 Kind 应从表里删除，实际 %+v", s.List())
	}
}

func TestLoadDropsUnknownKind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "r.json")
	items := []Reminder{
		{ID: "good", Kind: KindOnce, Text: "ok", Enabled: true, Next: time.Now().Add(time.Hour)},
		{ID: "bad", Kind: "bogus", Text: "x", Enabled: true, Next: time.Now().Add(time.Hour)},
	}
	b, _ := json.Marshal(items)
	if err := os.WriteFile(path, b, 0644); err != nil {
		t.Fatal(err)
	}
	s := New(path, nil)
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	list := s.List()
	if len(list) != 1 || list[0].ID != "good" {
		t.Fatalf("Load 应只保留合法 Kind，实际 %+v", list)
	}
}

func TestKindActionFiresOnceThenDeleted(t *testing.T) {
	s := New("", nil)
	fired := 0
	var got Reminder
	s.SetOnFire(func(r Reminder) { fired++; got = r })
	if _, err := s.Add(Reminder{Kind: KindAction, Text: "关客厅灯", Room: "客厅", Next: time.Now().Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	s.tick(time.Now())
	if fired != 1 || got.Kind != KindAction || got.Text != "关客厅灯" {
		t.Fatalf("KindAction 应触发一次，实际 fired=%d got=%+v", fired, got)
	}
	s.tick(time.Now())
	if fired != 1 {
		t.Fatalf("KindAction 触发后应删除、不再触发，实际 %d 次", fired)
	}
	if len(s.List()) != 0 {
		t.Fatalf("KindAction 触发后应删除，实际 %+v", s.List())
	}
}
