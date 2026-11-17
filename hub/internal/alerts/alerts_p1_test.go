package alerts

// P1-27 回归测试：Watcher.Rebind 在 HA 重连后换客户端并重新订阅。

import (
	"testing"
	"time"

	"github.com/huajun/open-xiaoai-next/hub/internal/ha"
)

func TestWatcherRebind(t *testing.T) {
	f1 := startFakeHA(t)
	c1, err := ha.Dial(f1.url, "tok", 5*time.Second)
	if err != nil {
		t.Fatalf("Dial 假 HA1: %v", err)
	}
	defer c1.Close()

	spoken := make(chan string, 8)
	w := New(c1, "23:00-07:00", func(text string) { spoken <- text })
	day := time.Date(2026, 11, 17, 14, 0, 0, 0, time.Local)
	w.SetNow(func() time.Time { return day })
	if err := w.Start(); err != nil {
		t.Fatalf("Watcher.Start: %v", err)
	}
	f1.pushStateChanged("binary_sensor.kitchen_leak", "on")
	select {
	case text := <-spoken:
		if text == "" {
			t.Fatal("播报不应为空")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("重绑前应能收到播报")
	}

	// HA 断线重连：新客户端 + Rebind，旧订阅失效后新订阅应继续工作。
	f2 := startFakeHA(t)
	c2, err := ha.Dial(f2.url, "tok", 5*time.Second)
	if err != nil {
		t.Fatalf("Dial 假 HA2: %v", err)
	}
	defer c2.Close()
	if err := w.Rebind(c2); err != nil {
		t.Fatalf("Rebind: %v", err)
	}
	f2.pushStateChanged("binary_sensor.living_room_smoke", "on")
	select {
	case text := <-spoken:
		if text == "" {
			t.Fatal("重绑后播报不应为空")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Rebind 后新 HA 的事件应继续播报")
	}

	// Rebind(nil) 应报错。
	if err := w.Rebind(nil); err == nil {
		t.Fatal("Rebind(nil) 应返回错误")
	}
}

func TestWatcherRebindBeforeStart(t *testing.T) {
	f1 := startFakeHA(t)
	c1, err := ha.Dial(f1.url, "tok", 5*time.Second)
	if err != nil {
		t.Fatalf("Dial 假 HA1: %v", err)
	}
	defer c1.Close()
	f2 := startFakeHA(t)
	c2, err := ha.Dial(f2.url, "tok", 5*time.Second)
	if err != nil {
		t.Fatalf("Dial 假 HA2: %v", err)
	}
	defer c2.Close()

	spoken := make(chan string, 8)
	w := New(c1, "23:00-07:00", func(text string) { spoken <- text })
	day := time.Date(2026, 11, 17, 14, 0, 0, 0, time.Local)
	w.SetNow(func() time.Time { return day })
	// 未 Start 过就 Rebind：只换客户端，不订阅，不报错。
	if err := w.Rebind(c2); err != nil {
		t.Fatalf("未 Start 时 Rebind 应成功，实际 %v", err)
	}
	// 随后 Start 应订到新客户端上。
	if err := w.Start(); err != nil {
		t.Fatalf("Rebind 后 Start: %v", err)
	}
	f2.pushStateChanged("binary_sensor.kitchen_leak", "on")
	select {
	case <-spoken:
	case <-time.After(3 * time.Second):
		t.Fatal("Rebind 后 Start 应订到新客户端并收到播报")
	}
	// 旧客户端上的事件不应再触发（订阅在新客户端上）。
	f1.pushStateChanged("binary_sensor.living_room_smoke", "on")
	select {
	case text := <-spoken:
		t.Fatalf("旧 HA 的事件不应再播报，却收到 %q", text)
	case <-time.After(300 * time.Millisecond):
	}
}
