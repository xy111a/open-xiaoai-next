package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// /api/status 应报告 KWS 边车状态：未注入时为 disabled 且 kws_down=false。
func TestStatusKWSDefaultDisabled(t *testing.T) {
	s, _ := testServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodGet, "/api/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/status 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var st map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st["kws_status"] != "disabled" {
		t.Fatalf("KWSStatus 未注入时 kws_status 应为 disabled，实际 %v", st["kws_status"])
	}
	if st["kws_down"] != false {
		t.Fatalf("KWSStatus 未注入时 kws_down 应为 false，实际 %v", st["kws_down"])
	}
}

// /api/status 应报告 KWS 边车状态：注入的 KWSStatus 返回什么就报告什么，
// down 时 kws_down=true，ok 时 kws_down=false。
func TestStatusKWSDownAndOK(t *testing.T) {
	for _, tc := range []struct {
		status string
		down   bool
	}{
		{"down", true},
		{"ok", false},
		{"disabled", false},
	} {
		s, _ := testServer(t)
		s.KWSStatus = func() string { return tc.status }
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, authedReq(http.MethodGet, "/api/status", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("/api/status 应 200，实际 %d", rec.Code)
		}
		var st map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&st); err != nil {
			t.Fatal(err)
		}
		if st["kws_status"] != tc.status {
			t.Fatalf("kws_status 应为 %q，实际 %v", tc.status, st["kws_status"])
		}
		if st["kws_down"] != tc.down {
			t.Fatalf("kws_status=%q 时 kws_down 应为 %v，实际 %v", tc.status, tc.down, st["kws_down"])
		}
	}
}
