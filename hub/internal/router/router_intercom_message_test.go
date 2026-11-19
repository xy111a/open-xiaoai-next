package router

// 留言式对讲（“叫一下客厅，吃饭了”）测试：解析表 + 路由分发。
// Speak 用 fake 记录调用（不走真 TTS）；房间/设备关系用 fake 的
// KnownRooms / DevicesByRoom / RoomOfDevice 注入，与生产 main.go
// 里 reg.ByRoomAll / reg.All 的接线语义一致。

import (
	"context"
	"strings"
	"testing"
)

// ---- 解析 ----

func TestParseIntercomMessage(t *testing.T) {
	rooms := []string{"客厅", "卧室", "客厅阳台"}
	cases := []struct {
		text    string
		room    string
		message string
		trigger string
		ok      bool
	}{
		{"叫一下客厅，吃饭了", "客厅", "吃饭了", "叫一下", true},
		{"叫一下客厅：吃饭了", "客厅", "吃饭了", "叫一下", true},
		{"叫一下客厅 吃饭了", "客厅", "吃饭了", "叫一下", true},
		{"叫一下客厅、吃饭了", "客厅", "吃饭了", "叫一下", true},
		{"广播到客厅：开会了", "客厅", "开会了", "广播到", true},
		{"广播到客厅，开会了", "客厅", "开会了", "广播到", true},
		{"告诉卧室，该睡觉了", "卧室", "该睡觉了", "告诉", true},
		// 无分隔：按已知房间最长前缀切分。
		{"告诉卧室该睡觉了", "卧室", "该睡觉了", "告诉", true},
		{"叫一下客厅吃饭了", "客厅", "吃饭了", "叫一下", true},
		{"叫一下客厅阳台吃饭了", "客厅阳台", "吃饭了", "叫一下", true},
		// 房间后缀“音箱”剥掉。
		{"叫一下客厅音箱，吃饭了", "客厅", "吃饭了", "叫一下", true},
		// 消息里可再带分隔符（只按第一个切）。
		{"叫一下客厅，吃饭了，快点", "客厅", "吃饭了，快点", "叫一下", true},

		// 不识别：没消息、没房间、无分隔且房间未知、不是传话。
		{"叫一下客厅", "", "", "", false},
		{"广播到客厅", "", "", "", false},
		{"叫一下客厅，", "", "", "", false},
		{"叫一下，吃饭了", "", "", "", false},
		{"告诉我一个笑话", "", "", "", false},
		{"叫一下小明吃饭了", "", "", "", false},
		{"播放周杰伦的青花瓷", "", "", "", false},
		{"呼叫卧室", "", "", "", false},
		{"全屋广播", "", "", "", false},
		{"", "", "", "", false},
	}
	for _, c := range cases {
		room, msg, trig, ok := ParseIntercomMessage(c.text, rooms)
		if ok != c.ok || room != c.room || msg != c.message || trig != c.trigger {
			t.Errorf("%q 应为 (%q, %q, %q, %v)，实际 (%q, %q, %q, %v)",
				c.text, c.room, c.message, c.trigger, c.ok, room, msg, trig, ok)
		}
	}
}

// 无分隔且 knownRooms 为空时不识别（不靠猜房间名）。
func TestParseIntercomMessageNoKnownRooms(t *testing.T) {
	if _, _, _, ok := ParseIntercomMessage("叫一下客厅吃饭了", nil); ok {
		t.Fatal("无已知房间时，无分隔的“叫一下客厅吃饭了”不应识别")
	}
}

// ---- 路由 ----

// speakCall 是 fake Speak 的一次调用记录。
type speakCall struct {
	deviceID string
	text     string
}

// messageRouter 造一个配好留言式对讲依赖的 Router：
// rooms 是已知房间；byRoom 是房间 → 在线设备；originRooms 是设备 →
// 所在房间；speak 接调用记录（传 nil 表示 Speak 未配置）。
func messageRouter(t *testing.T, rooms []string, byRoom map[string][]string, originRooms map[string]string, speak *[]speakCall) *Router {
	t.Helper()
	deps, _, _, _ := testDeps(t)
	deps.RoomOfDevice = func(deviceID string) string { return originRooms[deviceID] }
	deps.KnownRooms = func() []string { return rooms }
	deps.DevicesByRoom = func(room string) []string { return byRoom[room] }
	if speak != nil {
		deps.Speak = func(deviceID, text string) {
			*speak = append(*speak, speakCall{deviceID, text})
		}
	}
	return New(deps, nil)
}

func TestIntercomMessageDeliversToTargetRoomOnly(t *testing.T) {
	calls := []speakCall{}
	r := messageRouter(t,
		[]string{"客厅", "卧室"},
		map[string][]string{"客厅": {"oh2p-1", "oh2p-2"}, "卧室": {"lx06-1"}},
		map[string]string{"lx06-1": "卧室"},
		&calls)
	reply, err := r.Route(context.Background(), "lx06-1", "叫一下客厅，吃饭了")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "已经告诉客厅") {
		t.Fatalf("发起方应收到“已经告诉客厅”，实际 %q", reply)
	}
	// 客厅两台都播，卧室那台（发起方）一台都不播。
	if len(calls) != 2 {
		t.Fatalf("应播到客厅两台设备，实际 %v", calls)
	}
	for _, c := range calls {
		if c.deviceID != "oh2p-1" && c.deviceID != "oh2p-2" {
			t.Fatalf("不应播到客厅以外的设备，实际 %v", calls)
		}
		if !strings.Contains(c.text, "吃饭了") || !strings.Contains(c.text, "卧室说") {
			t.Fatalf("播报应带发起房间前缀与消息原文，实际 %q", c.text)
		}
	}
}

func TestIntercomMessageBroadcastForm(t *testing.T) {
	calls := []speakCall{}
	r := messageRouter(t,
		[]string{"客厅", "卧室"},
		map[string][]string{"客厅": {"oh2p-1"}, "卧室": {"lx06-1"}},
		map[string]string{"lx06-1": "卧室"},
		&calls)
	reply, err := r.Route(context.Background(), "lx06-1", "广播到客厅：开会了")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "已经告诉客厅") {
		t.Fatalf("广播到客厅应确认已告诉，实际 %q", reply)
	}
	if len(calls) != 1 || calls[0].deviceID != "oh2p-1" || !strings.Contains(calls[0].text, "开会了") {
		t.Fatalf("应只播到客厅 oh2p-1 且带“开会了”，实际 %v", calls)
	}
}

func TestIntercomMessageNoSeparatorKnownRoom(t *testing.T) {
	calls := []speakCall{}
	r := messageRouter(t,
		[]string{"客厅", "卧室"},
		map[string][]string{"客厅": {"oh2p-1"}},
		map[string]string{}, // 发起方房间未知：播报不带前缀
		&calls)
	reply, err := r.Route(context.Background(), "oh2p-9", "告诉客厅吃饭了")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "已经告诉客厅") {
		t.Fatalf("无分隔已知房间应识别并确认，实际 %q", reply)
	}
	if len(calls) != 1 || calls[0].deviceID != "oh2p-1" || calls[0].text != "吃饭了" {
		t.Fatalf("发起方房间未知时应原样播消息，实际 %v", calls)
	}
}

func TestIntercomMessageUnknownRoom(t *testing.T) {
	calls := []speakCall{}
	r := messageRouter(t,
		[]string{"客厅", "卧室"},
		map[string][]string{"客厅": {"oh2p-1"}, "卧室": {"lx06-1"}},
		map[string]string{"lx06-1": "卧室"},
		&calls)
	reply, err := r.Route(context.Background(), "lx06-1", "叫一下书房，吃饭了")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "不知道哪个是书房") {
		t.Fatalf("未知房间应在发起方明说，实际 %q", reply)
	}
	if len(calls) != 0 {
		t.Fatalf("未知房间不应播到任何设备，实际 %v", calls)
	}
}

func TestIntercomMessageRoomKnownButOffline(t *testing.T) {
	calls := []speakCall{}
	r := messageRouter(t,
		[]string{"客厅", "卧室", "书房"}, // 书房已知（注册表里有）但无在线设备
		map[string][]string{"客厅": {"oh2p-1"}, "卧室": {"lx06-1"}},
		map[string]string{"lx06-1": "卧室"},
		&calls)
	reply, err := r.Route(context.Background(), "lx06-1", "叫一下书房，吃饭了")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "书房") || !strings.Contains(reply, "不在线") {
		t.Fatalf("已知但离线应报“不在线”，而不是“不知道哪个是”，实际 %q", reply)
	}
	if len(calls) != 0 {
		t.Fatalf("离线房间不应播到任何设备，实际 %v", calls)
	}
}

func TestIntercomMessageSameRoom(t *testing.T) {
	calls := []speakCall{}
	r := messageRouter(t,
		[]string{"客厅", "卧室"},
		map[string][]string{"客厅": {"oh2p-1"}, "卧室": {"lx06-1"}},
		map[string]string{"lx06-1": "卧室"},
		&calls)
	reply, err := r.Route(context.Background(), "lx06-1", "叫一下卧室，吃饭了")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "直接喊一声") {
		t.Fatalf("同房间应回“直接喊一声”，实际 %q", reply)
	}
	if len(calls) != 0 {
		t.Fatalf("同房间不应向自己播报，实际 %v", calls)
	}
}

func TestIntercomMessageTellMeFallsThrough(t *testing.T) {
	calls := []speakCall{}
	r := messageRouter(t,
		[]string{"客厅", "卧室"},
		map[string][]string{"客厅": {"oh2p-1"}, "卧室": {"lx06-1"}},
		map[string]string{"lx06-1": "卧室"},
		&calls)
	// “告诉我一个笑话”不是传话：不识别，落到无大脑提示，不播报。
	reply, err := r.Route(context.Background(), "lx06-1", "告诉我一个笑话")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(reply, "已经告诉") {
		t.Fatalf("“告诉我……”不应被当成传话，实际 %q", reply)
	}
	if len(calls) != 0 {
		t.Fatalf("“告诉我……”不应播报，实际 %v", calls)
	}
}

func TestIntercomMessageTellUnknownRoomFallsThrough(t *testing.T) {
	calls := []speakCall{}
	r := messageRouter(t,
		[]string{"客厅", "卧室"},
		map[string][]string{"客厅": {"oh2p-1"}, "卧室": {"lx06-1"}},
		map[string]string{"lx06-1": "卧室"},
		&calls)
	// “告诉小明，吃饭了”：告诉 + 未知房间，按歧义规则放行给 Agent。
	reply, err := r.Route(context.Background(), "lx06-1", "告诉小明，吃饭了")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(reply, "已经告诉") || strings.Contains(reply, "不知道哪个是") {
		t.Fatalf("“告诉小明”应放行给 Agent，不认领，实际 %q", reply)
	}
	if len(calls) != 0 {
		t.Fatalf("“告诉小明”不应播报，实际 %v", calls)
	}
}

func TestIntercomMessageIncompletePrompt(t *testing.T) {
	calls := []speakCall{}
	r := messageRouter(t,
		[]string{"客厅", "卧室"},
		map[string][]string{"客厅": {"oh2p-1"}, "卧室": {"lx06-1"}},
		map[string]string{"lx06-1": "卧室"},
		&calls)
	// “叫一下客厅”没说消息：认领并提示正确说法，不掉进实时对讲分支。
	reply, err := r.Route(context.Background(), "lx06-1", "叫一下客厅")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "叫一下客厅，吃饭了") {
		t.Fatalf("没说消息时应提示正确说法，实际 %q", reply)
	}
	if strings.Contains(reply, "找不到") {
		t.Fatalf("不应回实时对讲的“找不到”，实际 %q", reply)
	}
	if len(calls) != 0 {
		t.Fatalf("没说消息时不应播报，实际 %v", calls)
	}
}

func TestIntercomMessageSpeakNotConfigured(t *testing.T) {
	r := messageRouter(t,
		[]string{"客厅", "卧室"},
		map[string][]string{"客厅": {"oh2p-1"}, "卧室": {"lx06-1"}},
		map[string]string{"lx06-1": "卧室"},
		nil) // Speak 未配置
	reply, err := r.Route(context.Background(), "lx06-1", "叫一下客厅，吃饭了")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "播报未配置") {
		t.Fatalf("Speak 未配时应明说播报未配置、不 panic，实际 %q", reply)
	}
}

func TestIntercomMessageDoesNotHijackLiveCall(t *testing.T) {
	deps, _, _, _ := testDeps(t)
	ic := &fakeIntercom{}
	deps.Intercom = ic
	deps.DeviceByRoom = func(room string) (string, bool) {
		if room == "卧室" {
			return "oh2p-1", true
		}
		return "", false
	}
	r := New(deps, nil)
	// “呼叫卧室”仍是实时对讲，不走留言式分支。
	reply, err := r.Route(context.Background(), "lx06-1", "呼叫卧室")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "正在呼叫卧室") {
		t.Fatalf("“呼叫卧室”仍应走实时对讲，实际 %q", reply)
	}
	if len(ic.starts) != 1 {
		t.Fatalf("应调 Start 一次，实际 %v", ic.starts)
	}
}
