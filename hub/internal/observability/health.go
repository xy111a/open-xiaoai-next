package observability

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 健康检查 JSON 契约（GET /api/health）：
//
//	{
//	  "services": [ServiceHealth...], // 固定 7 条，顺序与 deploy/docker-compose.yml 一致
//	  "checked_at": "2026-11-19T17:00:00+08:00",
//	  "healthy": 6, "total": 7, "down": 1
//	}
//
// ServiceHealth:
//
//	{
//	  "name": "whisper", "container": "oxn-whisper",
//	  "kind": "wyoming", "address": "127.0.0.1:10300",
//	  "status": "ok",        // ok | down | disabled
//	  "latency_ms": 12,      // 探活耗时；down 时是失败前耗掉的时间
//	  "detail": "describe → info" // ok 时的成功证据；down 时是失败原因
//	}
//
// 探活方式按服务定（deploy/docker-compose.yml 的端口与协议为准）：
//   - 有 HTTP /health 的走 HTTP：kws-sidecar（GET /health 须 200）；
//   - 没 /health 的走协议级 ping，不只 TCP 连通：
//     whisper/piper 用 Wyoming describe（发 describe、等 info 回包）；
//     mpd 发 ping、等 OK；snapserver 发 Server.GetStatus JSON-RPC、
//     等同 id 的回包；netease-api 没 /health，根路径 GET 任意 HTTP
//     回包（<500）即算通，状态码写进 detail；
//   - hub 自身（本进程）不探网络，status 恒 ok。
// 所有探活都有单服务超时（默认 3s）：超时报 down、不挂住 Check，
// 也不拖住其它服务（CheckAll 并发跑）。
//
// KWS 未启用时 kws-sidecar 一行 status 为 disabled，不探、不计入 down。

// DefaultHealthTimeout 是单个服务的探活超时。
const DefaultHealthTimeout = 3 * time.Second

// ServiceKind 是探活方式。
type ServiceKind string

const (
	// KindSelf 是 Hub 本进程：不探网络，恒 ok。
	KindSelf ServiceKind = "self"
	// KindHTTP 是 HTTP GET 探活（/health 存在时）。
	KindHTTP ServiceKind = "http"
	// KindWyoming 是 Wyoming describe 探活。
	KindWyoming ServiceKind = "wyoming"
	// KindMPD 是 MPD ping 探活。
	KindMPD ServiceKind = "mpd"
	// KindSnapcast 是 Snapcast JSON-RPC Server.GetStatus 探活。
	KindSnapcast ServiceKind = "snapcast"
)

// ServiceStatus 是 ServiceHealth.Status 的取值。
type ServiceStatus string

const (
	// StatusOK 探活通过。
	StatusOK ServiceStatus = "ok"
	// StatusDown 探活失败（超时、连不上、协议回包不对）。
	StatusDown ServiceStatus = "down"
	// StatusDisabled 服务未启用（目前只有 KWS 未启用时），不探活。
	StatusDisabled ServiceStatus = "disabled"
)

// Service 是一项待探活的服务定义（Checker 的输入）。
type Service struct {
	Name      string      `json:"name"`
	Container string      `json:"container"`
	Kind      ServiceKind `json:"kind"`
	Address   string      `json:"address"`
	// Enabled=false 时不探活，状态直接记 disabled（KWS 未启用时用）。
	Enabled bool `json:"-"`
	// AcceptAnyStatus=true 时 HTTP 探活接受任意 <500 的状态码
	// （netease-api 根路径没 /health：回 200 固然好，
	// 回 404 也证明进程在听，不应误报 down；状态码写进 detail）。
	// 为 false 时只接受 200。
	AcceptAnyStatus bool `json:"-"`
}

// ServiceHealth 是一项服务的探活结果。
type ServiceHealth struct {
	Name      string        `json:"name"`
	Container string        `json:"container"`
	Kind      ServiceKind   `json:"kind"`
	Address   string        `json:"address"`
	Status    ServiceStatus `json:"status"`
	LatencyMs int64         `json:"latency_ms"`
	Detail    string        `json:"detail"`
}

// HealthReport 是一次全量探活的结果。
type HealthReport struct {
	Services  []ServiceHealth `json:"services"`
	CheckedAt time.Time       `json:"checked_at"`
	Healthy   int             `json:"healthy"`
	Total     int             `json:"total"`
	Down      int             `json:"down"`
}

// Checker 对一组服务做并发探活。
type Checker struct {
	Services []Service
	// Timeout 是单个服务的探活超时；<=0 时用 DefaultHealthTimeout。
	Timeout time.Duration
	// HTTPClient 是 HTTP 探活用客户端；为 nil 时用默认（超时=Timeout）。
	// 测试可注入指向 httptest 服务器的客户端。
	HTTPClient *http.Client
}

// NewChecker 新建 Checker。
func NewChecker(services []Service, timeout time.Duration) *Checker {
	if timeout <= 0 {
		timeout = DefaultHealthTimeout
	}
	return &Checker{Services: services, Timeout: timeout}
}

// KWSHealthURL 把边车 WS 地址换算成 HTTP /health 地址：
// ws://127.0.0.1:10400/v1/kws → http://127.0.0.1:10400/health，
// wss://… → https://…。WS 地址本身不带路径时直接拼 /health。
// 换算失败（空串/解析失败）返回 ""，调用方据此把 kws 标 disabled。
func KWSHealthURL(wsURL string) string {
	wsURL = strings.TrimSpace(wsURL)
	if wsURL == "" {
		return ""
	}
	httpURL := wsURL
	switch {
	case strings.HasPrefix(httpURL, "ws://"):
		// ws:// → http:// 只是回环 KWS 边车地址的协议换算：输入只来自
		// config 的 kws.url（固定是 127.0.0.1 边车 ws 地址），换算出的
		// http 地址只用于本地 /health 探活，不对外取数。
		httpURL = "http://" + strings.TrimPrefix(httpURL, "ws://") // nosemgrep: open-xiaoai-r5-http-literal
	case strings.HasPrefix(httpURL, "wss://"):
		httpURL = "https://" + strings.TrimPrefix(httpURL, "wss://")
	default:
		return ""
	}
	httpURL = strings.TrimSuffix(httpURL, "/")
	httpURL = strings.TrimSuffix(httpURL, "/v1/kws")
	return httpURL + "/health"
}

// HealthAddrs 是 DefaultServices 的输入：各服务的地址都来自
// config.yaml 已有字段，不新增配置项。
type HealthAddrs struct {
	STT        string // hub.stt，例如 localhost:10300
	TTS        string // hub.tts，例如 localhost:10200
	KWSWSURL   string // kws.url，例如 ws://127.0.0.1:10400/v1/kws
	KWSEnabled bool   // kws.enabled
	Snapcast   string // hub.snapcast，例如 localhost:1705
	MPD        string // hub.mpd，例如 localhost:6600
	NeteaseAPI string // netease.api，例如 http://netease-api:3000
}

// DefaultServices 按 deploy/docker-compose.yml 的 7 个容器建服务表，
// 顺序与 compose 文件一致。地址为空的服务仍保留一行，
// 探活时报 down 并在 detail 里写“未配置地址”。
func DefaultServices(a HealthAddrs) []Service {
	kwsAddr := KWSHealthURL(a.KWSWSURL)
	return []Service{
		{Name: "hub", Container: "oxn-hub", Kind: KindSelf, Address: "本进程", Enabled: true},
		{Name: "whisper", Container: "oxn-whisper", Kind: KindWyoming, Address: a.STT, Enabled: a.STT != ""},
		{Name: "piper", Container: "oxn-piper", Kind: KindWyoming, Address: a.TTS, Enabled: a.TTS != ""},
		{Name: "kws-sidecar", Container: "oxn-kws-sidecar", Kind: KindHTTP, Address: kwsAddr, Enabled: a.KWSEnabled && kwsAddr != ""},
		{Name: "snapserver", Container: "oxn-snapserver", Kind: KindSnapcast, Address: a.Snapcast, Enabled: a.Snapcast != ""},
		{Name: "mpd", Container: "oxn-mpd", Kind: KindMPD, Address: a.MPD, Enabled: a.MPD != ""},
		{Name: "netease-api", Container: "oxn-netease-api", Kind: KindHTTP, Address: a.NeteaseAPI, Enabled: a.NeteaseAPI != "", AcceptAnyStatus: true},
	}
}

// Check 对全部服务做一次并发探活：每个服务单独超时，
// 一个服务卡住不拖其它服务；ctx 取消时在途探活也收尾返回。
// 返回的 Services 与 Checker.Services 一一对应、同顺序。
func (c *Checker) Check(ctx context.Context) HealthReport {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultHealthTimeout
	}
	now := time.Now()
	results := make([]ServiceHealth, len(c.Services))
	var wg sync.WaitGroup
	for i, svc := range c.Services {
		wg.Add(1)
		go func(i int, svc Service) {
			defer wg.Done()
			results[i] = c.checkOne(ctx, svc, timeout)
		}(i, svc)
	}
	wg.Wait()
	rep := HealthReport{Services: results, CheckedAt: now, Total: len(results)}
	for _, h := range results {
		switch h.Status {
		case StatusOK:
			rep.Healthy++
		case StatusDown:
			rep.Down++
		}
	}
	return rep
}

// checkOne 探活一个服务：disabled 直接返回；其余按 Kind 分发，
// 全程受单服务超时约束，返回里 latency_ms 是实耗。
func (c *Checker) checkOne(ctx context.Context, svc Service, timeout time.Duration) ServiceHealth {
	h := ServiceHealth{Name: svc.Name, Container: svc.Container, Kind: svc.Kind, Address: svc.Address}
	if !svc.Enabled {
		h.Status = StatusDisabled
		h.Detail = "未启用（配置里没开或没配地址），不探活"
		return h
	}
	if svc.Address == "" {
		h.Status = StatusDown
		h.Detail = "未配置地址，无法探活"
		return h
	}
	if svc.Kind == KindSelf {
		h.Status = StatusOK
		h.Detail = "Hub 本进程正在提供本次探活接口"
		return h
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	detail, err := c.probe(pctx, svc)
	h.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		h.Status = StatusDown
		h.Detail = err.Error()
		return h
	}
	h.Status = StatusOK
	h.Detail = detail
	return h
}

// probe 按 Kind 分发一次探活；返回成功时的 detail 或失败原因。
func (c *Checker) probe(ctx context.Context, svc Service) (string, error) {
	switch svc.Kind {
	case KindHTTP:
		return c.probeHTTP(ctx, svc)
	case KindWyoming:
		return probeWyoming(ctx, svc.Address)
	case KindMPD:
		return probeMPD(ctx, svc.Address)
	case KindSnapcast:
		return probeSnapcast(ctx, svc.Address)
	default:
		return "", fmt.Errorf("未知的探活方式 %q", svc.Kind)
	}
}

// probeHTTP GET 一次：200 即 ok；AcceptAnyStatus 时 <500 也算 ok
// （detail 里写明状态码）；其余状态码、建连/超时失败都报 down。
func (c *Checker) probeHTTP(ctx context.Context, svc Service) (string, error) {
	client := c.HTTPClient
	if client == nil {
		// 默认客户端的超时取 Checker.Timeout（<=0 时用
		// DefaultHealthTimeout 3s），与 Checker 注释“为 nil 时用默认
		// （超时=Timeout）”一致；之前这里是裸 &http.Client{}（无超时），
		// 代码与注释自相矛盾，且规则 3 要求客户端本身设 Timeout。
		// （单次请求另有 ctx 超时兜底，这里是把承诺落到客户端上。）
		timeout := c.Timeout
		if timeout <= 0 {
			timeout = DefaultHealthTimeout
		}
		client = &http.Client{Timeout: timeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, svc.Address, nil)
	if err != nil {
		return "", fmt.Errorf("建请求失败: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP GET 失败: %v", err)
	}
	defer resp.Body.Close()
	// 读掉（限量）再关，避免连接复用时残留 body 拖住。
	_, _ = io.CopyN(io.Discard, resp.Body, 4096)
	if resp.StatusCode == http.StatusOK {
		return "HTTP 200", nil
	}
	if svc.AcceptAnyStatus && resp.StatusCode < 500 {
		return fmt.Sprintf("HTTP %d（服务在听，但该路径非 200）", resp.StatusCode), nil
	}
	return "", fmt.Errorf("HTTP %d", resp.StatusCode)
}

// maxProbeLineBytes 是探活读单行回包的上限（64KB）：
// 异常服务端用无界长行打内存的缺口，正常回包（Wyoming info、
// MPD OK、Snapcast JSON-RPC）远小于此。
const maxProbeLineBytes = 64 << 10

// dialProbe 建一条带 ctx 超时的 TCP 连接，并设好读写 deadline
// （ctx 到期即读写失败，不永久阻塞）。
func dialProbe(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("TCP 连接失败: %v", err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	return conn, nil
}

// readProbeLine 限长读一行（带 \n），超限即错。
func readProbeLine(r *bufio.Reader) (string, error) {
	var line []byte
	for {
		frag, err := r.ReadSlice('\n')
		line = append(line, frag...)
		if len(line) > maxProbeLineBytes {
			return "", fmt.Errorf("回包行超过 %d 字节上限", maxProbeLineBytes)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("读回包失败: %v", err)
		}
		return strings.TrimSpace(string(line)), nil
	}
}

// probeWyoming：发 {"type":"describe"}，等一行 JSON 回包，
// type=="info" 才算 ok（只 TCP 通、回包不对的假活会被揪出来）。
func probeWyoming(ctx context.Context, addr string) (string, error) {
	conn, err := dialProbe(ctx, addr)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("{\"type\":\"describe\"}\n")); err != nil {
		return "", fmt.Errorf("发 describe 失败: %v", err)
	}
	line, err := readProbeLine(bufio.NewReader(conn))
	if err != nil {
		return "", err
	}
	var header map[string]any
	if err := json.Unmarshal([]byte(line), &header); err != nil {
		return "", fmt.Errorf("describe 回包不是 JSON: %v", err)
	}
	if header["type"] != "info" {
		return "", fmt.Errorf("describe 期望 info，实际 %v", header["type"])
	}
	return "describe → info", nil
}

// probeMPD：读欢迎行（须以 OK 开头），发 ping，等 OK；
// ACK 或欢迎行不对都算 down。
func probeMPD(ctx context.Context, addr string) (string, error) {
	conn, err := dialProbe(ctx, addr)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	greeting, err := readProbeLine(r)
	if err != nil {
		return "", fmt.Errorf("读欢迎行失败: %v", err)
	}
	if !strings.HasPrefix(greeting, "OK") {
		return "", fmt.Errorf("欢迎行异常: %s", greeting)
	}
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		return "", fmt.Errorf("发 ping 失败: %v", err)
	}
	for {
		line, err := readProbeLine(r)
		if err != nil {
			return "", err
		}
		if line == "OK" {
			return "ping → OK", nil
		}
		if strings.HasPrefix(line, "ACK") {
			return "", fmt.Errorf("MPD 报错: %s", line)
		}
		// 其余行（ping 本身不回数据行，但有备无患）：继续等 OK。
	}
}

// probeSnapcast：发 Server.GetStatus JSON-RPC（id=1），
// 逐行读直到 id==1 的回包（中间的异步通知行跳过，
// 最多跳 10 条——与 music.SnapClient.call 同款上限）；
// 回包带 error 或超时都算 down。
func probeSnapcast(ctx context.Context, addr string) (string, error) {
	conn, err := dialProbe(ctx, addr)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	req := []byte("{\"id\":1,\"jsonrpc\":\"2.0\",\"method\":\"Server.GetStatus\"}\n")
	if _, err := conn.Write(req); err != nil {
		return "", fmt.Errorf("发 Server.GetStatus 失败: %v", err)
	}
	r := bufio.NewReader(conn)
	for skipped := 0; ; skipped++ {
		line, err := readProbeLine(r)
		if err != nil {
			return "", err
		}
		var resp struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			return "", fmt.Errorf("回包不是 JSON-RPC: %v", err)
		}
		if resp.ID != 1 {
			if skipped >= 10 {
				return "", fmt.Errorf("跳过 10 条通知仍未等到 id=1 的回包")
			}
			continue
		}
		if resp.Error != nil {
			return "", fmt.Errorf("snapcast 报错: %s", resp.Error.Message)
		}
		return "Server.GetStatus → ok", nil
	}
}
