package web

// 多轮对话超时经 Web 面板可改：PUT /api/config 合并 hub.session_timeout_seconds，
// 负数校验失败；GET /api/config 能看到该字段（非敏感字段，不脱敏）。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/huajun/open-xiaoai-next/hub/internal/config"
)

func TestConfigPutSessionTimeout(t *testing.T) {
	s, cfgPath := testServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodPut, "/api/config", []byte(`{"hub":{"session_timeout_seconds":120}}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT session_timeout_seconds 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.SessionTimeout(); got != 120*time.Second {
		t.Fatalf("PUT 后会话超时应为 120s，实际 %v", got)
	}
	// 负数应校验失败（400），不落盘。
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodPut, "/api/config", []byte(`{"hub":{"session_timeout_seconds":-5}}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT 负数超时应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
	reloaded, err = config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.SessionTimeout(); got != 120*time.Second {
		t.Fatalf("失败的 PUT 不应改配置，超时仍应为 120s，实际 %v", got)
	}
	// GET 能看到该字段。
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodGet, "/api/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/config 应 200，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"session_timeout_seconds":120`) {
		t.Fatalf("GET /api/config 应含 session_timeout_seconds:120，实际 %s", rec.Body.String())
	}
}
