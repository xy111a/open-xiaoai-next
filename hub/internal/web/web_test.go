package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huajun/open-xiaoai-next/hub/internal/config"
	"github.com/huajun/open-xiaoai-next/hub/internal/ota"
	"github.com/huajun/open-xiaoai-next/hub/internal/registry"
)

const testWebToken = "test-web-token"

func testServer(t *testing.T) (*Server, string) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := &config.Config{
		WakeWord: "你好小李", MusicFolder: "/volume1/music",
		Devices: map[string]config.Device{"lx06-1": {Model: "lx06", IP: "192.168.1.2", Room: "客厅"}},
	}
	cfg.ApplyDefaults()
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	reg.Upsert(registry.Device{ID: "lx06-1", Model: "lx06", IP: "192.168.1.2", Room: "客厅"})
	reg.SetOnline("lx06-1", true)
	s := New(cfgPath, cfg, reg, ota.New())
	s.AuthToken = testWebToken
	s.OTAHosts = []string{"example.com"}
	return s, cfgPath
}

// authedReq 造一个带正确 Bearer Token 的请求。
func authedReq(method, path string, body []byte) *http.Request {
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, bytes.NewReader(body))
	}
	r.Header.Set("Authorization", "Bearer "+testWebToken)
	return r
}

func TestStatusAndDevices(t *testing.T) {
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
	if st["wake_word"] != "你好小李" || st["devices"] != float64(1) || st["online"] != float64(1) {
		t.Fatalf("/api/status 内容不对: %v", st)
	}

	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodGet, "/api/devices", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/devices 应 200，实际 %d", rec.Code)
	}
	var devs []map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&devs); err != nil {
		t.Fatal(err)
	}
	if len(devs) != 1 || devs[0]["ID"] != "lx06-1" {
		t.Fatalf("/api/devices 应有 lx06-1，实际 %v", devs)
	}
}

func TestConfigPutValidation(t *testing.T) {
	s, cfgPath := testServer(t)
	// 非法型号 → 400，文件不应被改写。
	// P1-22：PUT 体用 snake_case（与 YAML 一致）；PascalCase 字段不再识别。
	bad := `{"wake_word":"你好小李","music_folder":"/volume1/music","devices":{"d1":{"model":"lx07","ip":"192.168.1.9"}}}`
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodPut, "/api/config", []byte(bad)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法配置 PUT 应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
	// 合法配置 → 200，且落盘可重读。
	good := `{"wake_word":"你好小李","music_folder":"/volume1/music","devices":{"lx06-1":{"model":"lx06","ip":"192.168.1.2","room":"客厅"},"oh2p-1":{"model":"oh2p","ip":"192.168.1.3"}}}`
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodPut, "/api/config", []byte(good)))
	if rec.Code != http.StatusOK {
		t.Fatalf("合法配置 PUT 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.Devices["oh2p-1"]; !ok {
		t.Fatalf("PUT 后配置文件应含 oh2p-1，实际 %+v", reloaded.Devices)
	}
	// GET 应返回更新后的配置。
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodGet, "/api/config", nil))
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte("oh2p-1")) {
		t.Fatalf("GET /api/config 应含 oh2p-1，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

func TestOTAAPI(t *testing.T) {
	s, _ := testServer(t)
	var pushed []string
	s.PushOTA = func(deviceID, version, url, sha256 string) error {
		pushed = append(pushed, deviceID+"@"+version)
		return nil
	}
	// 非法 sha → 400。
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodPost, "/api/ota",
		[]byte(`{"device_id":"lx06-1","version":"1.94.14","url":"https://example.com/f.bin","sha256":"zz"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 sha 的 OTA 应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
	// 合法 → 200，PushOTA 被调，job 状态 pushing。
	sha := "a56a38619fd98e467adb8c998757f80109c4dac89db48d62dae985e3062476ed"
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodPost, "/api/ota",
		[]byte(`{"device_id":"lx06-1","version":"1.94.14","url":"https://example.com/f.bin","sha256":"`+sha+`"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("合法 OTA 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if len(pushed) != 1 || pushed[0] != "lx06-1@1.94.14" {
		t.Fatalf("PushOTA 应被调一次，实际 %v", pushed)
	}
	var job map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&job); err != nil {
		t.Fatal(err)
	}
	if job["Status"] != "pushing" {
		t.Fatalf("OTA job 状态应为 pushing，实际 %v", job)
	}
}

// P0-1：无 Token / 错误 Token 的 GET/PUT/POST 必须 401；正确 Token 才放行。
func TestAPIRequiresAuth(t *testing.T) {
	s, _ := testServer(t)
	// 无 Token。
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/status"},
		{http.MethodGet, "/api/devices"},
		{http.MethodGet, "/api/config"},
		{http.MethodPut, "/api/config"},
		{http.MethodPost, "/api/ota"},
	} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("无 Token 的 %s %s 应 401，实际 %d", tc.method, tc.path, rec.Code)
		}
	}
	// 错误 Token（用新 server，避免上面 5 次失败触发限流干扰断言）。
	s2, _ := testServer(t)
	rec := httptest.NewRecorder()
	bad := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	bad.Header.Set("Authorization", "Bearer wrong-token")
	s2.Handler().ServeHTTP(rec, bad)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("错误 Token 应 401，实际 %d", rec.Code)
	}
	// 正确 Token。
	rec = httptest.NewRecorder()
	s2.Handler().ServeHTTP(rec, authedReq(http.MethodGet, "/api/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("正确 Token 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// P0-1：未配置 Token 时 /api/* 一律 401（fail closed），即使请求带了 Token。
func TestAPIWithoutTokenConfigured(t *testing.T) {
	s, _ := testServer(t)
	s.AuthToken = ""
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodGet, "/api/status", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未配置 Token 时应 401，实际 %d", rec.Code)
	}
}

// P0-1：GET /api/config 不得返回敏感字段。
func TestConfigGetNoSensitiveFields(t *testing.T) {
	s, _ := testServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodGet, "/api/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/config 应 200，实际 %d", rec.Code)
	}
	body := strings.ToLower(rec.Body.String())
	for _, sub := range []string{"token", "password", "passwd", "secret", "apikey", "api_key", "private_key", "credential"} {
		if strings.Contains(body, sub) {
			t.Fatalf("GET /api/config 泄露敏感字段 %q：%s", sub, rec.Body.String())
		}
	}
	if !strings.Contains(body, "lx06-1") {
		t.Fatalf("GET /api/config 应含设备 lx06-1，实际 %s", rec.Body.String())
	}
}

// P0-1：PUT 不许把 hub.tls 从 true 改为 false；非法下游地址被拒绝。
func TestConfigPutRejectsTLSDowngradeAndPublicURL(t *testing.T) {
	s, _ := testServer(t)
	// tls 显式 false → 400。
	downgrade := `{"hub":{"tls":false}}`
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodPut, "/api/config", []byte(downgrade)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "hub.tls") {
		t.Fatalf("TLS 降级 PUT 应 400 且提示 hub.tls，实际 %d: %s", rec.Code, rec.Body.String())
	}
	// 公网 agent 地址：http 明文 → 400；https → 200（插件式 Agent：
	// 云端 LLM 的 API 本来就在公网，Bearer 密钥走 TLS，见 config.validateAgentURL）。
	publicHTTP := `{"agents":{"hermes":{"base_url":"http://203.0.113.1:8642/v1","model":"deepseek-chat"}}}`
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodPut, "/api/config", []byte(publicHTTP)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("http 公网下游地址 PUT 应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
	publicHTTPS := `{"agents":{"hermes":{"base_url":"https://api.example.com/v1","model":"deepseek-chat"}}}`
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodPut, "/api/config", []byte(publicHTTPS)))
	if rec.Code != http.StatusOK {
		t.Fatalf("https 公网 Agent PUT 应 200（云端 LLM 允许），实际 %d: %s", rec.Code, rec.Body.String())
	}
	// file:// 网易云 → 400。
	fileURL := `{"netease":{"api":"file:///etc/passwd"}}`
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodPut, "/api/config", []byte(fileURL)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("file:// 下游地址 PUT 应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// P0-1：OTA 只许 https + 白名单 host。
func TestOTAURLWhitelist(t *testing.T) {
	s, _ := testServer(t)
	s.PushOTA = func(deviceID, version, url, sha256 string) error { return nil }
	sha := "a56a38619fd98e467adb8c998757f80109c4dac89db48d62dae985e3062476ed"
	// http（非 https）→ 400。
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodPost, "/api/ota",
		[]byte(`{"device_id":"lx06-1","version":"1.94.14","url":"http://example.com/f.bin","sha256":"`+sha+`"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("http OTA 应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
	// https 但 host 不在白名单 → 400。
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodPost, "/api/ota",
		[]byte(`{"device_id":"lx06-1","version":"1.94.14","url":"https://evil.example.org/f.bin","sha256":"`+sha+`"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("白名单外 host 的 OTA 应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
	// 未配置白名单 → 一律拒绝。
	s.OTAHosts = nil
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodPost, "/api/ota",
		[]byte(`{"device_id":"lx06-1","version":"1.94.14","url":"https://example.com/f.bin","sha256":"`+sha+`"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("无白名单时 OTA 应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// P0-1：鉴权失败多次后限流（第 6 次失败返回 429）。
func TestAuthRateLimit(t *testing.T) {
	s, _ := testServer(t)
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		bad := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		bad.Header.Set("Authorization", "Bearer wrong-token")
		s.Handler().ServeHTTP(rec, bad)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次失败应 401，实际 %d", i+1, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	bad := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	bad.Header.Set("Authorization", "Bearer wrong-token")
	s.Handler().ServeHTTP(rec, bad)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("第 6 次失败应 429，实际 %d", rec.Code)
	}
}

// P0-1：NewHTTPServer 带读写/空闲超时，且默认监听回环地址。
func TestNewHTTPServerTimeouts(t *testing.T) {
	s, _ := testServer(t)
	srv := s.NewHTTPServer()
	if srv.ReadHeaderTimeout <= 0 || srv.ReadTimeout <= 0 || srv.WriteTimeout <= 0 || srv.IdleTimeout <= 0 {
		t.Fatalf("http.Server 超时未设置：%+v", srv)
	}
	if srv.Addr != "127.0.0.1:8080" {
		t.Fatalf("Web 默认监听应为 127.0.0.1:8080，实际 %q", srv.Addr)
	}
}
