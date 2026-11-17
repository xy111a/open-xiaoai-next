package music

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
)

// ---------- Snapcast 假服务器（真 TCP + JSON-RPC 行协议）----------

type fakeSnap struct {
	t       *testing.T
	ln      net.Listener
	mu      sync.Mutex
	methods []string
}

func startFakeSnap(t *testing.T) *fakeSnap {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSnap{t: t, ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.handle(conn)
		}
	}()
	return f
}

func (f *fakeSnap) handle(conn net.Conn) {
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
		f.mu.Unlock()
		var result any
		switch req.Method {
		case "Server.GetStatus":
			result = map[string]any{"server": map[string]any{"groups": []any{
				map[string]any{"id": "g1", "name": "全屋", "stream_id": "s1", "muted": false,
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

func TestSnapcastGetGroupsAndLatency(t *testing.T) {
	f := startFakeSnap(t)
	defer f.ln.Close()
	c, err := DialSnap(f.ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("DialSnap: %v", err)
	}
	defer c.Close()

	groups, err := c.GetGroups()
	if err != nil {
		t.Fatalf("GetGroups: %v", err)
	}
	if len(groups) != 1 || groups[0].ID != "g1" || len(groups[0].Clients) != 2 {
		t.Fatalf("GetGroups 应返回 1 组 2 台，实际 %+v", groups)
	}
	if groups[0].Clients[1].Latency != 15 {
		t.Fatalf("卧室延迟应为 15ms，实际 %d", groups[0].Clients[1].Latency)
	}
	if err := c.SetClientLatency("oh2p-1", 25); err != nil {
		t.Fatalf("SetClientLatency: %v", err)
	}
	if err := c.SetGroupStream("g1", "s2"); err != nil {
		t.Fatalf("SetGroupStream: %v", err)
	}
	if err := c.SetGroupMute("g1", true); err != nil {
		t.Fatalf("SetGroupMute: %v", err)
	}
	if err := c.SetClientVolume("lx06-1", 50); err != nil {
		t.Fatalf("SetClientVolume: %v", err)
	}
	f.mu.Lock()
	got := strings.Join(f.methods, ",")
	f.mu.Unlock()
	for _, m := range []string{"Server.GetStatus", "Client.SetLatency", "Group.SetStream", "Group.SetMute", "Client.SetVolume"} {
		if !strings.Contains(got, m) {
			t.Fatalf("服务端未收到 %s，实际收到: %s", m, got)
		}
	}
}

// ---------- NetEase 假 API ----------

func fakeNetease(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/search"):
			if r.URL.Query().Get("keywords") == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			fmt.Fprint(w, `{"code":200,"result":{"songs":[{"id":12345,"name":"青花瓷","artists":[{"name":"周杰伦"}]}]}}`)
		case strings.HasPrefix(r.URL.Path, "/song/url/v1"):
			fmt.Fprint(w, `{"code":200,"data":[{"id":12345,"url":"http://music.example.com/12345.mp3"}]}`)
		case strings.HasPrefix(r.URL.Path, "/lyric"):
			fmt.Fprint(w, `{"code":200,"lrc":{"lyric":"[00:00.00]素胚勾勒出青花"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestNeteaseSearchURLLyric(t *testing.T) {
	srv := fakeNetease(t)
	defer srv.Close()
	c := NewNetease(srv.URL)
	ctx := context.Background()

	songs, err := c.Search(ctx, "青花瓷", 1)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(songs) != 1 || songs[0].Name != "青花瓷" || songs[0].Artist != "周杰伦" || songs[0].ID != 12345 {
		t.Fatalf("Search 结果不对: %+v", songs)
	}
	u, err := c.SongURL(ctx, 12345)
	if err != nil {
		t.Fatalf("SongURL: %v", err)
	}
	if u != "http://music.example.com/12345.mp3" {
		t.Fatalf("SongURL 不对: %q", u)
	}
	lyric, err := c.Lyric(ctx, 12345)
	if err != nil {
		t.Fatalf("Lyric: %v", err)
	}
	if !strings.Contains(lyric, "素胚勾勒出青花") {
		t.Fatalf("Lyric 不对: %q", lyric)
	}
}

// ---------- Music Assistant 假 API ----------

func fakeMA(t *testing.T) (*httptest.Server, *[]string) {
	var got []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer ma-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		cmd, _ := body["command"].(string)
		mu.Lock()
		got = append(got, cmd)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch cmd {
		case "players/all":
			fmt.Fprint(w, `{"message_id":1,"result":[{"player_id":"p1","name":"客厅"}]}`)
		case "music/track_by_name":
			fmt.Fprint(w, `{"message_id":2,"result":{"uri":"library://track/12345"}}`)
		case "player_queues/play_media":
			fmt.Fprint(w, `{"message_id":3,"result":{}}`)
		case "player_queues/play", "player_queues/pause":
			fmt.Fprint(w, `{"message_id":4,"result":{}}`)
		default:
			fmt.Fprint(w, `{"message_id":0,"error":{"code":"unknown","message":"no such command"}}`)
		}
	}))
	return srv, &got
}

func TestMAPlayersAndPlay(t *testing.T) {
	srv, got := fakeMA(t)
	defer srv.Close()
	c := NewMA(srv.URL, "ma-token")
	ctx := context.Background()

	players, err := c.Players(ctx)
	if err != nil {
		t.Fatalf("Players: %v", err)
	}
	if len(players) != 1 || players[0].PlayerID != "p1" {
		t.Fatalf("Players 不对: %+v", players)
	}
	if err := c.PlayTrackByName(ctx, "p1", "青花瓷", "周杰伦"); err != nil {
		t.Fatalf("PlayTrackByName: %v", err)
	}
	if err := c.QueuePlay(ctx, "p1"); err != nil {
		t.Fatalf("QueuePlay: %v", err)
	}
	if err := c.QueuePause(ctx, "p1"); err != nil {
		t.Fatalf("QueuePause: %v", err)
	}
	for _, cmd := range []string{"players/all", "music/track_by_name", "player_queues/play", "player_queues/pause"} {
		found := false
		for _, g := range *got {
			if g == cmd {
				found = true
			}
		}
		if !found {
			t.Fatalf("MA 未收到命令 %s，实际收到 %v", cmd, *got)
		}
	}
}

func TestMAErrorSurfaces(t *testing.T) {
	// 始终返回 error 的 MA：客户端应把错误透传出来，而不是吞掉。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"message_id":1,"error":{"code":"player_not_found","message":"no such player"}}`)
	}))
	defer srv.Close()
	c := NewMA(srv.URL, "ma-token")
	if _, err := c.Players(context.Background()); err == nil {
		t.Fatal("MA 返回 error 时 Players 应返回错误")
	} else if !strings.Contains(err.Error(), "player_not_found") {
		t.Fatalf("错误应透传 MA 的 code，实际: %v", err)
	}
}

// ---------- MPD 假服务器 ----------

type fakeMPD struct {
	t    *testing.T
	ln   net.Listener
	mu   sync.Mutex
	cmds []string
}

func startFakeMPD(t *testing.T) *fakeMPD {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeMPD{t: t, ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.handle(conn)
		}
	}()
	return f
}

func (f *fakeMPD) handle(conn net.Conn) {
	defer conn.Close()
	_, _ = conn.Write([]byte("OK MPD 0.23.0\n"))
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		f.mu.Lock()
		f.cmds = append(f.cmds, strings.TrimSpace(line))
		f.mu.Unlock()
		_, _ = conn.Write([]byte("OK\n"))
	}
}

func TestMPDPlayURL(t *testing.T) {
	f := startFakeMPD(t)
	defer f.ln.Close()
	c, err := DialMPD(f.ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("DialMPD: %v", err)
	}
	defer c.Close()

	if err := c.PlayURL("http://music.example.com/12345.mp3"); err != nil {
		t.Fatalf("PlayURL: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	want := []string{"clear", "add http://music.example.com/12345.mp3", "play"}
	if len(f.cmds) != len(want) {
		t.Fatalf("MPD 应收到 %v，实际 %v", want, f.cmds)
	}
	for i, w := range want {
		if f.cmds[i] != w {
			t.Fatalf("MPD 第 %d 条命令应为 %q，实际 %q", i, w, f.cmds[i])
		}
	}
}
