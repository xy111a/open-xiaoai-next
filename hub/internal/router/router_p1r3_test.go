package router

// 第三轮审计 P1 回归测试（H-1）：点歌的网易云阶段（Search + SongURL）
// 有总预算（默认 8s），3 首歌的 SongURL 并行取；超时即先播报
// 「网易云有点慢，先试本地」再走本地兜底，不再静默拖 60–90s。

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huajun/open-xiaoai-next/hub/internal/music"
)

// fakeNeteaseSlowP1R3 是可控的慢网易云：searchDelay / urlDelay 控制
// 各阶段的延迟；urlMode 控制 SongURL 的返回值（"empty" 全空、
// "third" 只有第三首有地址等），测超时与并行用。
func fakeNeteaseSlowP1R3(t *testing.T, searchDelay, urlDelay time.Duration, urlMode string, starts *[]time.Time, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/search"):
			if searchDelay > 0 {
				time.Sleep(searchDelay)
			}
			fmt.Fprint(w, `{"code":200,"result":{"songs":[`+
				`{"id":11,"name":"第一首","artists":[{"name":"甲"}]},`+
				`{"id":22,"name":"第二首","artists":[{"name":"乙"}]},`+
				`{"id":33,"name":"第三首","artists":[{"name":"丙"}]}]}}`)
		case strings.HasPrefix(r.URL.Path, "/song/url/v1"):
			mu.Lock()
			*starts = append(*starts, time.Now())
			mu.Unlock()
			if urlDelay > 0 {
				time.Sleep(urlDelay)
			}
			id := r.URL.Query().Get("id")
			switch urlMode {
			case "empty":
				fmt.Fprint(w, `{"code":200,"data":[]}`)
			case "second":
				if id == "22" {
					fmt.Fprint(w, `{"code":200,"data":[{"id":22,"url":"http://music.example.com/22.mp3"}]}`)
				} else {
					fmt.Fprint(w, `{"code":200,"data":[]}`)
				}
			default:
				fmt.Fprintf(w, `{"code":200,"data":[{"id":%s,"url":"http://music.example.com/%s.mp3"}]}`, id, id)
			}
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestMusicBudgetDefaultIs8s(t *testing.T) {
	r := New(Deps{}, nil)
	if got := r.musicBudget(); got != 8*time.Second {
		t.Fatalf("默认点歌预算应为 8s，实际 %s", got)
	}
	r.MusicBudget = 200 * time.Millisecond
	if got := r.musicBudget(); got != 200*time.Millisecond {
		t.Fatalf("显式点歌预算应生效，实际 %s", got)
	}
}

// TestMusicSearchTimeoutFallsBackToLocal：搜歌阶段被预算掐断时，
// 点歌应在预算量级内返回（而不是等 15s 单次超时），回复句首播报
// 「网易云有点慢，先试本地」，并兜底播出本地版。
func TestMusicSearchTimeoutFallsBackToLocal(t *testing.T) {
	deps, _, _, _ := testDeps(t)
	var starts []time.Time
	var mu sync.Mutex
	ne := fakeNeteaseSlowP1R3(t, 3*time.Second, 0, "empty", &starts, &mu)
	defer ne.Close()
	deps.Netease = music.NewNetease(ne.URL)
	mpdFake := startFakeMPD(t)
	deps.MPD = mpdFake.client(t)
	deps.MusicFolder = t.TempDir()
	r := New(deps, nil)
	r.MusicBudget = 200 * time.Millisecond

	start := time.Now()
	reply, err := r.Route(context.Background(), "lx06-1", "播放青花瓷")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("搜歌超时后应在预算量级内转本地兜底，实际耗时 %s", elapsed)
	}
	if !strings.Contains(reply, "网易云有点慢，先试本地") {
		t.Fatalf("超时回复应先播报「网易云有点慢，先试本地」，实际 %q", reply)
	}
	if !strings.Contains(reply, "正在播放本地版青花瓷") {
		t.Fatalf("超时后应兜底播本地版，实际 %q", reply)
	}
}

// TestMusicSongURLTimeoutFallsBackToLocal：搜歌成功、取地址阶段被预算
// 掐断时，同样先播报再试本地（覆盖 fetchSongURLs 的超时分支）。
func TestMusicSongURLTimeoutFallsBackToLocal(t *testing.T) {
	deps, _, _, _ := testDeps(t)
	var starts []time.Time
	var mu sync.Mutex
	ne := fakeNeteaseSlowP1R3(t, 0, 3*time.Second, "empty", &starts, &mu)
	defer ne.Close()
	deps.Netease = music.NewNetease(ne.URL)
	mpdFake := startFakeMPD(t)
	deps.MPD = mpdFake.client(t)
	deps.MusicFolder = t.TempDir()
	r := New(deps, nil)
	r.MusicBudget = 200 * time.Millisecond

	start := time.Now()
	reply, err := r.Route(context.Background(), "lx06-1", "播放青花瓷")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("取地址超时后应在预算量级内转本地兜底，实际耗时 %s", elapsed)
	}
	if !strings.Contains(reply, "网易云有点慢，先试本地") {
		t.Fatalf("超时回复应先播报「网易云有点慢，先试本地」，实际 %q", reply)
	}
	if !strings.Contains(reply, "正在播放本地版青花瓷") {
		t.Fatalf("超时后应兜底播本地版，实际 %q", reply)
	}
}

// TestMusicSongURLParallel：3 首歌的 SongURL 并行取——第二首的取地址
// 请求应在第一首还在等时就已发出（串行实现下它要等第一首 800ms 结束
// 才发出）；且按搜歌顺序优先播（第一首无地址时播第二首，不抢第三首）。
func TestMusicSongURLParallel(t *testing.T) {
	deps, _, _, _ := testDeps(t)
	var starts []time.Time
	var mu sync.Mutex
	ne := fakeNeteaseSlowP1R3(t, 0, 800*time.Millisecond, "second", &starts, &mu)
	defer ne.Close()
	deps.Netease = music.NewNetease(ne.URL)
	mpdFake := startFakeMPD(t)
	deps.MPD = mpdFake.client(t)
	r := New(deps, nil)

	start := time.Now()
	reply, err := r.Route(context.Background(), "lx06-1", "播放青花瓷")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(starts) != 3 {
		t.Fatalf("应并行发出 3 次 SongURL 请求，实际 %d 次", len(starts))
	}
	// 三次请求的发出时间应互相重叠：最晚发出的一次与最早的一次之差
	// 应远小于单次 800ms 延迟（串行时差值 ≥1600ms）。
	if gap := starts[2].Sub(starts[0]); gap > 400*time.Millisecond {
		t.Fatalf("SongURL 应并行发出，3 次请求发出间隔 %s，疑似串行", gap)
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("并行取地址应约等于单次延迟，实际耗时 %s", elapsed)
	}
	if !strings.Contains(reply, "第二首") {
		t.Fatalf("第一首无地址时应播第二首，实际 %q", reply)
	}
	joined := strings.Join(mpdFake.commands(), "\n")
	if !strings.Contains(joined, "add http://music.example.com/22.mp3") {
		t.Fatalf("MPD 应收到第二首的地址，实际 %v", mpdFake.commands())
	}
	if strings.Contains(joined, "33.mp3") {
		t.Fatalf("第二首可播时不应抢播第三首，实际 %v", mpdFake.commands())
	}
}
