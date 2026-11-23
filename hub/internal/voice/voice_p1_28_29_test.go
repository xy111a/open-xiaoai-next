package voice

// P1-28 / P1-29 回归测试：断线 Discard 丢弃残留 utterance；
// 结算异步化（Frame/End 立即返回，回答在后台跑）；同一设备上一句
// 未答完时新结算先判撤退词（P1-14），不是撤退词才丢弃。

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/huajun/open-xiaoai-next/hub/internal/opus"
)

func TestDiscardDropsPendingUtterance(t *testing.T) {
	replied := make(chan [][]byte, 1)
	p := New(&fakeSTT{text: "你好"}, &fakeTTS{pcm: make([]byte, 640)}, &fakeRouter{reply: "在呢"}, "v",
		func(deviceID string, frames [][]byte) { replied <- frames })
	ctx := context.Background()
	enc, _ := opus.NewEncoder()
	dec, _ := opus.NewDecoder()
	loud := loudPacket(t, enc)
	for f := 0; f < 5; f++ {
		if err := p.Frame(ctx, "lx06-1", dec, loud); err != nil {
			t.Fatal(err)
		}
	}
	p.mu.Lock()
	if _, ok := p.utterances["lx06-1"]; !ok {
		p.mu.Unlock()
		t.Fatal("喂语音后应有未结算 utterance")
	}
	p.mu.Unlock()
	// P1-28：断线丢弃。
	p.Discard("lx06-1")
	p.mu.Lock()
	if _, ok := p.utterances["lx06-1"]; ok {
		p.mu.Unlock()
		t.Fatal("Discard 后不应再有残留 utterance")
	}
	p.mu.Unlock()
	// 丢弃后 End 不应触发回答。
	if err := p.End(ctx, "lx06-1"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-replied:
		t.Fatal("Discard 后 End 不应触发回答")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestSettleIsAsync(t *testing.T) {
	release := make(chan struct{})
	stt := &blockingSTT{release: release}
	replied := make(chan [][]byte, 1)
	p := New(stt, &fakeTTS{pcm: make([]byte, 640)}, &fakeRouter{reply: "在呢"}, "v",
		func(deviceID string, frames [][]byte) { replied <- frames })
	// 白盒：直接调 settle，STT 阻塞时 settle 应立即返回（不阻塞读循环）。
	start := time.Now()
	if err := p.settle(context.Background(), "lx06-1", []int16{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("settle 应立即返回，实际耗时 %v", d)
	}
	// 放行 STT 后后台回答完成，onReply 被调。
	close(release)
	select {
	case <-replied:
	case <-time.After(2 * time.Second):
		t.Fatal("放行 STT 后后台回答应完成")
	}
}

// countingBlockingSTT 包装 blockingSTT 并计数 Transcribe 调用。
type countingBlockingSTT struct {
	inner   *blockingSTT
	mu      sync.Mutex
	n       int
	entered chan struct{} // 第一次进入 Transcribe 时关闭（供测试同步）
	once    sync.Once
}

func (c *countingBlockingSTT) Transcribe(pcm []byte, language string) (string, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	c.once.Do(func() { close(c.entered) })
	return c.inner.Transcribe(pcm, language)
}

func (c *countingBlockingSTT) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func TestSettleDropsWhenInFlight(t *testing.T) {
	release := make(chan struct{})
	stt := &countingBlockingSTT{inner: &blockingSTT{release: release}, entered: make(chan struct{})}
	replied := make(chan [][]byte, 2)
	p := New(stt, &fakeTTS{pcm: make([]byte, 640)}, &fakeRouter{reply: "在呢"}, "v",
		func(deviceID string, frames [][]byte) { replied <- frames })
	if err := p.settle(context.Background(), "lx06-1", []int16{1}); err != nil {
		t.Fatal(err)
	}
	// 等第一句确实进入 STT（阻塞中）再结算第二句。
	select {
	case <-stt.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("第一句应进入 STT")
	}
	if err := p.settle(context.Background(), "lx06-1", []int16{2}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	// P1-14：in-flight 时新结算先判撤退词——被挡下的本句多一次 STT
	// （撤退词检查），不是撤退词则丢弃、不进路由。
	if n := stt.calls(); n != 2 {
		t.Fatalf("in-flight 时新结算应判撤退词，STT 应被调 2 次（回答 1 次 + 撤退检查 1 次），实际 %d", n)
	}
	// 放行后第一句答完；再结算一局应正常进行。
	close(release)
	select {
	case <-replied:
	case <-time.After(2 * time.Second):
		t.Fatal("第一句回答应完成")
	}
	// 被挡下的本句不是撤退词（blockingSTT 回“你好”），不应再有回答。
	select {
	case <-replied:
		t.Fatal("非撤退词的丢弃句不应触发回答")
	case <-time.After(300 * time.Millisecond):
	}
	// 等 inFlight 标记清除（最多 2 秒）。
	deadline := time.Now().Add(2 * time.Second)
	for {
		p.mu.Lock()
		_, busy := p.inFlight["lx06-1"]
		p.mu.Unlock()
		if !busy || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := p.settle(context.Background(), "lx06-1", []int16{3}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-replied:
	case <-time.After(2 * time.Second):
		t.Fatal("上一句答完后新结算应正常回答")
	}
	if n := stt.calls(); n != 3 {
		t.Fatalf("新结算应调 STT，累计应为 3 次，实际 %d", n)
	}
}
