package ha

// 第三轮审计 P2 回归测试（Hub 面）：
// H-6 Close() 并发双关不 panic（sync.Once 保护 close(done)）。

import (
	"sync"
	"testing"
	"time"
)

// H-6：看门狗探活失败与关停可能并发调 Close()——20 个 goroutine
// 同时 Close 同一个客户端，不得 panic；重复 Close 也安全。
// 旧代码 select-default 双关 close(done) 时有竞态窗口。
func TestConcurrentCloseNoPanic(t *testing.T) {
	for round := 0; round < 5; round++ {
		_, url := startFakeHA(t)
		c, err := Dial(url, "hass-token", 5*time.Second)
		if err != nil {
			t.Fatalf("第 %d 轮 Dial: %v", round, err)
		}
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = c.Close()
			}()
		}
		wg.Wait()
		// 顺序再关一次也不 panic。
		_ = c.Close()
	}
}
