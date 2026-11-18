// Package discovery 是设备指纹验证工具：手动验证一台候选设备的
// 证书指纹与型号，不做自动扫描。
//
// P1-44 安全模型（注册制，2026-11-17）：自动扫描已删除——Hub 不再主动
// 扫网段找音箱，改由音箱在已有 WS 连接上发 register 主动注册，Hub 按
// 指纹白名单校验后记为在线。本包只保留手动验证工具：
//   - ProbeFingerprint：匿名探测一台指定 IP 的证书指纹，不发 Token。
//   - ProbeModel 已删（H-13，P2-29）：生产代码无调用方，只剩测试在调，
//     且 Version 写死 "probe" 会在误接线时覆盖真实版本号进注册表；
//     如将来恢复自动发现，按真实 hello 的 Version 字段重写。
//
// 匿名探测只做原始 TLS 握手拿对端证书（自签证书会以
// CertificateVerificationError 形式带回未验证证书），算指纹返回，
// 全程不发 Token、不建 WS 连接；指纹与白名单的比对由调用方做。
// 全程 wss + 指纹校验：不用 InsecureSkipVerify。
// Token 字段为将来恢复 hello 握手时预留，当前探测流程不用它，
// 永不进日志与错误信息。
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
	"strings"
	"time"
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
// 指纹本身不是信任依据——调用方须再比对白名单。
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
