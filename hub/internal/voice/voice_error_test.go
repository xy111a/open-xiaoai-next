package voice

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/huajun/open-xiaoai-next/hub/internal/opus"
)

type errSTT struct{ err error }

func (f *errSTT) Transcribe(pcm []byte, language string) (string, error) {
	return "", f.err
}

type errTTS struct{ err error }

func (f *errTTS) Synthesize(text, voice string) ([]byte, error) {
	return nil, f.err
}

type errRouter struct{ err error }

func (f *errRouter) Route(ctx context.Context, deviceID, text string) (string, error) {
	return "", f.err
}

type errorCapture struct {
	deviceID string
	err      error
}

// driveOneUtterance 喂 5 帧语音 + End 强制结算（与 TestPipelineEndForcesAnswer 同款）。
func driveOneUtterance(t *testing.T, p *Pipeline) {
	t.Helper()
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
}

// newErrorPipeline 建一条带 onError 捕获的管线；onError 与 onReply 各进一个通道。
func newErrorPipeline(t *testing.T, stt STT, tts TTS, router Router) (*Pipeline, chan errorCapture, chan [][]byte) {
	t.Helper()
	errCh := make(chan errorCapture, 1)
	replied := make(chan [][]byte, 1)
	p := New(stt, tts, router, "v", func(deviceID string, frames [][]byte) { replied <- frames })
	p.SetOnError(func(deviceID string, err error) {
		errCh <- errorCapture{deviceID: deviceID, err: err}
	})
	return p, errCh, replied
}

// waitOnError 等一次 onError，超时即失败；同时断言 onReply 没有触发。
func waitOnError(t *testing.T, errCh chan errorCapture, replied chan [][]byte) errorCapture {
	t.Helper()
	select {
	case got := <-errCh:
		select {
		case <-replied:
			t.Fatal("失败时不应触发 onReply")
		default:
		}
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("onError 未触发")
		return errorCapture{}
	}
}

// TestPipelineOnErrorLLM：Router 报 LLM 错误时 onError 应触发，播报按 LLM 分类。
func TestPipelineOnErrorLLM(t *testing.T) {
	p, errCh, replied := newErrorPipeline(t,
		&fakeSTT{text: "你好"}, &fakeTTS{pcm: make([]byte, 640)},
		&errRouter{err: errors.New("agent http://192.168.88.247:9000/v3 返回 500")})
	driveOneUtterance(t, p)
	got := waitOnError(t, errCh, replied)
	if got.deviceID != "lx06-1" {
		t.Fatalf("onError deviceID 应为 lx06-1，实际 %q", got.deviceID)
	}
	if ErrorReply(got.err) != "大脑没连上，请稍后再试" {
		t.Fatalf("LLM 错误应播报大脑没连上，实际 %q", ErrorReply(got.err))
	}
}

// TestPipelineOnErrorHA：Router 报 HA 错误时 onError 应触发，播报按 HA 分类。
func TestPipelineOnErrorHA(t *testing.T) {
	p, errCh, replied := newErrorPipeline(t,
		&fakeSTT{text: "打开客厅灯"}, &fakeTTS{pcm: make([]byte, 640)},
		&errRouter{err: errors.New("ha: 调用超时")})
	driveOneUtterance(t, p)
	got := waitOnError(t, errCh, replied)
	if ErrorReply(got.err) != "家电没控制住，请稍后再试" {
		t.Fatalf("HA 错误应播报家电没控制住，实际 %q", ErrorReply(got.err))
	}
}

// TestPipelineOnErrorReminder：Router 报提醒错误时 onError 应触发，播报按提醒分类。
func TestPipelineOnErrorReminder(t *testing.T) {
	p, errCh, replied := newErrorPipeline(t,
		&fakeSTT{text: "提醒我吃药"}, &fakeTTS{pcm: make([]byte, 640)},
		&errRouter{err: errors.New("提醒文件损坏（已备份到 /tmp/x）")})
	driveOneUtterance(t, p)
	got := waitOnError(t, errCh, replied)
	if ErrorReply(got.err) != "提醒没记上，请再说一遍" {
		t.Fatalf("提醒错误应播报提醒没记上，实际 %q", ErrorReply(got.err))
	}
}

// TestPipelineOnErrorSTT：STT 失败时 onError 应触发，播报按 STT 分类。
func TestPipelineOnErrorSTT(t *testing.T) {
	p, errCh, replied := newErrorPipeline(t,
		&errSTT{err: errors.New("STT 不可用（Wyoming 未连接）")}, &fakeTTS{pcm: make([]byte, 640)},
		&fakeRouter{reply: "在呢"})
	driveOneUtterance(t, p)
	got := waitOnError(t, errCh, replied)
	if ErrorReply(got.err) != "语音识别没连上，请稍后再试" {
		t.Fatalf("STT 错误应播报语音识别没连上，实际 %q", ErrorReply(got.err))
	}
}

// TestPipelineOnErrorTTS：TTS 合成失败时 onError 应触发，播报按 TTS 分类。
func TestPipelineOnErrorTTS(t *testing.T) {
	p, errCh, replied := newErrorPipeline(t,
		&fakeSTT{text: "你好"}, &errTTS{err: errors.New("TTS 不可用（Wyoming 未连接）")},
		&fakeRouter{reply: "在呢"})
	driveOneUtterance(t, p)
	got := waitOnError(t, errCh, replied)
	if ErrorReply(got.err) != "语音合成没连上，请稍后再试" {
		t.Fatalf("TTS 错误应播报语音合成没连上，实际 %q", ErrorReply(got.err))
	}
}

// TestPipelineOnErrorNilSTT：STT 为 nil 时 answer 的明确错误也应走 onError。
func TestPipelineOnErrorNilSTT(t *testing.T) {
	p, errCh, replied := newErrorPipeline(t,
		nil, &fakeTTS{pcm: make([]byte, 640)},
		&fakeRouter{reply: "在呢"})
	driveOneUtterance(t, p)
	got := waitOnError(t, errCh, replied)
	if ErrorReply(got.err) != "语音识别没连上，请稍后再试" {
		t.Fatalf("STT 为 nil 应播报语音识别没连上，实际 %q", ErrorReply(got.err))
	}
}

// TestPipelineEmptyReplyNoOnError：空回复（撤退词、点歌快路径）仍保持
// 静默——不调 onError，也不触发 onReply（PR #14/#15 行为不变）。
func TestPipelineEmptyReplyNoOnError(t *testing.T) {
	errCh := make(chan errorCapture, 1)
	replied := make(chan [][]byte, 1)
	tts := &countingTTS{pcm: make([]byte, 640)}
	p := New(&fakeSTT{text: "播放青花瓷"}, tts, &fakeRouter{reply: ""}, "v",
		func(deviceID string, frames [][]byte) { replied <- frames })
	p.SetOnError(func(deviceID string, err error) {
		errCh <- errorCapture{deviceID: deviceID, err: err}
	})
	driveOneUtterance(t, p)
	select {
	case got := <-errCh:
		t.Fatalf("空回复时不应调 onError，实际 %v", got.err)
	case <-replied:
		t.Fatal("空回复时不应触发 onReply")
	case <-time.After(300 * time.Millisecond):
	}
	if got := tts.Calls(); got != 0 {
		t.Fatalf("空回复时 TTS 不应被调用，实际 %d 次", got)
	}
}

// TestPipelineCancelStaysSilent：回答中途被新唤醒打断（Cancel）时，
// onError 不触发、onReply 不触发——打断是正常行为，不是故障。
// 复用 voice_cancel_test.go 的 blockingRouter，直接调 settle。
func TestPipelineCancelStaysSilent(t *testing.T) {
	router := &blockingRouter{
		reply:   "旧答案",
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	errCh := make(chan errorCapture, 1)
	replied := make(chan [][]byte, 1)
	p := New(&fakeSTT{text: "你好"}, &fakeTTS{pcm: make([]byte, 640)},
		router, "v",
		func(deviceID string, frames [][]byte) { replied <- frames })
	p.SetOnError(func(deviceID string, err error) {
		errCh <- errorCapture{deviceID: deviceID, err: err}
	})
	if err := p.settle(context.Background(), "lx06-1", []int16{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-router.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("回答未进入 Route")
	}
	p.Cancel("lx06-1")
	select {
	case got := <-errCh:
		t.Fatalf("打断时不应调 onError，实际 %v", got.err)
	case <-replied:
		t.Fatal("打断后旧答案不应播出")
	case <-time.After(500 * time.Millisecond):
	}
}

// TestErrorReply：分类表——每类错误文本映射到对应的播报文案，未知错误走通用文案。
func TestErrorReply(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{errors.New("STT 不可用（Wyoming 未连接）"), "语音识别没连上，请稍后再试"},
		{errors.New("TTS 不可用（Wyoming 未连接）"), "语音合成没连上，请稍后再试"},
		{errors.New("语音管线繁忙，请稍后再试"), "我现在有点忙，请稍后再试"},
		{errors.New("agent error: unauthorized"), "大脑没连上，请稍后再试"},
		{errors.New("ha: 实体 light.x 不存在"), "家电没控制住，请稍后再试"},
		{errors.New("提醒文件损坏"), "提醒没记上，请再说一遍"},
		{errors.New("MPD 未连接"), "音乐没放出来，请稍后再试"},
		{errors.New("音乐服务没接好（网易云未配置）"), "音乐没放出来，请稍后再试"},
		{errors.New("voice: 回答超时"), "想得太久了，请再说一遍"},
		{errors.New("something unexpected"), "语音服务暂时没连上，请稍后再试"},
	}
	for _, c := range cases {
		if got := ErrorReply(c.err); got != c.want {
			t.Fatalf("ErrorReply(%q) = %q，应为 %q", c.err, got, c.want)
		}
	}
	if got := ErrorReply(nil); got != "" {
		t.Fatalf("ErrorReply(nil) 应为空，实际 %q", got)
	}
}
