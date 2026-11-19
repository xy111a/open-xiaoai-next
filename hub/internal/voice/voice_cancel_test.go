package voice

import (
	"context"
	"sync"
	"testing"
	"time"
)

// blockingRouter 是测试用的阻塞型 Router：Route 进入后一直等，直到
// release 被关闭（正常返回 reply）或 ctx 被取消（返回 ctx.Err()）。
// 用于模拟“上一轮回答卡在 LLM/路由阶段”。
type blockingRouter struct {
	reply   string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *blockingRouter) Route(ctx context.Context, deviceID, text string) (string, error) {
	r.once.Do(func() { close(r.entered) })
	select {
	case <-r.release:
		return r.reply, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// deviceBlockingRouter 只阻塞指定设备，其余设备直接返回 reply。
// 用于验证 Cancel 只影响目标设备。
type deviceBlockingRouter struct {
	blockDevice string
	reply       string
	entered     chan struct{}
	release     chan struct{}
	once        sync.Once
}

func (r *deviceBlockingRouter) Route(ctx context.Context, deviceID, text string) (string, error) {
	if deviceID != r.blockDevice {
		return r.reply, nil
	}
	r.once.Do(func() { close(r.entered) })
	select {
	case <-r.release:
		return r.reply, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// 打断后续（决定 6）：上一轮回答卡在 Router 时新唤醒到达 → Cancel 后
// 旧答案绝不播出；且 inFlight 立即释放，新一轮结算可马上进行并正常播出。
func TestPipelineCancelStopsInFlightAnswer(t *testing.T) {
	router := &blockingRouter{
		reply:   "旧答案",
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	replied := make(chan [][]byte, 2)
	p := New(&fakeSTT{text: "你好"}, &fakeTTS{pcm: make([]byte, 640)}, router, "v",
		func(deviceID string, frames [][]byte) { replied <- frames })

	// 第 1 句结算：卡在 Router 里。
	if err := p.settle(context.Background(), "lx06-1", []int16{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-router.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("第 1 句回答未进入 Router")
	}

	// 新唤醒到达 → 取消在途回答。
	p.Cancel("lx06-1")

	// 放行 Router：若取消没生效，旧答案会在此刻继续往下走并播出；
	// 取消生效时，旧答案已在 Router 返回前被 ctx 截住，绝不播出。
	close(router.release)
	select {
	case <-replied:
		t.Fatal("已取消的旧答案不应播出")
	case <-time.After(500 * time.Millisecond):
		// 旧答案没播，符合预期。
	}

	// 新一轮结算应马上进行（inFlight 已释放），并正常播出。
	if err := p.settle(context.Background(), "lx06-1", []int16{4, 5, 6}); err != nil {
		t.Fatal(err)
	}
	select {
	case frames := <-replied:
		if len(frames) == 0 {
			t.Fatal("新一轮回答应播出非空帧")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Cancel 后新一轮结算应正常播出")
	}
}

// 无在途回答时 Cancel 是空操作，不 panic、不影响后续正常流程。
func TestPipelineCancelNoopWhenIdle(t *testing.T) {
	replied := make(chan [][]byte, 1)
	p := New(&fakeSTT{text: "你好"}, &fakeTTS{pcm: make([]byte, 640)}, &fakeRouter{reply: "在呢"}, "v",
		func(deviceID string, frames [][]byte) { replied <- frames })
	p.Cancel("lx06-1")
	p.Cancel("不存在的设备")
	if err := p.settle(context.Background(), "lx06-1", []int16{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-replied:
	case <-time.After(2 * time.Second):
		t.Fatal("空操作 Cancel 后正常结算应播出")
	}
}

// Cancel 只影响目标设备：A 被取消不播，B 的回答不受影响正常播出。
func TestPipelineCancelOnlyAffectsTargetDevice(t *testing.T) {
	router := &deviceBlockingRouter{
		blockDevice: "lx06-1",
		reply:       "好的",
		entered:     make(chan struct{}),
		release:     make(chan struct{}),
	}
	replied := make(chan struct {
		device string
		frames [][]byte
	}, 2)
	p := New(&fakeSTT{text: "你好"}, &fakeTTS{pcm: make([]byte, 640)}, router, "v",
		func(deviceID string, frames [][]byte) {
			replied <- struct {
				device string
				frames [][]byte
			}{deviceID, frames}
		})

	// A 结算：卡在 Router 里。
	if err := p.settle(context.Background(), "lx06-1", []int16{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-router.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("A 的回答未进入 Router")
	}
	// B 结算：不同设备，不应被 A 的 inFlight 挡住，应直接播出。
	if err := p.settle(context.Background(), "oh2p-1", []int16{4, 5, 6}); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-replied:
		if r.device != "oh2p-1" {
			t.Fatalf("应先收到 B 的播出，实际是 %s", r.device)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("B 的回答应正常播出")
	}
	// 只取消 A。
	p.Cancel("lx06-1")
	close(router.release)
	// A 的旧答案绝不播出（再等 500ms 确认没有第二条播出）。
	select {
	case r := <-replied:
		t.Fatalf("A 被取消后不应播出，实际收到 %s 的播出", r.device)
	case <-time.After(500 * time.Millisecond):
	}
}
