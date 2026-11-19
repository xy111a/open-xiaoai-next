package music

// 跟随播放（follow-me）测试。
//
// 两层：
//  1. DecideFollowMe 纯函数表 + Mover 的内存假依赖（fmStubMPD /
//     fmStubSnap）：覆盖决策分支与搬家编排，不碰网络。
//  2. TestFollowMeEndToEndFakeServers：真 TCP 假 MPD + 真 TCP 假
//     Snapserver + 真 DialMPD/DialSnap + 真 Mover，
//     断言 Hub 发出的完整命令序列。
//
// 注意：无缝交接（搬家是否零卡顿、位置体感是否连续）这些测试
// 测不出来——假服务器只证明“发对了命令”，真机上的
// Snapcast 切组延迟、MPD→fifo→Snapcast 的缓冲行为只能在
// NAS + 两台真音箱上验证，见本文件末尾的说明。

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- 1. 决策 ----------

func TestDecideFollowMe(t *testing.T) {
	cases := []struct {
		name string
		in   DecideInput
		move bool
	}{
		{"开关关闭不搬", DecideInput{Enabled: false, Playing: true, WakeRoom: "卧室", MusicRooms: []string{"客厅"}, TargetOnline: true}, false},
		{"没在放不搬", DecideInput{Enabled: true, Playing: false, WakeRoom: "卧室", MusicRooms: nil, TargetOnline: true}, false},
		{"同房间独占不搬", DecideInput{Enabled: true, Playing: true, WakeRoom: "卧室", MusicRooms: []string{"卧室"}, TargetOnline: true}, false},
		{"换房间搬", DecideInput{Enabled: true, Playing: true, WakeRoom: "卧室", MusicRooms: []string{"客厅"}, TargetOnline: true}, true},
		{"单分组双房间收窄搬", DecideInput{Enabled: true, Playing: true, WakeRoom: "卧室", MusicRooms: []string{"客厅", "卧室"}, TargetOnline: true}, true},
		{"未认领房间不搬", DecideInput{Enabled: true, Playing: true, WakeRoom: "", MusicRooms: []string{"客厅"}, TargetOnline: true}, false},
		{"目标音箱离线不搬", DecideInput{Enabled: true, Playing: true, WakeRoom: "卧室", MusicRooms: []string{"客厅"}, TargetOnline: false}, false},
		{"直播流也搬", DecideInput{Enabled: true, Playing: true, Live: true, WakeRoom: "卧室", MusicRooms: []string{"客厅"}, TargetOnline: true}, true},
	}
	for _, c := range cases {
		got := DecideFollowMe(c.in)
		if got.Move != c.move {
			t.Errorf("%s：期望 Move=%v，实际 %+v", c.name, c.move, got)
		}
		if got.Reason == "" {
			t.Errorf("%s：Reason 不应为空", c.name)
		}
	}
}

// ---------- 2. Mover（内存假依赖）----------

// fmStubMPD 是内存假 MPD：只实现 Mover 要的两个读方法，
// 记录调用顺序；没有 Play/Stop/Seek 方法——搬家本来就不该调它们。
type fmStubMPD struct {
	mu        sync.Mutex
	calls     []string
	status    MPDStatus
	song      MPDSong
	statusErr error
}

func (f *fmStubMPD) Status() (MPDStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "status")
	return f.status, f.statusErr
}

func (f *fmStubMPD) CurrentSong() (MPDSong, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "currentsong")
	return f.song, nil
}

func (f *fmStubMPD) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// fmStubSnap 是内存假 Snapserver：分组状态可变，
// SetClients/SetGroupStream 真改状态（供 Move 后的验证读到），
// 并记录全部调用。
type fmStubSnap struct {
	mu     sync.Mutex
	groups []SnapGroup
	// clients 是全部已知客户端（id → 完整对象），SetClients 靠它拼分组。
	clients map[string]SnapDevice
	calls   []string
	// setClientsParams 记录每次 SetGroupClients 的参数。
	setClientsParams [][2]any // {groupID, []string}
	setStreamParams  [][2]string
	groupsErr        error
}

func (f *fmStubSnap) GetGroups() ([]SnapGroup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "GetGroups")
	if f.groupsErr != nil {
		return nil, f.groupsErr
	}
	out := make([]SnapGroup, len(f.groups))
	for i, g := range f.groups {
		out[i] = g
		out[i].Clients = append([]SnapDevice(nil), g.Clients...)
	}
	return out, nil
}

func (f *fmStubSnap) SetGroupClients(groupID string, clientIDs []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "SetGroupClients")
	f.setClientsParams = append(f.setClientsParams, [2]any{groupID, append([]string(nil), clientIDs...)})
	// 语义与真服务端一致：入选的客户端从原分组移过来，
	// 落选的若无处可去，进自动 idle 分组（流为空）。
	inSet := map[string]bool{}
	for _, id := range clientIDs {
		inSet[id] = true
	}
	var idle []SnapDevice
	for i := range f.groups {
		var kept []SnapDevice
		for _, c := range f.groups[i].Clients {
			if inSet[c.ID] {
				if f.groups[i].ID == groupID {
					kept = append(kept, c)
				}
				// 在别的分组里的入选客户端：移走（不在旧分组保留）。
				continue
			}
			if f.groups[i].ID == groupID {
				// 从目标分组落选：进 idle。
				idle = append(idle, c)
				continue
			}
			kept = append(kept, c)
		}
		f.groups[i].Clients = kept
	}
	// 目标分组按 clientIDs 顺序重建。
	for i := range f.groups {
		if f.groups[i].ID == groupID {
			var list []SnapDevice
			for _, id := range clientIDs {
				if c, ok := f.clients[id]; ok {
					list = append(list, c)
				}
			}
			f.groups[i].Clients = list
		}
	}
	if len(idle) > 0 {
		f.groups = append(f.groups, SnapGroup{ID: "g-idle", Name: "idle", StreamID: "", Clients: idle})
	}
	return nil
}

func (f *fmStubSnap) SetGroupStream(groupID, streamID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "SetGroupStream")
	f.setStreamParams = append(f.setStreamParams, [2]string{groupID, streamID})
	for i := range f.groups {
		if f.groups[i].ID == groupID {
			f.groups[i].StreamID = streamID
		}
	}
	return nil
}

// fmRooms 是测试用的房间映射：IP → 设备 → 房间。
func fmDeviceOf(ip2device map[string]string) func(SnapDevice) (string, bool) {
	return func(s SnapDevice) (string, bool) {
		id, ok := ip2device[s.Host.IP]
		return id, ok
	}
}

func fmRoomOf(device2room map[string]string) func(string) string {
	return func(deviceID string) string {
		return device2room[deviceID]
	}
}

func fmMover(mpd *fmStubMPD, snap *fmStubSnap, streamID string) (*Mover, *[]string) {
	var spoken []string
	m := NewMover(MoverDeps{
		MPD:          func() MPDReader { return mpd },
		Snap:         func() SnapRouting { return snap },
		StreamID:     streamID,
		RoomOfDevice: fmRoomOf(map[string]string{"lx06-192-168-1-11": "客厅", "oh2p-192-168-1-12": "卧室"}),
		DeviceOfSnapClient: fmDeviceOf(map[string]string{
			"192.168.1.11": "lx06-192-168-1-11",
			"192.168.1.12": "oh2p-192-168-1-12",
		}),
		Enabled: func() bool { return true },
		Speak:   func(deviceID, text string) { spoken = append(spoken, deviceID+":"+text) },
	})
	return m, &spoken
}

func fmPlayingMPD() *fmStubMPD {
	return &fmStubMPD{
		status: MPDStatus{State: "play", Elapsed: 83.5, Duration: 240},
		song:   MPDSong{File: "http://music.example.com/12345.mp3", Title: "青花瓷", Artist: "周杰伦", ID: 5},
	}
}

func fmSnapAB() *fmStubSnap {
	a := SnapDevice{ID: "snap-a", Connected: true, Host: SnapHost{Name: "lx06", IP: "192.168.1.11"}}
	b := SnapDevice{ID: "snap-b", Connected: true, Host: SnapHost{Name: "oh2p", IP: "192.168.1.12"}}
	return &fmStubSnap{
		clients: map[string]SnapDevice{"snap-a": a, "snap-b": b},
		groups: []SnapGroup{
			{ID: "g-music", Name: "音乐", StreamID: "NAS音乐", Clients: []SnapDevice{a, b}},
		},
	}
}

// TestMoverMoveRoomAToB：客厅在放（音乐分组只有客厅音箱），
// 卧室唤醒 → 搬到卧室，客厅停播。
func TestMoverMoveRoomAToB(t *testing.T) {
	mpd := fmPlayingMPD()
	a := SnapDevice{ID: "snap-a", Connected: true, Host: SnapHost{Name: "lx06", IP: "192.168.1.11"}}
	b := SnapDevice{ID: "snap-b", Connected: true, Host: SnapHost{Name: "oh2p", IP: "192.168.1.12"}}
	snap := &fmStubSnap{
		clients: map[string]SnapDevice{"snap-a": a, "snap-b": b},
		groups: []SnapGroup{
			{ID: "g-music", Name: "音乐", StreamID: "NAS音乐", Clients: []SnapDevice{a}},
			{ID: "g-other", Name: "其它", StreamID: "", Clients: []SnapDevice{b}},
		},
	}
	m, _ := fmMover(mpd, snap, "NAS音乐")

	out := m.Move(context.Background(), "oh2p-192-168-1-12")
	if !out.Moved {
		t.Fatalf("应搬家，实际 %+v", out)
	}
	if out.ToRoom != "卧室" || len(out.FromRooms) != 1 || out.FromRooms[0] != "客厅" {
		t.Fatalf("房间不对: %+v", out)
	}
	if out.Song.Title != "青花瓷" || out.Position != 83.5 || out.Live {
		t.Fatalf("快照不对: %+v", out)
	}
	// MPD 只应被查（status + currentsong），绝无播放类命令——
	// 内存假 MPD 根本没有播放方法，这条由类型系统保证，
	// 这里再断言调用顺序。
	if got := mpd.callList(); !reflect.DeepEqual(got, []string{"status", "currentsong"}) {
		t.Fatalf("MPD 应只收到 [status currentsong]，实际 %v", got)
	}
	// Snap 应收到：查分组 → 切分组（只剩卧室）→ 查分组验证。
	snap.mu.Lock()
	defer snap.mu.Unlock()
	if len(snap.setClientsParams) != 1 {
		t.Fatalf("应调 1 次 SetGroupClients，实际 %v", snap.setClientsParams)
	}
	if snap.setClientsParams[0][0] != "g-music" {
		t.Fatalf("SetGroupClients 应作用于 g-music，实际 %v", snap.setClientsParams[0])
	}
	if ids, _ := snap.setClientsParams[0][1].([]string); !reflect.DeepEqual(ids, []string{"snap-b"}) {
		t.Fatalf("SetGroupClients 后分组应只剩 snap-b，实际 %v", ids)
	}
}

// TestMoverSameRoomNoMove：音乐已独占卧室，卧室唤醒 → 什么都不做。
func TestMoverSameRoomNoMove(t *testing.T) {
	mpd := fmPlayingMPD()
	snap := fmSnapAB()
	// 把分组收窄到只剩卧室。
	snap.groups[0].Clients = snap.groups[0].Clients[1:]
	m, _ := fmMover(mpd, snap, "NAS音乐")

	out := m.Move(context.Background(), "oh2p-192-168-1-12")
	if out.Moved {
		t.Fatalf("同房间不应搬家，实际 %+v", out)
	}
	if out.Reason != "音乐已在该房间播放" {
		t.Fatalf("原因不对: %+v", out)
	}
	snap.mu.Lock()
	defer snap.mu.Unlock()
	if len(snap.setClientsParams) != 0 {
		t.Fatalf("同房间不应调 SetGroupClients，实际 %v", snap.setClientsParams)
	}
}

// TestMoverSharedGroupNarrows：默认单分组、两房间都在响，
// 卧室唤醒 → 分组收窄到卧室（客厅停播）。
func TestMoverSharedGroupNarrows(t *testing.T) {
	mpd := fmPlayingMPD()
	snap := fmSnapAB() // 单分组 g-music，snap-a + snap-b
	m, _ := fmMover(mpd, snap, "NAS音乐")

	out := m.Move(context.Background(), "oh2p-192-168-1-12")
	if !out.Moved {
		t.Fatalf("单分组双房间应收窄搬家，实际 %+v", out)
	}
	snap.mu.Lock()
	defer snap.mu.Unlock()
	if len(snap.setClientsParams) != 1 {
		t.Fatalf("应调 1 次 SetGroupClients，实际 %v", snap.setClientsParams)
	}
	if ids, _ := snap.setClientsParams[0][1].([]string); !reflect.DeepEqual(ids, []string{"snap-b"}) {
		t.Fatalf("收窄后分组应只剩 snap-b，实际 %v", ids)
	}
}

// TestMoverUnknownClientStays：音乐分组里有未注册设备（IP 对不上
// 注册表），搬家时不动它，只搬走已知在别的房间的音箱。
func TestMoverUnknownClientStays(t *testing.T) {
	mpd := fmPlayingMPD()
	a := SnapDevice{ID: "snap-a", Connected: true, Host: SnapHost{Name: "lx06", IP: "192.168.1.11"}}
	b := SnapDevice{ID: "snap-b", Connected: true, Host: SnapHost{Name: "oh2p", IP: "192.168.1.12"}}
	unknown := SnapDevice{ID: "snap-x", Connected: true, Host: SnapHost{Name: "phone", IP: "192.168.1.99"}}
	snap := &fmStubSnap{
		clients: map[string]SnapDevice{"snap-a": a, "snap-b": b, "snap-x": unknown},
		groups: []SnapGroup{
			{ID: "g-music", Name: "音乐", StreamID: "NAS音乐", Clients: []SnapDevice{a, unknown}},
			{ID: "g-other", Name: "其它", StreamID: "", Clients: []SnapDevice{b}},
		},
	}
	m, _ := fmMover(mpd, snap, "NAS音乐")
	out := m.Move(context.Background(), "oh2p-192-168-1-12")
	if !out.Moved {
		t.Fatalf("应搬家，实际 %+v", out)
	}
	snap.mu.Lock()
	defer snap.mu.Unlock()
	if len(snap.setClientsParams) != 1 {
		t.Fatalf("应调 1 次 SetGroupClients，实际 %v", snap.setClientsParams)
	}
	if ids, _ := snap.setClientsParams[0][1].([]string); !reflect.DeepEqual(ids, []string{"snap-b", "snap-x"}) {
		t.Fatalf("搬家后分组应为 [snap-b snap-x]（未知设备保留），实际 %v", ids)
	}
}

// TestMoverNoMusicNoMove：MPD 没在放 → 不查 Snapcast、不动路由。
func TestMoverNoMusicNoMove(t *testing.T) {
	mpd := &fmStubMPD{status: MPDStatus{State: "stop"}}
	snap := fmSnapAB()
	m, _ := fmMover(mpd, snap, "NAS音乐")

	out := m.Move(context.Background(), "oh2p-192-168-1-12")
	if out.Moved {
		t.Fatalf("没在放不应搬家，实际 %+v", out)
	}
	snap.mu.Lock()
	defer snap.mu.Unlock()
	if len(snap.calls) != 0 {
		t.Fatalf("没在放不应碰 Snapcast，实际调用 %v", snap.calls)
	}
}

// TestMoverTargetOffline：目标房间音箱离线 → 不搬，
// 且向唤醒设备说一句（Speak 回调），不崩。
func TestMoverTargetOffline(t *testing.T) {
	mpd := fmPlayingMPD()
	snap := fmSnapAB()
	snap.groups[0].Clients[1].Connected = false
	snap.clients["snap-b"] = snap.groups[0].Clients[1]
	m, spoken := fmMover(mpd, snap, "NAS音乐")

	out := m.Move(context.Background(), "oh2p-192-168-1-12")
	if out.Moved {
		t.Fatalf("目标离线不应搬家，实际 %+v", out)
	}
	if out.Reason != "目标房间音箱不在线" {
		t.Fatalf("原因不对: %+v", out)
	}
	if len(*spoken) != 1 || !strings.HasPrefix((*spoken)[0], "oh2p-192-168-1-12:") {
		t.Fatalf("离线时应向唤醒设备说一句，实际 %v", *spoken)
	}
	snap.mu.Lock()
	defer snap.mu.Unlock()
	if len(snap.setClientsParams) != 0 {
		t.Fatalf("目标离线不应调 SetGroupClients，实际 %v", snap.setClientsParams)
	}
}

// TestMoverLiveStreamMovesWithoutSeek：直播流（无总时长）也搬，
// 且不需要 seek——MPD 从不停，流直接续上。
func TestMoverLiveStreamMovesWithoutSeek(t *testing.T) {
	mpd := &fmStubMPD{
		status: MPDStatus{State: "play", Elapsed: 1200, Duration: 0}, // 直播：无 Duration
		song:   MPDSong{File: "http://live.example.com/radio.mp3", Title: "电台直播"},
	}
	snap := fmSnapAB()
	m, _ := fmMover(mpd, snap, "NAS音乐")

	out := m.Move(context.Background(), "oh2p-192-168-1-12")
	if !out.Moved || !out.Live {
		t.Fatalf("直播流应搬家且 Live=true，实际 %+v", out)
	}
	if got := mpd.callList(); !reflect.DeepEqual(got, []string{"status", "currentsong"}) {
		t.Fatalf("直播流也不应有 seek/重播命令，MPD 调用应只有查询，实际 %v", got)
	}
}

// TestMoverNoMusicGroupRoutesTargetGroup：MPD 在放但没分组在播
// 音乐流 → 把目标音箱所在分组切到音乐流。
func TestMoverNoMusicGroupRoutesTargetGroup(t *testing.T) {
	mpd := fmPlayingMPD()
	a := SnapDevice{ID: "snap-a", Connected: true, Host: SnapHost{Name: "lx06", IP: "192.168.1.11"}}
	b := SnapDevice{ID: "snap-b", Connected: true, Host: SnapHost{Name: "oh2p", IP: "192.168.1.12"}}
	snap := &fmStubSnap{
		clients: map[string]SnapDevice{"snap-a": a, "snap-b": b},
		groups: []SnapGroup{
			{ID: "g-a", Name: "客厅组", StreamID: "", Clients: []SnapDevice{a}},
			{ID: "g-b", Name: "卧室组", StreamID: "", Clients: []SnapDevice{b}},
		},
	}
	m, _ := fmMover(mpd, snap, "NAS音乐")

	out := m.Move(context.Background(), "oh2p-192-168-1-12")
	if !out.Moved {
		t.Fatalf("无音乐分组时应把目标分组切到音乐流，实际 %+v", out)
	}
	snap.mu.Lock()
	defer snap.mu.Unlock()
	if len(snap.setStreamParams) != 1 || snap.setStreamParams[0] != [2]string{"g-b", "NAS音乐"} {
		t.Fatalf("应把 g-b 切到 NAS音乐，实际 %v", snap.setStreamParams)
	}
}

// TestMoverDisabled：开关关闭 → 连 MPD 都不查。
func TestMoverDisabled(t *testing.T) {
	mpd := fmPlayingMPD()
	snap := fmSnapAB()
	m := NewMover(MoverDeps{
		MPD: func() MPDReader { return mpd }, Snap: func() SnapRouting { return snap },
		StreamID: "NAS音乐", Enabled: func() bool { return false },
		RoomOfDevice:       fmRoomOf(map[string]string{"oh2p-192-168-1-12": "卧室"}),
		DeviceOfSnapClient: fmDeviceOf(map[string]string{"192.168.1.12": "oh2p-192-168-1-12"}),
	})
	out := m.Move(context.Background(), "oh2p-192-168-1-12")
	if out.Moved || out.Reason != "follow-me 已关闭" {
		t.Fatalf("开关关闭不应搬家，实际 %+v", out)
	}
	if got := mpd.callList(); len(got) != 0 {
		t.Fatalf("开关关闭不应查 MPD，实际 %v", got)
	}
}

// ---------- 3. 真 TCP 假服务器端到端 ----------

// followFakeMPD 是真 TCP 假 MPD：回 canned 状态/当前曲，记录收到的命令。
type followFakeMPD struct {
	t    *testing.T
	ln   net.Listener
	mu   sync.Mutex
	cmds []string
	// 状态可配：state/elapsed/duration/song。
	state     string
	elapsed   float64
	duration  float64
	songFile  string
	songTitle string
}

func startFollowFakeMPD(t *testing.T, state string, elapsed, duration float64) *followFakeMPD {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &followFakeMPD{t: t, ln: ln, state: state, elapsed: elapsed, duration: duration,
		songFile: "http://music.example.com/12345.mp3", songTitle: "青花瓷"}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.handle(conn)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *followFakeMPD) handle(conn net.Conn) {
	defer conn.Close()
	_, _ = conn.Write([]byte("OK MPD 0.23.0\n"))
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimSpace(line)
		f.mu.Lock()
		f.cmds = append(f.cmds, cmd)
		f.mu.Unlock()
		switch cmd {
		case "status":
			dur := ""
			if f.duration > 0 {
				dur = fmt.Sprintf("duration: %.1f\n", f.duration)
			}
			_, _ = conn.Write([]byte(fmt.Sprintf("state: %s\nsong: 0\nsongid: 5\nelapsed: %.1f\n%sOK\n", f.state, f.elapsed, dur)))
		case "currentsong":
			_, _ = conn.Write([]byte(fmt.Sprintf("file: %s\nTitle: %s\nArtist: 周杰伦\nId: 5\nPos: 0\nOK\n", f.songFile, f.songTitle)))
		default:
			_, _ = conn.Write([]byte("OK\n"))
		}
	}
}

func (f *followFakeMPD) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cmds...)
}

// followFakeSnap 是真 TCP 假 Snapserver：分组状态可变，
// SetClients 真改状态（供 Move 后的验证读到），记录方法与参数。
type followFakeSnap struct {
	t       *testing.T
	ln      net.Listener
	mu      sync.Mutex
	methods []string
	params  []map[string]any
	// groups 是当前分组（key 为分组 ID）；clients 是全部客户端。
	groups  map[string]*followSnapGroup
	clients map[string]SnapDevice
}

type followSnapGroup struct {
	id      string
	stream  string
	clients []string // 客户端 ID 列表
}

func startFollowFakeSnap(t *testing.T) *followFakeSnap {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := SnapDevice{ID: "snap-a", Connected: true, Host: SnapHost{Name: "lx06", IP: "192.168.1.11"}}
	b := SnapDevice{ID: "snap-b", Connected: true, Host: SnapHost{Name: "oh2p", IP: "192.168.1.12"}}
	f := &followFakeSnap{
		t: t, ln: ln,
		clients: map[string]SnapDevice{"snap-a": a, "snap-b": b},
		// 任务场景的初始拓扑：音乐只在客厅分组播，
		// 卧室音箱在另一个（无流）分组里。
		groups: map[string]*followSnapGroup{
			"g-music": {id: "g-music", stream: "NAS音乐", clients: []string{"snap-a"}},
			"g-other": {id: "g-other", stream: "", clients: []string{"snap-b"}},
		},
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.handle(conn)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *followFakeSnap) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var req struct {
			ID     int            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(line, &req); err != nil {
			return
		}
		f.mu.Lock()
		f.methods = append(f.methods, req.Method)
		f.params = append(f.params, req.Params)
		var result any
		switch req.Method {
		case "Server.GetStatus":
			result = f.statusLocked()
		case "Group.SetClients":
			result = map[string]any{}
			f.applySetClientsLocked(req.Params)
		case "Group.SetStream":
			result = map[string]any{}
			if id, _ := req.Params["id"].(string); id != "" {
				if g, ok := f.groups[id]; ok {
					g.stream, _ = req.Params["stream_id"].(string)
				}
			}
		case "Client.SetLatency", "Group.SetMute", "Client.SetVolume":
			result = map[string]any{}
		default:
			f.mu.Unlock()
			_, _ = conn.Write([]byte(fmt.Sprintf(`{"id":%d,"error":{"code":-32601,"message":"unknown method"}}`+"\n", req.ID)))
			continue
		}
		f.mu.Unlock()
		b, _ := json.Marshal(map[string]any{"id": req.ID, "jsonrpc": "2.0", "result": result})
		_, _ = conn.Write(append(b, '\n'))
	}
}

// statusLocked 按真服务端形状拼 Server.GetStatus（调用方须持锁）。
func (f *followFakeSnap) statusLocked() any {
	var groups []any
	ids := make([]string, 0, len(f.groups))
	for id := range f.groups {
		ids = append(ids, id)
	}
	// 确定性顺序：按 ID 排序。
	for i := 0; i < len(ids); i++ {
		for j := i + 1; j < len(ids); j++ {
			if ids[j] < ids[i] {
				ids[i], ids[j] = ids[j], ids[i]
			}
		}
	}
	for _, id := range ids {
		g := f.groups[id]
		var clients []any
		for _, cid := range g.clients {
			c := f.clients[cid]
			clients = append(clients, map[string]any{
				"id": c.ID, "connected": c.Connected,
				"host": map[string]any{"name": c.Host.Name, "ip": c.Host.IP, "mac": ""},
			})
		}
		if clients == nil {
			clients = []any{}
		}
		groups = append(groups, map[string]any{
			"id": id, "name": id, "stream_id": g.stream, "muted": false, "clients": clients,
		})
	}
	if groups == nil {
		groups = []any{}
	}
	return map[string]any{"server": map[string]any{"groups": groups}}
}

// applySetClientsLocked 按真服务端语义改状态（调用方须持锁）：
// 入选的进目标分组、其余分组里移除；落选的进自动 idle 分组。
func (f *followFakeSnap) applySetClientsLocked(params map[string]any) {
	id, _ := params["id"].(string)
	raw, _ := params["clients"].([]any)
	var want []string
	for _, v := range raw {
		if s, ok := v.(string); ok {
			want = append(want, s)
		}
	}
	inSet := map[string]bool{}
	for _, cid := range want {
		inSet[cid] = true
	}
	var idle []string
	for _, g := range f.groups {
		if g.id == id {
			continue
		}
		var kept []string
		for _, cid := range g.clients {
			if inSet[cid] {
				continue // 移到目标分组
			}
			kept = append(kept, cid)
		}
		g.clients = kept
	}
	if g, ok := f.groups[id]; ok {
		for _, cid := range g.clients {
			if !inSet[cid] {
				idle = append(idle, cid)
			}
		}
		g.clients = append([]string(nil), want...)
	}
	if len(idle) > 0 {
		f.groups["g-idle"] = &followSnapGroup{id: "g-idle", stream: "", clients: idle}
	}
}

func (f *followFakeSnap) methodList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.methods...)
}

// setClientsCall 返回第 n 次 Group.SetClients 的参数（groupID, clientIDs）。
func (f *followFakeSnap) setClientsCall(n int) (string, []string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := -1
	for i, m := range f.methods {
		if m != "Group.SetClients" {
			continue
		}
		seen++
		if seen != n {
			continue
		}
		id, _ := f.params[i]["id"].(string)
		var ids []string
		if raw, ok := f.params[i]["clients"].([]any); ok {
			for _, v := range raw {
				if s, ok := v.(string); ok {
					ids = append(ids, s)
				}
			}
		}
		return id, ids, true
	}
	return "", nil, false
}

// TestSnapSetGroupClients：新方法本身走通（真 TCP + 真客户端）。
func TestSnapSetGroupClients(t *testing.T) {
	f := startFollowFakeSnap(t)
	c, err := DialSnap(f.ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("DialSnap: %v", err)
	}
	defer c.Close()
	if err := c.SetGroupClients("g-music", []string{"snap-b"}); err != nil {
		t.Fatalf("SetGroupClients: %v", err)
	}
	if id, ids, ok := f.setClientsCall(0); !ok || id != "g-music" || !reflect.DeepEqual(ids, []string{"snap-b"}) {
		t.Fatalf("服务端应收到 Group.SetClients(g-music, [snap-b])，实际 id=%q ids=%v ok=%v", id, ids, ok)
	}
}

// TestFollowMeEndToEndFakeServers：
// 假 MPD（在放青花瓷，83.5s/240s）+ 假 Snapserver（音乐只在客厅分组，
// 卧室音箱在另一分组）+ 卧室唤醒 → Hub 应发出：
// MPD: status, currentsong（仅此而已，无 clear/add/play/stop/seek）；
// Snap: GetStatus → SetClients(g-music, [snap-b]) → GetStatus（验证）。
func TestFollowMeEndToEndFakeServers(t *testing.T) {
	mpdFake := startFollowFakeMPD(t, "play", 83.5, 240)
	snapFake := startFollowFakeSnap(t)
	mpd, err := DialMPD(mpdFake.ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("DialMPD: %v", err)
	}
	defer mpd.Close()
	snap, err := DialSnap(snapFake.ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("DialSnap: %v", err)
	}
	defer snap.Close()

	m := NewMover(MoverDeps{
		MPD:          func() MPDReader { return mpd },
		Snap:         func() SnapRouting { return snap },
		StreamID:     "NAS音乐",
		RoomOfDevice: fmRoomOf(map[string]string{"lx06-192-168-1-11": "客厅", "oh2p-192-168-1-12": "卧室"}),
		DeviceOfSnapClient: fmDeviceOf(map[string]string{
			"192.168.1.11": "lx06-192-168-1-11",
			"192.168.1.12": "oh2p-192-168-1-12",
		}),
		Enabled: func() bool { return true },
	})

	out := m.Move(context.Background(), "oh2p-192-168-1-12")
	if !out.Moved {
		t.Fatalf("端到端应搬家，实际 %+v", out)
	}
	if out.Song.Title != "青花瓷" || out.Position != 83.5 || out.Duration != 240 {
		t.Fatalf("快照不对: %+v", out)
	}
	if got := mpdFake.commands(); !reflect.DeepEqual(got, []string{"status", "currentsong"}) {
		t.Fatalf("MPD 应只收到 [status currentsong]，实际 %v", got)
	}
	wantMethods := []string{"Server.GetStatus", "Group.SetClients", "Server.GetStatus"}
	if got := snapFake.methodList(); !reflect.DeepEqual(got, wantMethods) {
		t.Fatalf("Snapcast 方法序列应为 %v，实际 %v", wantMethods, got)
	}
	if id, ids, ok := snapFake.setClientsCall(0); !ok || id != "g-music" || !reflect.DeepEqual(ids, []string{"snap-b"}) {
		t.Fatalf("应把 g-music 切到只剩 snap-b（卧室），实际 id=%q ids=%v ok=%v", id, ids, ok)
	}
	// 终态：音乐分组只剩卧室音箱。
	groups, err := snap.GetGroups()
	if err != nil {
		t.Fatalf("终态 GetGroups: %v", err)
	}
	for _, g := range groups {
		if g.StreamID != "NAS音乐" {
			continue
		}
		if len(g.Clients) != 1 || g.Clients[0].ID != "snap-b" {
			t.Fatalf("终态音乐分组应只剩 snap-b，实际 %+v", g.Clients)
		}
	}
}

// TestFollowMeEndToEndLiveStream：直播流（无 duration）端到端也搬，
// 且同样不发任何 seek/重播命令。
func TestFollowMeEndToEndLiveStream(t *testing.T) {
	mpdFake := startFollowFakeMPD(t, "play", 1200, 0) // duration 0 = 直播
	snapFake := startFollowFakeSnap(t)
	mpd, err := DialMPD(mpdFake.ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("DialMPD: %v", err)
	}
	defer mpd.Close()
	snap, err := DialSnap(snapFake.ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("DialSnap: %v", err)
	}
	defer snap.Close()

	m := NewMover(MoverDeps{
		MPD:          func() MPDReader { return mpd },
		Snap:         func() SnapRouting { return snap },
		StreamID:     "NAS音乐",
		RoomOfDevice: fmRoomOf(map[string]string{"lx06-192-168-1-11": "客厅", "oh2p-192-168-1-12": "卧室"}),
		DeviceOfSnapClient: fmDeviceOf(map[string]string{
			"192.168.1.11": "lx06-192-168-1-11",
			"192.168.1.12": "oh2p-192-168-1-12",
		}),
		Enabled: func() bool { return true },
	})
	out := m.Move(context.Background(), "oh2p-192-168-1-12")
	if !out.Moved || !out.Live {
		t.Fatalf("直播流端到端应搬家且 Live=true，实际 %+v", out)
	}
	if got := mpdFake.commands(); !reflect.DeepEqual(got, []string{"status", "currentsong"}) {
		t.Fatalf("直播流 MPD 应只收到 [status currentsong]，实际 %v", got)
	}
}

// ---- 真机验证清单（这些测试测不出来，须在 NAS + 两台真音箱上做）----
//
//  - 搬家是否无缝：卧室唤醒后，音乐是否在 1–2s 内于卧室续上、
//    客厅停下，且没有可感知的卡顿/爆音/从头重播。
//    风险点：Snapcast 切组后客户端重同步的缓冲；MPD→fifo→Snapcast
//    本来就有约 1–2s 的管道延迟，搬家瞬间卧室可能先静一拍再续上。
//  - 位置是否体感连续：同一首歌在卧室续播的位置，与客厅停下时
//    的位置是否在 2s 以内（MPD 时间线本身没断，理论上是连续的，
//    但切组瞬间的缓冲可能吃掉或重复一小段）。
//  - SetClients 的安置语义：被移出音乐分组的客厅音箱，真服务端
//    是不是如文档所说另行安置进新分组；若真服务端把它踢下线
//    或留在无流分组里发呆，下次唤醒回客厅时要能再搬回来。
//  - host.ip 映射：真 Snapserver 上报的客户端 IP 是否与注册表
//    设备 IP 一致（NAT/多网卡/IPv6 时可能对不上）；对不上时
//    跟随会报“目标房间音箱不在线”，需在真机上核对一次。
//  - 直播流：真电台流在切组后是否不断流重连（不断即无缝）。
