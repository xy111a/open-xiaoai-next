package voice

// P1-2 回归测试：STT/TTS nil、单句 30s 上限、总并发上限。

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huajun/open-xiaoai-next/hub/internal/opus"
)

func TestAnswerNilSTTReturnsErrorNotPanic(t *testing.T) {
	replied := make(chan [][]byte, 1)
	p := New(nil, &fakeTTS{pcm: make([]byte, 640)}, &fakeRouter{reply: "在呢"}, "v",
		func(deviceID string, frames [][]byte) { replied <- frames })
	// 直接调 answer：nil STT 应返回明确错误，而不是 panic，且不触发 onReply。
	err := p.answer(context.Background(), "lx06-1", []int16{1, 2, 3})
	if err == nil {
		t.Fatal("STT 为 nil 时 answer 应返回错误")
	}
	if !strings.Contains(err.Error(), "STT 不可用") {
		t.Fatalf("错误应明说 STT 不可用，实际: %v", err)
	}
	select {
	case <-replied:
		t.Fatal("STT 为 nil 时不应触发 onReply")
	default:
	}
}

func TestAnswerNilTTSReturnsErrorNotPanic(t *testing.T) {
	replied := make(chan [][]byte, 1)
	p := New(&fakeSTT{text: "你好"}, nil, &fakeRouter{reply: "在呢"}, "v",
		func(deviceID string, frames [][]byte) { replied <- frames })
	err := p.answer(context.Background(), "lx06-1", []int16{1, 2, 3})
	if err == nil {
		t.Fatal("TTS 为 nil 时 answer 应返回错误")
	}
	if !strings.Contains(err.Error(), "TTS 不可用") {
		t.Fatalf("错误应明说 TTS 不可用，实际: %v", err)
	}
	select {
	case <-replied:
		t.Fatal("TTS 为 nil 时不应触发 onReply")
	default:
	}
}

func TestAnswerNilBothReturnsSTTErrorFirst(t *testing.T) {
	p := New(nil, nil, &fakeRouter{reply: "在呢"}, "v", nil)
	err := p.answer(context.Background(), "lx06-1", []int16{1})
	if err == nil || !strings.Contains(err.Error(), "STT 不可用") {
		t.Fatalf("STT/TTS 都为 nil 时应先报 STT 不可用，实际: %v", err)
	}
}

// recSTT 记录收到的 PCM 长度（验证单句截断）。
type recSTT struct {
	mu      sync.Mutex
	lastLen int
	calls   int
}

func (f *recSTT) Transcribe(pcm []byte, language string) (string, error) {
	f.mu.Lock()
	f.lastLen = len(pcm)
	f.calls++
	f.mu.Unlock()
	return "你好", nil
}

func loudPacket(t *testing.T, enc *opus.Encoder) []byte {
	t.Helper()
	frame := make([]int16, 320)
	for i := range frame {
		frame[i] = 10000
	}
	pkt, err := enc.Encode(frame)
	if err != nil {
		t.Fatal(err)
	}
	return pkt
}

func TestFrameSingleUtteranceCapForcesAnswer(t *testing.T) {
	replied := make(chan [][]byte, 1)
	stt := &recSTT{}
	p := New(stt, &fakeTTS{pcm: make([]byte, 640)}, &fakeRouter{reply: "在呢"}, "v",
		func(deviceID string, frames [][]byte) { replied <- frames })
	enc, _ := opus.NewEncoder()
	dec, _ := opus.NewDecoder()
	// 白盒预置：已累积 479900 点语音（差一帧到 30s 上限）。
	p.mu.Lock()
	p.utterances["lx06-1"] = &utterance{pcm: make([]int16, MaxUtteranceSamples-100), speech: true}
	p.mu.Unlock()
	if err := p.Frame(context.Background(), "lx06-1", dec, loudPacket(t, enc)); err != nil {
		t.Fatalf("Frame: %v", err)
	}
	select {
	case <-replied:
		// 已强制结算并回答。
	case <-time.After(2 * time.Second):
		t.Fatal("单句超 30s 上限时应强制结算并回答")
	}
	stt.mu.Lock()
	gotLen := stt.lastLen
	stt.mu.Unlock()
	// 480000 点 × 2 字节 = 960000 字节，截断后不应超过该值。
	if gotLen > MaxUtteranceSamples*2 {
		t.Fatalf("传给 STT 的 PCM 应截断到 ≤%d 字节，实际 %d", MaxUtteranceSamples*2, gotLen)
	}
	if gotLen != MaxUtteranceSamples*2 {
		t.Fatalf("传给 STT 的 PCM 应恰好为上限 %d 字节，实际 %d", MaxUtteranceSamples*2, gotLen)
	}
}

func TestFrameSilenceOnlyCapDiscards(t *testing.T) {
	replied := make(chan [][]byte, 1)
	p := New(&fakeSTT{text: "x"}, &fakeTTS{pcm: make([]byte, 640)}, &fakeRouter{reply: "y"}, "v",
		func(deviceID string, frames [][]byte) { replied <- frames })
	enc, _ := opus.NewEncoder()
	dec, _ := opus.NewDecoder()
	// 白盒预置：479900 点从未检出语音的累积（纯静音/底噪）。
	p.mu.Lock()
	p.utterances["lx06-1"] = &utterance{pcm: make([]int16, MaxUtteranceSamples-100)}
	p.mu.Unlock()
	silence, _ := enc.Encode(make([]int16, 320))
	if err := p.Frame(context.Background(), "lx06-1", dec, silence); err != nil {
		t.Fatalf("Frame: %v", err)
	}
	select {
	case <-replied:
		t.Fatal("纯静音超限应丢弃而不回答")
	default:
	}
	p.mu.Lock()
	u := p.utterances["lx06-1"]
	p.mu.Unlock()
	if u == nil {
		t.Fatal("纯静音超限后 utterance 不应被删除（只是清空累积）")
	}
	if len(u.pcm) != 0 {
		t.Fatalf("纯静音超限后累积应清空，实际 %d 点", len(u.pcm))
	}
}

// blockingSTT 在 release 关闭前一直阻塞（验证并发上限）。
type blockingSTT struct{ release chan struct{} }

func (b *blockingSTT) Transcribe(pcm []byte, language string) (string, error) {
	<-b.release
	return "你好", nil
}

func TestAnswerConcurrencyCap(t *testing.T) {
	release := make(chan struct{})
	p := New(&blockingSTT{release: release}, &fakeTTS{pcm: make([]byte, 640)},
		&fakeRouter{reply: "在呢"}, "v", nil)
	errs := make(chan error, MaxConcurrentAnswers)
	for i := 0; i < MaxConcurrentAnswers; i++ {
		go func() {
			errs <- p.answer(context.Background(), "lx06-1", []int16{1, 2, 3})
		}()
	}
	// 等 4 路都占住信号量（最多 2 秒）。
	deadline := time.Now().Add(2 * time.Second)
	for len(p.sem) < MaxConcurrentAnswers && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(p.sem) != MaxConcurrentAnswers {
		close(release)
		t.Fatalf("4 路 answer 应占满并发上限，实际占用 %d", len(p.sem))
	}
	// 第 5 路应立即报繁忙，而不是排队等前 4 路。
	done := make(chan error, 1)
	go func() { done <- p.answer(context.Background(), "lx06-1", []int16{1}) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "繁忙") {
			close(release)
			t.Fatalf("第 5 路应报“语音管线繁忙”，实际: %v", err)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("第 5 路 answer 应立即返回繁忙错误，不应阻塞等待")
	}
	close(release)
	for i := 0; i < MaxConcurrentAnswers; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("前 4 路放行后应成功，实际: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("前 4 路放行后未返回")
		}
	}
}
