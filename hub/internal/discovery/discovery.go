// Package discovery 是设备指纹验证工具：手动验证一台候选设备的
// 证书指纹与型号，不做自动扫描。
//
// P1-44 安全模型（注册制，2026-11-17）：自动扫描已删除——Hub 不再主动
// 扫网段找音箱，改由音箱在已有 WS 连接上发 register 主动注册，Hub 按
// 指纹白名单校验后记为在线。本包只保留手动验证工具：
//   - ProbeFingerprint：匿名探测一台指定 IP 的证书指纹，不发 Token；
//   - ProbeModel：指纹在白名单内时建 wss 连接发 hello，确认型号。
//
// 先匿名探测、后发 Token：对端证书的 SHA-256 指纹必须在
// Scanner.ExpectedFingerprints 白名单内，才建 wss 连接发 hello + Token。
// 指纹不匹配、未配置白名单，一律不发 Token。
// 全程 wss + 指纹校验：不用 InsecureSkipVerify。探测分两步：
//
//	(1) 原始 TLS 握手拿对端证书（自签证书会以
//	CertificateVerificationError 形式带回未验证证书），算指纹比对；
//	(2) 指纹匹配后，用该证书建 RootCAs 池、取证书 SAN 作 ServerName
//	重新拨号，做完整链验证后再发 hello。
//
// Token 永不进日志与错误信息。
package discovery

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// Device 是一台手动确认的候选设备。
type Device struct {
	IP    string
	Model string // lx06 / oh2p
}

// Scanner 是手动指纹验证器（P1-44：不再是扫描器）。
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
	// ExpectedFingerprints 是允许的设备证书 SHA-256 指纹白名单
	// （小写 hex，冒号可有可无）。为空时拒绝发 Token。
	ExpectedFingerprints []string
}

// New 新建 Scanner（手动验证用，不扫描）。
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
