package voice

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/huajun/open-xiaoai-next/hub/internal/opus"
)

type fakeSTT struct{ text string }

func (f *fakeSTT) Transcribe(pcm []byte, language string) (string, error) {
	if language != "zh" {
		return "", errWrongLang
	}
	return f.text, nil
}

type fakeTTS struct{ pcm []byte }

func (f *fakeTTS) Synthesize(text, voice string) ([]byte, error) {
	return f.pcm, nil
}

type fakeRouter struct{ reply string }

func (f *fakeRouter) Route(ctx context.Context, deviceID, text string) (string, error) {
	return f.reply, nil
}

var errWrongLang = errString("wrong language")

type errString string

func (e errString) Error() string { return string(e) }

func TestRMS(t *testing.T) {
	if got := RMS([]int16{3, 4}); math.Abs(got-3.5355339) > 1e-6 {
		t.Fatalf("RMS([3,4]) 应为 3.5355，实际 %f", got)
	}
	if got := RMS(nil); got != 0 {
		t.Fatalf("空输入 RMS 应为 0，实际 %f", got)
	}
	if got := RMS([]int16{0, 0, 0}); got != 0 {
		t.Fatalf("全零 RMS 应为 0，实际 %f", got)
	}
}

// 真跑整条管线：Opus 编码的语音帧 → VAD 800ms 静音结算 → STT → Router → TTS → Opus 回帧。
// STT/TTS/Router 是 fake（它们是被测管线的输入，不是被测对象本身），Opus 编解码是真的。
func TestPipelineEndToEnd(t *testing.T) {
	// TTS 返回 1 秒 440Hz 正弦 PCM（16000 点）。
	ttsPCM := make([]byte, 32000)
	for i := 0; i < 16000; i++ {
		v := int16(8000 * math.Sin(2*math.Pi*440*float64(i)/16000))
		ttsPCM[2*i] = byte(v)
		ttsPCM[2*i+1] = byte(v >> 8)
	}
	replied := make(chan [][]byte, 1)
	p := New(&fakeSTT{text: "打开客厅灯"}, &fakeTTS{pcm: ttsPCM}, &fakeRouter{reply: "好的，客厅灯开了"}, "zh_CN-huayan-medium",
		func(deviceID string, frames [][]byte) { replied <- frames })

	enc, err := opus.NewEncoder()
	if err != nil {
		t.Fatal(err)
	}
	dec, err := opus.NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// 喂 1 秒“语音”（响亮正弦，每帧 RMS 远大于 200）。
	for f := 0; f < 50; f++ {
		frame := make([]int16, 320)
		for i := range frame {
			frame[i] = int16(10000 * math.Sin(2*math.Pi*300*float64(f*320+i)/16000))
		}
		pkt, err := enc.Encode(frame)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Frame(ctx, "lx06-1", dec, pkt); err != nil {
			t.Fatalf("Frame: %v", err)
		}
	}
	// 再喂 45 帧静音（900ms）→ VAD 结算，触发回答。
	// 注意：每帧都要现编新鲜的静音包（模拟真实麦克风连续静音）；
	// 重复解同一个包会复现编码器残留音，RMS 恒 >200，VAD 永远等不到静音。
	// 另：静音首帧是语音尾音（RMS 约 4000），会把 silenceMs 清零一次，
	// 所以要多喂几帧，留出余量。
	for f := 0; f < 45; f++ {
		silence, err := enc.Encode(make([]int16, 320))
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Frame(ctx, "lx06-1", dec, silence); err != nil {
			t.Fatalf("Frame 静音: %v", err)
		}
	}
	select {
	case frames := <-replied:
		// TTS 的 1 秒 PCM = 50 帧 Opus。
		if len(frames) != 50 {
			t.Fatalf("onReply 应收到 50 帧 Opus，实际 %d 帧", len(frames))
		}
		// 解一帧验证确实是 Opus 数据（能解出 320 点）。
		out, err := dec.Decode(frames[0])
		if err != nil {
			t.Fatalf("回帧解码失败: %v", err)
		}
		if len(out) != 320 {
			t.Fatalf("回帧应解出 320 点，实际 %d", len(out))
		}
	case <-time.After(2 * time.Second):
		// P1-29：回答改为异步结算，这里等一等再断言。
		t.Fatal("VAD 800ms 静音后未触发回答")
	}
}

// audio_end 强制结算：不等 800ms 静音也应回答。
func TestPipelineEndForcesAnswer(t *testing.T) {
	replied := make(chan [][]byte, 1)
	p := New(&fakeSTT{text: "你好"}, &fakeTTS{pcm: make([]byte, 640)}, &fakeRouter{reply: "在呢"}, "v",
		func(deviceID string, frames [][]byte) { replied <- frames })
	enc, _ := opus.NewEncoder()
	dec, _ := opus.NewDecoder()
	ctx := context.Background()
	frame := make([]int16, 320)
	for i := range frame {
		frame[i] = int16(10000 * math.Sin(2*math.Pi*300*float64(i)/16000))
	}
	pkt, _ := enc.Encode(frame)
	for f := 0; f < 5; f++ {
		if err := p.Frame(ctx, "lx06-1", dec, pkt); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.End(ctx, "lx06-1"); err != nil {
		t.Fatalf("End: %v", err)
	}
	select {
	case frames := <-replied:
		if len(frames) != 1 {
			t.Fatalf("640 字节 TTS PCM 应编成 1 帧，实际 %d", len(frames))
		}
	case <-time.After(2 * time.Second):
		// P1-29：回答改为异步结算，这里等一等再断言。
		t.Fatal("End 后未触发回答")
	}
}

// 纯静音（没说过话）时 End 不应触发回答。
func TestPipelineSilenceOnlyNoAnswer(t *testing.T) {
	replied := make(chan [][]byte, 1)
	p := New(&fakeSTT{text: "x"}, &fakeTTS{pcm: make([]byte, 640)}, &fakeRouter{reply: "y"}, "v",
		func(deviceID string, frames [][]byte) { replied <- frames })
	dec, _ := opus.NewDecoder()
	ctx := context.Background()
	silenceEnc, _ := opus.NewEncoder()
	for f := 0; f < 10; f++ {
		silence, _ := silenceEnc.Encode(make([]int16, 320))
		_ = p.Frame(ctx, "lx06-1", dec, silence)
	}
	if err := p.End(ctx, "lx06-1"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-replied:
		t.Fatal("纯静音不应触发回答")
	case <-time.After(200 * time.Millisecond):
		// P1-29：回答异步，等 200ms 确认确实没触发（不是没来得及）。
	}
}
