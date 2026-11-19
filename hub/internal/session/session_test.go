package session

import (
	"sync"
	"testing"
	"time"
)

// fakeClock 是可拨动的假时钟：Store 的 now 字段测过期不用真等 60s。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	f.t = f.t.Add(d)
	f.mu.Unlock()
}

func newTestStore(timeout time.Duration) (*Store, *fakeClock) {
	fc := &fakeClock{t: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	s := New(timeout)
	s.now = fc.now
	return s, fc
}

func TestAppendAndHistory(t *testing.T) {
	s, _ := newTestStore(60 * time.Second)
	id := s.AppendTurn("device:lx06-1", "北京天气怎么样", "今天北京晴，25 度。")
	if id == "" {
		t.Fatal("AppendTurn 应返回非空会话 ID")
	}
	s.AppendTurn("device:lx06-1", "那明天呢", "明天多云，23 度。")
	h := s.History("device:lx06-1")
	if len(h) != 4 {
		t.Fatalf("两轮后历史应为 4 条，实际 %d: %v", len(h), h)
	}
	want := []Message{
		{Role: "user", Content: "北京天气怎么样"},
		{Role: "assistant", Content: "今天北京晴，25 度。"},
		{Role: "user", Content: "那明天呢"},
		{Role: "assistant", Content: "明天多云，23 度。"},
	}
	for i, w := range want {
		if h[i] != w {
			t.Fatalf("历史第 %d 条应为 %+v，实际 %+v", i, w, h[i])
		}
	}
	if got := s.SessionID("device:lx06-1"); got != id {
		t.Fatalf("同一会话内 ID 应不变，追加前 %q，追加后 %q", id, got)
	}
	// History 返回的是拷贝：改它不应污染存根。
	h[0].Content = "被改了"
	if got := s.History("device:lx06-1")[0].Content; got != "北京天气怎么样" {
		t.Fatalf("History 应返回拷贝，改返回值不应污染存根，实际 %q", got)
	}
}

func TestIsolation(t *testing.T) {
	s, _ := newTestStore(60 * time.Second)
	s.AppendTurn("room:客厅", "北京天气怎么样", "今天晴。")
	s.AppendTurn("room:卧室", "关灯", "灯关了。")
	s.AppendTurn("device:oh2p-9", "你好", "你好呀。")
	if got := len(s.History("room:客厅")); got != 2 {
		t.Fatalf("客厅历史应为 2 条，实际 %d", got)
	}
	if got := len(s.History("room:卧室")); got != 2 {
		t.Fatalf("卧室历史应为 2 条，实际 %d", got)
	}
	if got := s.History("room:卧室")[0].Content; got != "关灯" {
		t.Fatalf("卧室历史不应混进客厅的内容，实际 %q", got)
	}
	if got := len(s.History("device:oh2p-9")); got != 2 {
		t.Fatalf("未认领设备历史应为 2 条，实际 %d", got)
	}
	if got := s.History("room:厨房"); got != nil {
		t.Fatalf("从没聊过的 key 历史应为 nil，实际 %v", got)
	}
	if got := s.Active(); got != 3 {
		t.Fatalf("应有 3 个活跃会话，实际 %d", got)
	}
}

func TestExpiry(t *testing.T) {
	s, fc := newTestStore(60 * time.Second)
	id1 := s.AppendTurn("device:lx06-1", "北京天气怎么样", "今天晴。")
	// 59s 内还在：新一轮继续同一会话，ID 不变。
	fc.advance(59 * time.Second)
	if got := s.History("device:lx06-1"); len(got) != 2 {
		t.Fatalf("59s 内会话不应过期，历史应为 2 条，实际 %v", got)
	}
	id2 := s.AppendTurn("device:lx06-1", "那明天呢", "明天多云。")
	if id2 != id1 {
		t.Fatalf("59s 内追加应继续同一会话，ID %q → %q", id1, id2)
	}
	// 再过 61s（距上次活跃 61s > 60s）：过期，历史清空。
	fc.advance(61 * time.Second)
	if got := s.History("device:lx06-1"); got != nil {
		t.Fatalf("过期后历史应为 nil，实际 %v", got)
	}
	if got := s.SessionID("device:lx06-1"); got != "" {
		t.Fatalf("过期后 SessionID 应为空，实际 %q", got)
	}
	// 过期后新一轮是新会话：ID 不同。
	id3 := s.AppendTurn("device:lx06-1", "北京天气怎么样", "今天晴。")
	if id3 == id1 {
		t.Fatalf("过期后新开会话 ID 应不同，旧 %q，新 %q", id1, id3)
	}
	if got := len(s.History("device:lx06-1")); got != 2 {
		t.Fatalf("新会话历史应只有新一轮 2 条，实际 %d", got)
	}
}

func TestZeroTimeoutDefaultsTo60s(t *testing.T) {
	s, fc := newTestStore(0)
	s.AppendTurn("device:lx06-1", "你好", "你好呀。")
	fc.advance(59 * time.Second)
	if got := s.History("device:lx06-1"); len(got) != 2 {
		t.Fatalf("timeout=0 应默认 60s，59s 内不应过期，实际 %v", got)
	}
	fc.advance(2 * time.Second)
	if got := s.History("device:lx06-1"); got != nil {
		t.Fatalf("timeout=0 应默认 60s，61s 后应过期，实际 %v", got)
	}
}

func TestSweep(t *testing.T) {
	s, fc := newTestStore(60 * time.Second)
	s.AppendTurn("room:客厅", "你好", "你好呀。")
	fc.advance(30 * time.Second)
	s.AppendTurn("room:卧室", "你好", "你好呀。")
	// 再过 40s：客厅距活跃 70s 已过期，卧室 40s 还在。
	fc.advance(40 * time.Second)
	if n := s.Sweep(fc.now()); n != 1 {
		t.Fatalf("Sweep 应清理 1 个过期会话，实际 %d", n)
	}
	if got := s.History("room:客厅"); got != nil {
		t.Fatalf("客厅会话应已被 Sweep 清掉，实际 %v", got)
	}
	if got := len(s.History("room:卧室")); got != 2 {
		t.Fatalf("卧室会话不应被 Sweep 清掉，实际 %v", got)
	}
	if n := s.Sweep(fc.now()); n != 0 {
		t.Fatalf("无过期会话时 Sweep 应返回 0，实际 %d", n)
	}
}

func TestMaxMessages(t *testing.T) {
	s, _ := newTestStore(60 * time.Second)
	s.maxMessages = 4
	for i := 0; i < 3; i++ {
		s.AppendTurn("device:lx06-1", "问题", "回答")
	}
	h := s.History("device:lx06-1")
	if len(h) != 4 {
		t.Fatalf("上限 4 条时历史应截到 4 条，实际 %d: %v", len(h), h)
	}
	// 留的是最近两轮（第 2、3 轮），最旧一轮已丢。
	if h[0].Content != "问题" || h[3].Content != "回答" {
		t.Fatalf("截断后应留最近的轮次，实际 %v", h)
	}
}

func TestConcurrentAppend(t *testing.T) {
	s, _ := newTestStore(60 * time.Second)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				s.AppendTurn("device:lx06-1", "问题", "回答")
			}
		}()
	}
	wg.Wait()
	h := s.History("device:lx06-1")
	if len(h) != DefaultMaxMessages {
		t.Fatalf("并发追加 200 轮后历史应截到上限 %d 条，实际 %d", DefaultMaxMessages, len(h))
	}
}
