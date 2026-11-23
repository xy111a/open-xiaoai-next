package router

// 点歌快路径回归测试（2026-11-22）：playRe 命中的点歌命令播出成功时
// 返回空回复（语音管线收到 "" 直接跳过 TTS，不播“正在播放…”确认语），
// 大模型不应被调用；失败仍返回原有报错回复（会播报）；
// 非音乐命令与模糊的音乐闲聊仍走大模型；播放控制命令不在本次范围，
// 各自保留播报。
// 注意：音乐分支本来就不调大模型（确认语是模板回复），快路径省的是
// TTS 合成与播报；“首声压到 3 秒上下”是推算，部署日上机实测才能确认。

import (
	"context"
	"strings"
	"testing"

	"github.com/huajun/open-xiaoai-next/hub/internal/agent"
	"github.com/huajun/open-xiaoai-next/hub/internal/music"
)

// countingAgentDeps 返回带计数桩 Agent 的 Deps（cap.calls 计调用次数）。
func countingAgentDeps(t *testing.T) (Deps, *agentCapture) {
	t.Helper()
	deps, _, _, _ := testDeps(t)
	cap := &agentCapture{}
	agentSrv := startCaptureAgent(t, cap, "今天晴，22 度。")
	t.Cleanup(agentSrv.Close)
	deps.Agent = agent.New(agentSrv.URL+"/v1", "m", "")
	return deps, cap
}

func TestMusicFastPathDirectSuccess(t *testing.T) {
	ctx := context.Background()
	deps, cap := countingAgentDeps(t)
	var paths []string
	ne := fakeNeteaseFuse(t, "ok", &paths)
	defer ne.Close()
	deps.Netease = music.NewNetease(ne.URL)
	mpdFake := startFakeMPD(t)
	deps.MPD = mpdFake.client(t)
	r := New(deps, nil)

	reply, err := r.Route(ctx, "lx06-1", "播放周杰伦的青花瓷")
	if err != nil {
		t.Fatal(err)
	}
	// 快路径：直连播出成功，回空回复，不播报确认语。
	if reply != "" {
		t.Fatalf("快路径下点歌成功应回空回复，实际 %q", reply)
	}
	if cap.calls != 0 {
		t.Fatalf("快路径下大模型不应被调用，实际 %d 次", cap.calls)
	}
	// MPD 应收到 clear/add 直链/play，顺序不能错。
	if got := mpdFake.commands(); len(got) != 3 || got[0] != "clear" || got[1] != `add "http://music.example.com/12345.mp3"` || got[2] != "play" {
		t.Fatalf("MPD 应收到 clear/add/play，实际 %v", got)
	}
	if song, source, ok := r.CurrentTrack(); !ok || song.Name != "青花瓷" || song.Artist != "周杰伦" || source != "netease" {
		t.Fatalf("当前曲应记为网易云版青花瓷，实际 %+v source=%q ok=%v", song, source, ok)
	}
}

func TestMusicFastPathLocalFallbackSuccess(t *testing.T) {
	ctx := context.Background()
	deps, cap := countingAgentDeps(t)
	var paths []string
	ne := fakeNeteaseFuse(t, "empty", &paths) // 搜到歌但取不到地址
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
	// 快路径：本地兜底播出成功也不播报，回复应为空。
	if reply != "" {
		t.Fatalf("快路径下本地兜底播出成功应回空回复，实际 %q", reply)
	}
	if cap.calls != 0 {
		t.Fatalf("快路径下大模型不应被调用，实际 %d 次", cap.calls)
	}
	joined := strings.Join(mpdFake.commands(), "\n")
	if !strings.Contains(joined, "search filename") || !strings.Contains(joined, `add "青花瓷.mp3"`) {
		t.Fatalf("兜底应搜本地文件并播，MPD 实际收到: %v", mpdFake.commands())
	}
	if song, source, ok := r.CurrentTrack(); !ok || song.Name != "青花瓷" || source != "local" {
		t.Fatalf("当前曲应记为本地版青花瓷，实际 %+v source=%q ok=%v", song, source, ok)
	}
}

func TestMusicFastPathFailureStillSpeaks(t *testing.T) {
	ctx := context.Background()

	// 直连与本地都失败：应明说本地也没有（会播报），不静默、不落到 Agent。
	deps, cap := countingAgentDeps(t)
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
	if cap.calls != 0 {
		t.Fatalf("点歌失败时不应落到大模型，实际调用 %d 次", cap.calls)
	}

	// 搜不到歌：应明说没搜到（会播报），不静默、不落到 Agent。
	deps2, cap2 := countingAgentDeps(t)
	deps2.MusicSources = music.NewChain(
		&pluginStubSource{name: "netease", fn: func(query string) ([]music.Playable, error) { return nil, nil }},
		&pluginStubSource{name: "local", fn: func(query string) ([]music.Playable, error) { return nil, nil }},
	)
	r2 := New(deps2, nil)
	reply, err = r2.Route(ctx, "lx06-1", "播放不存在的歌名xyz")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "没搜到") {
		t.Fatalf("搜不到歌时应明说没搜到，实际 %q", reply)
	}
	if cap2.calls != 0 {
		t.Fatalf("搜不到歌时不应落到大模型，实际调用 %d 次", cap2.calls)
	}
}

func TestMusicFastPathNonMusicStillGoesToAgent(t *testing.T) {
	ctx := context.Background()
	deps, cap := countingAgentDeps(t)
	r := New(deps, nil)

	reply, err := r.Route(ctx, "lx06-1", "今天天气怎么样")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "今天晴，22 度。" {
		t.Fatalf("非音乐命令应走大模型，实际 %q", reply)
	}
	if cap.calls != 1 {
		t.Fatalf("非音乐命令应调大模型 1 次，实际 %d 次", cap.calls)
	}
}

func TestMusicFastPathFuzzyMusicChatStillGoesToAgent(t *testing.T) {
	ctx := context.Background()
	deps, cap := countingAgentDeps(t)
	var paths []string
	ne := fakeNeteaseFuse(t, "ok", &paths)
	defer ne.Close()
	deps.Netease = music.NewNetease(ne.URL)
	mpdFake := startFakeMPD(t)
	deps.MPD = mpdFake.client(t)
	r := New(deps, nil)

	// “我想听点轻音乐”没有“播放”动词，playRe 不命中，应落到大模型闲聊，
	// 不能被快路径当成点歌直接播了。
	reply, err := r.Route(ctx, "lx06-1", "我想听点轻音乐")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "今天晴，22 度。" {
		t.Fatalf("模糊的音乐闲聊应走大模型，实际 %q", reply)
	}
	if cap.calls != 1 {
		t.Fatalf("模糊的音乐闲聊应调大模型 1 次，实际 %d 次", cap.calls)
	}
	if cmds := mpdFake.commands(); len(cmds) != 0 {
		t.Fatalf("模糊的音乐闲聊不应碰 MPD，实际 %v", cmds)
	}
}

func TestMusicFastPathControlsKeepReplies(t *testing.T) {
	ctx := context.Background()
	deps, cap := countingAgentDeps(t)
	var paths []string
	ne := fakeNeteaseFuse(t, "ok", &paths)
	defer ne.Close()
	deps.Netease = music.NewNetease(ne.URL)
	mpdFake := startFakeMPD(t)
	deps.MPD = mpdFake.client(t)
	r := New(deps, nil)

	// 播放控制不在快路径范围：各自保留播报回复。
	cases := []struct {
		text string
		want string
		cmd  string
	}{
		{"暂停", "音乐已暂停", "pause 1"},
		{"下一首", "下一首", "next"},
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
	if cap.calls != 0 {
		t.Fatalf("播放控制不应调大模型，实际 %d 次", cap.calls)
	}
}
