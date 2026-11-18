package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// P2-46：单条 SSE 行超过旧 1MB 上限时，已生成的回答不再整个丢掉——
// 2MB 的长行能完整解析；超 4MB 的行返回已拼部分 + 错误。
func TestChatStreamLongLineP2(t *testing.T) {
	longContent := strings.Repeat("长", 700_000) // 约 2.1MB 的 content，行长 >1MB
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"你好\"}}]}\n\n")
		// 第二行是超长 JSON 行（旧 Scanner 1MB 上限会直接报错丢全文）。
		chunk := fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", longContent)
		fmt.Fprint(w, chunk)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	c := New(srv.URL+"/v1", "test-model", "test-key")
	out, err := c.ChatStream(context.Background(), []Message{{Role: "user", Content: "你好"}}, nil)
	if err != nil {
		t.Fatalf("2MB 长行应能解析，实际 %v", err)
	}
	if out != "你好"+longContent {
		t.Fatalf("长行内容应完整保留，实际长度 %d、期望 %d", len(out), len("你好"+longContent))
	}
}

// P2-46：行超过 4MB 上限时返回已拼部分 + 错误，而不是空串。
func TestChatStreamOversizeLineReturnsPartialP2(t *testing.T) {
	huge := strings.Repeat("x", 5<<20) // 5MB content，行必超 4MB
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"先说\"}}]}\n\n")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", huge)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	c := New(srv.URL+"/v1", "test-model", "test-key")
	out, err := c.ChatStream(context.Background(), []Message{{Role: "user", Content: "你好"}}, nil)
	if err == nil {
		t.Fatal("超 4MB 的行应返回错误")
	}
	if out != "先说" {
		t.Fatalf("出错时应返回已拼接的部分内容“先说”，实际 %q", out)
	}
}
