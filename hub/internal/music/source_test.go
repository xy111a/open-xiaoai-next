package music

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// splitArtistTrack 从 router 搬过来（本地音源搜歌用），测试跟过来。
func TestSplitArtistTrack(t *testing.T) {
	if a, tr := splitArtistTrack("周杰伦的青花瓷"); a != "周杰伦" || tr != "青花瓷" {
		t.Fatalf("应拆成 周杰伦/青花瓷，实际 %q/%q", a, tr)
	}
	if a, tr := splitArtistTrack("青花瓷"); a != "" || tr != "青花瓷" {
		t.Fatalf("无“的”时整句当歌名，实际 %q/%q", a, tr)
	}
	if a, tr := splitArtistTrack("的青花瓷"); a != "" || tr != "的青花瓷" {
		t.Fatalf("歌手为空时整句当歌名，实际 %q/%q", a, tr)
	}
}

func TestNeteaseSourceSearch(t *testing.T) {
	srv := fakeNetease(t)
	defer srv.Close()
	src := NewNeteaseSource(NewNetease(srv.URL))
	got, err := src.Search(context.Background(), "青花瓷", 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || got[0].Song.Name != "青花瓷" || got[0].Song.Artist != "周杰伦" {
		t.Fatalf("应返回一首可播的青花瓷，实际 %+v", got)
	}
	if got[0].URL != "http://music.example.com/12345.mp3" || got[0].File != "" {
		t.Fatalf("网易云歌应带 URL、不带 File，实际 %+v", got[0])
	}
	if src.Name() != "netease" {
		t.Fatalf("Name 应为 netease，实际 %q", src.Name())
	}
}

// 搜到了歌但取不到地址 → 哨兵错误（不是“没搜到”，链要继续试本地）。
func TestNeteaseSourceNoPlayable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/search") {
			fmt.Fprint(w, `{"code":200,"result":{"songs":[{"id":1,"name":"VIP歌","artists":[{"name":"甲"}]}]}}`)
			return
		}
		fmt.Fprint(w, `{"code":200,"data":[]}`)
	}))
	defer srv.Close()
	src := NewNeteaseSource(NewNetease(srv.URL))
	if _, err := src.Search(context.Background(), "VIP歌", 3); !errors.Is(err, ErrNoPlayable) {
		t.Fatalf("都没地址时应返回 ErrNoPlayable，实际 %v", err)
	}
}

func TestLocalSourceSearch(t *testing.T) {
	f := startFakeMPD(t)
	defer f.ln.Close()
	mpd, err := DialMPD(f.ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("DialMPD: %v", err)
	}
	defer mpd.Close()
	dir := t.TempDir()
	src := NewLocalSource(mpd, dir)
	if !src.Available() {
		t.Fatal("MPD 已接 + 目录存在时本地音源应可用")
	}
	got, err := src.Search(context.Background(), "周杰伦的青花瓷", 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || got[0].File != "青花瓷.mp3" {
		t.Fatalf("应返回本地文件 青花瓷.mp3，实际 %+v", got)
	}
	if got[0].Song.Name != "青花瓷" || got[0].Song.Artist != "周杰伦" {
		t.Fatalf("本地歌名/歌手应为 青花瓷/周杰伦，实际 %+v", got[0].Song)
	}
	if got[0].URL != "" {
		t.Fatalf("本地歌不应带 URL，实际 %+v", got[0])
	}
	// 目录不存在 → 不可用，Search 报错（不发 MPD 命令）。
	bad := NewLocalSource(mpd, "/nonexistent-dir-xyz/music")
	if bad.Available() {
		t.Fatal("目录不存在时本地音源应不可用")
	}
	if _, err := bad.Search(context.Background(), "青花瓷", 1); err == nil {
		t.Fatal("不可用时 Search 应报错")
	}
}

// ---- Chain 兜底 ----

type stubSource struct {
	name      string
	playables []Playable
	err       error
}

func (s *stubSource) Name() string { return s.name }
func (s *stubSource) Search(_ context.Context, _ string, _ int) ([]Playable, error) {
	return s.playables, s.err
}

func TestChainFallback(t *testing.T) {
	ctx := context.Background()
	// 主路出错 → 落到第二音源。
	chain := NewChain(
		&stubSource{name: "netease", err: fmt.Errorf("上游 500")},
		&stubSource{name: "local", playables: []Playable{{Song: NeteaseSong{Name: "青花瓷"}, File: "青花瓷.mp3"}}},
	)
	got, name, err := chain.Search(ctx, "青花瓷", 3)
	if err != nil {
		t.Fatalf("主路出错时应落到本地，不应返回错误: %v", err)
	}
	if name != "local" || len(got) != 1 || got[0].File != "青花瓷.mp3" {
		t.Fatalf("应返回本地音源的结果，实际 name=%q %+v", name, got)
	}
	// 主路搜到但没歌（空 slice）→ 同样落到下一个。
	chain2 := NewChain(
		&stubSource{name: "netease"},
		&stubSource{name: "local", playables: []Playable{{Song: NeteaseSong{Name: "夜曲"}, File: "夜曲.mp3"}}},
	)
	got, name, err = chain2.Search(ctx, "夜曲", 3)
	if err != nil || name != "local" || len(got) != 1 {
		t.Fatalf("主路没歌时应落到本地，实际 name=%q err=%v %+v", name, err, got)
	}
	// 都没歌 → 空 + nil error（调用方回“没搜到”）。
	chain3 := NewChain(&stubSource{name: "netease"}, &stubSource{name: "local"})
	got, name, err = chain3.Search(ctx, "不存在的歌", 3)
	if err != nil || name != "" || len(got) != 0 {
		t.Fatalf("都没歌时应返回空 + nil，实际 name=%q err=%v %+v", name, err, got)
	}
	// 都出错 → 返回最后一个错误。
	chain4 := NewChain(
		&stubSource{name: "netease", err: fmt.Errorf("主路挂了")},
		&stubSource{name: "local", err: fmt.Errorf("本地也挂了")},
	)
	if _, _, err := chain4.Search(ctx, "青花瓷", 3); err == nil || !strings.Contains(err.Error(), "本地也挂了") {
		t.Fatalf("都出错时应返回最后一个错误，实际 %v", err)
	}
	// 空链。
	if got, _, err := NewChain().Search(ctx, "青花瓷", 3); err != nil || len(got) != 0 {
		t.Fatalf("空链应返回空 + nil，实际 %+v %v", got, err)
	}
	if NewChain().Len() != 0 {
		t.Fatal("空链 Len 应为 0")
	}
}

func TestChainSearchSource(t *testing.T) {
	ctx := context.Background()
	chain := NewChain(
		&stubSource{name: "netease", playables: []Playable{{Song: NeteaseSong{Name: "网"}, URL: "http://x/1.mp3"}}},
		&stubSource{name: "local", playables: []Playable{{Song: NeteaseSong{Name: "本"}, File: "b.mp3"}}},
	)
	got, err := chain.SearchSource(ctx, "local", "本", 3)
	if err != nil || len(got) != 1 || got[0].File != "b.mp3" {
		t.Fatalf("SearchSource(local) 应只问本地音源，实际 %+v %v", got, err)
	}
	if _, err := chain.SearchSource(ctx, "nope", "x", 1); err == nil {
		t.Fatal("链里没有的音源名应报错")
	}
}

// 本地音源的 MPD 搜索不应无限等：MPD 侧用默认 5s deadline，
// 这里只验证 Search 在假 MPD 上能快速返回。
func TestLocalSourceSearchFast(t *testing.T) {
	f := startFakeMPD(t)
	defer f.ln.Close()
	mpd, err := DialMPD(f.ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("DialMPD: %v", err)
	}
	defer mpd.Close()
	src := NewLocalSource(mpd, t.TempDir())
	start := time.Now()
	if _, err := src.Search(context.Background(), "青花瓷", 1); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("本地搜索应快速返回，实际 %s", time.Since(start))
	}
}
