package arbitration

// P1-10 回归测试（2026-11-23 审查）：close() 的 Decide + lastWinner 更新 +
// onResult 调用要串行化——timer 回调里同步做网络 I/O 时，两个窗口的
// close 不能交错进对方的回调。

import (
	"sync"
	"testing"
	"time"
)

// TestCloseSerializesOnResult：第一个窗口的 close 正在 onResult 里阻塞时，
// 第二个窗口的 close 不能先跑进自己的 onResult；必须等第一个完全结束。
func TestCloseSerializesOnResult(t *testing.T) {
	window := 20 * time.Millisecond

	var mu sync.Mutex
	var events []string // "start:A"/"end:A"/"start:B"/"end:B" 顺序
	entered1 := make(chan struct{})
	entered2 := make(chan struct{})
	release1 := make(chan struct{})
	var once1, once2 sync.Once
	a := New(window, func(winner string, losers []string) {
		mu.Lock()
		events = append(events, "start:"+winner)
		mu.Unlock()
		if winner == "dev-A" {
			once1.Do(func() { close(entered1) })
			<-release1
		} else {
			once2.Do(func() { close(entered2) })
		}
		mu.Lock()
		events = append(events, "end:"+winner)
		mu.Unlock()
	})

	a.Submit(WakeEvent{DeviceID: "dev-A", WakeWord: "你好小李", RMSdB: -30, Timestamp: time.Now()})
	select {
	case <-entered1:
	case <-time.After(2 * time.Second):
		t.Fatal("第一个窗口的 onResult 应先进入")
	}
	// 第一个 close 还卡在 onResult 里；此时提交第二个窗口。
	a.Submit(WakeEvent{DeviceID: "dev-B", WakeWord: "你好小李", RMSdB: -30, Timestamp: time.Now()})
	// 给第二个窗口的 timer 足够时间触发（20ms）+ 尝试进 onResult。
	time.Sleep(200 * time.Millisecond)
	select {
	case <-entered2:
		t.Fatal("第一个 close 未结束时，第二个窗口不应进入 onResult")
	default:
	}
	// 放行第一个，第二个才能进。
	close(release1)
	select {
	case <-entered2:
	case <-time.After(2 * time.Second):
		t.Fatal("放行第一个后，第二个窗口应进入 onResult")
	}
	// 等第二个也结束，再验顺序：A 的 end 必须在 B 的 start 之前。
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(events)
		mu.Unlock()
		if n >= 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("两个 onResult 都应结束，实际 events=%v", events)
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"start:dev-A", "end:dev-A", "start:dev-B", "end:dev-B"}
	for i, w := range want {
		if events[i] != w {
			t.Fatalf("onResult 应严格串行，实际 %v，想要 %v", events, want)
		}
	}
}
