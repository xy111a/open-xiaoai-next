package voice

// P1-14 回归测试（2026-11-23 审查）：上一句合成中说“停”，撤退词应
// Cancel 在途回答并重新结算，不再被 settle() 悄悄丢弃。

import (
	"context"
	"sync"
	"testing"
	"time"
)

// scriptedSTT 按顺序返回文本；第一次调用阻塞在 release 上（模拟上一句
// STT/合成进行中），其余调用立即返回。entered 在第 1 次调用进入时关闭，
// entered3 在第 3 次调用进入时关闭——第 3 次调用即撤退重结算的 answer，
// 它进入时说明 maybeRetreat 已跑完 Cancel + 重结算。
type scriptedSTT struct {
	mu       sync.Mutex
	texts    []string
	calls    int
	release  chan struct{}
	entered  chan struct{}
	entered3 chan struct{}
	once     sync.Once
	once3    sync.Once
}

func (s *scriptedSTT) Transcribe(pcm []byte, language string) (string, error) {
	s.mu.Lock()
	s.calls++
	i := s.calls - 1
	s.mu.Unlock()
	s.once.Do(func() { close(s.entered) })
	if i == 2 {
		s.once3.Do(func() { close(s.entered3) })
	}
	if i == 0 {
		<-s.release
	}
	if i < len(s.texts) {
		return s.texts[i], nil
	}
	return "", nil
}

type countingRouter struct {
	mu    sync.Mutex
	texts []string
	reply string
}

func (c *countingRouter) Route(ctx context.Context, deviceID, text string) (string, error) {
	c.mu.Lock()
	c.texts = append(c.texts, text)
	c.mu.Unlock()
	return c.reply, nil
}

func TestSettleRetreatCancelsInFlight(t *testing.T) {
	release := make(chan struct{})
	stt := &scriptedSTT{
		texts:    []string{"你好", "停", "停"},
		release:  release,
		entered:  make(chan struct{}),
		entered3: make(chan struct{}),
	}
	rt := &countingRouter{reply: ""}
	replied := make(chan [][]byte, 2)
	p := New(stt, &fakeTTS{pcm: make([]byte, 640)}, rt, "v",
		func(deviceID string, frames [][]byte) { replied <- frames })

	// 第一句结算：STT 阻塞（模拟上一句合成中）。
	if err := p.settle(context.Background(), "lx06-1", []int16{1}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stt.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("第一句应进入 STT")
	}
	// 第二句（撤退词“停”）在上一句未答完时结算：应 Cancel 在途回答、
	// 重新结算“停”，而不是悄悄丢弃。
	if err := p.settle(context.Background(), "lx06-1", []int16{2}); err != nil {
		t.Fatal(err)
	}
	// 等第 3 次 STT 进入：说明 maybeRetreat 已跑完 Cancel + 重结算，
	// 此时再放行第一句的 STT，旧答案必然看到已取消的 ctx 而退出。
	select {
	case <-stt.entered3:
	case <-time.After(2 * time.Second):
		t.Fatal("撤退词应触发重结算（第 3 次 STT）")
	}
	close(release)

	// 等 Route 收到“停”（2s 内）。
	deadline := time.Now().Add(2 * time.Second)
	for {
		rt.mu.Lock()
		got := append([]string(nil), rt.texts...)
		rt.mu.Unlock()
		if len(got) >= 1 && got[0] == "停" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("撤退词应被重新结算并路由，实际 Route 文本 %v", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// 取消的旧答案不应播出；撤退词回空回复也不播。
	select {
	case <-replied:
		t.Fatal("取消的旧答案与撤退词都不应播出")
	case <-time.After(300 * time.Millisecond):
	}
}
