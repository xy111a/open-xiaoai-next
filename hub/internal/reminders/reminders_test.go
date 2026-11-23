package reminders

import (
	"path/filepath"
	"testing"
	"time"
)

func TestNextDaily(t *testing.T) {
	// 现在 2026-11-17 10:00，约 08:00 → 明天 08:00；约 12:00 → 今天 12:00。
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.Local)
	next, err := NextDaily("08:00", now)
	if err != nil {
		t.Fatal(err)
	}
	if next.Day() != 18 || next.Hour() != 8 {
		t.Fatalf("08:00 应约到明天，实际 %v", next)
	}
	next, err = NextDaily("12:00", now)
	if err != nil {
		t.Fatal(err)
	}
	if next.Day() != 17 || next.Hour() != 12 {
		t.Fatalf("12:00 应约到今天，实际 %v", next)
	}
	if _, err := NextDaily("25:00", now); err == nil {
		t.Fatal("25:00 应报错")
	}
	if _, err := NextDaily("8点", now); err == nil {
		t.Fatal("非法格式应报错")
	}
}

func TestOnceFiresAndDeletes(t *testing.T) {
	dir := t.TempDir()
	fired := make(chan Reminder, 10)
	s := New(filepath.Join(dir, "r.json"), func(r Reminder) error { fired <- r; return nil })
	id, err := s.Add(Reminder{Kind: KindOnce, Text: "吃药", Next: time.Now().Add(-time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	s.tick(time.Now())
	// P2-44：onFire 已改为异步（不阻塞调度），测试等回调而不是立即读。
	select {
	case r := <-fired:
		if r.Text != "吃药" {
			t.Fatalf("一次性提醒应触发“吃药”，实际 %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("一次性提醒应触发一次，未收到回调")
	}
	// P1-12：删除在 onFire 返回后异步发生，等删除完成（2s 内）。
	deadline := time.Now().Add(2 * time.Second)
	for len(s.List()) != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if len(s.List()) != 0 {
		t.Fatal("一次性提醒触发后应删除")
	}
	// 再 tick 一次不应重复触发。
	s.tick(time.Now())
	select {
	case r := <-fired:
		t.Fatalf("一次性提醒不应重复触发，实际 %+v", r)
	case <-time.After(300 * time.Millisecond):
	}
	_ = id
}

func TestDailyReschedules(t *testing.T) {
	dir := t.TempDir()
	fired := make(chan struct{}, 10)
	s := New(filepath.Join(dir, "r.json"), func(r Reminder) error { fired <- struct{}{}; return nil })
	now := time.Now()
	if _, err := s.Add(Reminder{Kind: KindDaily, Text: "吃药", At: "08:00", Next: now.Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	s.tick(now)
	// P2-44：onFire 已改为异步，等回调。
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("每日提醒应触发，未收到回调")
	}
	list := s.List()
	if len(list) != 1 {
		t.Fatalf("每日提醒触发后应保留，实际 %d 条", len(list))
	}
	if !list[0].Next.After(now) {
		t.Fatalf("每日提醒下次时间应推到未来，实际 %v", list[0].Next)
	}
}

func TestIntervalAdvances(t *testing.T) {
	dir := t.TempDir()
	fired := make(chan struct{}, 10)
	s := New(filepath.Join(dir, "r.json"), func(r Reminder) error { fired <- struct{}{}; return nil })
	now := time.Now()
	if _, err := s.Add(Reminder{Kind: KindInterval, Text: "喝水", Every: 30, Next: now.Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	s.tick(now)
	// P2-44：onFire 已改为异步，等回调。
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("间隔提醒应触发，未收到回调")
	}
	list := s.List()
	if len(list) != 1 || !list[0].Next.After(now.Add(29*time.Minute)) {
		t.Fatalf("间隔提醒下次应约 30 分钟后，实际 %+v", list)
	}
}

func TestCancelByTextAndPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "r.json")
	s := New(path, nil)
	if _, err := s.Add(Reminder{Kind: KindDaily, Text: "吃药", At: "08:00", Next: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(Reminder{Kind: KindDaily, Text: "吃药", At: "20:00", Next: time.Now().Add(2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// 持久化：新 Store 从同一文件读回，应有 2 条。
	s2 := New(path, nil)
	if err := s2.Load(); err != nil {
		t.Fatal(err)
	}
	if len(s2.List()) != 2 {
		t.Fatalf("持久化后应读回 2 条，实际 %d", len(s2.List()))
	}
	if n, err := s2.CancelByText("吃药"); err != nil || n != 2 {
		t.Fatalf("按文本取消应删 2 条，实际 %d / %v", n, err)
	}
	if n, err := s2.CancelByText("不存在"); err != nil || n != 0 {
		t.Fatalf("取消不存在的应为 0，实际 %d / %v", n, err)
	}
}

// Start/Stop 真跑 ticker：约 2 秒后到点的提醒应在 5 秒内触发一次。
func TestStartTicker(t *testing.T) {
	dir := t.TempDir()
	fired := make(chan struct{}, 1)
	s := New(filepath.Join(dir, "r.json"), func(r Reminder) error { fired <- struct{}{}; return nil })
	if _, err := s.Add(Reminder{Kind: KindInterval, Text: "喝水", Every: 1, Next: time.Now().Add(2 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	s.Start()
	defer s.Stop()
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("Start 后 5s 内提醒未触发")
	}
}
