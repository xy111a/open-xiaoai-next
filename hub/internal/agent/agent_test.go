package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 假 OpenAI 兼容服务：Chat 走普通 JSON，ChatStream 走真 SSE。
func fakeOpenAI(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, "unauthorized")
			return
		}
		var body struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		r.Body.Close()
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"你好，我是管家"}}]}`)
	}))
}

func TestChat(t *testing.T) {
	srv := fakeOpenAI(t)
	defer srv.Close()
	c := New(srv.URL+"/v1", "test-model", "test-key")
	out, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "你好"}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if out != "你好，我是管家" {
		t.Fatalf("Chat 应返回“你好，我是管家”，实际 %q", out)
	}
}

func TestChatStream(t *testing.T) {
	srv := fakeOpenAI(t)
	defer srv.Close()
	c := New(srv.URL+"/v1", "test-model", "test-key")
	var deltas []string
	out, err := c.ChatStream(context.Background(), []Message{{Role: "user", Content: "你好"}}, func(d string) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if out != "你好" {
		t.Fatalf("ChatStream 全文应为“你好”，实际 %q", out)
	}
	if strings.Join(deltas, "") != "你好" {
		t.Fatalf("onDelta 应收到“你”“好”，实际 %v", deltas)
	}
}

func TestChatAuthFailure(t *testing.T) {
	srv := fakeOpenAI(t)
	defer srv.Close()
	c := New(srv.URL+"/v1", "test-model", "wrong-key")
	if _, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "你好"}}); err == nil {
		t.Fatal("错误 key 应返回 401 错误")
	}
}
