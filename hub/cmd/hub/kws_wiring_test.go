package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/huajun/open-xiaoai-next/hub/internal/arbitration"
	"github.com/huajun/open-xiaoai-next/hub/internal/config"
	"github.com/huajun/open-xiaoai-next/hub/internal/intercom"
	"github.com/huajun/open-xiaoai-next/hub/internal/kws"
	"github.com/huajun/open-xiaoai-next/hub/internal/opus"
	"github.com/huajun/open-xiaoai-next/hub/internal/registry"
	"github.com/huajun/open-xiaoai-next/hub/internal/voice"
)

// 本文件是 Hub 侧 KWS 接线测试：合成 PCM 流 → 假边车 wake 事件 →
// 250ms 仲裁 → 赢家进语音管线。
// 注意：喂的是合成音频（正弦波 + Opus 编码），测的是接线不是真人唤醒率；
// 真人唤醒率未验证，不得据此声称。

// ---- 测试替身 ----

type kwsFakeSTT struct{ text string }

func (f *kwsFakeSTT) Transcribe(pcm []byte, language string) (string, error) {
	return f.text, nil
}

type kwsFakeTTS struct{ pcm []byte }

func (f *kwsFakeTTS) Synthesize(text, voice string) ([]byte, error) {
	return f.pcm, nil
}

type kwsFakeRouter struct{ reply string }

func (f *kwsFakeRouter) Route(ctx context.Context, deviceID, text string) (string, error) {
	return f.reply, nil
}

// kwsFakeSidecar 是测试用边车：WS /v1/kws + HTTP /health。
type kwsFakeSidecar struct {
	ln  net.Listener
	srv *http.Server

	mu       sync.Mutex
	conns    map[string]*websocket.Conn
	hellos   map[string]bool
	pcmFrame map[string]int
}

func newKWSFakeSidecar(t *testing.T) *kwsFakeSidecar {
	t.Helper()
	f := &kwsFakeSidecar{
		conns: map[string]*websocket.Conn{}, hellos: map[string]bool{}, pcmFrame: map[string]int{},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.ln = ln
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/kws", f.handleWS)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	f.srv = &http.Server{Handler: mux}
	go func() { _ = f.srv.Serve(ln) }()
	t.Cleanup(func() { _ = f.srv.Close() })
	return f
}

func (f *kwsFakeSidecar) url() string { return "ws://" + f.ln.Addr().String() + "/v1/kws" }

func (f *kwsFakeSidecar) handleWS(w http.ResponseWriter, r *http.Request) {
	up := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	_, first, err := conn.ReadMessage()
	if err != nil {
		_ = conn.Close()
		return
	}
	var hello map[string]any
	if err := json.Unmarshal(first, &hello); err != nil || hello["type"] != "hello" {
		_ = conn.Close()
		return
	}
	deviceID, _ := hello["device_id"].(string)
	f.mu.Lock()
	if old, ok := f.conns[deviceID]; ok {
		_ = old.Close()
	}
	f.conns[deviceID] = conn
	f.hellos[deviceID] = true
	f.mu.Unlock()
	_ = conn.WriteJSON(map[string]string{"type": "ready"})
	for {
		mt, _, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if mt == websocket.BinaryMessage {
			f.mu.Lock()
			f.pcmFrame[deviceID]++
			f.mu.Unlock()
		}
	}
}

func (f *kwsFakeSidecar) waitHello(t *testing.T, deviceID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		ok := f.hellos[deviceID]
		f.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("超时：假边车未收到设备 %s 的 hello", deviceID)
}

func (f *kwsFakeSidecar) pcmFrames(deviceID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pcmFrame[deviceID]
}

func (f *kwsFakeSidecar) waitPCMFrames(t *testing.T, deviceID string, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f.pcmFrames(deviceID) >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("超时：设备 %s 的 PCM 只收到 %d 帧，想要至少 %d", deviceID, f.pcmFrames(deviceID), n)
}

func (f *kwsFakeSidecar) sendWake(t *testing.T, deviceID, keyword string, rms float64) {
	t.Helper()
	f.mu.Lock()
	conn, ok := f.conns[deviceID]
	f.mu.Unlock()
	if !ok {
		t.Fatalf("假边车没有设备 %s 的连接，无法发 wake", deviceID)
	}
	if err := conn.WriteJSON(map[string]any{
		"type": "wake", "device_id": deviceID, "keyword": keyword,
		"rms": rms, "timestamp_ms": time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("发 wake 失败: %v", err)
	}
}

func (f *kwsFakeSidecar) closeConn(deviceID string) {
	f.mu.Lock()
	conn, ok := f.conns[deviceID]
	if ok {
		delete(f.conns, deviceID)
		delete(f.hellos, deviceID)
	}
	f.mu.Unlock()
	if ok {
		_ = conn.Close()
	}
}

// ---- 测试骨架 ----

type kwsHarness struct {
	t       *testing.T
	client  *kws.Client
	handler *hubHandler
	enc     *opus.Encoder
	won     chan string
	lost    chan []string
	replied chan [][]byte
	done    chan struct{}
}

func newKWSHarness(t *testing.T, sidecarURL string) *kwsHarness {
	t.Helper()
	ttsPCM := make([]byte, 32000)
	for i := 0; i < 16000; i++ {
		v := int16(8000 * math.Sin(2*math.Pi*440*float64(i)/16000))
		ttsPCM[2*i] = byte(v)
		ttsPCM[2*i+1] = byte(v >> 8)
	}
	h := &kwsHarness{
		t:       t,
		won:     make(chan string, 4),
		lost:    make(chan []string, 4),
		replied: make(chan [][]byte, 4),
		done:    make(chan struct{}),
	}
	enc, err := opus.NewEncoder()
	if err != nil {
		t.Fatal(err)
	}
	h.enc = enc
	pipeline := voice.New(&kwsFakeSTT{text: "打开客厅灯"}, &kwsFakeTTS{pcm: ttsPCM},
		&kwsFakeRouter{reply: "好的，客厅灯开了"}, "zh_CN-huayan-medium",
		func(deviceID string, frames [][]byte) { h.replied <- frames })
	handler := &hubHandler{
		cfg:      &config.Config{WakeWord: "你好小李"},
		reg:      registry.New(),
		decoders: map[string]*opus.Decoder{},
		intercom: intercom.New(func(deviceID string, opusFrame []byte) error { return fmt.Errorf("not online") }),
		sessions: map[string]time.Time{},
		playing:  map[string]bool{},
		pipeline: pipeline,
	}
	handler.arbiter = arbitration.New(arbitration.Window, func(winner string, losers []string) {
		if winner == "" {
			return
		}
		handler.openSession(winner)
		h.won <- winner
		h.lost <- losers
	})
	h.handler = handler

	client, err := kws.New(sidecarURL, kws.Options{
		Keywords:         []string{"你好小李"},
		DialTimeout:      2 * time.Second,
		ReconnectInitial: 50 * time.Millisecond,
		ReconnectMax:     200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.client = client
	handler.kws = client
	go func() {
		for {
			select {
			case ev := <-client.Events():
				handler.OnKWSWake(ev)
			case <-h.done:
				return
			}
		}
	}()
	t.Cleanup(func() {
		close(h.done)
		client.Close()
	})
	return h
}

// feedLoud 经 OnAudioFrame 喂 n 帧响亮合成语音（Opus 编码过的正弦波）。
func (h *kwsHarness) feedLoud(deviceID string, n int) {
	h.t.Helper()
	for f := 0; f < n; f++ {
		frame := make([]int16, 320)
		for i := range frame {
			frame[i] = int16(10000 * math.Sin(2*math.Pi*300*float64(f*320+i)/16000))
		}
		pkt, err := h.enc.Encode(frame)
		if err != nil {
			h.t.Fatal(err)
		}
		h.handler.OnAudioFrame(deviceID, pkt)
	}
}

// feedSilence 经 OnAudioFrame 喂 n 帧静音（每帧现编，避免编码器残留音）。
func (h *kwsHarness) feedSilence(deviceID string, n int) {
	h.t.Helper()
	for f := 0; f < n; f++ {
		pkt, err := h.enc.Encode(make([]int16, 320))
		if err != nil {
			h.t.Fatal(err)
		}
		h.handler.OnAudioFrame(deviceID, pkt)
	}
}

func (h *kwsHarness) waitWinner(t *testing.T, timeout time.Duration) string {
	t.Helper()
	select {
	case w := <-h.won:
		return w
	case <-time.After(timeout):
		t.Fatal("超时：仲裁未选出赢家")
		return ""
	}
}

// TestKWSWakeToPipeline：合成 PCM 流 → 假边车 wake → 仲裁赢 →
// 会话期内语音进管线 → STT/路由/TTS → onReply 出声。
// （合成语音，非真人唤醒率。）
func TestKWSWakeToPipeline(t *testing.T) {
	fake := newKWSFakeSidecar(t)
	h := newKWSHarness(t, fake.url())

	// 唤醒前：音频只进 KWS（无会话，管线不处理），边车应收到 PCM。
	h.feedLoud("lx06-1", 10)
	fake.waitPCMFrames(t, "lx06-1", 10, 3*time.Second)

	// 边车命中唤醒词 → 仲裁赢 → 开会话。
	fake.sendWake(t, "lx06-1", "你好小李", 0.42)
	if w := h.waitWinner(t, 3*time.Second); w != "lx06-1" {
		t.Fatalf("单设备唤醒应赢 lx06-1，实际赢 %q", w)
	}

	// 唤醒后：会话期内语音进管线，800ms 静音结算 → 回答。
	h.feedLoud("lx06-1", 50)
	h.feedSilence("lx06-1", 45)
	select {
	case frames := <-h.replied:
		if len(frames) != 50 {
			t.Fatalf("onReply 应收到 50 帧 Opus（1 秒 TTS），实际 %d 帧", len(frames))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("超时：唤醒后语音未进管线回答")
	}
}

// TestKWSDualDeviceArbitration：双设备同时唤醒，RMS 大的赢且只赢一台。
// （合成语音，非真人唤醒率。）
func TestKWSDualDeviceArbitration(t *testing.T) {
	fake := newKWSFakeSidecar(t)
	h := newKWSHarness(t, fake.url())

	h.feedLoud("lx06-1", 5)
	h.feedLoud("oh2p-1", 5)
	fake.waitPCMFrames(t, "lx06-1", 5, 3*time.Second)
	fake.waitPCMFrames(t, "oh2p-1", 5, 3*time.Second)

	// 两台几乎同时唤醒：lx06 RMS 0.2（轻），oh2p RMS 0.6（响）。
	fake.sendWake(t, "lx06-1", "你好小李", 0.2)
	fake.sendWake(t, "oh2p-1", "你好小李", 0.6)
	if w := h.waitWinner(t, 3*time.Second); w != "oh2p-1" {
		t.Fatalf("RMS 大的 oh2p-1 应赢，实际赢 %q", w)
	}
	select {
	case losers := <-h.lost:
		if len(losers) != 1 || losers[0] != "lx06-1" {
			t.Fatalf("输家应只有 lx06-1，实际 %v", losers)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("超时：仲裁未报告输家")
	}
	// 只裁决一次：窗口内没有第二轮赢家。
	select {
	case w := <-h.won:
		t.Fatalf("双设备仲裁应只赢一台，多出一轮赢家 %q", w)
	case <-time.After(600 * time.Millisecond):
	}
}

// TestKWSReconnectRestoresStream：边车断线/重连后音频流恢复，wake 仍能走通。
func TestKWSReconnectRestoresStream(t *testing.T) {
	fake := newKWSFakeSidecar(t)
	h := newKWSHarness(t, fake.url())

	h.feedLoud("lx06-1", 5)
	fake.waitPCMFrames(t, "lx06-1", 5, 3*time.Second)
	fake.closeConn("lx06-1")

	// 重连后（新 hello）再发音频，边车计数继续增长。
	fake.waitHello(t, "lx06-1", 5*time.Second)
	before := fake.pcmFrames("lx06-1")
	h.feedLoud("lx06-1", 5)
	fake.waitPCMFrames(t, "lx06-1", before+5, 5*time.Second)

	fake.sendWake(t, "lx06-1", "你好小李", 0.42)
	if w := h.waitWinner(t, 3*time.Second); w != "lx06-1" {
		t.Fatalf("重连后唤醒应赢 lx06-1，实际赢 %q", w)
	}
}

// TestKWSDownDegradesWithoutPanic：边车不可用时 Hub 不 panic、不静默丢音频
// （丢帧记日志、有计数），会话期内语音仍走管线，/api/status 侧由 web 包单测覆盖。
func TestKWSDownDegradesWithoutPanic(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := ln.Addr().String()
	_ = ln.Close()

	h := newKWSHarness(t, "ws://"+deadAddr+"/v1/kws")
	// 手动开会话（模拟已赢过仲裁）：即使 KWS 挂了，语音管线本身仍可用。
	h.handler.openSession("lx06-1")
	h.feedLoud("lx06-1", 50)
	h.feedSilence("lx06-1", 45)
	select {
	case frames := <-h.replied:
		if len(frames) != 50 {
			t.Fatalf("KWS 挂了时管线仍应回答 50 帧，实际 %d 帧", len(frames))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("超时：KWS 不可用时会话语音未进管线")
	}
	if got := h.client.DroppedFrames("lx06-1"); got == 0 {
		t.Fatal("边车不可用时应丢帧并计数，实际丢 0 帧")
	}
	if st := h.client.Status(); !st.Down {
		t.Fatalf("边车不可用时 Status().Down 应为 true，实际 %+v", st)
	}
}

// TestKWSOnDisconnectCleansStream：设备断开即清理其 KWS 流状态。
func TestKWSOnDisconnectCleansStream(t *testing.T) {
	fake := newKWSFakeSidecar(t)
	h := newKWSHarness(t, fake.url())

	h.feedLoud("lx06-1", 2)
	fake.waitHello(t, "lx06-1", 3*time.Second)
	if st := h.client.Status(); st.Streams != 1 {
		t.Fatalf("发音频后应有 1 条流，实际 %+v", st)
	}
	h.handler.OnDisconnect("lx06-1")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st := h.client.Status(); st.Streams == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("OnDisconnect 后 KWS 流状态应被清理")
}

// TestKWSRMSConversion：边车线性 rms 换算 dB 后仲裁仍按大小排序
// （0.6 → 约 -4.4dB 赢过 0.2 → 约 -14dB）；rms<=0 时不 panic。
func TestKWSRMSConversion(t *testing.T) {
	h := newKWSHarness(t, "ws://127.0.0.1:1/v1/kws")
	// 直接走 OnKWSWake（不经过边车）：rms 0 的事件不应 panic。
	h.handler.OnKWSWake(kws.WakeEvent{DeviceID: "lx06-1", Keyword: "你好小李", RMS: 0, TimestampMs: time.Now().UnixMilli()})
	if w := h.waitWinner(t, 3*time.Second); w != "lx06-1" {
		t.Fatalf("rms=0 的唤醒也应能仲裁，实际赢 %q", w)
	}
}
