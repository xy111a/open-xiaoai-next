package reminders

import (
	"errors"
	"testing"
	"time"
)

// P1-12（2026-11-23 审查）：一次性提醒执行失败时保留、Next 顺延 1 分钟，
// 重试 1 次仍失败才删除——不再“先删后执行”，失败就丢。
func TestOnceRetryOnceThenDrop(t *testing.T) {
	s := New("", nil)
	fired := make(chan Reminder, 10)
	s.SetOnFire(func(r Reminder) error {
		fired <- r
		return errors.New("boom")
	})
	if _, err := s.Add(Reminder{Kind: KindOnce, Text: "吃药", Next: time.Now().Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	s.tick(time.Now())
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("到点应触发一次")
	}
	// 给 fireAsync 一点时间跑完：失败时不删，Next 顺延到未来。
	time.Sleep(200 * time.Millisecond)
	list := s.List()
	if len(list) != 1 {
		t.Fatalf("执行失败时提醒应保留，实际 %+v", list)
	}
	if !list[0].Next.After(time.Now()) {
		t.Fatalf("失败后 Next 应顺延到未来，实际 %v", list[0].Next)
	}
	// 1 分钟后重试：仍失败 → 删除。
	s.tick(list[0].Next.Add(time.Second))
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("重试时应再触发一次")
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(s.List()) != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if len(s.List()) != 0 {
		t.Fatalf("重试 1 次仍失败时应删除，实际 %+v", s.List())
	}
}

// 失败一次、重试成功 → 删除，不再重试。
func TestOnceRetryThenSuccess(t *testing.T) {
	s := New("", nil)
	fired := make(chan Reminder, 10)
	fail := true
	s.SetOnFire(func(r Reminder) error {
		fired <- r
		if fail {
			return errors.New("boom")
		}
		return nil
	})
	if _, err := s.Add(Reminder{Kind: KindOnce, Text: "吃药", Next: time.Now().Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	s.tick(time.Now())
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("到点应触发一次")
	}
	time.Sleep(200 * time.Millisecond)
	list := s.List()
	if len(list) != 1 {
		t.Fatalf("第一次失败时提醒应保留，实际 %+v", list)
	}
	fail = false
	s.tick(list[0].Next.Add(time.Second))
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("重试时应再触发一次")
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(s.List()) != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if len(s.List()) != 0 {
		t.Fatalf("重试成功时应删除，实际 %+v", s.List())
	}
}
