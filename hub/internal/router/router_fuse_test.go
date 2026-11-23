package router

// 融合 B 回归测试：直连点歌主路（搜歌→取地址→PlayURL 的调用顺序与降级）、
// MPD 队列控制（语音暂停/继续/下一首/上一首/歌词）、播放状态归 Hub 管。
// 全部用 fake netease-api + fake MPD，不写真实 token/cookie。

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/huajun/open-xiaoai-next/hub/internal/music"
)

// fakeNeteaseFuse 是可控的假网易云：mode 控制 SongURL 的行为。
//   - "ok"：搜歌与取地址都成功；
//   - "empty"：搜到歌但取不到地址（data 为空，模拟 VIP/无版权）；
//   - "error"：搜歌直接 500。
//
// paths 按收到顺序记录请求路径（断言调用顺序用）。
func fakeNeteaseFuse(t *testing.T, mode string, paths *[]string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*paths = append(*paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/search"):
			if mode == "error" {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"code":500}`)
				return
			}
			fmt.Fprint(w, `{"code":200,"result":{"songs":[{"id":12345,"name":"青花瓷","artists":[{"name":"周杰伦"}]}]}}`)
		case strings.HasPrefix(r.URL.Path, "/song/url"):
			switch mode {
			case "ok":
				fmt.Fprint(w, `{"code":200,"data":[{"id":12345,"url":"http://music.example.com/12345.mp3"}]}`)
			case "empty":
				fmt.Fprint(w, `{"code":200,"data":[]}`)
			default:
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"code":500}`)
			}
		case strings.HasPrefix(r.URL.Path, "/lyric"):
			fmt.Fprint(w, `{"code":200,"lrc":{"lyric":"[00:00.00]素胚勾勒出青花"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
}

// startFakeMPDNoLocal 是 search 永远找不到歌的假 MPD（本地曲库为空时用）：
// 其余命令回 OK，search 只回 OK（无 file 行）。
func startFakeMPDNoLocal(t *testing.T) *fakeMPD {
	t.Helper()
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
			go func(c net.Conn) {
				defer c.Close()
				_, _ = c.Write([]byte("OK MPD 0.23.0\n"))
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					f.mu.Lock()
					f.cmds = append(f.cmds, strings.TrimSpace(line))
					f.mu.Unlock()
					_, _ = c.Write([]byte("OK\n"))
				}
			}(conn)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return f
}

func TestMusicDirectOrderAndFallbackToLocal(t *testing.T) {
	ctx := context.Background()
	deps, _, _, _ := testDeps(t)
	var paths []string
	ne := fakeNeteaseFuse(t, "empty", &paths) // 搜到歌但取不到地址
	defer ne.Close()
	deps.Netease = music.NewNetease(ne.URL)
	mpdFake := startFakeMPD(t)
	deps.MPD = mpdFake.client(t)
	deps.MusicFolder = t.TempDir() // 本地曲库目录存在
	r := New(deps, nil)

	reply, err := r.Route(ctx, "lx06-1", "播放周杰伦的青花瓷")
	if err != nil {
		t.Fatal(err)
	}
	// 快路径：兜底播出成功不播报确认，回复应为空；播出本身用 MPD 命令与当前曲断言。
	if reply != "" {
		t.Fatalf("快路径下兜底播出成功应回空回复，实际 %q", reply)
	}
	// 调用顺序：先搜歌，再取地址（失败），再本地搜文件、播本地文件。
	if len(paths) < 2 || paths[0] != "/search" || paths[1] != "/song/url" {
		t.Fatalf("应先调 /search 再调 /song/url，实际 %v", paths)
	}
	cmds := mpdFake.commands()
	joined := strings.Join(cmds, "\n")
	if !strings.Contains(joined, "search filename") || !strings.Contains(joined, `add "青花瓷.mp3"`) {
		t.Fatalf("兜底应搜本地文件并播，MPD 实际收到: %v", cmds)
	}
	// 内存当前曲应记为本地版。
	if song, source, ok := r.CurrentTrack(); !ok || song.Name != "青花瓷" || source != "local" {
		t.Fatalf("当前曲应记为本地版青花瓷，实际 %+v source=%q ok=%v", song, source, ok)
	}
}

func TestMusicSearchFailureFallsBackToLocal(t *testing.T) {
	ctx := context.Background()
	deps, _, _, _ := testDeps(t)
	var paths []string
	ne := fakeNeteaseFuse(t, "error", &paths) // 搜歌直接 500
	defer ne.Close()
	deps.Netease = music.NewNetease(ne.URL)
	mpdFake := startFakeMPD(t)
	deps.MPD = mpdFake.client(t)
	deps.MusicFolder = t.TempDir()
	r := New(deps, nil)

	reply, err := r.Route(ctx, "lx06-1", "播放青花瓷")
	if err != nil {
		t.Fatal(err)
	}
	// 快路径：搜歌失败兜底播出成功不播报确认，回复应为空；兜底是否真播出用 MPD 命令断言。
	if reply != "" {
		t.Fatalf("快路径下搜歌失败兜底播出成功应回空回复，实际 %q", reply)
	}
	joined := strings.Join(mpdFake.commands(), "\n")
	if !strings.Contains(joined, `add "青花瓷.mp3"`) {
		t.Fatalf("兜底应播本地文件，MPD 实际收到: %v", mpdFake.commands())
	}
}

func TestMusicDirectBothFailExplicit(t *testing.T) {
	ctx := context.Background()
	deps, _, _, _ := testDeps(t)
	var paths []string
	ne := fakeNeteaseFuse(t, "empty", &paths)
	defer ne.Close()
	deps.Netease = music.NewNetease(ne.URL)
	mpdFake := startFakeMPDNoLocal(t) // 本地曲库里也没有
	deps.MPD = mpdFake.client(t)
	deps.MusicFolder = t.TempDir()
	r := New(deps, nil)

	reply, err := r.Route(ctx, "lx06-1", "播放青花瓷")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "本地曲库里也没有") {
		t.Fatalf("直连与本地都失败时应明说本地也没有，实际 %q", reply)
	}
}

func TestMusicDirectLocalFolderMissing(t *testing.T) {
	ctx := context.Background()
	deps, _, _, _ := testDeps(t)
	var paths []string
	ne := fakeNeteaseFuse(t, "empty", &paths)
	defer ne.Close()
	deps.Netease = music.NewNetease(ne.URL)
	mpdFake := startFakeMPD(t)
	deps.MPD = mpdFake.client(t)
	deps.MusicFolder = "/nonexistent-dir-xyz/music" // 目录不存在
	r := New(deps, nil)

	reply, err := r.Route(ctx, "lx06-1", "播放青花瓷")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "本地曲库还没配好") {
		t.Fatalf("本地曲库目录不存在时应明说还没配好，实际 %q", reply)
	}
}

func TestMusicControls(t *testing.T) {
	ctx := context.Background()
	deps, _, _, _ := testDeps(t)
	var paths []string
	ne := fakeNeteaseFuse(t, "ok", &paths)
	defer ne.Close()
	deps.Netease = music.NewNetease(ne.URL)
	mpdFake := startFakeMPD(t)
	deps.MPD = mpdFake.client(t)
	r := New(deps, nil)

	cases := []struct {
		text string
		want string // 回复应含
		cmd  string // MPD 应收到
	}{
		{"暂停", "音乐已暂停", "pause 1"},
		{"继续播放", "继续播放", "pause 0"},
		{"下一首", "下一首", "next"},
		{"上一首", "上一首", "previous"},
	}
	for _, c := range cases {
		reply, err := r.Route(ctx, "lx06-1", c.text)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(reply, c.want) {
			t.Fatalf("%q 回复应含 %q，实际 %q", c.text, c.want, reply)
		}
		found := false
		for _, got := range mpdFake.commands() {
			if got == c.cmd {
				found = true
			}
		}
		if !found {
			t.Fatalf("%q 应让 MPD 收到 %q，实际 %v", c.text, c.cmd, mpdFake.commands())
		}
	}
	// MPD 未接时控制命令应明说，不要落到 Agent。
	r2 := New(deps, nil)
	r2.SetMPD(nil)
	reply, err := r2.Route(ctx, "lx06-1", "暂停")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "音乐还没接好") {
		t.Fatalf("MPD 未接时暂停应明说，实际 %q", reply)
	}
}

func TestMusicLyric(t *testing.T) {
	ctx := context.Background()
	deps, _, _, _ := testDeps(t)
	var paths []string
	ne := fakeNeteaseFuse(t, "ok", &paths)
	defer ne.Close()
	deps.Netease = music.NewNetease(ne.URL)
	mpdFake := startFakeMPD(t)
	deps.MPD = mpdFake.client(t)
	r := New(deps, nil)

	// 没放歌时要歌词：明说先点一首。
	reply, err := r.Route(ctx, "lx06-1", "歌词")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "还没在放歌") {
		t.Fatalf("没放歌时要歌词应明说，实际 %q", reply)
	}
	// 点歌成功后再要歌词：应返回当前曲的歌词。
	if _, err := r.Route(ctx, "lx06-1", "播放青花瓷"); err != nil {
		t.Fatal(err)
	}
	reply, err = r.Route(ctx, "lx06-1", "歌词")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "素胚勾勒出青花") {
		t.Fatalf("歌词应含当前曲歌词，实际 %q", reply)
	}
}

func TestMusicPlaybackState(t *testing.T) {
	ctx := context.Background()
	deps, _, _, _ := testDeps(t)
	var paths []string
	ne := fakeNeteaseFuse(t, "ok", &paths)
	defer ne.Close()
	deps.Netease = music.NewNetease(ne.URL)
	mpdFake := startFakeMPD(t)
	deps.MPD = mpdFake.client(t)
	r := New(deps, nil)

	if _, _, ok := r.CurrentTrack(); ok {
		t.Fatal("点歌前内存里不应有当前曲")
	}
	if _, err := r.Route(ctx, "lx06-1", "播放青花瓷"); err != nil {
		t.Fatal(err)
	}
	song, source, ok := r.CurrentTrack()
	if !ok || song.Name != "青花瓷" || song.Artist != "周杰伦" || song.ID != 12345 || source != "netease" {
		t.Fatalf("当前曲应记为网易云版青花瓷，实际 %+v source=%q ok=%v", song, source, ok)
	}
	state, cur, queue, err := r.PlaybackState(ctx)
	if err != nil {
		t.Fatalf("PlaybackState: %v", err)
	}
	if state != "play" {
		t.Fatalf("播放状态应为 play，实际 %q", state)
	}
	if cur.Title != "青花瓷" {
		t.Fatalf("当前曲目应为青花瓷，实际 %+v", cur)
	}
	if len(queue) != 2 {
		t.Fatalf("队列应有 2 首，实际 %+v", queue)
	}
	// MPD 未接时 PlaybackState 应报错（Web 层据此回 503）。
	r2 := New(deps, nil)
	r2.SetMPD(nil)
	if _, _, _, err := r2.PlaybackState(ctx); err == nil {
		t.Fatal("MPD 未接时 PlaybackState 应报错")
	}
}
