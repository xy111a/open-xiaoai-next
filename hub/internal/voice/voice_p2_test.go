package voice

import (
	"context"
	"testing"
	"time"
)

// P2-48：TTS 尾音不再丢失——合成 PCM 长度不是 320 点整数倍时，
// 尾部补零到整帧；短于一帧的回复也能出一帧，不再是空帧列表。
func TestAnswerPadsTailSamplesP2(t *testing.T) {
	// TTS 返回 500 点 PCM（1 整帧 320 + 尾部 180 点）：旧代码丢 180 点。
	ttsPCM := make([]byte, 1000)
	for i := range ttsPCM {
		ttsPCM[i] = byte(i)
	}
	replied := make(chan [][]byte, 1)
	p := New(&fakeSTT{text: "你好"}, &fakeTTS{pcm: ttsPCM}, &fakeRouter{reply: "你好"}, "v",
		func(deviceID string, frames [][]byte) { replied <- frames })
	pcm := make([]int16, 500)
	if err := p.answer(context.Background(), "d1", pcm); err != nil {
		t.Fatalf("answer: %v", err)
	}
	select {
	case frames := <-replied:
		if len(frames) != 2 {
			t.Fatalf("500 点应编成 2 帧（320 + 补零 180），实际 %d 帧", len(frames))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("answer 未回调 onReply")
	}
	// 短于一帧：100 点也应出一帧（旧代码零帧，onReply 收到空列表）。
	shortPCM := make([]byte, 200)
	replied2 := make(chan [][]byte, 1)
	p2 := New(&fakeSTT{text: "你好"}, &fakeTTS{pcm: shortPCM}, &fakeRouter{reply: "嗯"}, "v",
		func(deviceID string, frames [][]byte) { replied2 <- frames })
	if err := p2.answer(context.Background(), "d1", make([]int16, 100)); err != nil {
		t.Fatalf("answer: %v", err)
	}
	select {
	case frames := <-replied2:
		if len(frames) != 1 {
			t.Fatalf("100 点应编成 1 帧（补零），实际 %d 帧", len(frames))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("短回复未回调 onReply")
	}
}
