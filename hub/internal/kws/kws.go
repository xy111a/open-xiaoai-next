// Package kws 是 Hub 到服务端 KWS 边车（kws-sidecar）的 WebSocket 客户端。
//
// 接口契约（已定，见任务书；不要擅自改）：
//   - 边车地址默认 ws://127.0.0.1:10400/v1/kws（Hub 与边车都走 host 网络，
//     见 deploy/docker-compose.yml；容器名 kws-sidecar 在 host 网络下解析不到）；
//     健康检查 HTTP GET /health。
//   - Hub→边车首条消息 {"type":"hello","device_id":"...","sample_rate":16000}。
//   - Hub→边车二进制帧：原始 PCM int16 LE 单声道 16kHz（20ms 一帧 = 640 字节）。
//     PCM 由 Hub 的 Opus 解码结果转发，本包不再解码。
//   - 边车→Hub JSON：{"type":"ready"}、
//     {"type":"wake","device_id":"...","keyword":"你好小李","rms":0.42,"timestamp_ms":...}、
//     {"type":"error","message":"..."}。
//
// 连接模型：每设备一路独立 WS 流（边车靠连接区分设备，二进制帧本身不带
// device_id，所以不能多设备复用一条连接）。流断开后按退避重试自动重连；
// 单设备断开（Hub 侧 OnDisconnect）即清理其流状态。
//
// 背压：每设备队列默认 50 帧（约 1 秒音频）。队列满时丢最旧帧并计数、
// 节流记日志；SendPCM 永不阻塞调用方（不堵 OnAudioFrame）。
package kws

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/huajun/open-xiaoai-next/hub/internal/config"
)

// DefaultSampleRate 是边车期望的采样率（16kHz 单声道）。
const DefaultSampleRate = 16000

// DefaultQueueFrames 是每设备 PCM 队列上限（帧数，20ms 一帧，50 帧 ≈ 1 秒）。
const DefaultQueueFrames = 50

// maxInboundBytes 是边车下行 JSON 消息的长度上限（64KB；wake/ready/error
// 都是小 JSON，超限即断线重连，不读无界数据）。
const maxInboundBytes = 64 * 1024

// logDropEvery 是丢帧日志的节流间隔：首丢即记，之后每丢满这么多帧再记一次，
// 避免边车断线期间每帧都刷一条日志。
const logDropEvery = 100

// WakeEvent 是边车上报的一次唤醒命中。
type WakeEvent struct {
	DeviceID string
	Keyword  string
	RMS      float64 // 线性 RMS（0..1 量级，如 0.42）；仲裁换算 dB 由调用方做
	RMSPeak  float64 // 同一 1s 窗口内单帧 RMS 的峰值（S-1，边车发 rms_peak）；
	// rms 是均值，会被检出后的 trailing 静音稀释，仲裁应优先用峰值。
	TimestampMs int64
	ReceivedAt  time.Time
}

// EffectiveRMS 是仲裁用的线性强度：优先用边车上报的 1s 窗口峰值
// rms_peak（不受检出延迟导致的静音稀释影响）；旧边车不发 rms_peak
// （<=0）时回退到均值 rms。
func (ev WakeEvent) EffectiveRMS() float64 {
	if ev.RMSPeak > 0 {
		return ev.RMSPeak
	}
	return ev.RMS
}

// Options 是 Client 的可选项。
type Options struct {
	SampleRate int // 默认 16000
	// Keywords 是接受的唤醒词表；为空表示不过滤（全部接受）。
	// 正常由 config.KWS.Keywords 传入。
	Keywords []string
	// QueueFrames 是每设备 PCM 队列上限（帧数）；<=0 时用 DefaultQueueFrames。
	QueueFrames int
	// DialTimeout 是单次 WS 拨号超时；<=0 时 5s。
	DialTimeout time.Duration
	// ReconnectInitial / ReconnectMax 是断线重连的退避区间；<=0 时 500ms / 30s。
	ReconnectInitial time.Duration
	ReconnectMax     time.Duration
	// HealthInterval 是后台健康检查间隔；<=0 时 10s（Start 后生效）。
	// N-P2-5：旧默认 30s，边车挂掉/恢复后 kws_status 最长滞后 30 秒。
	HealthInterval time.Duration
	// TLSFingerprint 是边车 wss 自签证书的 SHA-256 指纹（小写 hex，
	// N-P2-10）：配了才允许 wss + 自签证书的边车，拨号与健康检查
	// 共用同一 pin 配置；不配时 wss 只认系统根证书；ws 地址下忽略。
	// 由 config.KWS.Fingerprint 传入。
	TLSFingerprint string
	// Token 是边车鉴权 Token（SC-P1-3）：与 Hub 的 OPEN_XIAOAI_TOKEN
	// 复用同一值，hello 里必须带上；边车缺失/错误即回 error + 1008 关闭。
	// 生产由 main.go 从 config.Token() 传入；为空时边车会拒绝连接
	// （Hub 启动时已由 transport.ValidateToken 保证 Token 非空且 ≥16 位）。
	Token string
	// Logger 为空时用标准 log。
	Logger *log.Logger
}

// Status 是客户端当前状态（供 /api/status 报告 kws_down 用）。
type Status struct {
	Streams       int               // 已建流的设备数（发过音频的设备）
	Connected     int               // 当前已连接边车的流数
	Down          bool              // 边车不可用：健康检查失败，或有流但一条都没连上
	LastError     string            // 最近一次拨号/健康检查错误（无则为空）
	DroppedFrames map[string]uint64 // 每设备累计丢帧数
}

func (o *Options) applyDefaults() {
	if o.SampleRate <= 0 {
		o.SampleRate = DefaultSampleRate
	}
	if o.QueueFrames <= 0 {
		o.QueueFrames = DefaultQueueFrames
	}
	if o.DialTimeout <= 0 {
		o.DialTimeout = 5 * time.Second
	}
	if o.ReconnectInitial <= 0 {
		o.ReconnectInitial = 500 * time.Millisecond
	}
	if o.ReconnectMax <= 0 {
		o.ReconnectMax = 30 * time.Second
	}
	if o.HealthInterval <= 0 {
		o.HealthInterval = 10 * time.Second
	}
	if o.Logger == nil {
		o.Logger = log.Default()
	}
}

// Client 是边车 WS 客户端：每设备一路流，wake 事件进 Events channel。
type Client struct {
	url       string
	healthURL string
	opts      Options
	keywords  map[string]bool
	// httpClient 是健康检查用的 HTTP 客户端（N-P2-10：与 WS 拨号
	// 共用同一 TLS 配置，wss + 自签证书时健康检查不再默认校验失败）。
	httpClient *http.Client

	events chan WakeEvent

	mu      sync.Mutex
	streams map[string]*stream
	closed  bool

	healthMu      sync.Mutex
	healthChecked bool
	healthOK      bool
	healthErr     string

	droppedMu sync.Mutex
	dropped   map[string]uint64

	closeOnce sync.Once
	done      chan struct{}
	wg        sync.WaitGroup
}

// ValidateURL 校验边车地址：只许 ws/wss + 内网 host（与 config.Validate
// 同一套内网判定），不许 userinfo，path 不许为空（空 path 拨号必 404，
// 与其静默连错地址，不如启动即报错）。
func ValidateURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("kws.url 为空（须配边车地址，如 ws://127.0.0.1:10400/v1/kws）")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("kws.url 不是合法 URL：%q", raw)
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return fmt.Errorf("kws.url 的 scheme 必须是 ws 或 wss，实际是 %q", u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("kws.url 不许带 userinfo（可能泄露凭据）")
	}
	if u.Host == "" || !config.IsPrivateHost(u.Hostname()) {
		return fmt.Errorf("kws.url 的 host 必须是内网地址，实际是 %q", raw)
	}
	if u.Path == "" || u.Path == "/" {
		return fmt.Errorf("kws.url 必须带路径（边车 WS 路径，如 /v1/kws），实际是 %q", raw)
	}
	return nil
}

// HealthURL 由 WS 地址推导健康检查地址：ws→http、wss→https，路径固定 /health。
func HealthURL(wsURL string) (string, error) {
	u, err := url.Parse(wsURL)
	if err != nil {
		return "", fmt.Errorf("kws.url 不是合法 URL：%q", wsURL)
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	default:
		return "", fmt.Errorf("kws.url 的 scheme 必须是 ws 或 wss，实际是 %q", u.Scheme)
	}
	u.Path = "/health"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

// New 新建 Client（只校验地址，不拨号；拨号在首帧 SendPCM 时按设备懒触发，
// 这样没音频的设备不占连接，边车晚启动也不阻塞 Hub 启动）。
func New(rawURL string, opts Options) (*Client, error) {
	if err := ValidateURL(rawURL); err != nil {
		return nil, err
	}
	healthURL, err := HealthURL(rawURL)
	if err != nil {
		return nil, err
	}
	opts.applyDefaults()
	if opts.TLSFingerprint != "" {
		if _, err := normalizeFingerprintHex(opts.TLSFingerprint); err != nil {
			return nil, err
		}
	}
	c := &Client{
		url:       rawURL,
		healthURL: healthURL,
		opts:      opts,
		events:    make(chan WakeEvent, 32),
		streams:   map[string]*stream{},
		dropped:   map[string]uint64{},
		done:      make(chan struct{}),
	}
	c.httpClient = &http.Client{
		Timeout:   3 * time.Second,
		Transport: &http.Transport{TLSClientConfig: c.tlsConfig()},
	}
	for _, kw := range opts.Keywords {
		if c.keywords == nil {
			c.keywords = map[string]bool{}
		}
		c.keywords[kw] = true
	}
	return c, nil
}

// normalizeFingerprintHex 归一化指纹（S-14：与 config 共用同一个
// 函数 NormalizeFingerprintHex，去冒号/空格/连字符，转小写），
// 必须是 64 位 hex（大小写不限），否则返回错误。
func normalizeFingerprintHex(fp string) (string, error) {
	normalized, err := config.NormalizeFingerprintHex(fp)
	if err != nil {
		return "", fmt.Errorf("kws TLS 指纹%w", err)
	}
	return normalized, nil
}

// tlsConfig 返回边车连接的 TLS 配置（N-P2-10）：
//   - ws 地址：nil（不走 TLS）；
//   - wss 且未配指纹：nil（系统根证书校验，默认行为）；
//   - wss 且配了指纹：pin 该指纹（InsecureSkipVerify + 自定义校验，
//     同时校验有效期），拨号与健康检查共用。
func (c *Client) tlsConfig() *tls.Config {
	u, err := url.Parse(c.url)
	if err != nil || u.Scheme != "wss" {
		return nil
	}
	fp, err := normalizeFingerprintHex(c.opts.TLSFingerprint)
	if err != nil || fp == "" {
		return nil
	}
	return &tls.Config{
		InsecureSkipVerify: true, // 指纹 pin 本身就是校验，不走系统根证书
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("kws: 边车未出示证书")
			}
			cert, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return fmt.Errorf("kws: 边车证书解析失败: %w", err)
			}
			now := time.Now()
			if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
				return fmt.Errorf("kws: 边车证书不在有效期内")
			}
			sum := sha256.Sum256(rawCerts[0])
			if hex.EncodeToString(sum[:]) != fp {
				return fmt.Errorf("kws: 边车证书指纹不匹配")
			}
			return nil
		},
	}
}

// Events 返回 wake 事件 channel（缓冲 32；满时丢事件并记日志，不堵读循环）。
func (c *Client) Events() <-chan WakeEvent { return c.events }

// Start 启动后台健康检查（每 HealthInterval GET 一次 /health，更新 Down 状态）。
// 不调 Start 也不影响音频流与重连；main 里调一次即可。
// N-P2-5：启动即先查一次，不等第一个 tick（边车挂了 30 秒内状态还是旧的）。
func (c *Client) Start() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.wg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		_ = c.CheckHealth(context.Background())
		t := time.NewTicker(c.opts.HealthInterval)
		defer t.Stop()
		for {
			select {
			case <-c.done:
				return
			case <-t.C:
				_ = c.CheckHealth(context.Background())
			}
		}
	}()
}

// CheckHealth 对边车做一次 HTTP GET /health（3s 超时），更新健康状态并返回结果。
// N-P2-10：走与 WS 拨号同一 TLS 配置（指纹 pin 时健康检查也认自签证书）。
func (c *Client) CheckHealth(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.healthURL, nil)
	if err != nil {
		c.setHealth(false, err.Error())
		return err
	}
	client := c.httpClient
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		c.setHealth(false, err.Error())
		return fmt.Errorf("kws 边车健康检查失败（GET %s）: %w", c.healthURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("kws 边车健康检查返回 HTTP %d（GET %s）", resp.StatusCode, c.healthURL)
		c.setHealth(false, err.Error())
		return err
	}
	c.setHealth(true, "")
	return nil
}

// triggerHealthCheck 在流连接状态变化时立即触发一次健康检查
// （N-P2-5：不等下一个 tick，kws_status 不再最长滞后 30 秒）。
// 异步跑，不阻塞调用方（拨号/断线路径）。
func (c *Client) triggerHealthCheck() {
	go func() { _ = c.CheckHealth(context.Background()) }()
}

func (c *Client) setHealth(ok bool, errMsg string) {
	c.healthMu.Lock()
	c.healthChecked = true
	c.healthOK = ok
	c.healthErr = errMsg
	c.healthMu.Unlock()
}

// Status 返回当前状态。
func (c *Client) Status() Status {
	c.mu.Lock()
	streams := len(c.streams)
	connected := 0
	for _, s := range c.streams {
		if s.isConnected() {
			connected++
		}
	}
	c.mu.Unlock()
	c.healthMu.Lock()
	healthChecked, healthOK, healthErr := c.healthChecked, c.healthOK, c.healthErr
	c.healthMu.Unlock()
	down := false
	if healthChecked && !healthOK {
		down = true
	}
	if streams > 0 && connected == 0 {
		down = true
	}
	c.droppedMu.Lock()
	dropped := make(map[string]uint64, len(c.dropped))
	for k, v := range c.dropped {
		dropped[k] = v
	}
	c.droppedMu.Unlock()
	return Status{
		Streams: streams, Connected: connected, Down: down,
		LastError: healthErr, DroppedFrames: dropped,
	}
}

// DroppedFrames 返回某设备的累计丢帧数。
func (c *Client) DroppedFrames(deviceID string) uint64 {
	c.droppedMu.Lock()
	defer c.droppedMu.Unlock()
	return c.dropped[deviceID]
}

// SendPCM 把一帧已解码 PCM（int16 LE 单声道 16kHz，正常 320 点 = 640 字节）
// 发给边车。永不阻塞：队列满时丢最旧帧并计数、节流记日志。
// 注意：pcm 切片调用后即被拷贝，调用方可以复用底层数组。
func (c *Client) SendPCM(deviceID string, pcm []int16) {
	if len(pcm) == 0 {
		return
	}
	buf := make([]byte, len(pcm)*2)
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(v))
	}
	// N-P2-2：closed 检查与建流必须在同一把 c.mu 内完成——
	// 旧代码先 isClosed() 检查（释放锁）、再 streamFor 建流，
	// 两步之间 Close() 可插进来完成 wg.Wait() 并返回，随后建流的
	// wg.Add(1) 启动一个永不被等的流 goroutine（拨号泄漏）。
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	s := c.streamForLocked(deviceID)
	c.mu.Unlock()
	select {
	case s.queue <- buf:
	default:
		// 队列满：丢最旧一帧再入队（边车断线/消费慢时不无限缓冲、不堵 OnAudioFrame）。
		select {
		case <-s.queue:
		default:
		}
		select {
		case s.queue <- buf:
		default:
			// 极端竞态下仍满：直接丢当前帧。
		}
		c.recordDrop(deviceID)
	}
}

func (c *Client) recordDrop(deviceID string) {
	c.recordDrops(deviceID, 1)
}

// recordDrops 累计丢帧数（N-P2-3：重连排空陈旧帧时批量记）。
func (c *Client) recordDrops(deviceID string, n uint64) {
	c.droppedMu.Lock()
	c.dropped[deviceID] += n
	total := c.dropped[deviceID]
	c.droppedMu.Unlock()
	if total == n || total%logDropEvery < n {
		c.opts.Logger.Printf("kws: 唤醒服务暂不可用，丢弃设备 %s 的音频帧（累计丢 %d 帧，边车断线或消费过慢）", deviceID, total)
	}
}

// streamForLocked 取或新建某设备的流（调用方须已持 c.mu；
// Close 后不再新建，见 N-P2-2）。
func (c *Client) streamForLocked(deviceID string) *stream {
	if s, ok := c.streams[deviceID]; ok {
		return s
	}
	s := newStream(c, deviceID)
	c.streams[deviceID] = s
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		s.run()
	}()
	return s
}

// RemoveDevice 关闭并清理某设备的流（设备断开时调；之后再有该设备音频会重建流）。
func (c *Client) RemoveDevice(deviceID string) {
	c.mu.Lock()
	s, ok := c.streams[deviceID]
	if ok {
		delete(c.streams, deviceID)
	}
	c.mu.Unlock()
	if ok {
		s.stop()
	}
}

// Close 停止全部流与后台健康检查（Hub 退出时调）。
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		close(c.done)
		c.mu.Lock()
		c.closed = true
		streams := make([]*stream, 0, len(c.streams))
		for _, s := range c.streams {
			streams = append(streams, s)
		}
		c.mu.Unlock()
		for _, s := range streams {
			s.stop()
		}
		c.wg.Wait()
	})
}

// ---- 每设备流 ----

type stream struct {
	client   *Client
	deviceID string
	queue    chan []byte

	stopCh   chan struct{}
	stopOnce sync.Once

	connMu    sync.Mutex
	conn      *websocket.Conn
	connected atomic.Bool
	// everConnected 是该流曾经连上过边车（N-P2-3：只在重连时排空
	// 陈旧 PCM；首次连接不排空）。只在 run/serve 同一 goroutine
	// 里读写，无需加锁。
	everConnected bool
}

func newStream(c *Client, deviceID string) *stream {
	return &stream{
		client:   c,
		deviceID: deviceID,
		queue:    make(chan []byte, c.opts.QueueFrames),
		stopCh:   make(chan struct{}),
	}
}

func (s *stream) isConnected() bool { return s.connected.Load() }

func (s *stream) stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
		s.closeConn()
	})
}

func (s *stream) closeConn() {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
}

func (s *stream) stopped() bool {
	select {
	case <-s.stopCh:
		return true
	case <-s.client.done:
		return true
	default:
		return false
	}
}

// sleepOrStop 可打断的退避等待；返回 false 表示流已停，应退出。
func (s *stream) sleepOrStop(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-s.stopCh:
		return false
	case <-s.client.done:
		return false
	case <-t.C:
		return true
	}
}

// run 是流的主循环：拨号 → hello → 收发 → 断线退避重连。
func (s *stream) run() {
	backoff := s.client.opts.ReconnectInitial
	for {
		if s.stopped() {
			return
		}
		conn, err := s.dial()
		if err != nil {
			s.client.opts.Logger.Printf("kws: 边车拨号失败（设备 %s，%s 后重试）: %v", s.deviceID, backoff, err)
			if !s.sleepOrStop(backoff) {
				return
			}
			backoff = nextBackoff(backoff, s.client.opts.ReconnectMax)
			continue
		}
		backoff = s.client.opts.ReconnectInitial
		if err := s.serve(conn); err != nil && !s.stopped() {
			s.client.opts.Logger.Printf("kws: 边车连接断开（设备 %s）: %v", s.deviceID, err)
		}
		s.connected.Store(false)
		// N-P2-5：断线即触发一次健康检查，kws_status 不等下一个 tick。
		s.client.triggerHealthCheck()
		if s.stopped() {
			return
		}
		// serve 刚断：立即重连一次太冲，先退避一轮再拨。
		if !s.sleepOrStop(backoff) {
			return
		}
		backoff = nextBackoff(backoff, s.client.opts.ReconnectMax)
	}
}

// nextBackoff 算下一次重连退避：翻倍，上限封顶，再加 ±25% jitter
// （N-P2-12：旧代码纯翻倍，两台设备的流在边车同一次重启后退避值
// 完全相同，重连脉冲同步化；jitter 把脉冲打散）。
func nextBackoff(cur, max time.Duration) time.Duration {
	nxt := cur * 2
	if nxt > max {
		nxt = max
	}
	if nxt <= 0 {
		return nxt
	}
	jitter := nxt / 4
	delta := time.Duration(rand.Int63n(int64(2*jitter+1))) - jitter
	nxt += delta
	if nxt > max {
		nxt = max
	}
	if nxt <= 0 {
		return cur
	}
	return nxt
}

func (s *stream) dial() (*websocket.Conn, error) {
	dialer := websocket.Dialer{
		HandshakeTimeout: s.client.opts.DialTimeout,
		// N-P2-10：wss + 自签证书时用指纹 pin 的 TLS 配置；
		// 未配指纹时为 nil（系统根证书，默认行为）。
		TLSClientConfig: s.client.tlsConfig(),
	}
	conn, _, err := dialer.Dial(s.client.url, nil)
	if err != nil {
		return nil, err
	}
	s.connMu.Lock()
	s.conn = conn
	s.connMu.Unlock()
	return conn, nil
}

// serve 在一条已建立的连接上跑收发：发 hello，写泵发 PCM、定时 ping，
// 读泵收 wake/ready/error；任一泵出错即返回（run 负责重连）。
func (s *stream) serve(conn *websocket.Conn) error {
	// N-P2-3：重连时先排空断线期积压的陈旧 PCM（约 1 秒旧音频）——
	// 旧代码直接排空同一队列，断线前/断线中的旧音频被当实时音频发给
	// 边车，检出关键词即幽灵唤醒。排空的帧计入丢帧数。
	// 注意：只在重连时排空——首次连接时队列里是拨号期间刚到的
	// 实时音频，丢掉就等于丢一整句的开头。
	// 顺序关键：排空必须在发 hello 之前。hello 是对端可观测到的
	// “已重连”信号，调用方（如测试）收到 hello 后发的实时帧若落在
	// hello 之后、排空之前，就会被误当陈旧帧丢掉（-race 下偶发，
	// TestReconnectAfterDrop 2026-11-18 复现）。先排空再发 hello，
	// 此后入队的一律是实时帧，不再被排空。
	if s.everConnected {
		if n := s.drainQueue(); n > 0 {
			s.client.opts.Logger.Printf("kws: 边车重连，丢弃设备 %s 断线期积压的 %d 帧陈旧音频", s.deviceID, n)
			s.client.recordDrops(s.deviceID, uint64(n))
		}
	}
	hello := map[string]any{
		"type": "hello", "device_id": s.deviceID, "sample_rate": s.client.opts.SampleRate,
		// SC-P1-3 对接：边车要求 hello 带 token（hmac 常数时间比较），
		// 缺失/错误即 error + 1008 关闭；不带 token 时升级边车后唤醒立即全断。
		"token": s.client.opts.Token,
	}
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := conn.WriteJSON(hello); err != nil {
		_ = conn.Close()
		return fmt.Errorf("发 hello 失败: %w", err)
	}
	conn.SetReadLimit(maxInboundBytes)
	s.connected.Store(true)
	s.client.opts.Logger.Printf("kws: 边车已连接（设备 %s，%s）", s.deviceID, s.client.url)
	s.everConnected = true
	// N-P2-5：连上即触发一次健康检查，kws_status 不等下一个 tick。
	s.client.triggerHealthCheck()

	done := make(chan struct{})
	var once sync.Once
	closeDone := func() { once.Do(func() { close(done) }) }

	errCh := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		errCh <- s.writePump(conn, done)
		closeDone()
	}()
	go func() {
		defer wg.Done()
		errCh <- s.readPump(conn, done)
		closeDone()
	}()
	// 任一泵退出即关连接（让另一泵也退出），等两泵都停，返回先到的错误。
	<-done
	_ = conn.Close()
	wg.Wait()
	s.closeConn()
	return <-errCh
}

// drainQueue 非阻塞排空流队列，返回丢弃的帧数（N-P2-3）。
func (s *stream) drainQueue() int {
	n := 0
	for {
		select {
		case <-s.queue:
			n++
		default:
			return n
		}
	}
}

// writePump 从队列取 PCM 发二进制帧；每 30s 发一次 ping 保活。
// done 关闭或写失败即退出，返回写错误（正常停止时返回 nil）。
func (s *stream) writePump(conn *websocket.Conn, done chan struct{}) error {
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-done:
			return nil
		case <-s.stopCh:
			return nil
		case <-s.client.done:
			return nil
		case <-ping.C:
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
				return fmt.Errorf("发 ping 失败: %w", err)
			}
		case frame := <-s.queue:
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
				return fmt.Errorf("发 PCM 失败: %w", err)
			}
		}
	}
}

// readPump 收边车下行 JSON：ready 记日志、wake 进事件 channel、
// error 记日志不断线；读失败即退出（触发重连），返回读错误（正常停止时返回 nil）。
func (s *stream) readPump(conn *websocket.Conn, done chan struct{}) error {
	conn.SetPongHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		return nil
	})
	for {
		select {
		case <-done:
			return nil
		default:
		}
		_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		mt, data, err := conn.ReadMessage()
		if err != nil {
			select {
			case <-done:
				return nil
			default:
				return fmt.Errorf("读边车消息失败: %w", err)
			}
		}
		if mt != websocket.TextMessage {
			continue
		}
		s.handleMessage(data)
	}
}

func (s *stream) handleMessage(data []byte) {
	var env struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		s.client.opts.Logger.Printf("kws: 边车消息解析失败（设备 %s）: %v", s.deviceID, err)
		return
	}
	switch env.Type {
	case "ready":
		s.client.opts.Logger.Printf("kws: 边车就绪（设备 %s）", s.deviceID)
	case "wake":
		var m struct {
			DeviceID    string  `json:"device_id"`
			Keyword     string  `json:"keyword"`
			RMS         float64 `json:"rms"`
			RMSPeak     float64 `json:"rms_peak"`
			TimestampMs int64   `json:"timestamp_ms"`
		}
		if err := json.Unmarshal(data, &m); err != nil {
			s.client.opts.Logger.Printf("kws: wake 消息解析失败（设备 %s）: %v", s.deviceID, err)
			return
		}
		if len(s.client.keywords) > 0 && !s.client.keywords[m.Keyword] {
			s.client.opts.Logger.Printf("kws: 忽略未配置的唤醒词 %q（设备 %s）", m.Keyword, s.deviceID)
			return
		}
		ev := WakeEvent{
			// N-P2-4：wake 事件的 device_id 一律用流自身的 ID，
			// 不轻信边车回传值——边车异常/被冒充回别的 device_id，
			// Hub 会给从未唤醒的设备开会话、发 wake_win。
			// 回传值不一致时记 warn。
			DeviceID:    s.deviceID,
			Keyword:     m.Keyword,
			RMS:         m.RMS,
			RMSPeak:     m.RMSPeak,
			TimestampMs: m.TimestampMs,
			ReceivedAt:  time.Now(),
		}
		if m.DeviceID != "" && m.DeviceID != s.deviceID {
			s.client.opts.Logger.Printf("kws: 边车回传的 wake device_id %q 与流 %q 不一致，以流 ID 为准", m.DeviceID, s.deviceID)
		}
		select {
		case s.client.events <- ev:
		default:
			s.client.opts.Logger.Printf("kws: wake 事件队列满，丢弃设备 %s 的唤醒 %q", ev.DeviceID, ev.Keyword)
		}
	case "error":
		var m struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(data, &m); err != nil {
			s.client.opts.Logger.Printf("kws: error 消息解析失败（设备 %s）: %v", s.deviceID, err)
			return
		}
		s.client.opts.Logger.Printf("kws: 边车报错（设备 %s）: %s", s.deviceID, m.Message)
	default:
		s.client.opts.Logger.Printf("kws: 边车未知消息类型 %q（设备 %s），忽略", env.Type, s.deviceID)
	}
}
