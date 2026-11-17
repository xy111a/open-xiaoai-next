package router

// P1-26 回归测试：DefaultQueueID、MA 未就绪明说、用 main 同款 Deps 构造
// Router 后点歌走通且 Snapcast 延迟/切流被调到。

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huajun/open-xiaoai-next/hub/internal/music"
)

// ---- 假 Snapcast（真 TCP + JSON-RPC 行协议，记录收到的调用）----

type fakeSnapP1 struct {
	t       *testing.T
	ln      net.Listener
	mu      sync.Mutex
	methods []string
	params  []map[string]any
}

func startFakeSnapP1(t *testing.T) *fakeSnapP1 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSnapP1{t: t, ln: ln}
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

func (f *fakeSnapP1) handle(conn net.Conn) {
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
		f.mu.Unlock()
		var result any
		switch req.Method {
		case "Server.GetStatus":
			result = map[string]any{"server": map[string]any{"groups": []any{
				map[string]any{"id": "g1", "name": "全屋", "stream_id": "NAS音乐", "muted": false,
					"clients": []any{
						map[string]any{"id": "lx06-1", "connected": true, "config.name": "客厅", "config.latency": 0},
						map[string]any{"id": "oh2p-1", "connected": true, "config.name": "卧室", "config.latency": 15},
					}},
			}}}
		case "Client.SetLatency", "Group.SetStream", "Group.SetMute", "Client.SetVolume":
			result = map[string]any{}
		default:
			_, _ = conn.Write([]byte(fmt.Sprintf(`{"id":%d,"error":{"code":-32601,"message":"unknown method"}}`+"\n", req.ID)))
			continue
		}
		b, _ := json.Marshal(map[string]any{"id": req.ID, "jsonrpc": "2.0", "result": result})
		_, _ = conn.Write(append(b, '\n'))
	}
}

func (f *fakeSnapP1) saw(method string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.methods {
		if m == method {
			return true
		}
	}
	return false
}

func (f *fakeSnapP1) sawParam(method, key string, want any) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, m := range f.methods {
		if m != method {
			continue
		}
		if fmt.Sprint(f.params[i][key]) == fmt.Sprint(want) {
			return true
		}
	}
	return false
}

// ---- 假 MA（players/all + track_by_name + play_media）----

func fakeMAP1(t *testing.T, playersJSON string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch body["command"] {
		case "players/all":
			fmt.Fprintf(w, `{"message_id":1,"result":%s}`, playersJSON)
		case "music/track_by_name":
			fmt.Fprint(w, `{"message_id":2,"result":{"uri":"library://track/12345"}}`)
		case "player_queues/play_media":
			fmt.Fprint(w, `{"message_id":3,"result":{}}`)
		default:
			fmt.Fprint(w, `{"message_id":0,"error":{"code":"unknown","message":"no such command"}}`)
		}
	}))
}

func TestDefaultQueueID(t *testing.T) {
	ctx := context.Background()
	// 有播放器：取第一个。
	srv := fakeMAP1(t, `[{"player_id":"p1","name":"客厅"},{"player_id":"p2","name":"卧室"}]`)
	defer srv.Close()
	q, err := DefaultQueueID(ctx, music.NewMA(srv.URL, ""))
	if err != nil {
		t.Fatalf("DefaultQueueID: %v", err)
	}
	if q != "p1" {
		t.Fatalf("默认 QueueID 应为第一个播放器 p1，实际 %q", q)
	}
	// 一个播放器都没有：明确报 MA 未就绪。
	empty := fakeMAP1(t, `[]`)
	defer empty.Close()
	if _, err := DefaultQueueID(ctx, music.NewMA(empty.URL, "")); err == nil {
		t.Fatal("没有播放器时 DefaultQueueID 应报错")
	} else if !strings.Contains(err.Error(), "MA 未就绪") {
		t.Fatalf("错误应明说 MA 未就绪，实际: %v", err)
	}
	// MA 未配置：同样报 MA 未就绪。
	if _, err := DefaultQueueID(ctx, nil); err == nil || !strings.Contains(err.Error(), "MA 未就绪") {
		t.Fatalf("MA 为 nil 时应报 MA 未就绪，实际: %v", err)
	}
}

func TestMusicMANotReadyMessage(t *testing.T) {
	deps, _, _, _ := testDeps(t)
	// MA 已配（客户端存在）但 QueueID 为空、MPD 也没有：点歌应明确报 MA 未就绪，
	// 而不是含糊的“播放器未配置”或静默走 Agent。
	srv := fakeMAP1(t, `[]`)
	defer srv.Close()
	deps.MA = music.NewMA(srv.URL, "")
	deps.QueueID = ""
	deps.MPD = nil
	deps.Netease = nil
	r := New(deps, nil)
	reply, err := r.Route(context.Background(), "lx06-1", "播放青花瓷")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "MA 未就绪") {
		t.Fatalf("点歌应明确报 MA 未就绪，实际 %q", reply)
	}
}

// TestMainStyleDepsMusicIntegration：用 main.go 同款 Deps 构造 Router
// （MA → Players 取 QueueID、Netease、MPD、Snapcast 全接上），点歌应走通，
// 且 Snapcast 延迟校准与分组切流都被真实调到。
func TestMainStyleDepsMusicIntegration(t *testing.T) {
	ctx := context.Background()
	deps, _, _, _ := testDeps(t)

	// --- 与 cmd/hub/main.go 同款的接线顺序 ---
	maSrv := fakeMAP1(t, `[{"player_id":"p1","name":"客厅"}]`)
	defer maSrv.Close()
	maClient := music.NewMA(maSrv.URL, "")
	queueID, err := DefaultQueueID(ctx, maClient)
	if err != nil {
		t.Fatalf("启动时查 MA 播放器失败: %v", err)
	}
	ne := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/search") {
			fmt.Fprint(w, `{"code":200,"result":{"songs":[{"id":12345,"name":"青花瓷","artists":[{"name":"周杰伦"}]}]}}`)
			return
		}
		fmt.Fprint(w, `{"code":200,"data":[{"id":12345,"url":"http://music.example.com/12345.mp3"}]}`)
	}))
	defer ne.Close()
	mpdFake := startFakeMPD(t)
	snapFake := startFakeSnapP1(t)
	snapClient, err := music.DialSnap(snapFake.ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("DialSnap: %v", err)
	}
	defer snapClient.Close()

	deps.MA = maClient
	deps.QueueID = queueID
	deps.Netease = music.NewNetease(ne.URL)
	deps.MPD = mpdFake.client(t)
	deps.Snap = snapClient
	deps.SnapLatencies = map[string]int{"oh2p-1": 15}
	deps.SnapGroupID = "g1"
	deps.SnapStreamID = "NAS音乐"
	r := New(deps, nil)

	reply, err := r.Route(ctx, "lx06-1", "播放青花瓷")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "正在播放青花瓷") {
		t.Fatalf("main 同款 Deps 下点歌应走通，实际 %q", reply)
	}
	// Snapcast 接线：应调过延迟校准与分组切流。
	deadline := time.Now().Add(2 * time.Second)
	for (!snapFake.saw("Client.SetLatency") || !snapFake.saw("Group.SetStream")) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !snapFake.saw("Client.SetLatency") {
		t.Fatal("点歌成功后应调 SetClientLatency 校准延迟")
	}
	if !snapFake.sawParam("Client.SetLatency", "id", "oh2p-1") || !snapFake.sawParam("Client.SetLatency", "latency", 15) {
		t.Fatalf("应把 oh2p-1 延迟设为 15ms，实际调用: %v %v", snapFake.methods, snapFake.params)
	}
	if !snapFake.sawParam("Group.SetStream", "id", "g1") || !snapFake.sawParam("Group.SetStream", "stream_id", "NAS音乐") {
		t.Fatalf("应把分组 g1 切到流 NAS音乐，实际调用: %v %v", snapFake.methods, snapFake.params)
	}
}

// MPD 降级路径同样要调 Snapcast：MA 无播放器时走网易云 + MPD，点歌成功
// 后延迟/切流仍应执行；且 SetMPD/SetSnap 晚到补接后应生效。
func TestMainStyleDepsMusicFallbackAppliesSnapcast(t *testing.T) {
	ctx := context.Background()
	deps, _, _, _ := testDeps(t)
	ne := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/search") {
			fmt.Fprint(w, `{"code":200,"result":{"songs":[{"id":12345,"name":"青花瓷","artists":[{"name":"周杰伦"}]}]}}`)
			return
		}
		fmt.Fprint(w, `{"code":200,"data":[{"id":12345,"url":"http://music.example.com/12345.mp3"}]}`)
	}))
	defer ne.Close()
	maSrv := fakeMAP1(t, `[]`)
	defer maSrv.Close()
	deps.MA = music.NewMA(maSrv.URL, "")
	deps.QueueID = "" // 启动时 Players() 为空
	deps.Netease = music.NewNetease(ne.URL)
	deps.SnapLatencies = map[string]int{"卧室": 15} // 按显示名匹配
	deps.SnapGroupID = "g1"
	deps.SnapStreamID = "NAS音乐"
	r := New(deps, nil)

	// 启动时 MPD/Snap 都没接上：MA 无播放器且 MPD 为 nil，点歌应明确报 MA 未就绪。
	if reply, err := r.Route(ctx, "lx06-1", "播放青花瓷"); err != nil || !strings.Contains(reply, "MA 未就绪") {
		t.Fatalf("MPD 未接时应报 MA 未就绪，实际 reply=%q err=%v", reply, err)
	}
	// 后台重试晚到补接（main.go 的 retry 行为）：SetMPD/SetSnap 后应走通。
	mpdFake := startFakeMPD(t)
	snapFake := startFakeSnapP1(t)
	snapClient, err := music.DialSnap(snapFake.ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("DialSnap: %v", err)
	}
	defer snapClient.Close()
	r.SetMPD(mpdFake.client(t))
	r.SetSnap(snapClient)

	reply, err := r.Route(ctx, "lx06-1", "播放青花瓷")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "正在播放周杰伦的青花瓷") {
		t.Fatalf("补接 MPD 后降级点歌应走通，实际 %q", reply)
	}
	if !snapFake.saw("Client.SetLatency") || !snapFake.saw("Group.SetStream") {
		t.Fatalf("降级点歌成功后也应调 Snapcast 延迟/切流，实际收到: %v", snapFake.methods)
	}
}
