package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/huajun/open-xiaoai-next/hub/internal/observability"
)

// 造一个已记了两条交互的 Recorder：一条完整语音问答
// （kws/stt/llm/tts/music 五段全有），一条独立点歌。
func seededRecorder() *observability.Recorder {
	r := observability.NewRecorder(20)
	r.NoteWake("lx06-1", 120)
	inter := r.Begin("lx06-1", "voice")
	r.RecordStage(inter, observability.StageSTT, 900*time.Millisecond)
	r.RecordStageForDevice("lx06-1", observability.StageLLM, 800*time.Millisecond)
	r.RecordStage(inter, observability.StageTTS, 600*time.Millisecond)
	r.RecordMusic("lx06-1", 1500*time.Millisecond)
	r.Finish(inter, nil)
	r.RecordMusic("oh2p-1", 2000*time.Millisecond)
	return r
}

func TestLatencyAPI(t *testing.T) {
	s, _ := testServer(t)
	s.Latency = seededRecorder()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodGet, "/api/latency", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/latency 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["count"] != float64(2) {
		t.Fatalf("count 应为 2，实际 %v", body)
	}
	items, ok := body["interactions"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("interactions 应有 2 条，实际 %v", body)
	}
	// 倒序：独立点歌（最新）在前。
	if items[0].(map[string]any)["kind"] != "music" {
		t.Fatalf("第一条应为 music，实际 %v", items[0])
	}
	voice := items[1].(map[string]any)
	stages := voice["stages"].(map[string]any)
	for k, want := range map[string]float64{
		"kws_ms": 120, "stt_ms": 900, "llm_ms": 800, "tts_ms": 600, "music_ms": 1500,
	} {
		if stages[k] != want {
			t.Fatalf("stages[%q] 应为 %v，实际 %v", k, want, stages[k])
		}
	}
}

// Latency 未注入时 /api/latency 返回空列表，不 503。
func TestLatencyAPIWithoutRecorder(t *testing.T) {
	s, _ := testServer(t)
	s.Latency = nil
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodGet, "/api/latency", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("无 Recorder 时 /api/latency 应 200 空列表，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["count"] != float64(0) {
		t.Fatalf("无 Recorder 时 count 应为 0，实际 %v", body)
	}
}

func TestLatencyAPIRequiresAuthAndGetOnly(t *testing.T) {
	s, _ := testServer(t)
	s.Latency = seededRecorder()
	// 无 Token → 401。
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/latency", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无 Token 的 /api/latency 应 401，实际 %d", rec.Code)
	}
	// POST → 405。
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodPost, "/api/latency", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/latency 应 405，实际 %d", rec.Code)
	}
}

// Health：一个 200 的假 HTTP 服务 + 一个不通的端口，
// 再加 Hub 自身；校验 status 分类与汇总数。
func TestHealthAPI(t *testing.T) {
	s, _ := testServer(t)
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer okSrv.Close()
	s.Health = observability.NewChecker([]observability.Service{
		{Name: "hub", Container: "oxn-hub", Kind: observability.KindSelf, Address: "本进程", Enabled: true},
		{Name: "kws-sidecar", Container: "oxn-kws-sidecar", Kind: observability.KindHTTP, Address: okSrv.URL + "/health", Enabled: true},
		{Name: "whisper", Container: "oxn-whisper", Kind: observability.KindWyoming, Address: "127.0.0.1:1", Enabled: true},
		{Name: "kws-disabled", Container: "oxn-kws-sidecar", Kind: observability.KindHTTP, Address: "", Enabled: false},
	}, 500*time.Millisecond)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/health 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var rep map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&rep); err != nil {
		t.Fatal(err)
	}
	if rep["total"] != float64(4) || rep["healthy"] != float64(2) || rep["down"] != float64(1) {
		t.Fatalf("汇总应为 total=4 healthy=2 down=1（disabled 不计），实际 %v", rep)
	}
	services := rep["services"].([]any)
	byName := map[string]map[string]any{}
	for _, sv := range services {
		m := sv.(map[string]any)
		byName[m["name"].(string)] = m
	}
	if byName["kws-sidecar"]["status"] != "ok" {
		t.Fatalf("200 的假服务应 ok，实际 %v", byName["kws-sidecar"])
	}
	if byName["whisper"]["status"] != "down" {
		t.Fatalf("端口不通应 down，实际 %v", byName["whisper"])
	}
	if byName["kws-disabled"]["status"] != "disabled" {
		t.Fatalf("未启用应 disabled，实际 %v", byName["kws-disabled"])
	}
}

// Health 未注入时 /api/health 回 503；无 Token 时 401；POST 时 405。
func TestHealthAPINotWiredAuthAndMethod(t *testing.T) {
	s, _ := testServer(t)
	s.Health = nil
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未接线时 /api/health 应 503，实际 %d: %s", rec.Code, rec.Body.String())
	}
	s.Health = observability.NewChecker(nil, 0)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无 Token 的 /api/health 应 401，实际 %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodPost, "/api/health", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/health 应 405，实际 %d", rec.Code)
	}
}

// /api/status 附最新一条延迟记录；没有记录时为 null。
func TestStatusIncludesLatestLatency(t *testing.T) {
	s, _ := testServer(t)
	s.Latency = seededRecorder()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodGet, "/api/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/status 应 200，实际 %d", rec.Code)
	}
	var st map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	latest, ok := st["latest_latency"].(map[string]any)
	if !ok {
		t.Fatalf("/api/status 应含 latest_latency，实际 %v", st)
	}
	// 最新一条是独立点歌（oh2p-1）。
	if latest["device_id"] != "oh2p-1" || latest["kind"] != "music" {
		t.Fatalf("latest_latency 应为最新一条（oh2p-1 点歌），实际 %v", latest)
	}

	s2, _ := testServer(t)
	rec = httptest.NewRecorder()
	s2.Handler().ServeHTTP(rec, authedReq(http.MethodGet, "/api/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/status 应 200，实际 %d", rec.Code)
	}
	var st2 map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&st2); err != nil {
		t.Fatal(err)
	}
	if st2["latest_latency"] != nil {
		t.Fatalf("无记录时 latest_latency 应为 null，实际 %v", st2)
	}
}

// 首页含系统健康与延迟区、调 /api/health 与 /api/latency、
// 带 viewport（320px 不横向溢出的前提之一）。
func TestIndexRendersSystemPage(t *testing.T) {
	s, _ := testServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / 应 200，实际 %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("首页 Content-Type 应为 text/html，实际 %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"系统健康", "语音延迟", "最近交互",
		"/api/health", "/api/latency", "/api/status", "/api/devices",
		"width=device-width", "tablewrap", "oxn-web-token",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("首页应含 %q，实际缺失", want)
		}
	}
	// 旧占位（无鉴权的裸 fetch）已删：首页不再有 id="s" 的裸 JSON 区。
	if strings.Contains(body, `id="s"`) {
		t.Fatal("首页不应再含旧占位 id=\"s\"")
	}
	// 首页本身不要求 Token（Token 在 JS 里走 sessionStorage 发）。
}
