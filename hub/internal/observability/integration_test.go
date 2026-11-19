package observability

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Mock 集成测试：假服务（200 / 500 / 超时）+ 假交互（已知分段时长），
// 校验 HealthReport 与延迟列表序列化后的 JSON 契约——
// 即 Web 面板 /api/health 与 /api/latency 将原样返回的结构。
// Go 运行时才能跑；本地等价物见同目录 mock_contract_check.py。
func TestMockIntegrationHealthAndLatencyContract(t *testing.T) {
	// --- 假服务：kws 200、netease 500、snapserver 超时 ---
	kwsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer kwsSrv.Close()
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer badSrv.Close()
	slowSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer slowSrv.Close()

	checker := NewChecker([]Service{
		{Name: "hub", Container: "oxn-hub", Kind: KindSelf, Address: "本进程", Enabled: true},
		{Name: "kws-sidecar", Container: "oxn-kws-sidecar", Kind: KindHTTP, Address: kwsSrv.URL + "/health", Enabled: true},
		{Name: "netease-api", Container: "oxn-netease-api", Kind: KindHTTP, Address: badSrv.URL, Enabled: true},
		{Name: "snapserver", Container: "oxn-snapserver", Kind: KindHTTP, Address: slowSrv.URL, Enabled: true},
	}, 500*time.Millisecond)
	rep := checker.Check(context.Background())

	// 序列化再按契约反序列化：面板收到的就是这份 JSON。
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"services", "checked_at", "healthy", "total", "down"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("health JSON 缺字段 %q：%s", key, raw)
		}
	}
	services, ok := decoded["services"].([]any)
	if !ok || len(services) != 4 {
		t.Fatalf("health JSON services 应为 4 项，实际 %s", raw)
	}
	byName := map[string]map[string]any{}
	for _, s := range services {
		m := s.(map[string]any)
		byName[m["name"].(string)] = m
	}
	if byName["hub"]["status"] != "ok" {
		t.Fatalf("hub 应 ok，实际 %s", raw)
	}
	if byName["kws-sidecar"]["status"] != "ok" {
		t.Fatalf("kws 200 应 ok，实际 %s", raw)
	}
	if byName["netease-api"]["status"] != "down" {
		t.Fatalf("netease 500 应 down，实际 %s", raw)
	}
	if byName["snapserver"]["status"] != "down" {
		t.Fatalf("snapserver 超时应 down，实际 %s", raw)
	}
	if decoded["healthy"] != float64(2) || decoded["down"] != float64(2) || decoded["total"] != float64(4) {
		t.Fatalf("汇总应为 healthy=2 down=2 total=4，实际 %s", raw)
	}
	for _, m := range byName {
		for _, key := range []string{"name", "container", "kind", "address", "status", "latency_ms", "detail"} {
			if _, ok := m[key]; !ok {
				t.Fatalf("service 健康项缺字段 %q：%s", key, raw)
			}
		}
	}

	// --- 假交互：已知分段时长 ---
	rec := NewRecorder(20)
	rec.NoteWake("lx06-1", 120)
	inter := rec.Begin("lx06-1", "voice")
	rec.RecordStage(inter, StageSTT, 900*time.Millisecond)
	rec.RecordStageForDevice("lx06-1", StageLLM, 800*time.Millisecond)
	rec.RecordStage(inter, StageTTS, 600*time.Millisecond)
	rec.RecordMusic("lx06-1", 1500*time.Millisecond) // 写进同一条语音交互
	rec.Finish(inter, nil)
	// 一次独立点歌（无进行中问答）。
	rec.RecordMusic("oh2p-1", 2000*time.Millisecond)

	payload := map[string]any{"interactions": rec.Recent(), "count": len(rec.Recent())}
	raw, err = json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var latDecoded map[string]any
	if err := json.Unmarshal(raw, &latDecoded); err != nil {
		t.Fatal(err)
	}
	items, ok := latDecoded["interactions"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("latency JSON 应有 2 条交互，实际 %s", raw)
	}
	// 倒序：最新（独立点歌）在前。
	music := items[0].(map[string]any)
	if music["kind"] != "music" || music["device_id"] != "oh2p-1" {
		t.Fatalf("第一条应为 oh2p-1 的独立点歌，实际 %s", raw)
	}
	voice := items[1].(map[string]any)
	if voice["kind"] != "voice" {
		t.Fatalf("第二条应为语音问答，实际 %s", raw)
	}
	stages, ok := voice["stages"].(map[string]any)
	if !ok {
		t.Fatalf("语音交互缺 stages：%s", raw)
	}
	wantStages := map[string]float64{
		"kws_ms": 120, "stt_ms": 900, "llm_ms": 800, "tts_ms": 600, "music_ms": 1500,
	}
	for k, want := range wantStages {
		if stages[k] != want {
			t.Fatalf("语音交互 stages[%q] 应为 %v，实际 %v（%s）", k, want, stages[k], raw)
		}
	}
	if voice["ok"] != true {
		t.Fatalf("成功交互 ok 应为 true，实际 %s", raw)
	}
	if _, hasErr := voice["error"]; hasErr {
		t.Fatalf("成功交互不应有 error 字段，实际 %s", raw)
	}
	if latDecoded["count"] != float64(2) {
		t.Fatalf("count 应为 2，实际 %s", raw)
	}
}
