package voice

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/huajun/open-xiaoai-next/hub/internal/observability"
	"github.com/huajun/open-xiaoai-next/hub/internal/opus"
)

// 管线接上延迟记录器后，一次完整问答应留下一条 voice 记录，
// 其中 stt_ms / tts_ms 真被测到（fake STT/TTS 只是快，
// 不是零——毫秒数 >= 0 且字段存在）；
// fakeRouter 不走 LLM，所以 llm_ms 应缺失（不是没测到，
// 是本句根本没走 LLM，见 Router.Latency 的注释）。
func TestPipelineRecordsLatency(t *testing.T) {
	rec := observability.NewRecorder(20)
	rec.NoteWake("lx06-1", 120)
	replied := make(chan [][]byte, 1)
	p := New(&fakeSTT{text: "打开客厅灯"}, &fakeTTS{pcm: make([]byte, 640)}, &fakeRouter{reply: "好的"}, "v",
		func(deviceID string, frames [][]byte) { replied <- frames })
	p.Latency = rec

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
	case <-replied:
	case <-time.After(2 * time.Second):
		t.Fatal("End 后未触发回答")
	}
	// answer 是异步结算：onReply 已触发不代表 Finish 已落盘，
	// 轮询等记录出现（最多 2s）。
	var recent []observability.Interaction
	for i := 0; i < 40; i++ {
		recent = rec.Recent()
		if len(recent) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(recent) != 1 {
		t.Fatalf("应留下 1 条延迟记录，实际 %d", len(recent))
	}
	got := recent[0]
	if got.Kind != "voice" || got.DeviceID != "lx06-1" || !got.OK {
		t.Fatalf("记录应为成功的 voice 问答，实际 %+v", got)
	}
	if got.Stages[observability.StageKWS] != 120 {
		t.Fatalf("kws_ms 应为 NoteWake 记的 120，实际 %v", got.Stages)
	}
	if _, ok := got.Stages[observability.StageSTT]; !ok {
		t.Fatalf("应测到 stt_ms，实际 %v", got.Stages)
	}
	if _, ok := got.Stages[observability.StageTTS]; !ok {
		t.Fatalf("应测到 tts_ms，实际 %v", got.Stages)
	}
	if _, ok := got.Stages[observability.StageLLM]; ok {
		t.Fatalf("fakeRouter 没走 LLM，llm_ms 应缺失，实际 %v", got.Stages)
	}
}
