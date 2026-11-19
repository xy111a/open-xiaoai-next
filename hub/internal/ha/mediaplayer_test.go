package ha

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// stubHAREST 是假 HA REST 端点：只实现 POST /api/states/<entity_id>，
// 把每次上报的 entity/state/attributes 与 Authorization 头都记下来，
// 供断言“Hub 发了什么”。它不做真 HA 的任何事。
type stubHAREST struct {
	mu       sync.Mutex
	requests []recordedState
	// failFirst 让第一次上报返回 401（测“失败不记为已上报、下次重试”）。
	failFirst bool
	calls     int
	srvURL    string
}

type recordedState struct {
	entityID string
	auth     string
	state    string
	attrs    map[string]any
}

func startStubHAREST(t *testing.T) *stubHAREST {
	s := &stubHAREST{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.calls++
		if s.failFirst && s.calls == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message": "unauthorized"}`))
			return
		}
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/api/states/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var body struct {
			State      string         `json:"state"`
			Attributes map[string]any `json:"attributes"`
		}
		if err := json.Unmarshal(b, &body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.requests = append(s.requests, recordedState{
			entityID: strings.TrimPrefix(r.URL.Path, "/api/states/"),
			auth:     r.Header.Get("Authorization"),
			state:    body.State,
			attrs:    body.Attributes,
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"entity_id":"` + strings.TrimPrefix(r.URL.Path, "/api/states/") + `","state":"` + body.State + `"}`))
	}))
	t.Cleanup(srv.Close)
	s.srvURL = srv.URL
	return s
}

func (s *stubHAREST) url() string { return s.srvURL }

func (s *stubHAREST) lastState(entityID string) (recordedState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.requests) - 1; i >= 0; i-- {
		if s.requests[i].entityID == entityID {
			return s.requests[i], true
		}
	}
	return recordedState{}, false
}

func (s *stubHAREST) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func TestEntityIDSlug(t *testing.T) {
	for deviceID, want := range map[string]string{
		"lx06-192-168-1-20": "media_player.open_xiaoai_lx06_192_168_1_20",
		"OH2P_卧室":           "media_player.open_xiaoai_oh2p",
		"":                  "media_player.open_xiaoai_speaker",
		"lx06..192":         "media_player.open_xiaoai_lx06_192",
	} {
		if got := EntityID(deviceID); got != want {
			t.Fatalf("EntityID(%q) 应为 %q，实际 %q", deviceID, want, got)
		}
	}
}

func TestStateForMapping(t *testing.T) {
	cases := []struct {
		name       string
		sp         SpeakerPlayback
		wantState  string
		wantVolume float64
		hasVolume  bool
	}{
		{"播放在线", SpeakerPlayback{DeviceID: "lx06-1", Online: true, MPDState: "play", Volume: 60}, "playing", 0.6, true},
		{"暂停", SpeakerPlayback{DeviceID: "lx06-1", Online: true, MPDState: "pause", Volume: 60}, "paused", 0.6, true},
		{"停止", SpeakerPlayback{DeviceID: "lx06-1", Online: true, MPDState: "stop", Volume: 60}, "idle", 0.6, true},
		{"MPD 状态未知按 idle", SpeakerPlayback{DeviceID: "lx06-1", Online: true, MPDState: "", Volume: -1}, "idle", 0, false},
		{"离线不报过期 playing", SpeakerPlayback{DeviceID: "lx06-1", Online: false, MPDState: "play", Volume: 60}, "unavailable", 0.6, true},
		{"音量 0 是有效值", SpeakerPlayback{DeviceID: "lx06-1", Online: true, MPDState: "play", Volume: 0}, "playing", 0, true},
		{"音量超限不上报", SpeakerPlayback{DeviceID: "lx06-1", Online: true, MPDState: "play", Volume: 150}, "playing", 0, false},
	}
	for _, c := range cases {
		st := StateFor(c.sp)
		if st.State != c.wantState {
			t.Fatalf("%s：state 应为 %q，实际 %q", c.name, c.wantState, st.State)
		}
		if st.HasVolume != c.hasVolume || (c.hasVolume && st.VolumeLevel != c.wantVolume) {
			t.Fatalf("%s：volume 应为 %v/%v，实际 %v/%v", c.name, c.wantVolume, c.hasVolume, st.VolumeLevel, st.HasVolume)
		}
	}
}

func TestFriendlyName(t *testing.T) {
	if got := FriendlyName("lx06-192-168-1-20", "客厅"); got != "客厅音箱" {
		t.Fatalf("有房间应为“客厅音箱”，实际 %q", got)
	}
	if got := FriendlyName("lx06-192-168-1-20", ""); got != "open-xiaoai lx06-192-168-1-20" {
		t.Fatalf("未认领应带设备 ID，实际 %q", got)
	}
}

func TestRESTBaseURL(t *testing.T) {
	for in, want := range map[string]string{
		"ws://192.168.1.10:8123/api/websocket":  "http://192.168.1.10:8123",
		"wss://192.168.1.10:8123/api/websocket": "https://192.168.1.10:8123",
		"http://192.168.1.10:8123":              "http://192.168.1.10:8123",
	} {
		got, err := RESTBaseURL(in)
		if err != nil {
			t.Fatalf("RESTBaseURL(%q) 不应报错：%v", in, err)
		}
		if got != want {
			t.Fatalf("RESTBaseURL(%q) 应为 %q，实际 %q", in, want, got)
		}
	}
	for _, bad := range []string{"ftp://192.168.1.10:8123", "ws:///api/websocket", ""} {
		if _, err := RESTBaseURL(bad); err == nil {
			t.Fatalf("RESTBaseURL(%q) 应报错", bad)
		}
	}
}

func TestNewReporterEmptyToken(t *testing.T) {
	if _, err := NewReporter("http://192.168.1.10:8123", ""); err == nil {
		t.Fatal("空 token 应拒绝建 Reporter")
	}
	if _, err := NewReporterFromWSURL("ws://192.168.1.10:8123/api/websocket", ""); err == nil {
		t.Fatal("空 token 应拒绝建 Reporter（WS 地址版）")
	}
}

func TestReporterPushAndDedup(t *testing.T) {
	stub := startStubHAREST(t)
	r, err := NewReporter(stub.url(), "hass-token")
	if err != nil {
		t.Fatalf("NewReporter: %v", err)
	}
	ctx := context.Background()
	sp := SpeakerPlayback{DeviceID: "lx06-192-168-1-20", Room: "客厅", Online: true, Volume: 60, MPDState: "play", Title: "青花瓷", Artist: "周杰伦"}

	changed, err := r.Push(ctx, sp)
	if err != nil || !changed {
		t.Fatalf("第一次 Push 应上报成功 changed=true，实际 changed=%v err=%v", changed, err)
	}
	if stub.count() != 1 {
		t.Fatalf("应收到 1 次上报，实际 %d", stub.count())
	}
	rec, ok := stub.lastState("media_player.open_xiaoai_lx06_192_168_1_20")
	if !ok {
		t.Fatal("stub 没收到对应实体的上报")
	}
	if rec.auth != "Bearer hass-token" {
		t.Fatalf("上报须带 Bearer token，实际 %q", rec.auth)
	}
	if rec.state != "playing" {
		t.Fatalf("state 应为 playing，实际 %q", rec.state)
	}
	if rec.attrs["friendly_name"] != "客厅音箱" || rec.attrs["media_title"] != "青花瓷" || rec.attrs["media_artist"] != "周杰伦" {
		t.Fatalf("attributes 不对：%v", rec.attrs)
	}
	if v, _ := rec.attrs["volume_level"].(float64); v != 0.6 {
		t.Fatalf("volume_level 应为 0.6，实际 %v", rec.attrs["volume_level"])
	}
	if v, _ := rec.attrs["supported_features"].(float64); v != 0 {
		t.Fatalf("supported_features 应为 0（只上报不控制），实际 %v", rec.attrs["supported_features"])
	}

	// 完全相同的状态再推一次：去重，不应再发请求。
	changed, err = r.Push(ctx, sp)
	if err != nil || changed {
		t.Fatalf("相同状态第二次 Push 应跳过 changed=false，实际 changed=%v err=%v", changed, err)
	}
	if stub.count() != 1 {
		t.Fatalf("去重后仍应只有 1 次上报，实际 %d", stub.count())
	}

	// 音量变了：必须再推一次，且新音量正确。
	sp.Volume = 80
	changed, err = r.Push(ctx, sp)
	if err != nil || !changed {
		t.Fatalf("音量变化后 Push 应上报，实际 changed=%v err=%v", changed, err)
	}
	if stub.count() != 2 {
		t.Fatalf("音量变化后应有 2 次上报，实际 %d", stub.count())
	}
	rec, _ = stub.lastState("media_player.open_xiaoai_lx06_192_168_1_20")
	if v, _ := rec.attrs["volume_level"].(float64); v != 0.8 {
		t.Fatalf("第二次上报 volume_level 应为 0.8，实际 %v", rec.attrs["volume_level"])
	}
}

func TestReporterPushAllOffline(t *testing.T) {
	stub := startStubHAREST(t)
	r, err := NewReporter(stub.url(), "hass-token")
	if err != nil {
		t.Fatalf("NewReporter: %v", err)
	}
	speakers := BuildPlayback([]DeviceInfo{
		{ID: "lx06-192-168-1-20", Room: "客厅", Model: "lx06", Online: true},
		{ID: "oh2p-192-168-1-21", Room: "卧室", Model: "oh2p", Online: false},
	}, "play", 50, "青花瓷", "周杰伦")
	if err := r.PushAll(context.Background(), speakers); err != nil {
		t.Fatalf("PushAll: %v", err)
	}
	if stub.count() != 2 {
		t.Fatalf("两台音箱应上报 2 个实体，实际 %d", stub.count())
	}
	rec, _ := stub.lastState("media_player.open_xiaoai_oh2p_192_168_1_21")
	if rec.state != "unavailable" {
		t.Fatalf("离线音箱应报 unavailable，实际 %q", rec.state)
	}
	// BuildPlayback 按 ID 排序：lx06 在 oh2p 之前。
	if speakers[0].DeviceID != "lx06-192-168-1-20" || speakers[1].DeviceID != "oh2p-192-168-1-21" {
		t.Fatalf("BuildPlayback 应按设备 ID 排序，实际 %+v", speakers)
	}
}

func TestReporterPushUnauthorizedRetries(t *testing.T) {
	stub := startStubHAREST(t)
	stub.failFirst = true
	r, err := NewReporter(stub.url(), "wrong-token")
	if err != nil {
		t.Fatalf("NewReporter: %v", err)
	}
	sp := SpeakerPlayback{DeviceID: "lx06-1", Online: true, MPDState: "play", Volume: 50}
	if _, err := r.Push(context.Background(), sp); err == nil {
		t.Fatal("HA 返回 401 时 Push 应报错")
	}
	if stub.count() != 0 {
		t.Fatal("401 的上报不应被记为已收到")
	}
	// 下一次同步（同一状态）必须重试，而不是被去重吞掉。
	if _, err := r.Push(context.Background(), sp); err != nil {
		t.Fatalf("重试应成功，实际 %v", err)
	}
	if stub.count() != 1 {
		t.Fatalf("重试后应收到 1 次上报，实际 %d", stub.count())
	}
}

// TestMediaPlayerSyncEndToEnd 是 mock 集成测试：stub HA + 真 Reporter +
// 假音箱（注册表快照）+ 假 MPD 状态，验证“音箱开始/停止放歌 →
// HA 实体状态跟着变”的整条链路（main.go 的同步循环用的就是
// BuildPlayback + PushAll 这两个调用）。
func TestMediaPlayerSyncEndToEnd(t *testing.T) {
	stub := startStubHAREST(t)
	r, err := NewReporter(stub.url(), "hass-token")
	if err != nil {
		t.Fatalf("NewReporter: %v", err)
	}
	ctx := context.Background()
	devices := []DeviceInfo{
		{ID: "lx06-192-168-1-20", Room: "客厅", Model: "lx06", Online: true},
		{ID: "oh2p-192-168-1-21", Room: "卧室", Model: "oh2p", Online: true},
	}
	entity := func(id string) string { return EntityID(id) }

	syncOnce := func(mpdState string, volume int, title, artist string) {
		t.Helper()
		if err := r.PushAll(ctx, BuildPlayback(devices, mpdState, volume, title, artist)); err != nil {
			t.Fatalf("PushAll(%s): %v", mpdState, err)
		}
	}
	want := func(id, state, title string) {
		t.Helper()
		rec, ok := stub.lastState(entity(id))
		if !ok {
			t.Fatalf("实体 %s 未收到上报", entity(id))
		}
		if rec.state != state {
			t.Fatalf("实体 %s state 应为 %q，实际 %q", entity(id), state, rec.state)
		}
		gotTitle, _ := rec.attrs["media_title"].(string)
		if gotTitle != title {
			t.Fatalf("实体 %s media_title 应为 %q，实际 %q", entity(id), title, gotTitle)
		}
	}

	// 1. 初始：MPD stop、无曲 → 两台都 idle、无曲名。
	syncOnce("stop", 50, "", "")
	want("lx06-192-168-1-20", "idle", "")
	want("oh2p-192-168-1-21", "idle", "")

	// 2. 开始放歌：MPD play + 曲名 → 两台都 playing、带曲名。
	syncOnce("play", 60, "青花瓷", "周杰伦")
	want("lx06-192-168-1-20", "playing", "青花瓷")
	want("oh2p-192-168-1-21", "playing", "青花瓷")

	// 3. 暂停：MPD pause → 两台都 paused、曲名保留。
	syncOnce("pause", 60, "青花瓷", "周杰伦")
	want("lx06-192-168-1-20", "paused", "青花瓷")

	// 4. 停止：MPD stop → 两台都 idle、曲名清空。
	syncOnce("stop", 60, "", "")
	want("lx06-192-168-1-20", "idle", "")
	want("oh2p-192-168-1-21", "idle", "")

	// 5. 一台离线：只报 unavailable，不报过期 playing。
	devices[1].Online = false
	syncOnce("play", 60, "青花瓷", "周杰伦")
	want("lx06-192-168-1-20", "playing", "青花瓷")
	want("oh2p-192-168-1-21", "unavailable", "青花瓷")
}
