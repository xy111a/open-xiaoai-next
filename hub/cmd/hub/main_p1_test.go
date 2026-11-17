package main

// P1 全量修复 main.go 接线回归测试：P1-13 唤醒时间戳 + P1-38 唤醒词
// 记录、P1-16 统一播放协议、P1-15/P1-28/P1-9 OnDisconnect 清理。

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/huajun/open-xiaoai-next/hub/internal/agent"
	"github.com/huajun/open-xiaoai-next/hub/internal/arbitration"
	"github.com/huajun/open-xiaoai-next/hub/internal/config"
	"github.com/huajun/open-xiaoai-next/hub/internal/intercom"
	"github.com/huajun/open-xiaoai-next/hub/internal/opus"
	"github.com/huajun/open-xiaoai-next/hub/internal/ota"
	"github.com/huajun/open-xiaoai-next/hub/internal/registry"
	"github.com/huajun/open-xiaoai-next/hub/internal/router"
	"github.com/huajun/open-xiaoai-next/hub/internal/transport"
	"github.com/huajun/open-xiaoai-next/hub/internal/voice"
)

// ---- P1-13：唤醒时间戳 ----

func TestWakeTimestamp(t *testing.T) {
	if got := wakeTimestamp(transport.Wake{TsMs: 1794000000000}); !got.Equal(time.UnixMilli(1794000000000)) {
		t.Fatalf("TsMs > 0 时应用事件时间戳，实际 %v", got)
	}
	before := time.Now()
	got := wakeTimestamp(transport.Wake{})
	if got.Before(before) || got.After(time.Now()) {
		t.Fatalf("TsMs == 0 时应补 Hub 收到时间，实际 %v", got)
	}
}

// ---- P1-13 + P1-38：OnWake 去重 + 记唤醒词供 Router 选 Agent ----

func fakeAgentForMain(t *testing.T, reply string) *agent.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q}}]}`, reply)
	}))
	t.Cleanup(srv.Close)
	return agent.New(srv.URL+"/v1", "m", "")
}

func TestOnWakeDedupAndWakeWord(t *testing.T) {
	rt := router.New(router.Deps{
		Agents: map[string]*agent.Client{
			"hermes": fakeAgentForMain(t, "hermes 回复"),
			"claw":   fakeAgentForMain(t, "claw 回复"),
		},
		WakeWordRoutes: map[string]string{"你好小李": "hermes", "嘿小李": "claw"},
	}, nil)
	results := make(chan string, 4)
	h := &hubHandler{
		reg:      registry.New(),
		decoders: map[string]*opus.Decoder{},
		intercom: intercom.New(nil),
		sessions: map[string]time.Time{},
		playing:  map[string]bool{},
		playGen:  map[string]uint64{},
		router:   rt,
	}
	h.arbiter = arbitration.New(100*time.Millisecond, func(winner string, losers []string) {
		results <- winner
	})
	now := time.Now().UnixMilli()
	// 同一声唤醒双通道重复（同一 TsMs）：只应裁决一次。
	h.OnWake("lx06-1", transport.Wake{WakeWord: "你好小李", RMSdB: -10, TsMs: now})
	h.OnWake("lx06-1", transport.Wake{WakeWord: "你好小李", RMSdB: -12, TsMs: now})
	select {
	case w := <-results:
		if w != "lx06-1" {
			t.Fatalf("赢家应为 lx06-1，实际 %q", w)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("仲裁超时")
	}
	select {
	case w := <-results:
		t.Fatalf("去重后不应再裁决，实际多一次 %q", w)
	case <-time.After(300 * time.Millisecond):
	}
	// P1-38：OnWake 记下的唤醒词应让 Router 选到 hermes。
	reply, err := rt.Route(context.Background(), "lx06-1", "今天天气怎么样")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "hermes 回复" {
		t.Fatalf("唤醒词“你好小李”应走 hermes，实际 %q", reply)
	}
}

// ---- P1-16：统一播放协议 ----

type stubTransportHandler struct{}

func (stubTransportHandler) OnWake(deviceID string, w transport.Wake)           {}
func (stubTransportHandler) OnAudioStart(deviceID string)                       {}
func (stubTransportHandler) OnAudioFrame(deviceID string, opus []byte)          {}
func (stubTransportHandler) OnAudioEnd(deviceID string)                         {}
func (stubTransportHandler) OnRoomClaim(deviceID string, room string)           {}
func (stubTransportHandler) OnStatus(deviceID string, s transport.Status)       {}
func (stubTransportHandler) OnRPCResult(deviceID string, r transport.RPCResult) {}
func (stubTransportHandler) OnDisconnect(deviceID string)                       {}

// startPlayServer 起一个真 transport 服务 + 一个已 hello 的客户端连接，
// 返回 handler 与客户端连接（供 playFrames 断言 start/stop_play）。
func startPlayServer(t *testing.T) (*hubHandler, *websocket.Conn, string) {
	t.Helper()
	reg := registry.New()
	h := &hubHandler{
		cfg:      &config.Config{WakeWord: "你好小李"},
		reg:      reg,
		decoders: map[string]*opus.Decoder{},
		intercom: intercom.New(nil),
		sessions: map[string]time.Time{},
		playing:  map[string]bool{},
		playGen:  map[string]uint64{},
	}
	server := transport.New("test-token-1234567890", reg, stubTransportHandler{})
	h.server = server
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(ln, nil) }()
	t.Cleanup(func() { ln.Close() })

	dialer := websocket.DefaultDialer
	c, _, err := dialer.Dial("ws://"+ln.Addr().String()+"/ws", nil)
	if err != nil {
		t.Fatalf("Dial 假 Hub: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	if err := c.WriteJSON(map[string]any{
		"type": "hello", "token": "test-token-1234567890",
		"model": "lx06", "room": "客厅", "version": "1.0",
	}); err != nil {
		t.Fatal(err)
	}
	var helloOK map[string]any
	if err := c.ReadJSON(&helloOK); err != nil {
		t.Fatalf("读 hello_ok: %v", err)
	}
	if helloOK["type"] != "hello_ok" {
		t.Fatalf("应收到 hello_ok，实际 %v", helloOK)
	}
	return h, c, "lx06-127-0-0-1"
}

func TestPlayFramesStartStop(t *testing.T) {
	h, c, deviceID := startPlayServer(t)
	frames := [][]byte{{0x01, 0x02}, {0x03, 0x04}, {0x05}}
	h.playFrames(deviceID, frames)
	// playing 在播报时长内应为 true（3 帧 = 60ms）。
	h.playMu.Lock()
	if !h.playing[deviceID] {
		h.playMu.Unlock()
		t.Fatal("播报期间 playing 应为 true")
	}
	h.playMu.Unlock()
	// 客户端应收到 start_play → 3 帧二进制 → stop_play。
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	readRPC := func() map[string]any {
		t.Helper()
		mt, msg, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("读 RPC: %v", err)
		}
		if mt != websocket.TextMessage {
			t.Fatalf("RPC 应为文本消息，实际类型 %d", mt)
		}
		var m map[string]any
		if err := json.Unmarshal(msg, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	m := readRPC()
	if m["type"] != "rpc" || m["method"] != "start_play" {
		t.Fatalf("第一条应为 start_play RPC，实际 %v", m)
	}
	for i := 0; i < 3; i++ {
		mt, msg, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("读第 %d 帧: %v", i, err)
		}
		if mt != websocket.BinaryMessage {
			t.Fatalf("音频帧应为二进制消息，实际类型 %d", mt)
		}
		if len(msg) != len(frames[i])+1 || msg[0] != 0x01 {
			t.Fatalf("第 %d 帧内容不对: %v", i, msg)
		}
	}
	m = readRPC()
	if m["type"] != "rpc" || m["method"] != "stop_play" {
		t.Fatalf("最后一条应为 stop_play RPC，实际 %v", m)
	}
	// 60ms 播放时长过后 playing 应清除。
	time.Sleep(200 * time.Millisecond)
	h.playMu.Lock()
	if h.playing[deviceID] {
		h.playMu.Unlock()
		t.Fatal("播放时长过后 playing 应清除")
	}
	h.playMu.Unlock()
	// 空帧列表：什么都不发。
	h.playFrames(deviceID, nil)
	_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, _, err := c.ReadMessage(); err == nil {
		t.Fatal("空帧列表不应发任何消息")
	} else if !os.IsTimeout(err) {
		t.Fatalf("空帧后读消息应超时（无消息），实际错误: %v", err)
	}
}

func TestPlayFramesSerializesPerDevice(t *testing.T) {
	h, c, deviceID := startPlayServer(t)
	// 两路并发播报同一设备：帧不得交错——应是 start,帧,帧,stop,start,帧,帧,stop
	// 或相反顺序的两段完整序列（不能出现 start,start 或帧穿插）。
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.playFrames(deviceID, [][]byte{{0xAA}, {0xBB}})
		}()
	}
	wg.Wait()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	var methods []string
	binaries := 0
	for i := 0; i < 8; i++ {
		mt, msg, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("读第 %d 条: %v", i, err)
		}
		if mt == websocket.BinaryMessage {
			binaries++
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(msg, &m); err != nil {
			t.Fatal(err)
		}
		methods = append(methods, m["method"].(string))
	}
	if len(methods) != 4 || binaries != 4 {
		t.Fatalf("应收到 4 条 RPC + 4 帧二进制，实际 RPC %v + %d 帧", methods, binaries)
	}
	// 序列必须是 start,stop,start,stop（两段完整播报，不交错）。
	want := []string{"start_play", "stop_play", "start_play", "stop_play"}
	for i, w := range want {
		if methods[i] != w {
			t.Fatalf("播报序列应为 %v（不交错），实际 %v", want, methods)
		}
	}
}

// ---- P1-15/P1-28/P1-9：OnDisconnect 清理 ----

func TestOnDisconnectCleansUp(t *testing.T) {
	replied := make(chan [][]byte, 4)
	pipeline := voice.New(&kwsFakeSTT{text: "你好"}, &kwsFakeTTS{pcm: make([]byte, 640)},
		&kwsFakeRouter{reply: "在呢"}, "v",
		func(deviceID string, frames [][]byte) { replied <- frames })
	otaMgr := ota.New()
	h := &hubHandler{
		cfg:      &config.Config{WakeWord: "你好小李"},
		reg:      registry.New(),
		decoders: map[string]*opus.Decoder{},
		intercom: intercom.New(func(deviceID string, opusFrame []byte) error { return fmt.Errorf("not online") }),
		otaMgr:   otaMgr,
		sessions: map[string]time.Time{},
		playing:  map[string]bool{},
		playGen:  map[string]uint64{},
		pipeline: pipeline,
	}
	// OTA pushing 任务：断线应转 verifying（P1-9）。
	sha := strings.Repeat("a", 64)
	if _, err := otaMgr.Push("lx06-1", "1.0.0", "http://x/f.bin", sha); err != nil {
		t.Fatal(err)
	}
	// 先喂几帧语音，造一个未结算 utterance + decoder（注意顺序：
	// 对讲会话一旦开始，帧会走 Forward 不再进管线，所以先喂音频）。
	h.openSession("lx06-1")
	enc, _ := opus.NewEncoder()
	frame := make([]int16, 320)
	for i := range frame {
		frame[i] = 10000
	}
	pkt, _ := enc.Encode(frame)
	for i := 0; i < 5; i++ {
		h.OnAudioFrame("lx06-1", pkt)
	}
	if len(h.decoders) == 0 {
		t.Fatal("喂音频后应有 decoder")
	}
	// 对讲会话：发起方 lx06-1 → 目标 oh2p-1（P1-15 双向 End 用目标方断线测）。
	if !h.intercom.Start("lx06-1", "oh2p-1") {
		t.Fatal("Start 对讲应成功")
	}
	// 目标方断线：双向结束对讲 + 清 utterance/decoder + OTA 转 verifying。
	h.OnDisconnect("oh2p-1")
	if to, isB, err := h.intercom.Forward("lx06-1", pkt); err != nil || to != "" || isB {
		t.Fatalf("目标方断线后对讲会话应结束，Forward 实际 (%q, %v, %v)", to, isB, err)
	}
	if len(h.decoders) == 0 {
		t.Fatal("目标方断线与本设备无关，decoder 不应被删")
	}
	// 发起方断线：decoder 应删、utterance 应丢、OTA 应转 verifying。
	h.OnDisconnect("lx06-1")
	if len(h.decoders) != 0 {
		t.Fatalf("断线后 decoder 应删除，实际 %d 个", len(h.decoders))
	}
	if j, _ := otaMgr.Get("lx06-1"); j.Status != ota.StatusVerifying {
		t.Fatalf("断线后 OTA 应转 verifying，实际 %s", j.Status)
	}
	// 丢弃后 End 不应触发回答（P1-28）。
	if err := h.pipeline.End(context.Background(), "lx06-1"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-replied:
		t.Fatal("Discard 后 End 不应触发回答")
	case <-time.After(300 * time.Millisecond):
	}
}
