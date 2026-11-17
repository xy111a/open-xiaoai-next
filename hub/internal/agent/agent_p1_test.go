package agent

// P1-6 回归测试：上游错误响应体截断，不全文回显。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatErrorBodyTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(strings.Repeat("E", 100000)))
	}))
	defer srv.Close()
	c := New(srv.URL, "m", "")
	_, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "你好"}})
	if err == nil {
		t.Fatal("上游 502 时 Chat 应返回错误")
	}
	if len(err.Error()) > 700 {
		t.Fatalf("错误信息应截断到 512 字节摘要，实际 %d 字节", len(err.Error()))
	}
	if strings.Contains(err.Error(), strings.Repeat("E", 1000)) {
		t.Fatal("错误信息不应含上游响应全文")
	}
	_, err = c.ChatStream(context.Background(), []Message{{Role: "user", Content: "你好"}}, nil)
	if err == nil {
		t.Fatal("上游 502 时 ChatStream 应返回错误")
	}
	if len(err.Error()) > 700 {
		t.Fatalf("ChatStream 错误信息应截断，实际 %d 字节", len(err.Error()))
	}
}
