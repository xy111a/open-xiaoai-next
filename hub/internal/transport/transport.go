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
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
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

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
	// 局域网内使用；生产部署如需收紧，在此校验 Origin。
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
}

// SetClientCAs 启用 mTLS 客户端证书校验（传入 CA 证书池；传 nil 则关闭）。
func (s *Server) SetClientCAs(pool *x509.CertPool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clientCAs = pool
}

// New 新建 Server。token 为空表示不鉴权（只允许测试用，生产必须设）。
func New(token string, reg *registry.Registry, h Handler) *Server {
	return &Server{token: token, reg: reg, handler: h, conns: map[string]*Conn{}}
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
	srv := &http.Server{Handler: mux}
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
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, first, err := ws.ReadMessage()
	if err != nil {
		return
	}
	var env Envelope
	if err := json.Unmarshal(first, &env); err != nil || env.Type != "hello" {
		_ = ws.WriteJSON(map[string]string{"type": "hello_reject", "reason": "first message must be hello"})
		return
	}
	var hello Hello
	if err := json.Unmarshal(first, &hello); err != nil {
		_ = ws.WriteJSON(map[string]string{"type": "hello_reject", "reason": "bad hello"})
		return
	}
	if s.token != "" && !tokenEqual(hello.Token, s.token) {
		_ = ws.WriteJSON(map[string]string{"type": "hello_reject", "reason": "bad token"})
		return
	}
	if hello.Model != "lx06" && hello.Model != "oh2p" {
		_ = ws.WriteJSON(map[string]string{"type": "hello_reject", "reason": "unsupported model"})
		return
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	deviceID := hello.Model + "-" + strings.ReplaceAll(host, ".", "-")
	c := &Conn{DeviceID: deviceID, conn: ws}
	s.mu.Lock()
	if old, ok := s.conns[deviceID]; ok {
		_ = old.Close()
	}
	s.conns[deviceID] = c
	s.mu.Unlock()
	s.reg.Upsert(registry.Device{
		ID: deviceID, Model: hello.Model, IP: host,
		Room: hello.Room, Version: hello.Version,
	})
	s.reg.SetOnline(deviceID, true)
	_ = ws.WriteJSON(map[string]any{"type": "hello_ok", "device_id": deviceID, "room": hello.Room})
	_ = ws.SetReadDeadline(time.Time{}) // 取消读超时，靠 ping/pong 保活
	ws.SetPongHandler(func(string) error {
		_ = ws.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})
	_ = ws.SetReadDeadline(time.Now().Add(60 * time.Second))

	defer func() {
		s.mu.Lock()
		delete(s.conns, deviceID)
		s.mu.Unlock()
		s.reg.SetOnline(deviceID, false)
		if s.handler != nil {
			s.handler.OnDisconnect(deviceID)
		}
	}()

	for {
		mt, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
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

// DeviceIDFor 供测试用：按 model + IP 算设备 ID（与 handleWS 同规则）。
func DeviceIDFor(model, ip string) string {
	return fmt.Sprintf("%s-%s", model, strings.ReplaceAll(ip, ".", "-"))
}
