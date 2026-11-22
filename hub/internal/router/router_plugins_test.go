package router

// 插件集成测试（mock）：桩 Agent + 桩 Skill + 桩音源链走真实 Router，
// 验证端到端路由：触发词文本进技能、其他文本进 Agent（带该 Agent 配的
// system_prompt）、点歌主路失败时落到本地音源。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/huajun/open-xiaoai-next/hub/internal/agent"
	"github.com/huajun/open-xiaoai-next/hub/internal/music"
	"github.com/huajun/open-xiaoai-next/hub/internal/skill"
)

// pluginStubSource 是按 query 决定返回的桩音源。
type pluginStubSource struct {
	name string
	fn   func(query string) ([]music.Playable, error)
}

func (s *pluginStubSource) Name() string { return s.name }
func (s *pluginStubSource) Search(_ context.Context, query string, _ int) ([]music.Playable, error) {
	return s.fn(query)
}

// agentCapture 记录桩 Agent 收到的每次请求。
type agentCapture struct {
	mu       sync.Mutex
	calls    int
	models   []string
	systems  []string
	userText []string
}

func startCaptureAgent(t *testing.T, cap *agentCapture, reply string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		cap.mu.Lock()
		cap.calls++
		cap.models = append(cap.models, body.Model)
		if len(body.Messages) > 0 {
			cap.systems = append(cap.systems, body.Messages[0].Content)
		}
		if len(body.Messages) > 1 {
			cap.userText = append(cap.userText, body.Messages[1].Content)
		}
		cap.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q}}]}`, reply)
	}))
}

func TestPluginIntegrationSkillAgentMusic(t *testing.T) {
	ctx := context.Background()
	deps, _, _, _ := testDeps(t)

	// 桩 Agent：system_prompt 配成自定义值，验证 Router 真把它发过去。
	cap := &agentCapture{}
	agentSrv := startCaptureAgent(t, cap, "今天晴，22 度。")
	defer agentSrv.Close()
	deps.Agent = agent.NewFull(agentSrv.URL+"/v1", "deepseek-chat", "", "你是测试管家，只说一句话。")

	// 桩 Skill：只认“新闻”，回固定文本。
	skillSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["text"] == nil || body["device_id"] != "lx06-1" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"reply": "今日新闻：测试头条。"}`)
	}))
	defer skillSrv.Close()
	deps.Skills = skill.New([]skill.Skill{
		{Name: "news", Triggers: []string{"新闻", "今日头条"}, Endpoint: skillSrv.URL},
	})

	// 桩音源链：主路对“青花瓷”报错（模拟上游故障 → 落到本地），
	// 其他歌直接返回“没有”（模拟搜不到）。
	deps.MusicSources = music.NewChain(
		&pluginStubSource{name: "netease", fn: func(query string) ([]music.Playable, error) {
			if strings.Contains(query, "青花瓷") {
				return nil, fmt.Errorf("上游 500")
			}
			return nil, nil
		}},
		&pluginStubSource{name: "local", fn: func(query string) ([]music.Playable, error) {
			if strings.Contains(query, "青花瓷") {
				return []music.Playable{{Song: music.NeteaseSong{Name: "青花瓷", Artist: "周杰伦"}, File: "青花瓷.mp3"}}, nil
			}
			return nil, nil
		}},
	)
	mpdFake := startFakeMPD(t)
	deps.MPD = mpdFake.client(t)
	deps.MusicFolder = t.TempDir()

	r := New(deps, nil)

	// 1. 触发词文本 → 技能，Agent 不应被调用。
	reply, err := r.Route(ctx, "lx06-1", "今天有什么新闻")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "今日新闻：测试头条。" {
		t.Fatalf("触发词文本应走技能，实际 %q", reply)
	}
	if cap.calls != 0 {
		t.Fatalf("技能命中时 Agent 不应被调用，实际 %d 次", cap.calls)
	}

	// 2. 非触发词文本 → Agent，且 system 提示词是配的那句。
	reply, err = r.Route(ctx, "lx06-1", "今天天气怎么样")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "今天晴，22 度。" {
		t.Fatalf("非触发词文本应走 Agent，实际 %q", reply)
	}
	if cap.calls != 1 || cap.models[0] != "deepseek-chat" || cap.systems[0] != "你是测试管家，只说一句话。" || cap.userText[0] != "今天天气怎么样" {
		t.Fatalf("Agent 应收到 model + 自定义 system_prompt + 原文，实际 calls=%d models=%v systems=%v user=%v",
			cap.calls, cap.models, cap.systems, cap.userText)
	}

	// 3. 点歌主路失败 → 本地兜底播出，MPD 收到播本地文件的命令。
	// 快路径：兜底播出成功不播报确认，回复应为空。
	reply, err = r.Route(ctx, "lx06-1", "播放青花瓷")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "" {
		t.Fatalf("快路径下主路失败兜底播出成功应回空回复，实际 %q", reply)
	}
	joined := strings.Join(mpdFake.commands(), "\n")
	if !strings.Contains(joined, "add 青花瓷.mp3") {
		t.Fatalf("MPD 应收到 add 青花瓷.mp3，实际 %v", mpdFake.commands())
	}
	if _, source, ok := r.CurrentTrack(); !ok || source != "local" {
		t.Fatalf("当前曲来源应记为 local，实际 %q ok=%v", source, ok)
	}

	// 4. 本地歌没有在线歌词：明说，不回“还没在放歌”。
	reply, err = r.Route(ctx, "lx06-1", "歌词")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "本地歌曲没有在线歌词") {
		t.Fatalf("本地歌查歌词应明说没有在线歌词，实际 %q", reply)
	}

	// 5. 两路都没歌 → “没搜到”，不落到 Agent。
	before := cap.calls
	reply, err = r.Route(ctx, "lx06-1", "播放不存在的歌名xyz")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "没搜到") {
		t.Fatalf("都没歌时应回“没搜到”，实际 %q", reply)
	}
	if cap.calls != before {
		t.Fatal("点歌没搜到时不应落到 Agent 闲聊")
	}
}

// 技能调用失败（500）→ 落回 Agent，不谎报、不卡死。
func TestPluginSkillFailureFallsBackToAgent(t *testing.T) {
	ctx := context.Background()
	deps, _, _, _ := testDeps(t)
	cap := &agentCapture{}
	agentSrv := startCaptureAgent(t, cap, "Agent 兜底回复。")
	defer agentSrv.Close()
	deps.Agent = agent.New(agentSrv.URL+"/v1", "m", "")
	badSkill := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "boom")
	}))
	defer badSkill.Close()
	deps.Skills = skill.New([]skill.Skill{
		{Name: "news", Triggers: []string{"新闻"}, Endpoint: badSkill.URL},
	})
	r := New(deps, nil)
	reply, err := r.Route(ctx, "lx06-1", "今天有什么新闻")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "Agent 兜底回复。" {
		t.Fatalf("技能失败时应落回 Agent，实际 %q", reply)
	}
	if cap.calls != 1 {
		t.Fatalf("技能失败后 Agent 应被调用 1 次，实际 %d", cap.calls)
	}
}

// Agent 没配 system_prompt 时发默认提示词。
func TestPluginAgentDefaultPrompt(t *testing.T) {
	ctx := context.Background()
	deps, _, _, _ := testDeps(t)
	cap := &agentCapture{}
	agentSrv := startCaptureAgent(t, cap, "你好。")
	defer agentSrv.Close()
	deps.Agent = agent.New(agentSrv.URL+"/v1", "m", "")
	r := New(deps, nil)
	if _, err := r.Route(ctx, "lx06-1", "随便聊聊"); err != nil {
		t.Fatal(err)
	}
	if len(cap.systems) != 1 || cap.systems[0] != agent.DefaultSystemPrompt {
		t.Fatalf("没配 system_prompt 时应发默认提示词，实际 %v", cap.systems)
	}
}
