package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// kwsRestartURL：边车 WS 地址换成 HTTP 重启端点。
func TestKWSRestartURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"ws://127.0.0.1:10400/v1/kws", "http://127.0.0.1:10400/restart"},
		{"wss://127.0.0.1:10400/v1/kws", "https://127.0.0.1:10400/restart"},
		{"ws://127.0.0.1:10400/v1/kws?token=x", "http://127.0.0.1:10400/restart"},
	}
	for _, c := range cases {
		got, err := kwsRestartURL(c.in)
		if err != nil {
			t.Fatalf("kwsRestartURL(%q) 报错：%v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("kwsRestartURL(%q) = %q，想要 %q", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"http://127.0.0.1:10400/v1/kws", "127.0.0.1:10400", "ftp://x/"} {
		if _, err := kwsRestartURL(bad); err == nil {
			t.Fatalf("kwsRestartURL(%q) 应报错", bad)
		}
	}
}

// POST /api/restart：KWS 未启用时直接重启 Hub（回 200，并触发退出）。
func TestRestartKWSOff(t *testing.T) {
	s, _ := testServer(t)
	// testServer 的配置未启用 KWS（Enabled=false），直接走 Hub 重启。
	called := make(chan int, 1)
	old := exitFunc
	exitFunc = func(code int) { called <- code }
	defer func() { exitFunc = old }()

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodPost, "/api/restart", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/restart 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != `{"status":"restarting"}`+"\n" {
		t.Fatalf("响应不对: %q", got)
	}
	select {
	case code := <-called:
		if code != 0 {
			t.Fatalf("退出码应为 0，实际 %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("500ms 后应触发退出，未触发")
	}
}

// POST /api/restart：KWS 已启用但边车连不上时回 502，且不退出 Hub。
func TestRestartSidecarDown(t *testing.T) {
	s, _ := testServer(t)
	s.cfg.KWS.Enabled = true
	s.cfg.KWS.URL = "ws://127.0.0.1:10400/v1/kws" // 测试里无人监听
	called := make(chan int, 1)
	old := exitFunc
	exitFunc = func(code int) { called <- code }
	defer func() { exitFunc = old }()

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodPost, "/api/restart", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("边车连不上时应 502，实际 %d: %s", rec.Code, rec.Body.String())
	}
	select {
	case <-called:
		t.Fatal("边车重启失败时不应退出 Hub")
	case <-time.After(1200 * time.Millisecond):
		// 500ms 的退出窗口已过，未退出即正确。
	}
}

// GET /api/restart 应 405；未鉴权应 401。
func TestRestartMethodAndAuth(t *testing.T) {
	s, _ := testServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodGet, "/api/restart", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/restart 应 405，实际 %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/restart", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未鉴权 POST /api/restart 应 401，实际 %d", rec.Code)
	}
}
