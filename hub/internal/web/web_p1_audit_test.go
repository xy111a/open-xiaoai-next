package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// P1-6（2026-11-23 审查）：PUT 改配置前先写备份，POST /api/config/restore
// 可恢复上一份配置——改错配置不再只能手动翻文件。
func TestConfigBackupAndRestore(t *testing.T) {
	s, cfgPath := testServer(t)
	dir := filepath.Dir(cfgPath)

	// PUT 改唤醒词 → 应产生一份备份，响应里带 backup 备份名。
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodPut, "/api/config", []byte(`{"wake_word":"你好小明"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var putResp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&putResp); err != nil {
		t.Fatal(err)
	}
	bak, _ := putResp["backup"].(string)
	if bak == "" {
		t.Fatalf("PUT 响应应带 backup 备份名，实际 %v", putResp)
	}
	if _, err := os.Stat(filepath.Join(dir, bak)); err != nil {
		t.Fatalf("备份文件应存在：%s: %v", bak, err)
	}

	// GET 确认新值已生效。
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodGet, "/api/config", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "你好小明") {
		t.Fatalf("GET 应看到新唤醒词，实际 %d: %s", rec.Code, rec.Body.String())
	}

	// 再 PUT 改一次（产生第二份备份），然后 restore（无体 = 最新一份）→
	// 回到“你好小明”。
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodPut, "/api/config", []byte(`{"wake_word":"你好小刚"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("第二次 PUT 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodPost, "/api/config/restore", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("restore 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodGet, "/api/config", nil))
	if !strings.Contains(rec.Body.String(), "你好小明") {
		t.Fatalf("restore 后应回到“你好小明”，实际 %s", rec.Body.String())
	}
}

// 无备份时 restore 应 404；带非法备份名时应 400（防路径穿越）。
func TestConfigRestoreErrors(t *testing.T) {
	s, _ := testServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodPost, "/api/config/restore", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("无备份时 restore 应 404，实际 %d: %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodPost, "/api/config/restore", []byte(`{"backup":"../../etc/passwd"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法备份名应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
}
