// 留言式对讲 mock 集成测试：两台假音箱（客厅 lx06、卧室 oh2p）连上
// 真 transport 服务；一条假 STT 文本“叫一下客厅，吃饭了”经真 Router
// 路由后，客厅那台应收到 start_play RPC + 一帧 Opus + stop_play，
// 卧室那台在 300ms 内应收不到任何消息。
//
// Speak 在这里是测试版的 playFrames（main.go 里 h.speakText 的同款
// 播放协议：start_play → Opus 帧 → stop_play），只省掉真 TTS 合成
// 与 Opus 编码——被测的是“路由选对设备 + 发对协议”，不是 Piper 本身。
package hub_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/huajun/open-xiaoai-next/hub/internal/registry"
	"github.com/huajun/open-xiaoai-next/hub/internal/router"
	"github.com/huajun/open-xiaoai-next/hub/internal/transport"
)

// noopHandler 是 transport.Handler 的空实现（本测试不走唤醒/音频管线）。
type noopHandler struct{}

func (noopHandler) OnWake(string, transport.Wake)           {}
func (noopHandler) OnAudioStart(string)                     {}
func (noopHandler) OnAudioFrame(string, []byte)             {}
func (noopHandler) OnAudioEnd(string)                       {}
func (noopHandler) OnRoomClaim(string, string)              {}
func (noopHandler) OnStatus(string, transport.Status)       {}
func (noopHandler) OnRPCResult(string, transport.RPCResult) {}
func (noopHandler) OnDisconnect(string)                     {}

// dialRoomClient 连上 Hub 并用带 room 的 hello 完成握手，返回连接
// （调用方负责 Close）。hello 里的 room 经 FillRoomIfEmpty 进注册表。
func dialRoomClient(t *testing.T, url, model, room, token string) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("Dial %s: %v", url, err)
	}
	if err := c.WriteJSON(map[string]any{
		"type": "hello", "model": model, "ram_mb": 256,
		"version": "test", "token": token, "room": room,
	}); err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := c.ReadJSON(&resp); err != nil {
		t.Fatalf("读 hello 响应: %v", err)
	}
	if resp["type"] != "hello_ok" {
		t.Fatalf("hello 应 ok，实际 %v", resp)
	}
	return c
}

func TestIntercomMessageRoutesToTargetRoomOnly(t *testing.T) {
	reg := registry.New()
	srv := transport.New("secret", reg, noopHandler{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { _ = srv.Serve(ln, nil) }()
	url := "ws://" + ln.Addr().String() + "/ws"

	// 两台假音箱：lx06 在客厅，oh2p 在卧室（同 IP 不同型号 → 不同设备 ID）。
	living := dialRoomClient(t, url, "lx06", "客厅", "secret")
	defer living.Close()
	bedroom := dialRoomClient(t, url, "oh2p", "卧室", "secret")
	defer bedroom.Close()

	livingID := transport.DeviceIDFor("lx06", "127.0.0.1")
	bedroomID := transport.DeviceIDFor("oh2p", "127.0.0.1")
	if d, ok := reg.Get(livingID); !ok || d.Room != "客厅" || !d.Online {
		t.Fatalf("客厅设备应已注册在线，实际 %+v (ok=%v)", d, ok)
	}
	if d, ok := reg.Get(bedroomID); !ok || d.Room != "卧室" || !d.Online {
		t.Fatalf("卧室设备应已注册在线，实际 %+v (ok=%v)", d, ok)
	}

	// 真 Router + main.go 同款接线（DevicesByRoom/KnownRooms 读注册表，
	// Speak 走 start_play → Opus 帧 → stop_play 的播放协议）。
	rt := router.New(router.Deps{
		RoomOfDevice: func(deviceID string) string {
			if d, ok := reg.Get(deviceID); ok {
				return d.Room
			}
			return ""
		},
		DevicesByRoom: func(room string) []string {
			var ids []string
			for _, d := range reg.ByRoomAll(room) {
				ids = append(ids, d.ID)
			}
			return ids
		},
		KnownRooms: func() []string {
			seen := map[string]bool{}
			var rooms []string
			for _, d := range reg.All() {
				if d.Room != "" && !seen[d.Room] {
					seen[d.Room] = true
					rooms = append(rooms, d.Room)
				}
			}
			return rooms
		},
		Speak: func(deviceID, text string) {
			c, ok := srv.Conn(deviceID)
			if !ok {
				t.Errorf("Speak：设备 %s 不在线", deviceID)
				return
			}
			// 测试版 playFrames：真 TTS/Opus 编码由 Piper 负责，
			// 这里只发一帧占位 Opus，验证协议与目标选择。
			_ = c.SendRPC("play-test", "start_play", nil)
			_ = c.SendBinary([]byte{0xde, 0xad})
			_ = c.SendRPC("stop-test", "stop_play", nil)
		},
	}, nil)

	// 假 STT 文本：卧室的音箱听到用户说“叫一下客厅，吃饭了”。
	reply, err := rt.Route(context.Background(), bedroomID, "叫一下客厅，吃饭了")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "已经告诉客厅") {
		t.Fatalf("发起方应收到“已经告诉客厅”，实际 %q", reply)
	}

	// 客厅那台应按顺序收到 start_play RPC、一帧 Opus、stop_play RPC。
	_ = living.SetReadDeadline(time.Now().Add(3 * time.Second))
	mt, data, err := living.ReadMessage()
	if err != nil {
		t.Fatalf("客厅读 start_play: %v", err)
	}
	if mt != websocket.TextMessage || !strings.Contains(string(data), "start_play") {
		t.Fatalf("客厅第一条应是 start_play RPC，实际 %s", data)
	}
	mt, data, err = living.ReadMessage()
	if err != nil {
		t.Fatalf("客厅读 Opus 帧: %v", err)
	}
	if mt != websocket.BinaryMessage || len(data) < 1 || data[0] != 0x01 {
		t.Fatalf("客厅第二条应是 0x01 开头的 Opus 帧，实际 mt=%d len=%d", mt, len(data))
	}
	mt, data, err = living.ReadMessage()
	if err != nil {
		t.Fatalf("客厅读 stop_play: %v", err)
	}
	if mt != websocket.TextMessage || !strings.Contains(string(data), "stop_play") {
		t.Fatalf("客厅第三条应是 stop_play RPC，实际 %s", data)
	}

	// 卧室那台在 300ms 内应收不到任何消息（没误播到发起方）。
	_ = bedroom.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, _, err := bedroom.ReadMessage(); err == nil {
		t.Fatal("卧室那台不应收到任何消息，却收到了一条")
	} else {
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatalf("卧室那台应是读超时（无消息），实际错误 %v", err)
		}
	}
}
