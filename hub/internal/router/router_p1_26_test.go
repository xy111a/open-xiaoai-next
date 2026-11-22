package router

// P1-26 回归测试（融合 B：MA 已移除）：用 main 同款 Deps 构造 Router
// 后直连点歌走通且 Snapcast 延迟/切流被调到。

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

// TestMainStyleDepsMusicIntegration：用 main.go 同款 Deps 构造 Router
// （Netease、MPD、Snapcast 全接上，不再经 MA），点歌应走通，
// 且 Snapcast 延迟校准与分组切流都被真实调到。
func TestMainStyleDepsMusicIntegration(t *testing.T) {
	ctx := context.Background()
	deps, _, _, _ := testDeps(t)

	// --- 与 cmd/hub/main.go 同款的接线顺序（融合 B：直连，无 MA）---
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
	// 快路径：点歌成功回空回复；Snapcast 接线断言在下面，不受影响。
	if reply != "" {
		t.Fatalf("main 同款 Deps 下点歌成功应回空回复（快路径），实际 %q", reply)
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

// 直连点歌同样要调 Snapcast：MPD 未接时点歌应明说播放器未配置；
// SetMPD/SetSnap 晚到补接后应走通，延迟/切流仍应执行。
func TestMainStyleDepsMusicDirectAppliesSnapcast(t *testing.T) {
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
	deps.Netease = music.NewNetease(ne.URL)
	deps.SnapLatencies = map[string]int{"卧室": 15} // 按显示名匹配
	deps.SnapGroupID = "g1"
	deps.SnapStreamID = "NAS音乐"
	r := New(deps, nil)

	// 启动时 MPD/Snap 都没接上：点歌应明确报播放器未配置，而不是含糊地落到 Agent。
	if reply, err := r.Route(ctx, "lx06-1", "播放青花瓷"); err != nil || !strings.Contains(reply, "播放器未配置") {
		t.Fatalf("MPD 未接时应报播放器未配置，实际 reply=%q err=%v", reply, err)
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
	// 快路径：补接后点歌成功回空回复；Snapcast 接线断言在下面，不受影响。
	if reply != "" {
		t.Fatalf("补接 MPD 后直连点歌成功应回空回复（快路径），实际 %q", reply)
	}
	if !snapFake.saw("Client.SetLatency") || !snapFake.saw("Group.SetStream") {
		t.Fatalf("直连点歌成功后也应调 Snapcast 延迟/切流，实际收到: %v", snapFake.methods)
	}
}
