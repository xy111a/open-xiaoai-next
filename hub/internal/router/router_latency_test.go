package router

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/huajun/open-xiaoai-next/hub/internal/agent"
	"github.com/huajun/open-xiaoai-next/hub/internal/music"
	"github.com/huajun/open-xiaoai-next/hub/internal/observability"
)

// Route 走 LLM 时，llm_ms 应写进语音管线 Begin 的进行中交互；
// 走家电本地快路时 llm_ms 应缺失（不是没测到，是没走 LLM）。
func TestRouterRecordsLLMLatency(t *testing.T) {
	deps, _, _, _ := testDeps(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond) // 假 LLM 耗时：断言下限用
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"今天晴，22 度。"}}]}`)
	}))
	defer srv.Close()
	deps.Agent = agent.New(srv.URL+"/v1", "m", "")
	r := New(deps, nil)
	rec := observability.NewRecorder(20)
	r.Latency = rec

	// 模拟语音管线：先 Begin 一条进行中交互，再 Route。
	inter := rec.Begin("lx06-1", "voice")
	reply, err := r.Route(context.Background(), "lx06-1", "今天天气怎么样")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "今天晴，22 度。" {
		t.Fatalf("Agent 回复不对: %q", reply)
	}
	if got := inter.Stages[observability.StageLLM]; got < 50 {
		t.Fatalf("llm_ms 应 >= 50（假 LLM 睡了 50ms），实际 %d", got)
	}
	rec.Finish(inter, nil)

	// 家电快路：同样 Begin 后 Route，llm_ms 应缺失。
	deps2, _, _, _ := testDeps(t)
	r2 := New(deps2, map[string]string{"客厅灯": "light.living_room"})
	r2.Latency = rec
	inter2 := rec.Begin("lx06-1", "voice")
	if _, err := r2.Route(context.Background(), "lx06-1", "开客厅灯"); err != nil {
		t.Fatal(err)
	}
	if _, ok := inter2.Stages[observability.StageLLM]; ok {
		t.Fatalf("家电快路不应记 llm_ms，实际 %v", inter2.Stages)
	}
	rec.Finish(inter2, nil)
}

// 点歌成功时 music_ms 应写进进行中交互（网易云直连主路）。
func TestRouterRecordsMusicLatency(t *testing.T) {
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
	mpdLn := startFakeMPD(t)
	deps.MPD = mpdLn.client(t)
	r := New(deps, nil)
	rec := observability.NewRecorder(20)
	r.Latency = rec

	inter := rec.Begin("lx06-1", "voice")
	reply, err := r.Route(context.Background(), "lx06-1", "播放青花瓷")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "正在播放周杰伦的青花瓷") {
		t.Fatalf("直连点歌回复不对: %q", reply)
	}
	if _, ok := inter.Stages[observability.StageMusic]; !ok {
		t.Fatalf("点歌成功应记 music_ms，实际 %v", inter.Stages)
	}
	rec.Finish(inter, nil)
	// 不在问答里点歌：RecordMusic 应单独立一条 music 记录。
	rec.RecordMusic("oh2p-1", 1500*time.Millisecond)
	recent := rec.Recent()
	if len(recent) != 2 {
		t.Fatalf("应有 2 条记录（语音 1 条 + 独立点歌 1 条），实际 %d", len(recent))
	}
	if recent[0].Kind != "music" || recent[0].DeviceID != "oh2p-1" {
		t.Fatalf("最新一条应为 oh2p-1 的独立点歌，实际 %+v", recent[0])
	}
}
