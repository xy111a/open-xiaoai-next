package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huajun/open-xiaoai-next/hub/internal/agent"
	"github.com/huajun/open-xiaoai-next/hub/internal/session"
)

// recordingAgent 是桩 LLM：记录每次 Chat 收到的完整 messages，
// 按顺序回 canned 回复；failContains 非空时，user 内容含该串的
// 请求回 500（模拟 LLM 失败）。
type recordingAgent struct {
	mu           sync.Mutex
	calls        [][]agent.Message
	replies      []string
	failContains string
}

func (r *recordingAgent) client(t *testing.T) *agent.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Messages []agent.Message `json:"messages"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, m := range body.Messages {
			if r.failContains != "" && m.Role == "user" && strings.Contains(m.Content, r.failContains) {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"error":{"message":"mock failure"}}`)
				return
			}
		}
		r.calls = append(r.calls, body.Messages)
		reply := ""
		if len(r.replies) > 0 {
			reply = r.replies[len(r.calls)-1]
			if len(r.calls) > len(r.replies) {
				reply = r.replies[len(r.replies)-1]
			}
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q}}]}`, reply)
	}))
	t.Cleanup(srv.Close)
	return agent.New(srv.URL+"/v1", "m", "")
}

func (r *recordingAgent) lastCall() []agent.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return nil
	}
	return r.calls[len(r.calls)-1]
}

func (r *recordingAgent) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// messagesText 把一次调用的 messages 拼成 "role:content" 列表，便于断言顺序。
func messagesText(msgs []agent.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Role + ":" + m.Content
	}
	return out
}

func equalStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s 长度不对：want %v，got %v", what, want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s 第 %d 条不对：want %q，got %q（全部 %v）", what, i, want[i], got[i], got)
		}
	}
}

// 核心场景：假 STT 文本两轮（“北京天气怎么样”→“那明天呢”），
// 桩 LLM 记录每次收到的历史；第二轮的 LLM 调用必须含第一轮。
func TestMultiTurnHistorySentToLLM(t *testing.T) {
	deps, _, _, _ := testDeps(t)
	rec := &recordingAgent{replies: []string{"今天北京晴，25 度。", "明天北京多云，23 度。"}}
	deps.Agent = rec.client(t)
	deps.Sessions = session.New(60 * time.Second)
	r := New(deps, nil)
	ctx := context.Background()

	reply, err := r.Route(ctx, "lx06-1", "北京天气怎么样")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "今天北京晴，25 度。" {
		t.Fatalf("第一轮回复不对：%q", reply)
	}
	equalStrings(t, "第一轮 LLM 入参", messagesText(rec.lastCall()), []string{
		"system:你是家里的语音管家，回答简短口语化，一两句话。",
		"user:北京天气怎么样",
	})

	// 模拟新一次唤醒（OnWake 不重置会话）：同一设备/房间继续同一会话。
	r.SetWakeWord("lx06-1", "你好小李")
	reply, err = r.Route(ctx, "lx06-1", "那明天呢")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "明天北京多云，23 度。" {
		t.Fatalf("第二轮回复不对：%q", reply)
	}
	equalStrings(t, "第二轮 LLM 入参（须含第一轮）", messagesText(rec.lastCall()), []string{
		"system:你是家里的语音管家，回答简短口语化，一两句话。",
		"user:北京天气怎么样",
		"assistant:今天北京晴，25 度。",
		"user:那明天呢",
	})
	if got := deps.Sessions.SessionID("room:客厅"); got == "" {
		t.Fatal("两轮后 room:客厅 会话 ID 不应为空")
	}
}

// 不同房间各自独立：卧室的跟进问题看不到客厅的前文。
func TestMultiTurnDifferentRoomIsolated(t *testing.T) {
	deps, _, _, _ := testDeps(t)
	deps.RoomOfDevice = func(deviceID string) string {
		if deviceID == "oh2p-1" {
			return "卧室"
		}
		return "客厅"
	}
	rec := &recordingAgent{replies: []string{"今天北京晴。", "明天多云。"}}
	deps.Agent = rec.client(t)
	deps.Sessions = session.New(60 * time.Second)
	r := New(deps, nil)
	ctx := context.Background()

	if _, err := r.Route(ctx, "lx06-1", "北京天气怎么样"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Route(ctx, "oh2p-1", "那明天呢"); err != nil {
		t.Fatal(err)
	}
	equalStrings(t, "卧室 LLM 入参（不应含客厅前文）", messagesText(rec.lastCall()), []string{
		"system:你是家里的语音管家，回答简短口语化，一两句话。",
		"user:那明天呢",
	})
}

// 未认领房间时按设备隔离：两台设备各自独立。
func TestMultiTurnUnclaimedDevicesIsolated(t *testing.T) {
	deps, _, _, _ := testDeps(t)
	deps.RoomOfDevice = func(deviceID string) string { return "" }
	rec := &recordingAgent{replies: []string{"今天北京晴。", "明天多云。"}}
	deps.Agent = rec.client(t)
	deps.Sessions = session.New(60 * time.Second)
	r := New(deps, nil)
	ctx := context.Background()

	if _, err := r.Route(ctx, "lx06-1", "北京天气怎么样"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Route(ctx, "oh2p-1", "那明天呢"); err != nil {
		t.Fatal(err)
	}
	equalStrings(t, "oh2p-1 LLM 入参（不应含 lx06-1 前文）", messagesText(rec.lastCall()), []string{
		"system:你是家里的语音管家，回答简短口语化，一两句话。",
		"user:那明天呢",
	})
}

// 过期后重置：30ms 超时，等 100ms 再问，LLM 只收到新一轮。
func TestMultiTurnExpiryResets(t *testing.T) {
	deps, _, _, _ := testDeps(t)
	rec := &recordingAgent{replies: []string{"今天北京晴。", "今天北京晴。"}}
	deps.Agent = rec.client(t)
	deps.Sessions = session.New(30 * time.Millisecond)
	r := New(deps, nil)
	ctx := context.Background()

	if _, err := r.Route(ctx, "lx06-1", "北京天气怎么样"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := r.SweepSessions(time.Now()); n != 1 {
		t.Fatalf("过期后 SweepSessions 应清理 1 个会话，实际 %d", n)
	}
	if _, err := r.Route(ctx, "lx06-1", "那明天呢"); err != nil {
		t.Fatal(err)
	}
	equalStrings(t, "过期后 LLM 入参（应只有新一轮）", messagesText(rec.lastCall()), []string{
		"system:你是家里的语音管家，回答简短口语化，一两句话。",
		"user:那明天呢",
	})
}

// LLM 失败的那轮不进历史：下一轮的入参里没有失败的那句 user。
func TestMultiTurnAgentErrorNotRecorded(t *testing.T) {
	deps, _, _, _ := testDeps(t)
	rec := &recordingAgent{
		replies:      []string{"今天北京晴。", "今天北京晴。"},
		failContains: "会失败",
	}
	deps.Agent = rec.client(t)
	deps.Sessions = session.New(60 * time.Second)
	r := New(deps, nil)
	ctx := context.Background()

	if _, err := r.Route(ctx, "lx06-1", "北京天气怎么样"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Route(ctx, "lx06-1", "这句会失败"); err == nil {
		t.Fatal("桩 LLM 回 500 时 Route 应返回错误")
	}
	if got := rec.callCount(); got != 1 {
		t.Fatalf("失败的调用不应被记为一次成功调用，实际 %d 次", got)
	}
	if _, err := r.Route(ctx, "lx06-1", "明天呢"); err != nil {
		t.Fatal(err)
	}
	equalStrings(t, "失败后的下一轮入参（不应含失败那句）", messagesText(rec.lastCall()), []string{
		"system:你是家里的语音管家，回答简短口语化，一两句话。",
		"user:北京天气怎么样",
		"assistant:今天北京晴。",
		"user:明天呢",
	})
}

// Sessions 为 nil 时保持旧行为：只发本轮，不带历史。
func TestMultiTurnNilSessionsKeepsSingleTurn(t *testing.T) {
	deps, _, _, _ := testDeps(t)
	rec := &recordingAgent{replies: []string{"今天北京晴。", "明天多云。"}}
	deps.Agent = rec.client(t)
	// deps.Sessions 保持 nil。
	r := New(deps, nil)
	ctx := context.Background()

	if _, err := r.Route(ctx, "lx06-1", "北京天气怎么样"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Route(ctx, "lx06-1", "那明天呢"); err != nil {
		t.Fatal(err)
	}
	equalStrings(t, "无 Sessions 时第二轮入参（只发本轮）", messagesText(rec.lastCall()), []string{
		"system:你是家里的语音管家，回答简短口语化，一两句话。",
		"user:那明天呢",
	})
	if n := r.SweepSessions(time.Now()); n != 0 {
		t.Fatalf("无 Sessions 时 SweepSessions 应返回 0，实际 %d", n)
	}
}
