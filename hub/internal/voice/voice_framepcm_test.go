package voice

import (
	"context"
	"math"
	"testing"
	"time"
)

// FramePCM 与 Frame 走同一条 VAD/结算链路：已解码的 PCM 帧直接喂管线，
// 800ms 静音后同样触发回答（Hub 的 OnAudioFrame 用它避免为 KWS 再解一次 Opus）。
func TestFramePCMSameAsFrame(t *testing.T) {
	ttsPCM := make([]byte, 32000)
	for i := 0; i < 16000; i++ {
		v := int16(8000 * math.Sin(2*math.Pi*440*float64(i)/16000))
		ttsPCM[2*i] = byte(v)
		ttsPCM[2*i+1] = byte(v >> 8)
	}
	replied := make(chan [][]byte, 1)
	p := New(&fakeSTT{text: "打开客厅灯"}, &fakeTTS{pcm: ttsPCM}, &fakeRouter{reply: "好的，客厅灯开了"}, "zh_CN-huayan-medium",
		func(deviceID string, frames [][]byte) { replied <- frames })

	ctx := context.Background()
	loud := make([]int16, 320)
	for i := range loud {
		loud[i] = int16(10000 * math.Sin(2*math.Pi*300*float64(i)/16000))
	}
	// 喂 1 秒“语音”（响亮正弦，每帧 RMS 远大于 200）。
	for f := 0; f < 50; f++ {
		if err := p.FramePCM(ctx, "lx06-1", loud); err != nil {
			t.Fatalf("FramePCM 语音帧: %v", err)
		}
	}
	// 再喂 45 帧静音（900ms）→ VAD 结算，触发回答。
	for f := 0; f < 45; f++ {
		if err := p.FramePCM(ctx, "lx06-1", make([]int16, 320)); err != nil {
			t.Fatalf("FramePCM 静音帧: %v", err)
		}
	}
	select {
	case frames := <-replied:
		if len(frames) != 50 {
			t.Fatalf("onReply 应收到 50 帧 Opus，实际 %d 帧", len(frames))
		}
	case <-time.After(2 * time.Second):
		// P1-29：回答改为异步结算，这里等一等再断言。
		t.Fatal("VAD 800ms 静音后未触发回答")
	}
}

// FramePCM 空帧不应 panic、不应触发回答。
func TestFramePCMEmptyNoPanic(t *testing.T) {
	replied := make(chan [][]byte, 1)
	p := New(&fakeSTT{text: "x"}, &fakeTTS{pcm: make([]byte, 640)}, &fakeRouter{reply: "y"}, "v",
		func(deviceID string, frames [][]byte) { replied <- frames })
	if err := p.FramePCM(context.Background(), "lx06-1", nil); err != nil {
		t.Fatalf("空 PCM 不应报错: %v", err)
	}
	select {
	case <-replied:
		t.Fatal("空 PCM 不应触发回答")
	default:
	}
}
