// Package discovery 是设备自动发现：Hub 启动时扫描局域网，
// 找到音箱的 IP 和型号，写入 Device Registry。不用手填 IP。
//
// P0-4 安全模型（重做后）：
//   - 扫描默认关闭：Scanner.Enabled 为 false 时 Scan 直接拒绝，
//     需调用方显式开启并传入限定的 scan_cidr。
//   - 先匿名探测、后发 Token：ProbeFingerprint 只做 TLS 握手，
//     不发任何应用层数据、更不发 Token；对端证书的 SHA-256 指纹
//     必须在 Scanner.ExpectedFingerprints 白名单内，才建 wss 连接
//     发 hello + Token。指纹不匹配、未配置白名单、一律不发 Token。
//   - 全程 wss + 指纹校验：不用 InsecureSkipVerify。探测分两步：
//     (1) 原始 TLS 握手拿对端证书（自签证书会以
//     CertificateVerificationError 形式带回未验证证书），算指纹比对；
//     (2) 指纹匹配后，用该证书建 RootCAs 池、取证书 SAN 作 ServerName
//     重新拨号，做完整链验证后再发 hello。
//   - Token 永不进日志与错误信息。
package discovery

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Device 是扫描到的一台候选设备。
type Device struct {
	IP    string
	Model string // lx06 / oh2p
}

// Scanner 是扫描器。
type Scanner struct {
	// Port 是 Client 接入端口，默认 4399；调用方应取 cfg.Hub.Listen 的端口，
	// 不要写死（见 P0-4：端口取 cfg.Hub.Listen）。
	Port int
	// Token 是 hello 握手用的 Token。只在指纹匹配后才发出，永不记日志。
	Token string
	// Timeout 是单个 IP 的探测/握手超时。
	Timeout time.Duration
	// TLS 为 true 时用 wss 握手。必须为 true；false 时拒绝探测
	// （明文扫描不发送 Token，也不做身份校验）。
	TLS bool
	// Enabled 是扫描总开关，默认 false：Scan 未显式开启即拒绝。
	Enabled bool
	// ExpectedFingerprints 是允许的设备证书 SHA-256 指纹白名单
	// （小写 hex，冒号可有可无）。为空时拒绝发 Token。
	ExpectedFingerprints []string
}

// New 新建 Scanner。注意：扫描默认关闭（Enabled=false），
// 调用方需显式置 Enabled=true 并配好 ExpectedFingerprints 才能扫描。
func New(port int, token string) *Scanner {
	return &Scanner{Port: port, Token: token, Timeout: 2 * time.Second}
}

// normalizeFingerprint 归一化指纹：去冒号/空格/横线，转小写。
func normalizeFingerprint(fp string) string {
	fp = strings.ReplaceAll(fp, ":", "")
	fp = strings.ReplaceAll(fp, " ", "")
	fp = strings.ReplaceAll(fp, "-", "")
	return strings.ToLower(fp)
}

// fingerprintOf 算一本证书的 SHA-256 指纹（小写 hex）。
func fingerprintOf(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// fingerprintAllowed 判断指纹是否在白名单内。
func (s *Scanner) fingerprintAllowed(fp string) bool {
	fp = normalizeFingerprint(fp)
	if fp == "" {
		return false
	}
	for _, want := range s.ExpectedFingerprints {
		if fp == normalizeFingerprint(want) {
			return true
		}
	}
	return false
}

// probeLeafCert 做匿名探测：只建 TCP + TLS 握手，不发应用层数据、不发 Token。
// 返回对端叶子证书。握手失败时，若错误是证书验证失败，
// 从 CertificateVerificationError 里取未验证证书（自签证书即走此路）。
func (s *Scanner) probeLeafCert(ctx context.Context, ip string) (*x509.Certificate, error) {
	dialer := &net.Dialer{Timeout: s.Timeout}
	raw, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip, fmt.Sprint(s.Port)))
	if err != nil {
		return nil, fmt.Errorf("discovery: 连接 %s 失败: %w", ip, err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(s.Timeout))
	tc := tls.Client(raw, &tls.Config{ServerName: ip, MinVersion: tls.VersionTLS12})
	err = tc.HandshakeContext(ctx)
	if err == nil {
		certs := tc.ConnectionState().PeerCertificates
		if len(certs) == 0 {
			return nil, fmt.Errorf("discovery: %s 未出示证书", ip)
		}
		return certs[0], nil
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) && len(certErr.UnverifiedCertificates) > 0 {
		return certErr.UnverifiedCertificates[0], nil
	}
	return nil, fmt.Errorf("discovery: %s TLS 握手失败: %w", ip, err)
}

// ProbeFingerprint 匿名探测一台候选设备，只返回其证书指纹，不发 Token。
// 指纹本身不是信任依据——调用方（ProbeModel）须再比对白名单。
func (s *Scanner) ProbeFingerprint(ctx context.Context, ip string) (string, error) {
	if !s.TLS {
		return "", fmt.Errorf("discovery: 拒绝明文探测（须走 wss + 指纹校验）")
	}
	cert, err := s.probeLeafCert(ctx, ip)
	if err != nil {
		return "", err
	}
	if now := time.Now(); now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
		return "", fmt.Errorf("discovery: %s 的证书不在有效期内", ip)
	}
	return fingerprintOf(cert), nil
}

// serverNameForCert 取证书 SAN 中的一个名字作 TLS ServerName，
// 使第二步的完整链验证能通过（只认指纹，不认名字本身）。
func serverNameForCert(cert *x509.Certificate) (string, error) {
	if len(cert.DNSNames) > 0 {
		return cert.DNSNames[0], nil
	}
	if len(cert.IPAddresses) > 0 {
		return cert.IPAddresses[0].String(), nil
	}
	return "", fmt.Errorf("discovery: 证书无 SAN，无法做完整链验证")
}

// ProbeModel 用指定型号做 hello 握手，成功返回 hello_ok 里的 device_id。
// 流程：匿名探测取证书指纹 → 指纹不在白名单即拒绝（不发 Token、不建 WS）；
// 指纹匹配后，用该证书建信任池重新拨 wss，做完整链验证，再发 hello + Token。
func (s *Scanner) ProbeModel(ctx context.Context, ip, model string) (string, error) {
	if !s.TLS {
		return "", fmt.Errorf("discovery: 拒绝明文握手（须走 wss + 指纹校验）")
	}
	if len(s.ExpectedFingerprints) == 0 {
		return "", fmt.Errorf("discovery: 未配置设备证书指纹白名单，拒绝发 Token")
	}
	cert, err := s.probeLeafCert(ctx, ip)
	if err != nil {
		return "", err
	}
	if now := time.Now(); now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
		return "", fmt.Errorf("discovery: %s 的证书不在有效期内", ip)
	}
	fp := fingerprintOf(cert)
	if !s.fingerprintAllowed(fp) {
		return "", fmt.Errorf("discovery: %s 的证书指纹不在白名单内，拒绝发 Token（指纹 %s）", ip, fp)
	}
	// 指纹已验证：用该证书建信任池，做完整链验证后再发 hello。
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	serverName, err := serverNameForCert(cert)
	if err != nil {
		return "", err
	}
	u := url.URL{Scheme: "wss", Host: net.JoinHostPort(ip, fmt.Sprint(s.Port)), Path: "/ws"}
	dialer := websocket.Dialer{
		HandshakeTimeout: s.Timeout,
		TLSClientConfig:  &tls.Config{RootCAs: pool, ServerName: serverName, MinVersion: tls.VersionTLS12},
	}
	conn, _, err := dialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		return "", fmt.Errorf("discovery: %s wss 拨号失败: %w", ip, err)
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(s.Timeout))
	if err := conn.WriteJSON(map[string]any{
		"type": "hello", "model": model, "ram_mb": 256,
		"version": "probe", "token": s.Token,
	}); err != nil {
		return "", err
	}
	_ = conn.SetReadDeadline(time.Now().Add(s.Timeout))
	var resp struct {
		Type     string `json:"type"`
		DeviceID string `json:"device_id"`
		Reason   string `json:"reason"`
	}
	if err := conn.ReadJSON(&resp); err != nil {
		return "", err
	}
	if resp.Type != "hello_ok" {
		return "", fmt.Errorf("rejected: %s", resp.Reason)
	}
	return resp.DeviceID, nil
}

// arpNeighbors 读 /proc/net/arp 的邻居 IP。
func arpNeighbors() []string {
	f, err := os.Open("/proc/net/arp")
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Scan() // 表头
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 1 {
			if ip := net.ParseIP(fields[0]); ip != nil {
				out = append(out, fields[0])
			}
		}
	}
	return out
}

// hostsInCIDR 展开一个网段内的全部主机 IP（/24 约 254 个；更大网段只取前 1024 个）。
func hostsInCIDR(cidr string) []string {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil
	}
	var out []string
	ip := ipnet.IP.Mask(ipnet.Mask)
	for ip := ip.Mask(ipnet.Mask); ipnet.Contains(ip); inc(ip) {
		out = append(out, ip.String())
		if len(out) >= 1024 {
			break
		}
	}
	return out
}

func inc(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}

// Scan 扫一遍：ARP 邻居 + 网段展开，去重后并发握手，返回确认的设备。
// 默认关闭：Enabled=false 或 cidr 为空/非内网网段时直接拒绝。
// ctx 超时即停止。每个 IP 先试 lx06 再试 oh2p。
func (s *Scanner) Scan(ctx context.Context, cidr string) ([]Device, error) {
	if !s.Enabled {
		return nil, fmt.Errorf("discovery: 扫描默认关闭，需显式开启（Scanner.Enabled=true）")
	}
	if cidr == "" {
		return nil, fmt.Errorf("discovery: 扫描需显式限定 scan_cidr")
	}
	if _, ipnet, err := net.ParseCIDR(cidr); err != nil {
		return nil, fmt.Errorf("discovery: scan_cidr 不是合法 CIDR：%q", cidr)
	} else if ip := ipnet.IP; !(ip.IsPrivate() || ip.IsLoopback()) {
		return nil, fmt.Errorf("discovery: scan_cidr 必须是内网网段，实际是 %q", cidr)
	}
	candidates := map[string]bool{}
	for _, ip := range arpNeighbors() {
		candidates[ip] = true
	}
	for _, ip := range hostsInCIDR(cidr) {
		candidates[ip] = true
	}
	// 自己的 IP 不扫。
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok {
				delete(candidates, ipnet.IP.String())
			}
		}
	}
	var (
		mu    sync.Mutex
		found []Device
		sem   = make(chan struct{}, 32)
		wg    sync.WaitGroup
	)
	for ip := range candidates {
		ip := ip
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			for _, model := range []string{"lx06", "oh2p"} {
				if _, err := s.ProbeModel(ctx, ip, model); err == nil {
					mu.Lock()
					found = append(found, Device{IP: ip, Model: model})
					mu.Unlock()
					return
				}
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	return found, nil
}
