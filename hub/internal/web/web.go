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
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/huajun/open-xiaoai-next/hub/internal/config"
	"github.com/huajun/open-xiaoai-next/hub/internal/music"
	"github.com/huajun/open-xiaoai-next/hub/internal/observability"
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

// maxQRPerMinute / qrWindow 是建二维码的节流（H-12）：
// 每 IP 每分钟最多新建 maxQRPerMinute 个二维码。
const (
	maxQRPerMinute = 10
	qrWindow       = time.Minute
)

// allowQRCreate 记一次建二维码尝试：窗口内未满即记下并放行，
// 满了即拒绝（不记）。调用方在调上游 CreateQR 之前调它。
func (s *Server) allowQRCreate(ip string) bool {
	s.qrMu.Lock()
	defer s.qrMu.Unlock()
	if s.qrAttempts == nil {
		s.qrAttempts = map[string][]time.Time{}
	}
	now := time.Now()
	kept := s.qrAttempts[ip][:0]
	for _, t := range s.qrAttempts[ip] {
		if now.Sub(t) <= qrWindow {
			kept = append(kept, t)
		}
	}
	if len(kept) >= maxQRPerMinute {
		s.qrAttempts[ip] = kept
		return false
	}
	s.qrAttempts[ip] = append(kept, now)
	return true
}

// MusicBackend 是 Web 音乐端点用的 Router 侧接口（融合 B：Hub 直连，
// MPD 队列 + Hub 内存当前曲）。由 main 把 *router.Router 接进来；
// 为 nil 时音乐端点一律 503（未接线，不假装成功）。
type MusicBackend interface {
	MusicPause(ctx context.Context) error
	MusicResume(ctx context.Context) error
	MusicNext(ctx context.Context) error
	MusicPrevious(ctx context.Context) error
	// PlaybackState 返回 MPD 状态、当前曲、队列。
	PlaybackState(ctx context.Context) (state string, current music.MPDSong, queue []music.MPDSong, err error)
	MusicQueue(ctx context.Context) ([]music.MPDSong, error)
	MusicLyric(ctx context.Context, id int64) (string, error)
	// CurrentTrack 返回 Hub 内存里的当前曲（网易云歌名/歌手/来源）。
	CurrentTrack() (music.NeteaseSong, string, bool)
}

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

	// Music 是音乐后端（融合 B：main 把 Router 接进来），音乐端点
	// /api/music/* 都经它调 MPD 队列；为 nil 时一律 503。
	Music MusicBackend
	// Netease 是网易云客户端（歌词端点用；扫码登录成功后 main 会把
	// cookie 交给它，VIP 歌才能取地址）。
	Netease *music.NeteaseClient
	// NeteaseAuth 是网易云扫码登录管理器（/api/netease/* 用）；
	// 为 nil 时登录端点一律 503。
	NeteaseAuth *music.NeteaseAuth

	// Latency 是分段延迟记录器（可观测性，main 注入）：
	// /api/latency 读它的最近 20 次交互，/api/status 附最新一条。
	// 为 nil 时 /api/latency 返回空列表，不 503——“还没人说过话”
	// 与“没接线”在面板上没有区别，不值得为此多一个错误态。
	Latency *observability.Recorder
	// Health 是 7 容器健康检查器（可观测性，main 注入）：
	// /api/health 每次调用都实时探活一次。
	// 为 nil 时 /api/health 回 503（健康检查未接线）。
	Health *observability.Checker

	authMu       sync.Mutex
	authFailures map[string]*authAttempt

	// qrMu/qrAttempts 是 /api/netease/login/qr 的新建节流（H-12）：
	// 每 IP 每分钟最多 maxQRPerMinute 次新建，防循环建二维码刷
	// 上游 unikey。
	qrMu       sync.Mutex
	qrAttempts map[string][]time.Time
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
	mux.HandleFunc("/api/latency", s.requireAuth(s.latency))
	mux.HandleFunc("/api/health", s.requireAuth(s.health))
	mux.HandleFunc("/api/config", s.requireAuth(s.configAPI))
	mux.HandleFunc("/api/ota", s.requireAuth(s.otaAPI))
	mux.HandleFunc("/api/fingerprint", s.requireAuth(s.fingerprintAPI))
	// 网易云扫码登录（融合 B）与音乐控制：与上面一样走 requireAuth，
	// 不新开无鉴权口。
	mux.HandleFunc("/api/netease/status", s.requireAuth(s.neteaseStatus))
	mux.HandleFunc("/api/netease/login/qr", s.requireAuth(s.neteaseLoginQR))
	mux.HandleFunc("/api/netease/login/qr/check", s.requireAuth(s.neteaseLoginQRCheck))
	mux.HandleFunc("/api/netease/logout", s.requireAuth(s.neteaseLogout))
	mux.HandleFunc("/api/music/status", s.requireAuth(s.musicStatus))
	mux.HandleFunc("/api/music/pause", s.requireAuth(s.musicPause))
	mux.HandleFunc("/api/music/resume", s.requireAuth(s.musicResume))
	mux.HandleFunc("/api/music/next", s.requireAuth(s.musicNext))
	mux.HandleFunc("/api/music/previous", s.requireAuth(s.musicPrevious))
	mux.HandleFunc("/api/music/queue", s.requireAuth(s.musicQueue))
	mux.HandleFunc("/api/music/lyric", s.requireAuth(s.musicLyric))
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
// 例外：以 _env / -env 结尾的是环境变量名（如 agents[].api_key_env），
// 不是密钥本身——面板 GET 要照常显示，否则用户在面板里看不到、
// 也改不了 Agent 的密钥变量名。
func sensitiveKey(k string) bool {
	lk := strings.ToLower(k)
	if strings.HasSuffix(lk, "_env") || strings.HasSuffix(lk, "-env") {
		return false
	}
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
	// P2-56：只响应 "/"，其余未匹配路径返回 404——旧代码里
	// mux 的 "/" 匹配所有未命中路径并返回 200 首页，
	// 调用方拼错 API 路径（如 /api/statuss）会被掩盖。
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	// Web 面板：状态 + 设备 + 系统健康（7 容器探活）+ 分段延迟。
	// /api/* 都要 Bearer Token（OPEN_XIAOAI_WEB_TOKEN）：Token 存在
	// sessionStorage（关掉标签页即丢，不落 localStorage），
	// 未填/401 时页面顶部提示，不静默画空表。
	// 响应式：卡片用 auto-fit 网格（320px 下单列），表格包在
	// .tablewrap 里横向滚动，页面本身不横向溢出。
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 2026-11-19 重写：旧版首页只是标题 + 一段裸 JSON，还不带 Token，
	// 实际访问时 /api/status 会 401，页面永远停在“加载中…”。
	// 新版是完整的管理面板：Token 一次粘贴（sessionStorage 保存，
	// 所有 API 调用都带 Bearer）、状态卡片、设备表、音乐控制、
	// 网易云扫码登录、OTA、Hub 指纹、配置只读视图。单文件内嵌，
	// 无外部依赖；JS 里不用反引号（Go raw string 不能含反引号）。
	fmt.Fprint(w, `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>小爱音箱 · Hub</title>
<style>
:root{--bg:#f4f5f7;--card:#fff;--text:#1a1a1a;--muted:#666;--line:#e2e2e2;--accent:#1677ff;--ok:#1a9e54;--bad:#d93025;--warn:#b06000}
*{box-sizing:border-box}
body{font-family:system-ui,-apple-system,"PingFang SC","Microsoft YaHei",sans-serif;background:var(--bg);color:var(--text);margin:0;padding:0 14px 40px}
header{max-width:860px;margin:0 auto;padding:22px 0 6px}
header h1{font-size:22px;margin:0}
header p{color:var(--muted);margin:6px 0 0;font-size:14px}
main{max-width:860px;margin:0 auto;display:grid;gap:14px}
.card{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:16px 18px}
.card h2{font-size:16px;margin:0 0 12px}
.card h2 span{color:var(--muted);font-weight:400;font-size:13px}
.row{display:flex;gap:10px;flex-wrap:wrap}
.stat{flex:1;min-width:120px;background:#f8f9fb;border:1px solid var(--line);border-radius:10px;padding:12px}
.stat .k{font-size:13px;color:var(--muted)}
.stat .v{font-size:20px;font-weight:700;margin-top:4px}
.stat .v.ok{color:var(--ok)}
.stat .v.bad{color:var(--bad)}
.stat .v.warn{color:var(--warn)}
table{width:100%;border-collapse:collapse;font-size:14px}
td,th{border-bottom:1px solid var(--line);padding:8px 6px;text-align:left;vertical-align:top}
th{color:var(--muted);font-weight:600}
.mono{font-family:ui-monospace,Menlo,Consolas,monospace;font-size:12px;word-break:break-all}
button{font:inherit;background:var(--accent);color:#fff;border:0;border-radius:8px;padding:8px 14px;cursor:pointer}
button.ghost{background:#fff;color:var(--accent);border:1px solid var(--accent)}
button.danger{background:var(--bad)}
button:disabled{opacity:.5;cursor:default}
input,select{font:inherit;padding:8px 10px;border:1px solid #ccc;border-radius:8px;width:100%}
label{font-size:13px;color:var(--muted);display:block;margin:0 0 4px}
.field{flex:1;min-width:160px;margin-bottom:10px}
.muted{color:var(--muted);font-size:13px}
.err{color:var(--bad);font-size:13px}
.ok{color:var(--ok)}
#tokenbar{display:none}
#tokenbar.show{display:block}
#qrimg{max-width:220px;border:1px solid var(--line);border-radius:8px;display:none;margin-top:10px}
.pill{display:inline-block;padding:2px 8px;border-radius:99px;font-size:12px;background:#e8f5ee;color:var(--ok)}
.pill.off{background:#fdecea;color:var(--bad)}
.pill.warn{background:#fff3e0;color:var(--warn)}
.tablewrap{overflow-x:auto}
.bar-row{display:grid;grid-template-columns:88px 1fr 64px;gap:8px;align-items:center;margin:6px 0;font-size:13px}
.bar-track{height:10px;background:#eee;border-radius:5px;overflow:hidden}
.bar-fill{height:100%;background:#4a90d9;border-radius:5px}
.bar-ms{text-align:right;white-space:nowrap}
</style>
</head><body>
<header><h1>小爱音箱 · Hub</h1><p>open-xiaoai-next · 内网管理面板（经 SSH 隧道访问）</p></header>
<main>
<div class="card" id="tokenbar"><h2>登录 Token <span>首次打开需粘贴一次 OPEN_XIAOAI_WEB_TOKEN，之后保存在本浏览器会话里</span></h2>
<div class="row"><div class="field" style="flex:3"><input id="token" type="password" placeholder="粘贴 OPEN_XIAOAI_WEB_TOKEN" autocomplete="off"></div>
<div class="field" style="flex:0 0 auto"><button id="saveToken">保存并加载</button></div></div>
<div class="err" id="tokenmsg"></div></div>

<div class="card"><h2>状态 <span id="statusline">加载中…</span></h2>
<div class="row">
<div class="stat"><div class="k">在线设备</div><div class="v" id="st-online">–</div></div>
<div class="stat"><div class="k">注册设备</div><div class="v" id="st-devices">–</div></div>
<div class="stat"><div class="k">唤醒服务 (KWS)</div><div class="v" id="st-kws">–</div></div>
<div class="stat"><div class="k">唤醒词</div><div class="v" id="st-wake" style="font-size:16px">–</div></div>
</div>
<div class="muted" id="st-rooms" style="margin-top:10px"></div></div>

<div class="card"><h2>设备</h2><div id="devlist"><p class="muted">加载中…</p></div></div>

<div class="card"><h2>系统健康 <span>7 个容器</span></h2><div id="health"><p class="muted">加载中…</p></div></div>

<div class="card"><h2>语音延迟 <span>最近一次</span></h2><div id="latency"><p class="muted">加载中…</p></div></div>

<div class="card"><h2>最近交互 <span>最多 20 条</span></h2><div class="tablewrap"><div id="recent"><p class="muted">加载中…</p></div></div></div>

<div class="card"><h2>音乐</h2>
<div id="musnow"><p class="muted">加载中…</p></div>
<div class="row" style="margin-top:12px">
<button id="m-prev">上一首</button><button id="m-pause">暂停</button><button id="m-resume">继续</button><button id="m-next">下一首</button>
<button class="ghost" id="m-refresh">刷新</button></div>
<div id="musqueue" style="margin-top:12px"></div>
<div id="muslyric" style="margin-top:12px"></div></div>

<div class="card"><h2>网易云音乐</h2><div id="neinfo"><p class="muted">加载中…</p></div>
<div class="row" style="margin-top:12px">
<button id="ne-qr">扫码登录</button><button class="ghost" id="ne-refresh">刷新状态</button>
<button class="danger" id="ne-logout">退出登录</button></div>
<img id="qrimg" alt="扫码登录二维码">
<div class="muted" id="qrmsg" style="margin-top:8px"></div></div>

<div class="card"><h2>固件 OTA</h2><div id="otajobs"><p class="muted">加载中…</p></div>
<div class="row" style="margin-top:12px">
<div class="field"><label>设备 ID</label><input id="ota-dev" placeholder="如 oh2p-xxx"></div>
<div class="field"><label>版本</label><input id="ota-ver" placeholder="如 0.2.0"></div></div>
<div class="row"><div class="field"><label>固件 URL（只许 https 白名单）</label><input id="ota-url" placeholder="https://…"></div>
<div class="field"><label>SHA-256</label><input id="ota-sha" placeholder="64 位 hex"></div></div>
<button id="ota-push">下发 OTA</button> <span class="err" id="otaerr"></span></div>

<div class="card"><h2>Hub 指纹 <span>写进 client.toml 的 hub_ca_fingerprint</span></h2>
<div class="mono" id="fp">加载中…</div></div>

<div class="card"><h2>配置 <span>只读视图（脱敏）；改配置请走 /api/config 或改 config.yaml</span></h2>
<div id="cfgview"><p class="muted">加载中…</p></div></div>
</main>
<script>
var T = sessionStorage.getItem('oxn_token') || '';
var tokenbar = document.getElementById('tokenbar');
var tokenmsg = document.getElementById('tokenmsg');
function showToken(msg){ tokenbar.classList.add('show'); if(msg) tokenmsg.textContent = msg; }
function hideToken(){ tokenbar.classList.remove('show'); tokenmsg.textContent=''; }
document.getElementById('saveToken').onclick = function(){
  T = document.getElementById('token').value.trim();
  if(!T){ tokenmsg.textContent = '请先粘贴 Token'; return; }
  sessionStorage.setItem('oxn_token', T);
  document.getElementById('token').value = '';
  hideToken(); loadAll();
};
function api(p, o){
  o = o || {};
  o.headers = o.headers || {};
  if(T) o.headers['Authorization'] = 'Bearer ' + T;
  return fetch(p, o).then(function(r){
    if(r.status === 401){ showToken('Token 无效或未填，请粘贴 OPEN_XIAOAI_WEB_TOKEN 后重试。'); throw new Error('401'); }
    return r.json().then(function(j){ if(!r.ok) throw new Error((j && j.error) || ('HTTP ' + r.status)); return j; });
  });
}
function esc(s){ return String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;'); }
function loadStatus(){
  return api('/api/status').then(function(j){
    document.getElementById('statusline').textContent = '已连接';
    document.getElementById('st-online').textContent = j.online;
    document.getElementById('st-devices').textContent = j.devices;
    var k = document.getElementById('st-kws');
    k.textContent = j.kws_status || '–';
    k.className = 'v ' + (j.kws_status === 'ok' ? 'ok' : (j.kws_status === 'down' ? 'bad' : 'warn'));
    document.getElementById('st-wake').textContent = j.wake_word || '–';
    document.getElementById('st-rooms').textContent = (j.rooms && j.rooms.length) ? ('房间：' + j.rooms.join('、')) : '暂无房间认领（对音箱说“你好小李，我在客厅”认领）';
    document.getElementById('latency').innerHTML = stageBars(j.latest_latency||null);
  });
}
function loadDevices(){
  return api('/api/devices').then(function(list){
    var el = document.getElementById('devlist');
    if(!list || !list.length){ el.innerHTML = '<p class="muted">暂无设备注册。音箱开机连上 Hub 后会在这里出现。</p>'; return; }
    var h = '<table><tr><th>设备</th><th>型号</th><th>IP</th><th>房间</th><th>状态</th><th>版本</th><th>最后在线</th></tr>';
    list.forEach(function(d){
      h += '<tr><td class="mono">' + esc(d.ID || d.id) + '</td><td>' + esc(d.Model || d.model) + '</td><td class="mono">' + esc(d.IP || d.ip) + '</td><td>' + esc(d.Room || d.room || '未认领') + '</td><td>' + ((d.Online || d.online) ? '<span class="pill">在线</span>' : '<span class="pill off">离线</span>') + '</td><td class="mono">' + esc(d.Version || d.version || '–') + '</td><td class="muted">' + esc(d.LastSeen || d.last_seen || '–') + '</td></tr>';
    });
    el.innerHTML = h + '</table>';
  });
}
function loadMusic(){
  return api('/api/music/status').then(function(j){
    var el = document.getElementById('musnow');
    var cur = j.current;
    var name = cur ? (cur.name + (cur.artist ? ' — ' + cur.artist : '')) : '未在播放';
    var src = cur && cur.source ? ('（来源：' + esc(cur.source) + '）') : '';
    var pill = j.state === 'play' ? '<span class="pill">播放中</span>' : (j.state === 'pause' ? '<span class="pill warn">已暂停</span>' : '<span class="pill off">已停止</span>');
    el.innerHTML = '<div style="font-size:18px;font-weight:700">' + esc(name) + '</div><div style="margin-top:6px">' + pill + ' <span class="muted">' + src + ' · 队列 ' + (j.queue_len || 0) + ' 首</span></div>';
    var q = document.getElementById('musqueue');
    if(j.queue_len){ api('/api/music/queue').then(function(qj){
      var items = (qj.queue || []).slice(0, 8).map(function(s,i){ return '<li>' + esc(s.Title || s.Name || s.File || '') + (s.Artist ? ' — ' + esc(s.Artist) : '') + '</li>'; }).join('');
      q.innerHTML = items ? '<div class="muted">队列（前 8 首）</div><ol style="margin:6px 0 0;padding-left:20px;font-size:14px">' + items + '</ol>' : '';
    }).catch(function(){}); } else { q.innerHTML = ''; }
    var ly = document.getElementById('muslyric');
    if(cur && cur.id){ api('/api/music/lyric?id=' + cur.id).then(function(lj){
      ly.innerHTML = lj.lyric ? '<details><summary style="cursor:pointer">歌词</summary><pre style="white-space:pre-wrap;font-size:13px;max-height:220px;overflow:auto">' + esc(lj.lyric) + '</pre></details>' : '';
    }).catch(function(){ ly.innerHTML = ''; }); } else { ly.innerHTML = ''; }
  }).catch(function(e){ if(e.message !== '401') document.getElementById('musnow').innerHTML = '<p class="err">音乐状态不可用：' + esc(e.message) + '</p>'; });
}
function musicAct(path){
  api(path, {method:'POST'}).then(function(){ loadMusic(); }).catch(function(e){ if(e.message !== '401') alert('操作失败：' + e.message); });
}
document.getElementById('m-pause').onclick = function(){ musicAct('/api/music/pause'); };
document.getElementById('m-resume').onclick = function(){ musicAct('/api/music/resume'); };
document.getElementById('m-next').onclick = function(){ musicAct('/api/music/next'); };
document.getElementById('m-prev').onclick = function(){ musicAct('/api/music/previous'); };
document.getElementById('m-refresh').onclick = function(){ loadMusic(); };
function loadNetease(){
  return api('/api/netease/status').then(function(j){
    var el = document.getElementById('neinfo');
    if(j.logged_in){
      el.innerHTML = '<span class="pill">已登录</span> <b>' + esc(j.nickname || '') + '</b>' + (j.expires_at ? ' <span class="muted">过期：' + esc(j.expires_at) + '</span>' : ' <span class="muted">过期时间未知（约 1–2 个月）</span>');
    } else {
      el.innerHTML = '<span class="pill off">未登录</span> <span class="muted">点“扫码登录”，用网易云音乐 App 扫码。</span>';
    }
  });
}
var qrTimer = null;
document.getElementById('ne-qr').onclick = function(){
  var msg = document.getElementById('qrmsg');
  msg.textContent = '正在生成二维码…';
  api('/api/netease/login/qr', {method:'POST'}).then(function(j){
    var img = document.getElementById('qrimg');
    img.src = j.qrimg || j.qrurl; img.style.display = 'block';
    msg.textContent = '请用网易云音乐 App 扫码，正在等待…';
    if(qrTimer) clearInterval(qrTimer);
    qrTimer = setInterval(function(){
      api('/api/netease/login/qr/check?key=' + encodeURIComponent(j.key)).then(function(c){
        if(c.code === 803){ clearInterval(qrTimer); msg.textContent = '登录成功！'; img.style.display = 'none'; loadNetease(); }
        else if(c.code === 802){ msg.textContent = '已扫码，请在手机上点确认…'; }
        else if(c.code === 800){ clearInterval(qrTimer); msg.textContent = '二维码已过期，请重新点“扫码登录”。'; }
      }).catch(function(){});
    }, 2000);
  }).catch(function(e){ if(e.message !== '401') msg.textContent = '生成二维码失败：' + e.message; });
};
document.getElementById('ne-refresh').onclick = function(){ loadNetease(); };
document.getElementById('ne-logout').onclick = function(){
  if(!confirm('确定退出网易云登录吗？退出后 VIP 歌只能试听 30 秒。')) return;
  api('/api/netease/logout', {method:'POST'}).then(function(){ loadNetease(); }).catch(function(e){ if(e.message !== '401') alert('退出失败：' + e.message); });
};
function loadOTA(){
  return api('/api/ota').then(function(list){
    var el = document.getElementById('otajobs');
    if(!list || !list.length){ el.innerHTML = '<p class="muted">暂无 OTA 任务。</p>'; return; }
    var h = '<table><tr><th>设备</th><th>版本</th><th>状态</th><th>更新时间</th></tr>';
    list.forEach(function(jb){
      h += '<tr><td class="mono">' + esc(jb.device_id || jb.DeviceID || '') + '</td><td>' + esc(jb.version || jb.Version || '') + '</td><td>' + esc(jb.status || jb.Status || '') + '</td><td class="muted">' + esc(jb.updated_at || jb.UpdatedAt || '') + '</td></tr>';
    });
    el.innerHTML = h + '</table>';
  }).catch(function(e){ if(e.message !== '401') document.getElementById('otajobs').innerHTML = '<p class="err">OTA 状态不可用：' + esc(e.message) + '</p>'; });
}
document.getElementById('ota-push').onclick = function(){
  var err = document.getElementById('otaerr'); err.textContent = '';
  var body = {device_id: document.getElementById('ota-dev').value.trim(), version: document.getElementById('ota-ver').value.trim(), url: document.getElementById('ota-url').value.trim(), sha256: document.getElementById('ota-sha').value.trim()};
  if(!body.device_id || !body.version || !body.url || !body.sha256){ err.textContent = '四项都要填。'; return; }
  api('/api/ota', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify(body)}).then(function(){ loadOTA(); }).catch(function(e){ if(e.message !== '401') err.textContent = '下发失败：' + e.message; });
};
function loadFP(){
  return api('/api/fingerprint').then(function(j){
    document.getElementById('fp').textContent = j.fingerprint || '–';
  }).catch(function(e){ if(e.message !== '401') document.getElementById('fp').textContent = '不可用：' + e.message; });
}
function loadCfg(){
  return api('/api/config').then(function(j){
    var el = document.getElementById('cfgview');
    var rows = [];
    if(j.wake_word) rows.push(['唤醒词', j.wake_word]);
    if(j.home_assistant && j.home_assistant.url) rows.push(['Home Assistant', j.home_assistant.url]);
    if(j.music_folder) rows.push(['音乐文件夹', j.music_folder]);
    if(j.netease && j.netease.api) rows.push(['网易云 API', j.netease.api]);
    if(j.agents){ Object.keys(j.agents).forEach(function(n){ var a = j.agents[n]; rows.push(['Agent ' + n, (a.model || '') + ' @ ' + (a.base_url || '')]); }); }
    if(!rows.length){ el.innerHTML = '<p class="muted">配置为空。</p>'; return; }
    el.innerHTML = '<table>' + rows.map(function(r){ return '<tr><th style="width:160px">' + esc(r[0]) + '</th><td class="mono">' + esc(r[1]) + '</td></tr>'; }).join('') + '</table>';
  }).catch(function(e){ if(e.message !== '401') document.getElementById('cfgview').innerHTML = '<p class="err">配置不可用：' + esc(e.message) + '</p>'; });
}
var STAGE_LABELS={'kws_ms':'唤醒 KWS','stt_ms':'识别 STT','llm_ms':'对话 LLM','tts_ms':'合成 TTS','music_ms':'点歌播放'};
var STAGE_ORDER=['kws_ms','stt_ms','llm_ms','tts_ms','music_ms'];
function fmtTime(s){ if(!s) return '—'; try{ return new Date(s).toLocaleString('zh-CN',{hour12:false}); }catch(e){ return s; } }
function stageBars(it){
  if(!it) return '<p class="muted">暂无语音交互记录。</p>';
  var total=it.total_ms||0;
  var h='<p>设备 '+esc(it.device_id)+' · '+esc(it.kind==='music'?'点歌':'语音问答')+' · '+fmtTime(it.started_at)+' · 总计 <b>'+total+' ms</b>'+(it.ok?'':' · <span class="err">失败：'+esc(it.error||'')+'</span>')+'</p>';
  STAGE_ORDER.forEach(function(k){
    var v=(it.stages||{})[k];
    if(v==null) return;
    var pct=total>0?Math.min(100,Math.round(v/total*100)):0;
    h+='<div class="bar-row"><span>'+STAGE_LABELS[k]+'</span><span class="bar-track"><span class="bar-fill" style="display:block;width:'+pct+'%"></span></span><span class="bar-ms">'+v+' ms</span></div>';
  });
  return h;
}
function renderHealth(rep){
  var h='<p>'+esc(rep.healthy)+' / '+esc(rep.total)+' 正常'+(rep.down?(' · <span class="err">'+esc(rep.down)+' 个异常</span>'):'')+'<span class="muted">（'+fmtTime(rep.checked_at)+' 探活）</span></p>';
  h+='<div class="tablewrap"><table><tr><th>服务</th><th>容器</th><th>状态</th><th>延迟</th><th>详情</th></tr>';
  (rep.services||[]).forEach(function(s){
    var st=s.status==='ok'?'<span class="ok">正常</span>':s.status==='disabled'?'<span class="muted">未启用</span>':'<span class="err">异常</span>';
    h+='<tr><td>'+esc(s.name)+'</td><td>'+esc(s.container)+'</td><td>'+st+'</td><td>'+esc(s.latency_ms)+' ms</td><td>'+esc(s.detail||'')+'</td></tr>';
  });
  document.getElementById('health').innerHTML=h+'</table></div>';
}
function renderRecent(j){
  var list=j.interactions||[];
  if(list.length===0){ document.getElementById('recent').innerHTML='<p class="muted">暂无记录——说一句话或点一首歌后这里会出现分段耗时。</p>'; return; }
  var h='<table><tr><th>时间</th><th>设备</th><th>类型</th><th>总计</th><th>KWS</th><th>STT</th><th>LLM</th><th>TTS</th><th>点歌</th><th>结果</th></tr>';
  list.forEach(function(it){
    var st=it.stages||{};
    function cell(k){ return st[k]==null?'—':st[k]+' ms'; }
    h+='<tr><td>'+fmtTime(it.started_at)+'</td><td>'+esc(it.device_id)+'</td><td>'+esc(it.kind==='music'?'点歌':'语音')+'</td><td>'+esc(it.total_ms)+' ms</td><td>'+cell('kws_ms')+'</td><td>'+cell('stt_ms')+'</td><td>'+cell('llm_ms')+'</td><td>'+cell('tts_ms')+'</td><td>'+cell('music_ms')+'</td><td>'+(it.ok?'<span class="ok">成功</span>':'<span class="err">失败</span>')+'</td></tr>';
  });
  document.getElementById('recent').innerHTML=h+'</table>';
}
function loadHealth(){
  return api('/api/health').then(renderHealth).catch(function(e){ if(e.message!=='401') document.getElementById('health').innerHTML='<p class="err">健康检查失败：'+esc(e.message)+'</p>'; });
}
function loadRecent(){
  return api('/api/latency').then(renderRecent).catch(function(){});
}
function loadAll(){
  if(!T){ showToken('首次打开请粘贴 OPEN_XIAOAI_WEB_TOKEN。'); return; }
  hideToken();
  loadStatus().catch(function(){});
  loadDevices().catch(function(){});
  loadMusic().catch(function(){});
  loadNetease().catch(function(){});
  loadOTA().catch(function(){});
  loadFP().catch(function(){});
  loadCfg().catch(function(){});
  loadHealth().catch(function(){});
  loadRecent().catch(function(){});
}
if(T){ loadAll(); } else { showToken('首次打开请粘贴 OPEN_XIAOAI_WEB_TOKEN。'); }
setInterval(function(){ if(T){ loadStatus().catch(function(){}); loadMusic().catch(function(){}); loadRecent().catch(function(){}); } }, 5000);
setInterval(function(){ if(T){ loadHealth().catch(function(){}); } }, 30000);
</script></body></html>`)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 GET"})
		return
	}
	// H-7：配置读走共享 CfgLock（与 configAPI PUT 的 *s.cfg 写同一把
	// 锁），不要只用 s.mu——旧代码在 s.mu 下读 cfg.WakeWord/RoomNames，
	// 与 PUT 并发时是对同一 Config 结构体的无锁读写，-race 可检出。
	// 先在锁内快照出 wake_word 与 rooms，再读 KWSStatus（s.mu 管）。
	lock := s.cfgLock()
	lock.Lock()
	var wakeWord string
	var rooms []string
	if s.cfg != nil {
		wakeWord = s.cfg.WakeWord
		rooms = s.cfg.RoomNames()
	}
	lock.Unlock()
	s.mu.Lock()
	kwsFn := s.KWSStatus
	lat := s.Latency
	s.mu.Unlock()
	kwsStatus := "disabled"
	if kwsFn != nil {
		kwsStatus = kwsFn()
	}
	// 可观测性：/api/status 附最新一条延迟记录（没有时为 null），
	// 面板首页不用再等 /api/latency 也能看到上次问答各段耗时。
	var latest *observability.Interaction
	if lat != nil {
		latest = lat.Latest()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"wake_word": wakeWord,
		"rooms":     rooms,
		"devices":   len(s.reg.All()),
		"online":    len(s.reg.Online()),
		// KWS 边车状态：ok=可用，down=已启用但不可用（唤醒服务暂不可用），
		// disabled=未启用。kws_down=true 时语音唤醒降级。
		"kws_status":     kwsStatus,
		"kws_down":       kwsStatus == "down",
		"latest_latency": latest,
	})
}

func (s *Server) devices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 GET"})
		return
	}
	writeJSON(w, http.StatusOK, s.reg.All())
}

// latency：GET /api/latency，返回最近 20 次交互的分段延迟
// （倒序：最新在前）。JSON 契约见
// hub/internal/observability/latency.go 的包注释。
// Latency 未注入时返回空列表（还没人说过话与没接线无区别，
// 不另设错误态）。
func (s *Server) latency(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 GET"})
		return
	}
	s.mu.Lock()
	lat := s.Latency
	s.mu.Unlock()
	var items []observability.Interaction
	if lat != nil {
		items = lat.Recent()
	}
	if items == nil {
		items = []observability.Interaction{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"interactions": items,
		"count":        len(items),
	})
}

// health：GET /api/health，对 7 个容器实时探活一次并返回结果
// （JSON 契约见 hub/internal/observability/health.go 的包注释）。
// 每次调用都重新探活（单服务超时 3s，全量最坏约 3s，并发跑）；
// Health 未注入时回 503（健康检查未接线）。
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 GET"})
		return
	}
	s.mu.Lock()
	chk := s.Health
	s.mu.Unlock()
	if chk == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "健康检查未接线"})
		return
	}
	writeJSON(w, http.StatusOK, chk.Check(r.Context()))
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
		// 融合 B：netease.music_assistant 已废弃（MA 已移除），经 Web
		// 写回来也要明确拒绝，不要静默丢掉——否则用户以为 MA 还在。
		if ne, ok := patch["netease"].(map[string]any); ok {
			if _, ok := ne["music_assistant"]; ok {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "netease.music_assistant 已废弃：MA 已移除，Hub 改为直连网易云"})
				return
			}
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

// ---- 网易云扫码登录（融合 B：/api/netease/*）----

// neteaseStatus：GET /api/netease/status，返回登录状态。
// logged_in=false 时 needs_rescan=true（从未登录或 cookie 已过期，需重扫）。
func (s *Server) neteaseStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 GET"})
		return
	}
	if s.NeteaseAuth == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "网易云登录未接线"})
		return
	}
	loggedIn, nickname, expiresAt, err := s.NeteaseAuth.Status(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "查登录状态失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"logged_in":    loggedIn,
		"nickname":     nickname,
		"expires_at":   expiresAt,
		"needs_rescan": !loggedIn,
	})
}

// neteaseLoginQR：POST /api/netease/login/qr，建一个登录二维码。
// 返回 key（轮询用）与 qrurl/qrimg（前端展示扫码）。
func (s *Server) neteaseLoginQR(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 POST"})
		return
	}
	if s.NeteaseAuth == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "网易云登录未接线"})
		return
	}
	// H-12：建二维码节流（每 IP 每分钟 ≤10 次新建），超限即 429，
	// 不调上游、不新建 unikey。
	if !s.allowQRCreate(clientIP(r)) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "建二维码过于频繁，稍后再试"})
		return
	}
	key, qrURL, qrImg, err := s.NeteaseAuth.CreateQR(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "建二维码失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"key": key, "qrurl": qrURL, "qrimg": qrImg})
}

// neteaseLoginQRCheck：GET /api/netease/login/qr/check?key=K，轮询扫码结果。
// code：800 过期（需重新建码）、801 等待扫码、802 待确认、803 成功。
// 803 时 cookie 已写盘，同时交给 Netease 客户端（VIP 歌取地址用）。
func (s *Server) neteaseLoginQRCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 GET"})
		return
	}
	if s.NeteaseAuth == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "网易云登录未接线"})
		return
	}
	key := r.URL.Query().Get("key")
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 key 参数"})
		return
	}
	code, message, err := s.NeteaseAuth.CheckQR(r.Context(), key)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "查扫码结果失败: " + err.Error()})
		return
	}
	if code == music.QRCodeSuccess && s.Netease != nil {
		s.Netease.SetCookie(s.NeteaseAuth.Cookie())
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"code":      code,
		"message":   message,
		"logged_in": code == music.QRCodeSuccess,
	})
}

// neteaseLogout：POST /api/netease/logout，清除登录（删数据卷 cookie 文件，
// 同时清 Netease 客户端的 Cookie）。
func (s *Server) neteaseLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 POST"})
		return
	}
	if s.NeteaseAuth == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "网易云登录未接线"})
		return
	}
	if err := s.NeteaseAuth.Logout(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "登出失败: " + err.Error()})
		return
	}
	if s.Netease != nil {
		s.Netease.SetCookie("")
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ---- 音乐控制（融合 B：/api/music/*，经 Music 后端调 MPD）----

func (s *Server) musicBackend(w http.ResponseWriter) MusicBackend {
	if s.Music == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "音乐未接线（MPD/Router 未配置）"})
		return nil
	}
	return s.Music
}

// musicStatus：GET /api/music/status，返回当前曲 + 播放状态。
// current 优先用 Hub 内存里的当前曲（网易云歌名/歌手）；没点过歌时
// 用 MPD 当前曲目（文件/标题）；state 是 MPD 状态 play/pause/stop。
func (s *Server) musicStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 GET"})
		return
	}
	m := s.musicBackend(w)
	if m == nil {
		return
	}
	state, cur, queue, err := m.PlaybackState(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "取播放状态失败: " + err.Error()})
		return
	}
	var current map[string]any
	if song, source, ok := m.CurrentTrack(); ok {
		current = map[string]any{"id": song.ID, "name": song.Name, "artist": song.Artist, "source": source}
	} else if cur.File != "" {
		name := cur.Title
		if name == "" {
			name = cur.Name
		}
		if name == "" {
			name = cur.File
		}
		current = map[string]any{"name": name, "artist": cur.Artist, "file": cur.File, "source": "mpd"}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"state":     state,
		"current":   current,
		"queue_len": len(queue),
	})
}

// musicControl 是 pause/resume/next/previous 四个 POST 端点的公共实现。
func (s *Server) musicControl(w http.ResponseWriter, r *http.Request, action string, fn func(context.Context) error) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 POST"})
		return
	}
	m := s.musicBackend(w)
	if m == nil {
		return
	}
	if err := fn(r.Context()); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": action + "失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) musicPause(w http.ResponseWriter, r *http.Request) {
	s.musicControl(w, r, "暂停", func(ctx context.Context) error { return s.Music.MusicPause(ctx) })
}

func (s *Server) musicResume(w http.ResponseWriter, r *http.Request) {
	s.musicControl(w, r, "继续", func(ctx context.Context) error { return s.Music.MusicResume(ctx) })
}

func (s *Server) musicNext(w http.ResponseWriter, r *http.Request) {
	s.musicControl(w, r, "下一首", func(ctx context.Context) error { return s.Music.MusicNext(ctx) })
}

func (s *Server) musicPrevious(w http.ResponseWriter, r *http.Request) {
	s.musicControl(w, r, "上一首", func(ctx context.Context) error { return s.Music.MusicPrevious(ctx) })
}

// musicQueue：GET /api/music/queue，返回当前队列。
func (s *Server) musicQueue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 GET"})
		return
	}
	m := s.musicBackend(w)
	if m == nil {
		return
	}
	q, err := m.MusicQueue(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "取队列失败: " + err.Error()})
		return
	}
	if q == nil {
		q = []music.MPDSong{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"queue": q})
}

// musicLyric：GET /api/music/lyric?id=N，返回歌词；不带 id 时用当前曲。
func (s *Server) musicLyric(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 GET"})
		return
	}
	m := s.musicBackend(w)
	if m == nil {
		return
	}
	var id int64
	if raw := r.URL.Query().Get("id"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id 不是数字"})
			return
		}
		// H-11：负数不再直通上游（旧代码只 ParseInt、不判负数，
		// id=-5 会拼进 /lyric?id=-5 发给上游）。
		if n < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id 非法"})
			return
		}
		id = n
	}
	lyric, err := m.MusicLyric(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if id == 0 {
		if song, _, ok := m.CurrentTrack(); ok {
			id = song.ID
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "lyric": lyric})
}
