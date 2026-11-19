package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/huajun/open-xiaoai-next/hub/internal/config"
)

// 插件配置经面板可读写：api_key_env 只是变量名、不是密钥，
// GET 不应脱敏掉它；agents[].system_prompt 经 PUT 可改且落盘。
func TestConfigPluginFieldsViaPanel(t *testing.T) {
	s, cfgPath := testServer(t)
	// 先 PUT 一个带插件字段的 Agent 与技能。
	patch := `{"agents":{"deepseek":{"base_url":"https://api.deepseek.com/v1","model":"deepseek-chat","api_key_env":"DEEPSEEK_API_KEY","system_prompt":"你是测试管家。"}},"skills":[{"name":"news","triggers":["新闻"],"endpoint":"http://127.0.0.1:18080/v1/skill/news","timeout_seconds":10}],"music":{"primary":"netease","fallback":"local"}}`
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodPut, "/api/config", []byte(patch)))
	if rec.Code != http.StatusOK {
		t.Fatalf("插件配置 PUT 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	ag, ok := reloaded.Agents["deepseek"]
	if !ok || ag.APIKeyEnv != "DEEPSEEK_API_KEY" || ag.SystemPrompt != "你是测试管家。" {
		t.Fatalf("PUT 后 deepseek Agent 应落盘，实际 %+v", reloaded.Agents)
	}
	if len(reloaded.Skills) != 1 || reloaded.Skills[0].Name != "news" {
		t.Fatalf("PUT 后技能应落盘，实际 %+v", reloaded.Skills)
	}
	if reloaded.Music.Primary != "netease" || reloaded.Music.Fallback != "local" {
		t.Fatalf("PUT 后音源顺序应落盘，实际 %+v", reloaded.Music)
	}
	// GET：api_key_env 必须可见（它是变量名，不是密钥）；
	// skills 与 music 也应可见。
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodGet, "/api/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/config 应 200，实际 %d", rec.Code)
	}
	var view map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	agents, _ := view["agents"].(map[string]any)
	ds, _ := agents["deepseek"].(map[string]any)
	if ds["api_key_env"] != "DEEPSEEK_API_KEY" {
		t.Fatalf("GET 不应脱敏 api_key_env（只是变量名），实际 %v", ds)
	}
	if ds["system_prompt"] != "你是测试管家。" {
		t.Fatalf("GET 应含 system_prompt，实际 %v", ds)
	}
	if _, ok := view["skills"]; !ok {
		t.Fatalf("GET 应含 skills，实际 %v", view)
	}
	// 公网 http 的 Agent 经面板也应被拦（与 config.Validate 一致）。
	bad := `{"agents":{"evil":{"base_url":"http://203.0.113.1:8642/v1","model":"m"}}}`
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedReq(http.MethodPut, "/api/config", []byte(bad)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("http 公网 Agent PUT 应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("https")) {
		t.Fatalf("400 提示应指引改用 https，实际 %s", rec.Body.String())
	}
}
