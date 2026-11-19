// Package main 的打断（barge-in）模拟集成测试：真 transport 服务 +
// 真 hubHandler + 真仲裁器 + 两个模拟 Client（gorilla WS），
// 再加假 KWS 边车唤醒事件（直接调 OnKWSWake，不起边车进程）。
//
// 模拟的是“音箱放 TTS 时用户喊唤醒词”的完整链路：
// fake Client 在 Speaking → 假 KWS 唤醒 → Hub 发 interrupt →
// 设备进 Listening → 仲裁裁决。真机验证（AEC、真人语音、
// 真实房间混响）仍需硬件，见最终报告的硬件验证清单。
package main

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/huajun/open-xiaoai-next/hub/internal/arbitration"
	"github.com/huajun/open-xiaoai-next/hub/internal/intercom"
	"github.com/huajun/open-xiaoai-next/hub/internal/kws"
	"github.com/huajun/open-xiaoai-next/hub/internal/registry"
	"github.com/huajun/open-xiaoai-next/hub/internal/transport"
)

// newBargeInServer 建一套真 Hub（transport + hubHandler + 仲裁器，
// 仲裁回调就是生产 main() 用的 h.onArbitrationResult），
// 返回 handler 与 ws:// URL。
func newBargeInServer(t *testing.T) (*hubHandler, string) {
	t.Helper()
	reg := registry.New()
	h := &hubHandler{
		reg:      reg,
		intercom: intercom.New(func(string, []byte) error { return nil }),
		sessions: map[string]time.Time{},
		states:   map[string]DeviceState{},
		playGen:  map[string]uint64{},
	}
	h.arbiter = arbitration.New(arbitration.Window, h.onArbitrationResult)
	srv := transport.New("secret", reg, h)
	h.server = srv
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() { _ = srv.Serve(ln, nil) }()
	return h, "ws://" + ln.Addr().String() + "/ws"
}

// dialBargeInClient 连上 Hub 并完成 hello 握手，返回连接与分配的 deviceID。
func dialBargeInClient(t *testing.T, url, model string) (*websocket.Conn, string) {
	t.Helper()
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("Dial %s: %v", url, err)
	}
	if err := c.WriteJSON(map[string]any{"type": "hello", "model": model, "ram_mb": 256, "version": "test", "token": "secret"}); err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := c.ReadJSON(&resp); err != nil {
		t.Fatalf("读 hello_ok: %v", err)
	}
	if resp["type"] != "hello_ok" {
		t.Fatalf("应收到 hello_ok，实际 %v", resp)
	}
	id, _ := resp["device_id"].(string)
	if id == "" {
		t.Fatalf("hello_ok 缺 device_id: %v", resp)
	}
	return c, id
}

// startBargeInReader 起一个读 goroutine，把收到的消息类型（文本帧的
// type，二进制帧记为 "binary"）推入带缓冲通道；连接出错即停。
func startBargeInReader(t *testing.T, c *websocket.Conn) <-chan string {
	t.Helper()
	ch := make(chan string, 128)
	go func() {
		for {
			mt, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			if mt == websocket.BinaryMessage {
				ch <- "binary"
				continue
			}
			var m map[string]any
			if err := json.Unmarshal(data, &m); err != nil {
				continue
			}
			if typ, _ := m["type"].(string); typ != "" {
				ch <- typ
			}
		}
	}()
	return ch
}

// waitForMsgType 等通道里出现某类消息（其余类型跳过），超时即失败。
func waitForMsgType(t *testing.T, ch <-chan string, want string, timeout time.Duration) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case got, ok := <-ch:
			if !ok {
				t.Fatalf("等 %q 时连接已关闭", want)
			}
			if got == want {
				return
			}
		case <-timer.C:
			t.Fatalf("等 %q 超时（%v）", want, timeout)
		}
	}
}

// expectNoMsgType 在 d 内排空通道，若出现 unwanted 即失败。
// （interrupt 若被错发，收到时已进缓冲，事后排空同样能抓住。）
func expectNoMsgType(t *testing.T, ch <-chan string, unwanted string, d time.Duration) {
	t.Helper()
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case got := <-ch:
			if got == unwanted {
				t.Fatalf("不应收到 %q，却收到了", unwanted)
			}
		case <-timer.C:
			return
		}
	}
}

// waitForState 轮询设备态到目标值（裁决在仲裁 goroutine 里跑，
// 状态翻转与消息到达之间有微小竞态，直接断言会抖）。
func waitForState(t *testing.T, h *hubHandler, deviceID string, want DeviceState, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := h.deviceState(deviceID); got == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("设备 %s 应为 %q，实际 %q", deviceID, want, h.deviceState(deviceID))
}

// speakingFrames 是播报用的假 Opus 帧（50 帧 ≈ 1s 播报，内容不重要，
// 接收端只计数不解码；播报时长要盖过“唤醒 → 仲裁”的 250ms 窗口）。
func speakingFrames() [][]byte {
	frames := make([][]byte, 50)
	for i := range frames {
		frames[i] = []byte{0x01, 0x02, 0x03}
	}
	return frames
}

func TestBargeInSpeakingDeviceGetsInterruptAndListens(t *testing.T) {
	h, url := newBargeInServer(t)
	c, deviceID := dialBargeInClient(t, url, "lx06")
	defer c.Close()
	msgs := startBargeInReader(t, c)

	// 正在放 TTS：设备进 Speaking。
	h.playFrames(deviceID, speakingFrames())
	if got := h.deviceState(deviceID); got != DeviceStateSpeaking {
		t.Fatalf("播报开始后应为 Speaking，实际 %q", got)
	}

	// 假 KWS 边车唤醒事件（放音中检出“你好小李”）。
	h.OnKWSWake(kws.WakeEvent{
		DeviceID: deviceID, Keyword: "你好小李",
		RMS: 0.4, RMSPeak: 0.5, TimestampMs: time.Now().UnixMilli(),
	})
	// Hub 必须立刻给这台设备发 interrupt（不等 250ms 仲裁）。
	waitForMsgType(t, msgs, "interrupt", 3*time.Second)
	// 设备进 Listening（新一轮听），且仲裁随后判它赢。
	waitForState(t, h, deviceID, DeviceStateListening, 3*time.Second)
	waitForMsgType(t, msgs, "wake_win", 3*time.Second)
	waitForState(t, h, deviceID, DeviceStateListening, 3*time.Second)
}

func TestBargeInLoudLoserGoesIdleWinnerListens(t *testing.T) {
	h, url := newBargeInServer(t)
	a, aID := dialBargeInClient(t, url, "lx06")
	defer a.Close()
	b, bID := dialBargeInClient(t, url, "oh2p")
	defer b.Close()
	aMsgs := startBargeInReader(t, a)
	bMsgs := startBargeInReader(t, b)

	// A 正在放 TTS（无会话的提醒式播报），B 静默。
	h.playFrames(aID, speakingFrames())
	if got := h.deviceState(aID); got != DeviceStateSpeaking {
		t.Fatalf("A 播报开始后应为 Speaking，实际 %q", got)
	}
	if got := h.deviceState(bID); got != DeviceStateIdle {
		t.Fatalf("B 应为 Idle，实际 %q", got)
	}

	// 两台几乎同时唤醒：A 轻（RMS 峰值 0.3）、B 响（0.6）——B 赢。
	now := time.Now().UnixMilli()
	h.OnKWSWake(kws.WakeEvent{DeviceID: aID, Keyword: "你好小李", RMS: 0.3, RMSPeak: 0.3, TimestampMs: now})
	h.OnKWSWake(kws.WakeEvent{DeviceID: bID, Keyword: "你好小李", RMS: 0.6, RMSPeak: 0.6, TimestampMs: now})
	// A 被打断：只停 A 的播报。
	waitForMsgType(t, aMsgs, "interrupt", 3*time.Second)
	// 仲裁：B 赢、A 输。
	waitForMsgType(t, bMsgs, "wake_win", 3*time.Second)
	waitForMsgType(t, aMsgs, "wake_lose", 3*time.Second)
	// B 从没放过音：绝不能收到 interrupt（排空期内若有错发仍在缓冲里）。
	expectNoMsgType(t, bMsgs, "interrupt", 600*time.Millisecond)
	// 终态：A 回 Idle（输了且无会话），B 在 Listening。
	waitForState(t, h, aID, DeviceStateIdle, 3*time.Second)
	waitForState(t, h, bID, DeviceStateListening, 3*time.Second)
}
