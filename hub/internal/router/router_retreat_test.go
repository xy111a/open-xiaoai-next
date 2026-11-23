package router

// 撤退词本地快路径回归测试（2026-11-23）：整句是“停”“别说了”等
// 撤退词时，本地立即切断声音（MPD 暂停 + StopAudio 停 TTS 播报），
// 不进 LLM、不进仲裁、不播报确认语（回空回复）；带更多内容的句子
// 与播放控制命令不受影响。

import (
	"context"
	"strings"
	"testing"

	"github.com/huajun/open-xiaoai-next/hub/internal/music"
)

func TestRetreatStopsMusicAndSkipsTTS(t *testing.T) {
	ctx := context.Background()
	deps, cap := countingAgentDeps(t)
	var paths []string
	ne := fakeNeteaseFuse(t, "ok", &paths)
	defer ne.Close()
	deps.Netease = music.NewNetease(ne.URL)
	mpdFake := startFakeMPD(t)
	deps.MPD = mpdFake.client(t)
	var stopped []string
	deps.StopAudio = func(deviceID string) { stopped = append(stopped, deviceID) }
	r := New(deps, nil)

	reply, err := r.Route(ctx, "lx06-1", "停")
	if err != nil {
		t.Fatal(err)
	}
	// 快路径：回空回复，语音管线跳过 TTS，不多说一句话。
	if reply != "" {
		t.Fatalf("撤退词应回空回复，实际 %q", reply)
	}
	if cap.calls != 0 {
		t.Fatalf("撤退词不应调大模型，实际 %d 次", cap.calls)
	}
	found := false
	for _, got := range mpdFake.commands() {
		if got == "pause 1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("撤退词应让 MPD 暂停，实际收到 %v", mpdFake.commands())
	}
	if len(stopped) != 1 || stopped[0] != "lx06-1" {
		t.Fatalf("撤退词应调一次 StopAudio(lx06-1)，实际 %v", stopped)
	}
}

func TestRetreatWords(t *testing.T) {
	ctx := context.Background()
	for _, word := range []string{"停", "停止", "停下", "别说了", "别播了", "不要说了"} {
		deps, cap := countingAgentDeps(t)
		deps.StopAudio = func(deviceID string) {}
		r := New(deps, nil)
		reply, err := r.Route(ctx, "lx06-1", word)
		if err != nil {
			t.Fatal(err)
		}
		if reply != "" {
			t.Fatalf("撤退词 %q 应回空回复，实际 %q", word, reply)
		}
		if cap.calls != 0 {
			t.Fatalf("撤退词 %q 不应调大模型，实际 %d 次", word, cap.calls)
		}
	}
}

func TestRetreatDoesNotSwallowChatOrControls(t *testing.T) {
	ctx := context.Background()
	deps, cap := countingAgentDeps(t)
	mpdFake := startFakeMPD(t)
	deps.MPD = mpdFake.client(t)
	deps.StopAudio = func(deviceID string) {}
	r := New(deps, nil)

	// 带更多内容的句子不是撤退词：落到大模型闲聊。
	reply, err := r.Route(ctx, "lx06-1", "停一下，我想想")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "今天晴，22 度。" || cap.calls != 1 {
		t.Fatalf("“停一下，我想想”应走大模型，实际 reply=%q calls=%d", reply, cap.calls)
	}
	reply, err = r.Route(ctx, "lx06-1", "停车去哪")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "今天晴，22 度。" || cap.calls != 2 {
		t.Fatalf("“停车去哪”应走大模型，实际 reply=%q calls=%d", reply, cap.calls)
	}

	// 播放控制不受撤退词影响：各自保留播报回复。
	reply, err = r.Route(ctx, "lx06-1", "暂停")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "音乐已暂停") {
		t.Fatalf("“暂停”应保留播报回复，实际 %q", reply)
	}
	reply, err = r.Route(ctx, "lx06-1", "下一首")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "下一首") {
		t.Fatalf("“下一首”应保留播报回复，实际 %q", reply)
	}
	if cap.calls != 2 {
		t.Fatalf("播放控制不应调大模型，实际 %d 次", cap.calls)
	}
}

func TestRetreatWithoutDeps(t *testing.T) {
	// MPD/StopAudio/Agent 全未配：撤退词仍应静默返回，不 panic。
	deps, _, _, _ := testDeps(t)
	deps.MPD = nil
	deps.Agent = nil
	r := New(deps, nil)
	reply, err := r.Route(context.Background(), "lx06-1", "停")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "" {
		t.Fatalf("无下游时撤退词仍应回空回复，实际 %q", reply)
	}
}
