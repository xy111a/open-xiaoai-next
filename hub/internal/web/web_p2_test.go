package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// P2-56：未匹配路径返回 404，不再全返回 200 首页——
// 拼错的 API 路径（如 /api/statuss）不应被首页掩盖。
func TestUnknownPathReturns404P2(t *testing.T) {
	s, _ := testServer(t)
	for _, path := range []string{"/api/statuss", "/api/unknown", "/favicon.ico", "/foo/bar"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s 应 404，实际 %d: %s", path, rec.Code, rec.Body.String())
		}
	}
	// "/" 本身仍返回 200 首页。
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / 应 200，实际 %d", rec.Code)
	}
}
