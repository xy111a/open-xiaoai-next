// Package transport 是 Hub 的 Client 接入层：wss :4399。
// 只做帧处理与鉴权，不做业务逻辑：收到 hello 先验 Token，
// 验过才注册设备；wake/音频/认领等事件交给 Handler（语音管线等）处理。
// 协议见 docs/PROTOCOL.md。
package transport

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/huajun/open-xiaoai-next/hub/internal/config"
	"github.com/huajun/open-xiaoai-next/hub/internal/registry"
)

// ---- 消息结构（与 docs/PROTOCOL.md 一一对应）----

// Hello 是 Client 连接后第一条消息。
type Hello struct {
	Type    string `json:"type"`
	Model   string `json:"model"`
	RAMMB   int    `json:"ram_mb"`
	Version string `json:"version"`
	Token   string `json:"token"`
	Room    string `json:"room,omitempty"`
}

// Wake 是本地 KWS 命中上报。
type Wake struct {
	Type     string  `json:"type"`
	WakeWord string  `json:"wake_word"`
	RMSdB    float64 `json:"rms_db"`
	TsMs     int64   `json:"ts_ms"`
}

// RoomClaim 是语音认领房间。
type RoomClaim struct {
	Type string `json:"type"`
	Room string `json:"room"`
}

// Status 是 Client 定期状态上报。
type Status struct {
	Type    string `json:"type"`
	Volume  int    `json:"volume"`
	Playing bool   `json:"playing"`
	Version string `json:"version"`
}

// RPCResult 是 Client 对 Hub rpc 的回应。
type RPCResult struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Register 是 Client 在已有 WS 连接上的主动注册（P1-44 注册制）：
// 客户端连接建立后立即发一条，之后每 30s 发 heartbeat。
// Hub 按指纹白名单校验，通过才记为在线，否则记日志拒绝。
type Register struct {
	Type        string `json:"type"`
	DeviceID    string `json:"device_id"`
	Model       string `json:"model"`
	Room        string `json:"room"`
	Fingerprint string `json:"fingerprint"`
}

// Heartbeat 是注册后的周期心跳（P1-44）：每 30s 一条。
type Heartbeat struct {
	Type string `json:"type"`
}

// RPC 是 Hub 下发给 Client 的调用。
type RPC struct {
	Type   string         `json:"type"`
	ID     string         `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params,omitempty"`
}

// Envelope 是所有 JSON 消息的通用包（先读 type 再按类型解析）。
type Envelope struct {
	Type string `json:"type"`
}

// ---- Handler：业务层实现的回调 ----

// Handler 接收 transport 解析后的事件。
type Handler interface {
	OnWake(deviceID string, w Wake)
	OnAudioStart(deviceID string)
	OnAudioFrame(deviceID string, opus []byte)
	OnAudioEnd(deviceID string)
	OnRoomClaim(deviceID string, room string)
	OnStatus(deviceID string, s Status)
	OnRPCResult(deviceID string, r RPCResult)
	OnDisconnect(deviceID string)
}

// ---- Conn：一条已鉴权的 Client 连接 ----

// Conn 是一条 Client 连接。
type Conn struct {
	DeviceID string
	conn     *websocket.Conn
	mu       sync.Mutex
}

// SendJSON 发一条 JSON 消息。
func (c *Conn) SendJSON(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return c.conn.WriteJSON(v)
}

// SendBinary 发一帧二进制 Opus 音频（首字节 0x01，见 PROTOCOL.md）。
func (c *Conn) SendBinary(opus []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	frame := make([]byte, 0, len(opus)+1)
	frame = append(frame, 0x01)
	frame = append(frame, opus...)
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return c.conn.WriteMessage(websocket.BinaryMessage, frame)
}

// SendRPC 下发一次 RPC 调用。
func (c *Conn) SendRPC(id, method string, params map[string]any) error {
	return c.SendJSON(RPC{Type: "rpc", ID: id, Method: method, Params: params})
}

// SendInterrupt 下发打断。
func (c *Conn) SendInterrupt() error {
	return c.SendJSON(map[string]string{"type": "interrupt"})
}

// SendPing 发一次 WS Ping（P1-11）：走与 SendJSON 同一把写锁，
// gorilla websocket 禁止同一连接并发写，ping goroutine 不许绕开 Conn.mu。
func (c *Conn) SendPing() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return c.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
}

// Close 关闭连接。
func (c *Conn) Close() error {
	return c.conn.Close()
}

// ---- Token 强度校验（P0-3）----

// weakTokens 是公开/常见的弱 Token（小写比较）。长度 <16 的一律拒绝。
var weakTokens = map[string]bool{
	"change_me": true, "changeme": true, "secret": true, "password": true,
	"123456": true, "12345678": true, "123456789": true, "admin": true,
	"letmein": true, "qwerty": true, "open-xiaoai": true, "open_xiaoai": true,
	"xiaoai": true, "test": true, "token": true, "1234567890": true,
	"0000000000000000": true, "aaaaaaaaaaaaaaaa": true,
}

// ValidateToken 校验 Hub 启动用的 Client 鉴权 Token（P0-3）：
// 长度 <16、为空、或等于常见弱值即返回错误。main.go 应在建 Server 前
// 调它，失败即 Fatal（错误里给出生成命令）。
// 注意：New() 刻意不调它——单元测试与本地联调用短 Token，生产启动
// 必须经 ValidateToken 把关。
func ValidateToken(token string) error {
	if token == "" {
		return fmt.Errorf("OPEN_XIAOAI_TOKEN 为空（Client 鉴权必须）；生成一个：openssl rand -hex 16")
	}
	if len(token) < 16 {
		return fmt.Errorf("OPEN_XIAOAI_TOKEN 太短（%d 位，至少 16 位）；生成一个：openssl rand -hex 16", len(token))
	}
	if weakTokens[strings.ToLower(strings.TrimSpace(token))] {
		return fmt.Errorf("OPEN_XIAOAI_TOKEN 是公开/常见弱值；生成一个：openssl rand -hex 16")
	}
	return nil
}

// tokenEqual 用 SHA-256 后再恒定时间比较，避免短路比较泄露 Token 前缀。
func tokenEqual(a, b string) bool {
	ha := sha256.Sum256([]byte(a))
	hb := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

// ---- Server ----

// maxReadBytes 是单条 WS 消息的上限（P1-3）：
// 音频帧 <2KB、文本消息 <1KB，64KB 绰绰有余；超限即断线。
const maxReadBytes = 64 << 10

// hello 失败限流（P1-3）：同一 IP 1 分钟内失败满 5 次即封禁 5 分钟。
const (
	maxHelloFailures   = 5
	helloFailWindow    = time.Minute
	helloBlockDuration = 5 * time.Minute
)

// originAllowed 校验 WS 握手的 Origin（P1-3）：
// 音箱客户端（tungstenite）不发 Origin，空 Origin 放行；
// 发了 Origin 的只许内网 host，不许任意跨站页面直连 4399。
func originAllowed(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "ws" && u.Scheme != "wss" {
		return false
	}
	return config.IsPrivateHost(u.Hostname())
}

var upgrader = websocket.Upgrader{
	CheckOrigin: originAllowed,
	// 握手请求体上限：握手不带 body，64KB 封顶。
}

// Server 是 wss 接入服务。
type Server struct {
	token   string
	reg     *registry.Registry
	handler Handler

	// clientCAs 非空时，Serve() 对 wss 启用 mTLS：
	// RequireAndVerifyClientCert，客户端必须出示该 CA 签发的证书。
	// 由 tlscert.LoadOrCreateMTLS() 提供；未设置时只做 Token 鉴权
	// （与旧行为一致，但会在启动日志里警告）。
	clientCAs *x509.CertPool

	mu    sync.RWMutex
	conns map[string]*Conn

	// PingInterval 是 Hub 主动发 Ping 的间隔（P1-11，默认 30s）；
	// PongWait 是收到 Pong 后续期的读 deadline（默认 60s）。
	// 测试可调小这两个值；<=0 时用默认值。
	PingInterval time.Duration
	PongWait     time.Duration

	// Fingerprints 是注册（P1-44）的设备证书 SHA-256 指纹白名单
	// （小写 hex，冒号可有可无）；为空时注册一律拒绝（fail closed）。
	// 由 main.go 从 cfg.Hub.DiscoveryFingerprints 传入。
	Fingerprints []string

	// OnHello 在 hello 成功后调用（P1-9：main.go 接 otaMgr.ConfirmHello，
	// 设备用新版本重新 hello 即确认 OTA 成功）；为 nil 时不调用。
	OnHello func(deviceID, version string)

	// registered 是已通过 register 校验的设备 → 最近一次心跳时间。
	regMu      sync.Mutex
	registered map[string]time.Time

	// hello 失败限流（P1-3）。
	failMu            sync.Mutex
	helloFails        map[string][]time.Time
	helloBlockedUntil map[string]time.Time
}

func (s *Server) pingInterval() time.Duration {
	if s.PingInterval <= 0 {
		return 30 * time.Second
	}
	return s.PingInterval
}

func (s *Server) pongWait() time.Duration {
	if s.PongWait <= 0 {
		return 60 * time.Second
	}
	return s.PongWait
}

// SetFingerprints 设置注册指纹白名单（P1-44）。
func (s *Server) SetFingerprints(fps []string) {
	s.regMu.Lock()
	defer s.regMu.Unlock()
	s.Fingerprints = append([]string(nil), fps...)
}

// IsRegistered 查某设备是否已通过 register 校验。
func (s *Server) IsRegistered(deviceID string) bool {
	s.regMu.Lock()
	defer s.regMu.Unlock()
	_, ok := s.registered[deviceID]
	return ok
}

// LastHeartbeat 取某设备最近一次 register/heartbeat 的时间；未注册返回零值。
func (s *Server) LastHeartbeat(deviceID string) time.Time {
	s.regMu.Lock()
	defer s.regMu.Unlock()
	return s.registered[deviceID]
}

// fingerprintAllowed 判断指纹是否在白名单内（归一化后比较）。
func (s *Server) fingerprintAllowed(fp string) bool {
	fp = normalizeFingerprint(fp)
	if fp == "" {
		return false
	}
	s.regMu.Lock()
	defer s.regMu.Unlock()
	for _, want := range s.Fingerprints {
		if fp == normalizeFingerprint(want) {
			return true
		}
	}
	return false
}

func normalizeFingerprint(fp string) string {
	fp = strings.ReplaceAll(fp, ":", "")
	fp = strings.ReplaceAll(fp, " ", "")
	fp = strings.ReplaceAll(fp, "-", "")
	return strings.ToLower(fp)
}

// recordHelloFailure 记一次 hello 失败；同一 IP 在窗口内满
// maxHelloFailures 次即封禁 helloBlockDuration。
func (s *Server) recordHelloFailure(ip string) {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	if s.helloFails == nil {
		s.helloFails = map[string][]time.Time{}
	}
	if s.helloBlockedUntil == nil {
		s.helloBlockedUntil = map[string]time.Time{}
	}
	now := time.Now()
	kept := s.helloFails[ip][:0]
	for _, t := range s.helloFails[ip] {
		if now.Sub(t) <= helloFailWindow {
			kept = append(kept, t)
		}
	}
	s.helloFails[ip] = append(kept, now)
	if len(s.helloFails[ip]) >= maxHelloFailures {
		s.helloBlockedUntil[ip] = now.Add(helloBlockDuration)
		log.Printf("transport: IP %s hello 失败 %d 次，封禁 %v", ip, len(s.helloFails[ip]), helloBlockDuration)
	}
}

// helloBlocked 查某 IP 是否在封禁期内。
func (s *Server) helloBlocked(ip string) bool {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	until, ok := s.helloBlockedUntil[ip]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(s.helloBlockedUntil, ip)
		delete(s.helloFails, ip)
		return false
	}
	return true
}

// clearHelloFailures 清掉某 IP 的 hello 失败计数（hello 成功后调）。
func (s *Server) clearHelloFailures(ip string) {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	delete(s.helloFails, ip)
	delete(s.helloBlockedUntil, ip)
}

// SetClientCAs 启用 mTLS 客户端证书校验（传入 CA 证书池；传 nil 则关闭）。
func (s *Server) SetClientCAs(pool *x509.CertPool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clientCAs = pool
}

// New 新建 Server。token 为空表示不鉴权（只允许测试用，生产必须设）。
func New(token string, reg *registry.Registry, h Handler) *Server {
	return &Server{
		token: token, reg: reg, handler: h, conns: map[string]*Conn{},
		registered: map[string]time.Time{},
		helloFails: map[string][]time.Time{}, helloBlockedUntil: map[string]time.Time{},
	}
}

// Conn 取一台在线设备的连接。
func (s *Server) Conn(deviceID string) (*Conn, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.conns[deviceID]
	return c, ok
}

// BroadcastJSON 给所有在线设备发一条 JSON 消息。
func (s *Server) BroadcastJSON(v any) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range s.conns {
		_ = c.SendJSON(v)
	}
}

// Serve 在已建好的 listener 上服务 HTTP/WebSocket。
// tlsCert 为 nil 时走纯 ws（仅本地测试）；非 nil 时走 wss。
// 事先调过 SetClientCAs() 时，wss 要求客户端证书（mTLS，
// RequireAndVerifyClientCert）；否则只做 Token 鉴权并记一条警告。
func (s *Server) Serve(l net.Listener, tlsCert *tls.Certificate) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWS)
	// P1-3：http.Server 加超时与握手体上限（旧代码无任何超时，
	// 慢速/黑洞客户端可永久占住握手 goroutine）。
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}
	if tlsCert != nil {
		s.mu.RLock()
		pool := s.clientCAs
		s.mu.RUnlock()
		tlsConf := &tls.Config{Certificates: []tls.Certificate{*tlsCert}}
		if pool != nil {
			tlsConf.ClientAuth = tls.RequireAndVerifyClientCert
			tlsConf.ClientCAs = pool
		} else {
			log.Printf("警告：4399 未启用 mTLS 客户端证书校验（仅 Token 鉴权）；" +
				"生产部署请用 tlscert.LoadOrCreateMTLS() + SetClientCAs() 启用 mTLS")
		}
		srv.TLSConfig = tlsConf
		return srv.ServeTLS(l, "", "")
	}
	return srv.Serve(l)
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	// P1-3：封禁期内的 IP 直接 429，不走到握手。
	if s.helloBlocked(host) {
		http.Error(w, "too many failed hellos", http.StatusTooManyRequests)
		return
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	// P1-3：单条消息限长（P2-N2 的 Hub 侧同款防护）。
	ws.SetReadLimit(maxReadBytes)
	// P1-11：读 deadline 不再一次性 60 秒——初始 PongWait，
	// 收到 Pong 或任意消息都续期；Hub 每 PingInterval 主动发 Ping，
	// 客户端回 Pong 即续期，空闲连接不再每 60 秒被误杀。
	ws.SetPongHandler(func(string) error {
		_ = ws.SetReadDeadline(time.Now().Add(s.pongWait()))
		return nil
	})
	_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, first, err := ws.ReadMessage()
	if err != nil {
		_ = ws.Close()
		return
	}
	// hello 失败统一记限流（P1-3），拒绝原因仍按 P2-2 的区分枚举
	// 原样返回（本轮不改拒绝文案，避免客户端解析依赖断裂）。
	reject := func(reason string) {
		s.recordHelloFailure(host)
		_ = ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_ = ws.WriteJSON(map[string]string{"type": "hello_reject", "reason": reason})
		_ = ws.Close()
	}
	var env Envelope
	if err := json.Unmarshal(first, &env); err != nil || env.Type != "hello" {
		reject("first message must be hello")
		return
	}
	var hello Hello
	if err := json.Unmarshal(first, &hello); err != nil {
		reject("bad hello")
		return
	}
	if s.token != "" && !tokenEqual(hello.Token, s.token) {
		reject("bad token")
		return
	}
	if hello.Model != "lx06" && hello.Model != "oh2p" {
		reject("unsupported model")
		return
	}
	s.clearHelloFailures(host)
	deviceID := hello.Model + "-" + strings.ReplaceAll(host, ".", "-")
	c := &Conn{DeviceID: deviceID, conn: ws}
	s.mu.Lock()
	if old, ok := s.conns[deviceID]; ok {
		// P1-3 设备 ID 碰撞：同 ID 新连接顶掉旧连接（断网重连的正常路径），
		// 记一条日志；旧连接的 defer 不得误删新连接，见下面的身份比对。
		log.Printf("transport: 设备 %s 新连接顶掉旧连接", deviceID)
		_ = old.Close()
	}
	s.conns[deviceID] = c
	s.mu.Unlock()
	s.reg.Upsert(registry.Device{
		ID: deviceID, Model: hello.Model, IP: host,
		Room: hello.Room, Version: hello.Version,
	})
	s.reg.SetOnline(deviceID, true)
	// N-P2-9（顺带）：hello_ok 写前设写 deadline，对端只建连不读时
	// 不再永久阻塞在写上。必须走 c.SendJSON 而不是 ws.WriteJSON：
	// 客户端读到帧时服务端的 WriteJSON 收尾可能还没返回，此后任何
	// Conn.SendJSON 都会与之并发写同一连接（gorilla 禁止并发写）。
	_ = ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := c.SendJSON(map[string]any{"type": "hello_ok", "device_id": deviceID, "room": hello.Room}); err != nil {
		s.mu.Lock()
		if s.conns[deviceID] == c {
			delete(s.conns, deviceID)
		}
		s.mu.Unlock()
		s.reg.SetOnline(deviceID, false)
		_ = ws.Close()
		return
	}
	_ = ws.SetReadDeadline(time.Now().Add(s.pongWait()))
	// P1-9：hello 成功即调 OnHello（main.go 接 OTA ConfirmHello）。
	if s.OnHello != nil {
		s.OnHello(deviceID, hello.Version)
	}

	// P1-11：Hub 主动发 Ping 的 goroutine；退出时等它停稳再关连接，
	// 避免 Close 与 WriteControl 并发。
	pingDone := make(chan struct{})
	var pingWg sync.WaitGroup
	pingWg.Add(1)
	go func() {
		defer pingWg.Done()
		t := time.NewTicker(s.pingInterval())
		defer t.Stop()
		for {
			select {
			case <-pingDone:
				return
			case <-t.C:
				if err := c.SendPing(); err != nil {
					return
				}
			}
		}
	}()

	defer func() {
		close(pingDone)
		pingWg.Wait()
		_ = ws.Close()
		// P1-12：按连接身份比对清理——只有 map 里仍是本连接时才删；
		// 旧连接的 defer 不得误删重连后刚注册的新连接。
		s.mu.Lock()
		if s.conns[deviceID] == c {
			delete(s.conns, deviceID)
		}
		s.mu.Unlock()
		s.regMu.Lock()
		delete(s.registered, deviceID)
		s.regMu.Unlock()
		// 只有被删的才是本连接时才标离线：新连接已在 map 里时，
		// 本连接的退出不得把设备标离线。
		s.mu.RLock()
		_, stillSame := s.conns[deviceID]
		s.mu.RUnlock()
		if !stillSame {
			s.reg.SetOnline(deviceID, false)
		}
		if s.handler != nil {
			s.handler.OnDisconnect(deviceID)
		}
	}()

	for {
		mt, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		// 任意消息都续期读 deadline（P1-11：活跃连接不被误杀）。
		_ = ws.SetReadDeadline(time.Now().Add(s.pongWait()))
		if mt == websocket.BinaryMessage {
			if len(data) >= 1 && data[0] == 0x01 && s.handler != nil {
				s.handler.OnAudioFrame(deviceID, data[1:])
			}
			continue
		}
		var e Envelope
		if err := json.Unmarshal(data, &e); err != nil {
			continue
		}
		// P1-44：注册与心跳在已有 WS 连接上处理，不经过 Handler。
		switch e.Type {
		case "register":
			s.handleRegister(c, deviceID, hello.Model, data)
			continue
		case "heartbeat":
			s.regMu.Lock()
			if _, ok := s.registered[deviceID]; ok {
				s.registered[deviceID] = time.Now()
			}
			s.regMu.Unlock()
			continue
		}
		if s.handler == nil {
			continue
		}
		switch e.Type {
		case "wake":
			var m Wake
			if json.Unmarshal(data, &m) == nil {
				s.handler.OnWake(deviceID, m)
			}
		case "audio_start":
			s.handler.OnAudioStart(deviceID)
		case "audio_end":
			s.handler.OnAudioEnd(deviceID)
		case "room_claim":
			var m RoomClaim
			if json.Unmarshal(data, &m) == nil {
				s.handler.OnRoomClaim(deviceID, m.Room)
			}
		case "status":
			var m Status
			if json.Unmarshal(data, &m) == nil {
				s.handler.OnStatus(deviceID, m)
			}
		case "rpc_result":
			var m RPCResult
			if json.Unmarshal(data, &m) == nil {
				s.handler.OnRPCResult(deviceID, m)
			}
		}
	}
}

// handleRegister 处理客户端的主动注册（P1-44）：
// device_id 须与本连接的 deviceID 一致、model 须与 hello 一致、
// 指纹须在白名单内，三者任一不符即记日志、回 register_reject 并断开。
// 通过则记为在线（注册表更新型号/房间）、记心跳时间、回 register_ok。
func (s *Server) handleRegister(c *Conn, deviceID, helloModel string, data []byte) {
	var m Register
	if err := json.Unmarshal(data, &m); err != nil {
		log.Printf("transport: 设备 %s register 解析失败: %v", deviceID, err)
		return
	}
	reject := func(reason string) {
		log.Printf("transport: 设备 %s 注册被拒（%s）", deviceID, reason)
		_ = c.SendJSON(map[string]string{"type": "register_reject", "reason": reason})
		_ = c.Close()
	}
	if m.DeviceID != deviceID {
		reject("device_id 与连接不一致")
		return
	}
	if m.Model != helloModel {
		reject("model 与 hello 不一致")
		return
	}
	if len(s.Fingerprints) == 0 {
		reject("未配置指纹白名单")
		return
	}
	if !s.fingerprintAllowed(m.Fingerprint) {
		reject("fingerprint 不在白名单内")
		return
	}
	s.regMu.Lock()
	s.registered[deviceID] = time.Now()
	s.regMu.Unlock()
	s.reg.Upsert(registry.Device{ID: deviceID, Model: m.Model, Room: m.Room})
	s.reg.SetOnline(deviceID, true)
	_ = c.SendJSON(map[string]string{"type": "register_ok", "device_id": deviceID})
}

// DeviceIDFor 供测试用：按 model + IP 算设备 ID（与 handleWS 同规则）。
func DeviceIDFor(model, ip string) string {
	return fmt.Sprintf("%s-%s", model, strings.ReplaceAll(ip, ".", "-"))
}
