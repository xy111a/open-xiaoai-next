package router

// 跟随播放（follow-me）的 Router 层测试：
// New() 是否把 Mover 接好、FollowMeOnWake 是否一路调通真 MPD/Snap 假服务器，
// 以及看门狗晚到补接（SetMPD/SetSnap）后跟随是否自动跟上。

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

	"github.com/huajun/open-xiaoai-next/hub/internal/music"
)

// ---- 真 TCP 假 Snapserver（带 host IP + 可变分组，支持 Group.SetClients）----

type fmSnapServer struct {
	t       *testing.T
	ln      net.Listener
	mu      sync.Mutex
	methods []string
	params  []map[string]any
	groups  map[string]*fmSnapGroup
	clients map[string]music.SnapDevice
}

type fmSnapGroup struct {
	id      string
	stream  string
	clients []string
}

func startFMSnapServer(t *testing.T) *fmSnapServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := music.SnapDevice{ID: "snap-a", Connected: true, Host: music.SnapHost{Name: "lx06", IP: "192.168.1.11"}}
	b := music.SnapDevice{ID: "snap-b", Connected: true, Host: music.SnapHost{Name: "oh2p", IP: "192.168.1.12"}}
	f := &fmSnapServer{
		t: t, ln: ln,
		clients: map[string]music.SnapDevice{"snap-a": a, "snap-b": b},
		// 任务场景的初始拓扑：音乐只在客厅分组播，
		// 卧室音箱在另一个（无流）分组里。
		groups: map[string]*fmSnapGroup{
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

func (f *fmSnapServer) handle(conn net.Conn) {
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

func (f *fmSnapServer) statusLocked() any {
	var groups []any
	ids := make([]string, 0, len(f.groups))
	for id := range f.groups {
		ids = append(ids, id)
	}
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

func (f *fmSnapServer) applySetClientsLocked(params map[string]any) {
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
				continue
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
		f.groups["g-idle"] = &fmSnapGroup{id: "g-idle", stream: "", clients: idle}
	}
}

func (f *fmSnapServer) setClientsCalls() [][2]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][2]any
	for i, m := range f.methods {
		if m != "Group.SetClients" {
			continue
		}
		var ids []string
		if raw, ok := f.params[i]["clients"].([]any); ok {
			for _, v := range raw {
				if s, ok := v.(string); ok {
					ids = append(ids, s)
				}
			}
		}
		id, _ := f.params[i]["id"].(string)
		out = append(out, [2]any{id, ids})
	}
	return out
}

// fmRoomOfDevice 是测试用的房间映射（与 music 包测试同款数据）。
func fmRoomOfDevice(deviceID string) string {
	switch deviceID {
	case "lx06-192-168-1-11":
		return "客厅"
	case "oh2p-192-168-1-12":
		return "卧室"
	}
	return ""
}

func fmSnapClientDevice(snap music.SnapDevice) (string, bool) {
	switch snap.Host.IP {
	case "192.168.1.11":
		return "lx06-192-168-1-11", true
	case "192.168.1.12":
		return "oh2p-192-168-1-12", true
	}
	return "", false
}

// TestFollowMeOnWakeNilDeps：Deps 全空也不崩，只报未连接。
func TestFollowMeOnWakeNilDeps(t *testing.T) {
	r := New(Deps{}, nil)
	out := r.FollowMeOnWake(context.Background(), "oh2p-192-168-1-12")
	if out.Moved {
		t.Fatalf("空 Deps 不应搬家，实际 %+v", out)
	}
	if out.Reason != "MPD 未连接" {
		t.Fatalf("空 Deps 原因应为 MPD 未连接，实际 %+v", out)
	}
}

// TestFollowMeOnWakeMovesMusic：Router 层端到端——
// 假 MPD 在放（青花瓷 12.5s/240s）+ 假 Snapserver（客厅+卧室同组），
// 卧室唤醒 → 音乐跟到卧室；MPD 只收到查询命令。
// 另覆盖看门狗晚到补接：先不接 Snap，报未连接；SetSnap 后再调即走通。
func TestFollowMeOnWakeMovesMusic(t *testing.T) {
	ctx := context.Background()
	deps, _, _, _ := testDeps(t)
	deps.RoomOfDevice = fmRoomOfDevice
	deps.SnapClientDevice = fmSnapClientDevice
	deps.FollowMeEnabled = func() bool { return true }
	deps.SnapStreamID = "NAS音乐"
	mpdFake := startFakeMPD(t)
	snapFake := startFMSnapServer(t)
	snapClient, err := music.DialSnap(snapFake.ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("DialSnap: %v", err)
	}
	defer snapClient.Close()
	deps.MPD = mpdFake.client(t)
	// 先不接 Snap：应报 Snapcast 未连接（看门狗晚到场景）。
	r := New(deps, nil)
	out := r.FollowMeOnWake(ctx, "oh2p-192-168-1-12")
	if out.Moved || out.Reason != "Snapcast 未连接" {
		t.Fatalf("Snap 未接时应报 Snapcast 未连接，实际 %+v", out)
	}
	// 晚到补接后应走通（Mover 经提供方取最新客户端）。
	r.SetSnap(snapClient)
	out = r.FollowMeOnWake(ctx, "oh2p-192-168-1-12")
	if !out.Moved {
		t.Fatalf("补接后卧室唤醒应搬家，实际 %+v", out)
	}
	if out.ToRoom != "卧室" || len(out.FromRooms) != 1 || out.FromRooms[0] != "客厅" {
		t.Fatalf("房间不对: %+v", out)
	}
	if out.Song.Title != "青花瓷" || out.Position != 12.5 {
		t.Fatalf("快照不对: %+v", out)
	}
	if got := mpdFake.commands(); !reflect.DeepEqual(got, []string{"status", "currentsong"}) {
		t.Fatalf("MPD 应只收到 [status currentsong]，实际 %v", got)
	}
	calls := snapFake.setClientsCalls()
	if len(calls) != 1 || calls[0][0] != "g-music" {
		t.Fatalf("应调 1 次 SetClients(g-music, …)，实际 %v", calls)
	}
	if ids, _ := calls[0][1].([]string); !reflect.DeepEqual(ids, []string{"snap-b"}) {
		t.Fatalf("切组后应只剩 snap-b（卧室），实际 %v", ids)
	}
}

// TestFollowMeOnWakeSameRoom：音乐已在卧室独占时不搬。
func TestFollowMeOnWakeSameRoom(t *testing.T) {
	ctx := context.Background()
	deps, _, _, _ := testDeps(t)
	deps.RoomOfDevice = fmRoomOfDevice
	deps.SnapClientDevice = fmSnapClientDevice
	deps.FollowMeEnabled = func() bool { return true }
	deps.SnapStreamID = "NAS音乐"
	mpdFake := startFakeMPD(t)
	snapFake := startFMSnapServer(t)
	// 预先收窄：音乐分组只剩卧室。
	snapFake.mu.Lock()
	snapFake.groups["g-music"].clients = []string{"snap-b"}
	snapFake.mu.Unlock()
	snapClient, err := music.DialSnap(snapFake.ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("DialSnap: %v", err)
	}
	defer snapClient.Close()
	deps.MPD = mpdFake.client(t)
	deps.Snap = snapClient
	r := New(deps, nil)
	out := r.FollowMeOnWake(ctx, "oh2p-192-168-1-12")
	if out.Moved {
		t.Fatalf("同房间不应搬家，实际 %+v", out)
	}
	if !strings.Contains(out.Reason, "已在该房间") {
		t.Fatalf("原因应说明已在该房间，实际 %+v", out)
	}
	if calls := snapFake.setClientsCalls(); len(calls) != 0 {
		t.Fatalf("同房间不应调 SetClients，实际 %v", calls)
	}
}
