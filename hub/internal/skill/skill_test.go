package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMatch(t *testing.T) {
	r := New([]Skill{
		{Name: "news", Triggers: []string{"新闻", "今日头条"}, Endpoint: "http://127.0.0.1:18080/x"},
		{Name: "weather", Triggers: []string{"天气"}, Endpoint: "http://127.0.0.1:18080/y"},
	})
	if got := r.Match("今天有什么新闻"); got == nil || got.Name != "news" {
		t.Fatalf("含“新闻”应命中 news，实际 %+v", got)
	}
	if got := r.Match("今天天气怎么样"); got == nil || got.Name != "weather" {
		t.Fatalf("含“天气”应命中 weather，实际 %+v", got)
	}
	if got := r.Match("随便聊聊"); got != nil {
		t.Fatalf("无触发词时应返回 nil，实际 %+v", got)
	}
	// 多个技能都命中时按配置顺序取第一个。
	r2 := New([]Skill{
		{Name: "first", Triggers: []string{"新闻"}, Endpoint: "http://127.0.0.1:18080/a"},
		{Name: "second", Triggers: []string{"新闻头条"}, Endpoint: "http://127.0.0.1:18080/b"},
	})
	if got := r2.Match("新闻头条"); got == nil || got.Name != "first" {
		t.Fatalf("都命中时应取配置顺序第一个，实际 %+v", got)
	}
	// 空表不命中。
	if got := New(nil).Match("新闻"); got != nil {
		t.Fatalf("没配技能时 Match 应返回 nil，实际 %+v", got)
	}
}

func TestCallJSONReply(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("技能调用应用 POST，实际 %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("技能调用应为 application/json，实际 %q", ct)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"reply": "今日头条：测试新闻。 "}`)
	}))
	defer srv.Close()
	r := New([]Skill{{Name: "news", Triggers: []string{"新闻"}, Endpoint: srv.URL}})
	reply, err := r.Call(context.Background(), &r.skills[0], "lx06-1", "今天有什么新闻")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if reply != "今日头条：测试新闻。" {
		t.Fatalf("应返回端点的 reply（去空白），实际 %q", reply)
	}
	if gotBody["text"] != "今天有什么新闻" || gotBody["device_id"] != "lx06-1" {
		t.Fatalf("POST 体应含 text 与 device_id，实际 %v", gotBody)
	}
}

func TestCallPlainTextReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "  纯文本回复。\n")
	}))
	defer srv.Close()
	r := New([]Skill{{Name: "echo", Triggers: []string{"回声"}, Endpoint: srv.URL}})
	reply, err := r.Call(context.Background(), &r.skills[0], "d1", "回声测试")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if reply != "纯文本回复。" {
		t.Fatalf("纯文本回复应原样返回（去空白），实际 %q", reply)
	}
}

func TestCallTextFieldReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"text": "text 字段的回复"}`)
	}))
	defer srv.Close()
	r := New([]Skill{{Name: "t", Triggers: []string{"x"}, Endpoint: srv.URL}})
	reply, err := r.Call(context.Background(), &r.skills[0], "d1", "x")
	if err != nil || reply != "text 字段的回复" {
		t.Fatalf("{\"text\": ...} 也应接受，实际 %q, %v", reply, err)
	}
}

func TestCallErrors(t *testing.T) {
	// 非 200。
	srv500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "boom")
	}))
	defer srv500.Close()
	r := New([]Skill{{Name: "bad", Triggers: []string{"x"}, Endpoint: srv500.URL}})
	if _, err := r.Call(context.Background(), &r.skills[0], "d1", "x"); err == nil {
		t.Fatal("500 应返回错误")
	}
	// 空回复。
	srvEmpty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"reply": ""}`)
	}))
	defer srvEmpty.Close()
	r2 := New([]Skill{{Name: "empty", Triggers: []string{"x"}, Endpoint: srvEmpty.URL}})
	if _, err := r2.Call(context.Background(), &r2.skills[0], "d1", "x"); err == nil {
		t.Fatal("空回复应返回错误（调用方落回 Agent）")
	}
	// 超时（默认 10s 太长，配 1s 测）。
	srvSlow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
		fmt.Fprint(w, "late")
	}))
	defer srvSlow.Close()
	r3 := New([]Skill{{Name: "slow", Triggers: []string{"x"}, Endpoint: srvSlow.URL, TimeoutSeconds: 1}})
	start := time.Now()
	if _, err := r3.Call(context.Background(), &r3.skills[0], "d1", "x"); err == nil {
		t.Fatal("超时应返回错误")
	} else if time.Since(start) > 2*time.Second {
		t.Fatalf("超时应在 1s 量级返回，实际 %s", time.Since(start))
	}
	// 不存在的端点（连接被拒）。
	r4 := New([]Skill{{Name: "dead", Triggers: []string{"x"}, Endpoint: "http://127.0.0.1:1/nope"}})
	if _, err := r4.Call(context.Background(), &r4.skills[0], "d1", "x"); err == nil {
		t.Fatal("连不上端点应返回错误")
	} else if !strings.Contains(err.Error(), "dead") {
		t.Fatalf("错误应带技能名，实际 %v", err)
	}
}
