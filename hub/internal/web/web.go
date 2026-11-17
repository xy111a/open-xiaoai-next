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
	// CfgLock 是与 main.go 共享的配置锁（P1-22）：Web PUT 与
	// main.go 的 OnRoomClaim 认领写配置走同一把锁；为 nil 时用 s.mu。
	// 注意：PUT 不再换指针（`*s.cfg = cfg` 原地更新），main.go 的 h.cfg
	// 与 s.cfg 是同一指针，更新后两边都看到。
	CfgLock *sync.Mutex

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

	// HubFingerprint 是 Hub TLS 服务端证书的 SHA-256 指纹（小写 hex，
	// P1-10）：由 main 从 tlscert.LoadOrCreateMTLS 的返回值注入；
	// /api/fingerprint 经鉴权返回它，供刷机前写进 client.toml 的
	// hub_ca_fingerprint（客户端启动时强制要求，不写即报错退出）。
	// 为空时该端点回 503（Hub 未启用 TLS）。
	HubFingerprint string

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

// cfgLock 返回保护配置内容的锁：共享锁已配时用它，否则用 s.mu。
func (s *Server) cfgLock() *sync.Mutex {
	if s.CfgLock != nil {
		return s.CfgLock
	}
	return &s.mu
}

// NewHTTPServer 返回带超时的 http.Server（P0-1：main 应改用它监听 WebListen）。
func (s *Server) NewHTTPServer() *http.Server {
	addr := "127.0.0.1:8080"
	lock := s.cfgLock()
	lock.Lock()
	if s.cfg != nil && s.cfg.Hub.WebListen != "" {
		addr = s.cfg.Hub.WebListen
	}
	lock.Unlock()
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
	mux.HandleFunc("/api/fingerprint", s.requireAuth(s.fingerprintAPI))
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
		// N-P2-7 顺带：顺手清理已过期且无窗口内失败记录的条目，
		// authFailures 表不再无限增长。
		now := time.Now()
		for ip2, att := range s.authFailures {
			if now.After(att.blockedUntil) {
				kept := att.failures[:0]
				for _, t := range att.failures {
					if now.Sub(t) <= authFailWindow {
						kept = append(kept, t)
					}
				}
				if len(kept) == 0 {
					delete(s.authFailures, ip2)
				} else {
					att.failures = kept
				}
			}
		}
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

// mergeJSON 把 patch 递归合并进 dst（P1-22：PUT 是合并不是全量替换）：
// map 对 map 递归合并，其余类型直接覆盖；返回 dst。
func mergeJSON(dst, patch map[string]any) map[string]any {
	for k, v := range patch {
		if pv, ok := v.(map[string]any); ok {
			if dv, ok := dst[k].(map[string]any); ok {
				mergeJSON(dv, pv)
				continue
			}
		}
		dst[k] = v
	}
	return dst
}

// configAPI 读/写 config.yaml。
// GET 返回脱敏后的当前配置；PUT 接收部分配置 JSON（snake_case，与 YAML
// 一致），与当前配置递归合并后校验、写文件、原地热更新内存配置
// （P1-22：不再全量替换、缺字段不再清空、不换指针）。
func (s *Server) configAPI(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		lock := s.cfgLock()
		lock.Lock()
		defer lock.Unlock()
		writeJSON(w, http.StatusOK, sanitizeConfig(s.cfg))
	case http.MethodPut:
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		var patch map[string]any
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "JSON 解析失败: " + err.Error()})
			return
		}
		lock := s.cfgLock()
		lock.Lock()
		defer lock.Unlock()
		// 当前配置的 JSON 视图（Config 带 json snake_case 标签）与
		// patch 递归合并，再反序列化回 Config。
		cur, err := json.Marshal(s.cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "配置序列化失败"})
			return
		}
		var merged map[string]any
		if err := json.Unmarshal(cur, &merged); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "配置序列化失败"})
			return
		}
		mergeJSON(merged, patch)
		mergedBytes, err := json.Marshal(merged)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "配置序列化失败"})
			return
		}
		var cfg config.Config
		if err := json.Unmarshal(mergedBytes, &cfg); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "JSON 解析失败: " + err.Error()})
			return
		}
		// 先补默认值再做 TLS 降级检查：缺失的 tls 补为 true，只有显式 false 才会触发拦截。
		cfg.ApplyDefaults()
		// hub.tls 不许经 Web 从 true 改为 false（防降级为明文 ws）。
		wasTLS := s.cfg.TLSEnabled()
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
		// 原地更新（不换指针）：main.go 的 h.cfg 与 s.cfg 是同一指针，
		// 更新后两边都看到。
		*s.cfg = cfg
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "note": "配置已保存；Agent/HA/Wyoming 等下游连接在 Hub 重启后生效"})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 GET/PUT"})
	}
}

// fingerprintAPI 返回 Hub TLS 服务端证书指纹（P1-10）：
// 刷机前操作员凭 Web Token 取回，写进 client.toml 的 hub_ca_fingerprint。
// 指纹本身是公开值（证书哈希），安全靠鉴权 + 回环/SSH 隧道通道保证——
// 不要把它贴到公开渠道。
func (s *Server) fingerprintAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 GET"})
		return
	}
	s.mu.Lock()
	fp := s.HubFingerprint
	s.mu.Unlock()
	if fp == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Hub 未启用 TLS，指纹不可用"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"fingerprint": fp})
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

// otaAPI：POST 一键 OTA {device_id, version, url, sha256}；
// GET 查任务状态（P1-9）：?device_id=xx 查单台，不带则列全部。
func (s *Server) otaAPI(w http.ResponseWriter, r *http.Request) {
	if s.otaMgr == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "OTA 未启用"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		if id := r.URL.Query().Get("device_id"); id != "" {
			job, ok := s.otaMgr.Get(id)
			if !ok {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "没有该设备的 OTA 任务"})
				return
			}
			writeJSON(w, http.StatusOK, job)
			return
		}
		writeJSON(w, http.StatusOK, s.otaMgr.List())
	case http.MethodPost:
		s.otaPost(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 GET/POST"})
	}
}

// otaPost 处理 POST 一键 OTA。
func (s *Server) otaPost(w http.ResponseWriter, r *http.Request) {
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
	// P1-9：PushOTA 未接线时不再假装成功回 200——直接 500，也不建任务
	// （否则任务永远停在 pushing，见 ota.Manager.PushTimeout）。
	if s.PushOTA == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "OTA 下发未接线（PushOTA 未配置），任务未创建"})
		return
	}
	job, err := s.otaMgr.Push(req.DeviceID, req.Version, req.URL, req.SHA256)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.PushOTA(req.DeviceID, req.Version, req.URL, req.SHA256); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "下发失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, job)
}
