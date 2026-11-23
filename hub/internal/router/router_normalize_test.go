package router

// STT 归一化回归测试（2026-11-21 换 FunASR Paraformer-large 后补）：
// FunASR 把唤醒词“小李”听成“小丽”（17 条里 11–12 条首跑）、把“播放”
// 听成“播芳/波放”；归一化必须发生在意图解析前，否则锚定正则
// （^播放、^下一首、^我在…）对不上，整句落到 Agent。
// 不碰“你好龙虾”回归测试（kws_test.go 的 TestUnknownKeywordFiltered
// 是已删除路由的防泄漏测试，与本文件无关）。

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/huajun/open-xiaoai-next/hub/internal/music"
)

func TestNormalizeSTTText(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// FunASR 实测转写（无标点）：唤醒词 + 播芳/波放都要修，剥离后锚定正则才能命中。
		{"你好小丽播放周杰伦的青花瓷", "播放周杰伦的青花瓷"},
		{"你好小李播芳陈奕迅的孤勇者", "播放陈奕迅的孤勇者"},
		{"你好小李波放一首轻音乐", "播放一首轻音乐"},
		{"小丽下一首", "下一首"},
		{"你好小丽下一首", "下一首"},
		{"你好小李我在客厅", "我在客厅"},
		// Whisper 时代转写（带标点）：前缀后的 ， 也要清掉。
		{"你好，小李，播放周杰伦的青花瓷。", "播放周杰伦的青花瓷。"},
		{"你好，小丽，下一首。", "下一首。"},
		// 正常路径（生产管线会话开后才喂音频，本来就不带唤醒词）：原样不动。
		{"播放周杰伦的青花瓷", "播放周杰伦的青花瓷"},
		{"把客厅的灯打开", "把客厅的灯打开"},
		{"提醒我下午三点开会", "提醒我下午三点开会"},
		{"暂停", "暂停"},
		// 只有唤醒词：剥离后为空，Route 应直接返回、不进任何分支。
		{"你好小李", ""},
		{"小丽", ""},
		{"", ""},
		{"   ", ""},
	}
	for _, c := range cases {
		if got := normalizeSTTText(c.in); got != c.want {
			t.Errorf("normalizeSTTText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// recordingNetease 是只记搜歌关键词的假网易云：搜歌永远回青花瓷，
// 取地址永远成功；断言点歌 query 是否被归一化修对（而不是只看回复）。
func recordingNetease(t *testing.T, queries *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/search"):
			*queries = append(*queries, r.URL.Query().Get("keywords"))
			fmt.Fprint(w, `{"code":200,"result":{"songs":[{"id":12345,"name":"青花瓷","artists":[{"name":"周杰伦"}]}]}}`)
		case strings.HasPrefix(r.URL.Path, "/song/url"):
			fmt.Fprint(w, `{"code":200,"data":[{"id":12345,"url":"http://music.example.com/12345.mp3"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestRouteSTTNormalizationMusic(t *testing.T) {
	ctx := context.Background()
	deps, _, _, _ := testDeps(t)
	var queries []string
	ne := recordingNetease(t, &queries)
	defer ne.Close()
	deps.Netease = music.NewNetease(ne.URL)
	mpdFake := startFakeMPD(t)
	deps.MPD = mpdFake.client(t)
	deps.MusicFolder = t.TempDir()
	r := New(deps, nil)

	// FunASR 实测转写：唤醒词听成“小丽” + “播放”听成“播芳”，
	// 归一化后应正常点歌，搜歌关键词里不能残留错字。
	// 快路径：点歌成功回空回复；是否真播出用搜歌关键词与 MPD 命令断言。
	reply, err := r.Route(ctx, "lx06-1", "你好小丽播芳陈奕迅的孤勇者")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "" {
		t.Fatalf("归一化后点歌成功应回空回复（快路径），实际 %q", reply)
	}
	if len(queries) != 1 || queries[0] != "陈奕迅的孤勇者" {
		t.Fatalf("搜歌关键词应为“陈奕迅的孤勇者”，实际 %v", queries)
	}
	joined := strings.Join(mpdFake.commands(), "\n")
	if !strings.Contains(joined, "add http://music.example.com/12345.mp3") {
		t.Fatalf("归一化后点歌应让 MPD 播直链，实际 %v", mpdFake.commands())
	}

	// “波放”同理。
	queries = queries[:0]
	reply, err = r.Route(ctx, "lx06-1", "你好小李波放一首轻音乐")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "" {
		t.Fatalf("“波放”归一化后点歌成功应回空回复（快路径），实际 %q", reply)
	}
	if len(queries) != 1 || queries[0] != "一首轻音乐" {
		t.Fatalf("搜歌关键词应为“一首轻音乐”，实际 %v", queries)
	}

	// 播放控制：唤醒词前缀不能挡住 ^下一首。
	reply, err = r.Route(ctx, "lx06-1", "你好小丽下一首")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "下一首") {
		t.Fatalf("“下一首”应进播放控制，实际 %q", reply)
	}
	found := false
	for _, got := range mpdFake.commands() {
		if got == "next" {
			found = true
		}
	}
	if !found {
		t.Fatalf("“下一首”应让 MPD 收到 next，实际 %v", mpdFake.commands())
	}
}

func TestRouteSTTNormalizationHomeAssistant(t *testing.T) {
	ctx := context.Background()
	deps, _, _, _ := testDeps(t)
	// fakeHA 自带 light.living_room=off；别名表只配这一个。
	r := New(deps, map[string]string{"客厅的灯": "light.living_room"})

	reply, err := r.Route(ctx, "lx06-1", "你好小丽打开客厅的灯")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "客厅的灯开了") {
		t.Fatalf("归一化后应进 HA 开灯，实际 %q", reply)
	}
}

func TestRouteSTTNormalizationRoomClaim(t *testing.T) {
	ctx := context.Background()
	deps, _, _, _ := testDeps(t)
	claimed := ""
	deps.RoomClaim = func(deviceID, room string) { claimed = room }
	r := New(deps, nil)

	reply, err := r.Route(ctx, "lx06-1", "你好小李我在客厅")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "好的，你在客厅了") {
		t.Fatalf("归一化后应进房间认领，实际 %q", reply)
	}
	if claimed != "客厅" {
		t.Fatalf("认领的房间应为“客厅”，实际 %q", claimed)
	}
}
