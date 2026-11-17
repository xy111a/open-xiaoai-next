// Package web 是 Web 面板（Phase 4）：状态查看 + config.yaml 读写 + 一键 OTA。
// 只做内网管理界面，不对外暴露；改配置走 API，写文件前先校验。
//
// P0-1 安全模型：
//   - /api/* 全部要求 Bearer Token 鉴权（与 OPEN_XIAOAI_TOKEN 独立的
//     OPEN_XIAOAI_WEB_TOKEN，见 New）；未配置 Token 时 /api/* 一律 401
//     （fail closed）。比较用 SHA-256 后恒定时间比较；鉴权失败记日志
//     （只记 IP 与路径，永不记 Token）并按 IP 限流。
//   - GET /api/config 只返回脱敏视图：token/password/secret/key 类字段一律剔除。
//   - PUT /api/config 不许把 hub.tls 从 true 改为 false（防降级为明文 ws）。
//   - POST /api/ota 只许 https + 白名单 host（Server.OTAHosts，
//     或环境变量 OPEN_XIAOAI_OTA_HOSTS 逗号分隔）；未配置白名单时一律拒绝。
//   - 请求体上限 1MB；带超时的 http.Server 由 NewHTTPServer 构造
//     （main 需改用它代替 http.ListenAndServe 才能生效）。
package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/huajun/open-xiaoai-next/hub/internal/config"
	"github.com/huajun/open-xiaoai-next/hub/internal/ota"
	"github.com/huajun/open-xiaoai-next/hub/internal/registry"
)

// maxRequestBody 是 PUT/POST 请求体上限（1MB）。
const maxRequestBody = 1 << 20

// maxAuthFailures 是触发限流的失败次数，authFailWindow 是统计窗口，
// authBlockDuration 是限流时长。
const (
	maxAuthFailures   = 5
	authFailWindow    = time.Minute
	authBlockDuration = 5 * time.Minute
)

// Server 是 Web 面板服务。
type Server struct {
	cfgPath string
	mu      sync.Mutex
	cfg     *config.Config
	reg     *registry.Registry
	otaMgr  *ota.Manager
	// PushOTA 是 OTA 下发的实际动作（由 main 注入：发 ota 消息给设备）。
	PushOTA func(deviceID, version, url, sha256 string) error

	// AuthToken 是 /api/* 的 Bearer Token，与 OPEN_XIAOAI_TOKEN 独立。
	// New 从环境变量 OPEN_XIAOAI_WEB_TOKEN 读取；也可由调用方直接赋值。
	// 为空时 /api/* 一律 401（fail closed）。
	AuthToken string
	// OTAHosts 是 OTA URL 白名单（只比 host，不含端口）。
	// New 从环境变量 OPEN_XIAOAI_OTA_HOSTS（逗号分隔）读取；也可直接赋值。
	// 为空时 OTA 一律拒绝。
	OTAHosts []string

	// KWSStatus 由 main 注入，返回边车状态："ok" / "down" / "disabled"。
	// 为 nil 时视为 "disabled"（KWS 未启用）。
	// /api/status 据此报告 kws_status 与 kws_down。
	KWSStatus func() string

	authMu       sync.Mutex
	authFailures map[string]*authAttempt
}

type authAttempt struct {
	failures     []time.Time
	blockedUntil time.Time
}

// New 新建 Server。
func New(cfgPath string, cfg *config.Config, reg *registry.Registry, otaMgr *ota.Manager) *Server {
	s := &Server{cfgPath: cfgPath, cfg: cfg, reg: reg, otaMgr: otaMgr, authFailures: map[string]*authAttempt{}}
	s.AuthToken = os.Getenv("OPEN_XIAOAI_WEB_TOKEN")
	if hosts := os.Getenv("OPEN_XIAOAI_OTA_HOSTS"); hosts != "" {
		for _, h := range strings.Split(hosts, ",") {
			if h = strings.TrimSpace(h); h != "" {
				s.OTAHosts = append(s.OTAHosts, strings.ToLower(h))
			}
		}
	}
	return s
}

// NewHTTPServer 返回带超时的 http.Server（P0-1：main 应改用它监听 WebListen）。
func (s *Server) NewHTTPServer() *http.Server {
	addr := "127.0.0.1:8080"
	s.mu.Lock()
	if s.cfg != nil && s.cfg.Hub.WebListen != "" {
		addr = s.cfg.Hub.WebListen
	}
	s.mu.Unlock()
	return &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

// Handler 返回 HTTP handler。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.index)
	mux.HandleFunc("/api/status", s.requireAuth(s.status))
	mux.HandleFunc("/api/devices", s.requireAuth(s.devices))
	mux.HandleFunc("/api/config", s.requireAuth(s.configAPI))
	mux.HandleFunc("/api/ota", s.requireAuth(s.otaAPI))
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// clientIP 取对端 IP（只用于日志与限流）。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// checkBearer 用恒定时间比较校验 Authorization: Bearer <token>。
// Token 为空（未配置）时一律失败。
func checkBearer(expected string, r *http.Request) bool {
	if expected == "" {
		return false
	}
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, prefix) {
		return false
	}
	got := strings.TrimSpace(auth[len(prefix):])
	// 先哈希再比较：避免长度信息泄露，比较本身恒定时间。
	gotSum := sha256.Sum256([]byte(got))
	expSum := sha256.Sum256([]byte(expected))
	return subtle.ConstantTimeCompare(gotSum[:], expSum[:]) == 1
}

// requireAuth 是 /api/* 的鉴权中间件：失败记日志（不记 Token）并按 IP 限流。
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		s.authMu.Lock()
		att, ok := s.authFailures[ip]
		if ok && time.Now().Before(att.blockedUntil) {
			s.authMu.Unlock()
			log.Printf("web 鉴权限流：%s %s（失败过多，暂拒）", ip, r.URL.Path)
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "鉴权失败过多，稍后再试"})
			return
		}
		s.authMu.Unlock()

		if !checkBearer(s.AuthToken, r) {
			s.authMu.Lock()
			if s.authFailures == nil {
				s.authFailures = map[string]*authAttempt{}
			}
			att := s.authFailures[ip]
			if att == nil {
				att = &authAttempt{}
				s.authFailures[ip] = att
			}
			now := time.Now()
			// 只保留窗口内的失败。
			kept := att.failures[:0]
			for _, t := range att.failures {
				if now.Sub(t) <= authFailWindow {
					kept = append(kept, t)
				}
			}
			att.failures = append(kept, now)
			if len(att.failures) >= maxAuthFailures {
				att.blockedUntil = now.Add(authBlockDuration)
			}
			s.authMu.Unlock()
			if s.AuthToken == "" {
				log.Printf("web 鉴权失败：%s %s（未配置 OPEN_XIAOAI_WEB_TOKEN）", ip, r.URL.Path)
			} else {
				log.Printf("web 鉴权失败：%s %s", ip, r.URL.Path)
			}
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "未鉴权：需 Bearer Token"})
			return
		}
		// 成功即清零该 IP 的失败计数。
		s.authMu.Lock()
		delete(s.authFailures, ip)
		s.authMu.Unlock()
		next(w, r)
	}
}

// sensitiveKey 判断 JSON 字段名是否敏感（token/password/secret/key 类）。
func sensitiveKey(k string) bool {
	lk := strings.ToLower(k)
	for _, sub := range []string{"token", "password", "passwd", "secret", "apikey", "api_key", "api-key", "private_key", "privatekey", "credential"} {
		if strings.Contains(lk, sub) {
			return true
		}
	}
	return false
}

// sanitizeConfig 把配置转成脱敏视图：递归剔除敏感字段。
func sanitizeConfig(cfg *config.Config) any {
	b, err := json.Marshal(cfg)
	if err != nil {
		return map[string]string{"error": "配置序列化失败"}
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return map[string]string{"error": "配置序列化失败"}
	}
	return sanitizeValue(v)
}

func sanitizeValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if sensitiveKey(k) {
				continue
			}
			out[k] = sanitizeValue(val)
		}
		return out
	case []any:
		for i, e := range t {
			t[i] = sanitizeValue(e)
		}
		return t
	default:
		return v
	}
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><title>open-xiaoai-next</title>
<style>body{font-family:system-ui;max-width:720px;margin:40px auto;padding:0 16px}table{border-collapse:collapse}td,th{border:1px solid #ccc;padding:6px 10px}</style>
</head><body><h1>open-xiaoai-next</h1><p>Hub 运行中。用 <code>/api/status</code>、<code>/api/devices</code>、<code>/api/config</code>、<code>/api/ota</code> 管理。</p>
<div id="s">加载中…</div><script>
fetch('/api/status').then(r=>r.json()).then(j=>{document.getElementById('s').innerHTML='<pre>'+JSON.stringify(j,null,2)+'</pre>'});
</script></body></html>`)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 GET"})
		return
	}
	s.mu.Lock()
	cfg := s.cfg
	kwsFn := s.KWSStatus
	s.mu.Unlock()
	kwsStatus := "disabled"
	if kwsFn != nil {
		kwsStatus = kwsFn()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"wake_word": cfg.WakeWord,
		"rooms":     cfg.RoomNames(),
		"devices":   len(s.reg.All()),
		"online":    len(s.reg.Online()),
		// KWS 边车状态：ok=可用，down=已启用但不可用（唤醒服务暂不可用），
		// disabled=未启用。kws_down=true 时语音唤醒降级。
		"kws_status": kwsStatus,
		"kws_down":   kwsStatus == "down",
	})
}

func (s *Server) devices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 GET"})
		return
	}
	writeJSON(w, http.StatusOK, s.reg.All())
}

// configAPI 读/写 config.yaml。
// GET 返回脱敏后的当前配置；PUT 接收完整配置 JSON，校验通过后写文件并热更新内存配置。
func (s *Server) configAPI(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.Lock()
		defer s.mu.Unlock()
		writeJSON(w, http.StatusOK, sanitizeConfig(s.cfg))
	case http.MethodPut:
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		var cfg config.Config
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "JSON 解析失败: " + err.Error()})
			return
		}
		// 先补默认值再做 TLS 降级检查：缺失的 tls 补为 true，只有显式 false 才会触发拦截。
		cfg.ApplyDefaults()
		// hub.tls 不许经 Web 从 true 改为 false（防降级为明文 ws）。
		s.mu.Lock()
		wasTLS := s.cfg.TLSEnabled()
		s.mu.Unlock()
		if wasTLS && !cfg.TLSEnabled() {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "hub.tls 不允许经 Web 关闭"})
			return
		}
		if err := cfg.Validate(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "校验失败: " + err.Error()})
			return
		}
		if err := cfg.Save(s.cfgPath); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "写文件失败: " + err.Error()})
			return
		}
		s.mu.Lock()
		s.cfg = &cfg
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 GET/PUT"})
	}
}

// checkOTAURL 校验 OTA URL：只许 https，且 host 必须在白名单内。
func (s *Server) checkOTAURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("ota: url 不是合法 URL：%q", raw)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("ota: url 只允许 https，实际是 %q", u.Scheme)
	}
	if len(s.OTAHosts) == 0 {
		return fmt.Errorf("ota: 未配置 OTA 仓库白名单，拒绝下发")
	}
	host := strings.ToLower(u.Hostname())
	for _, h := range s.OTAHosts {
		if host == h {
			return nil
		}
	}
	return fmt.Errorf("ota: url host %q 不在白名单内", host)
}

// otaAPI 一键 OTA：POST {device_id, version, url, sha256}。
func (s *Server) otaAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 POST"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	var req struct {
		DeviceID string `json:"device_id"`
		Version  string `json:"version"`
		URL      string `json:"url"`
		SHA256   string `json:"sha256"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "JSON 解析失败: " + err.Error()})
		return
	}
	if err := s.checkOTAURL(req.URL); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	job, err := s.otaMgr.Push(req.DeviceID, req.Version, req.URL, req.SHA256)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if s.PushOTA != nil {
		if err := s.PushOTA(req.DeviceID, req.Version, req.URL, req.SHA256); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "下发失败: " + err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, job)
}
