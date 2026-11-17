package web

// P1-22 / P1-9 回归测试：PUT 合并语义与原地更新、OTA GET 状态查询、
// PushOTA 未接线时 POST 回 500。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/huajun/open-xiaoai-next/hub/internal/config"
)

func TestConfigPutMerges(t *testing.T) {
	s, cfgPath := testServer(t)
	// 只改一个字段：其余字段（music_folder、devices）必须原样保留，
	// 而不是被全量替换清空。
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodPut, "/api/config", []byte(`{"wake_word":"你好龙虾"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("部分 PUT 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	// 响应应注明下游连接重启后生效。
	if !strings.Contains(rec.Body.String(), "重启后生效") {
		t.Fatalf("PUT 响应应注明下游连接重启后生效，实际 %s", rec.Body.String())
	}
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.WakeWord != "你好龙虾" {
		t.Fatalf("PUT 后 wake_word 应为 你好龙虾，实际 %q", reloaded.WakeWord)
	}
	if reloaded.MusicFolder != "/volume1/music" {
		t.Fatalf("PUT 合并后 music_folder 应保留，实际 %q", reloaded.MusicFolder)
	}
	if _, ok := reloaded.Devices["lx06-1"]; !ok {
		t.Fatalf("PUT 合并后原有设备 lx06-1 应保留，实际 %+v", reloaded.Devices)
	}
	// 嵌套合并：只加一台设备，原有设备保留。
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodPut, "/api/config",
		[]byte(`{"devices":{"oh2p-1":{"model":"oh2p","ip":"192.168.1.3"}}}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("嵌套合并 PUT 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	reloaded, err = config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.Devices["lx06-1"]; !ok {
		t.Fatalf("嵌套合并后 lx06-1 应保留，实际 %+v", reloaded.Devices)
	}
	if _, ok := reloaded.Devices["oh2p-1"]; !ok {
		t.Fatalf("嵌套合并后 oh2p-1 应新增，实际 %+v", reloaded.Devices)
	}
}

func TestConfigPutUpdatesInPlace(t *testing.T) {
	s, _ := testServer(t)
	// P1-22：PUT 原地更新指针，调用方手里的同一指针应看到新值。
	cfgBefore := s.cfg
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodPut, "/api/config", []byte(`{"wake_word":"你好龙虾"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if s.cfg != cfgBefore {
		t.Fatal("PUT 不应换指针（原地更新），否则 main.go 的 h.cfg 看不到")
	}
	if s.cfg.WakeWord != "你好龙虾" {
		t.Fatalf("同一指针应看到新 wake_word，实际 %q", s.cfg.WakeWord)
	}
}

func TestOTAGetStatus(t *testing.T) {
	s, _ := testServer(t)
	s.PushOTA = func(deviceID, version, url, sha256 string) error { return nil }
	sha := "a56a38619fd98e467adb8c998757f80109c4dac89db48d62dae985e3062476ed"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodPost, "/api/ota",
		[]byte(`{"device_id":"lx06-1","version":"1.94.14","url":"https://example.com/f.bin","sha256":"`+sha+`"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST OTA 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	// GET 单台状态。
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodGet, "/api/ota?device_id=lx06-1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET 单台 OTA 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var job map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&job); err != nil {
		t.Fatal(err)
	}
	if job["Status"] != "pushing" {
		t.Fatalf("单台 OTA 状态应为 pushing，实际 %v", job)
	}
	// GET 全部。
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodGet, "/api/ota", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET 全部 OTA 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var jobs []map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0]["DeviceID"] != "lx06-1" {
		t.Fatalf("GET 全部应含 lx06-1 一条，实际 %v", jobs)
	}
	// GET 不存在的设备 → 404。
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodGet, "/api/ota?device_id=oh2p-1", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET 不存在设备的 OTA 应 404，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

func TestOTAPushOTANilReturns500(t *testing.T) {
	s, _ := testServer(t)
	// PushOTA 未接线：POST 不再回 200 假装成功，且不建任务。
	sha := "a56a38619fd98e467adb8c998757f80109c4dac89db48d62dae985e3062476ed"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodPost, "/api/ota",
		[]byte(`{"device_id":"lx06-1","version":"1.94.14","url":"https://example.com/f.bin","sha256":"`+sha+`"}`)))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("PushOTA 未接线时 POST 应 500，实际 %d: %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodGet, "/api/ota", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET OTA 应 200，实际 %d", rec.Code)
	}
	var jobs []map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("未接线时不应建任何任务，实际 %v", jobs)
	}
}

func TestFingerprintAPI(t *testing.T) {
	s, _ := testServer(t)
	fp := "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	s.HubFingerprint = fp
	// 带 Token → 200，返回指纹。
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodGet, "/api/fingerprint", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/fingerprint 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["fingerprint"] != fp {
		t.Fatalf("指纹应原样返回，实际 %v", body)
	}
	// 无 Token → 401。
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/fingerprint", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无 Token 的 /api/fingerprint 应 401，实际 %d", rec.Code)
	}
	// 未注入指纹（TLS 未启用）→ 503。
	s2, _ := testServer(t)
	rec = httptest.NewRecorder()
	s2.Handler().ServeHTTP(rec, authedReq(http.MethodGet, "/api/fingerprint", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未注入指纹时应 503，实际 %d: %s", rec.Code, rec.Body.String())
	}
}
