// Package tlscert 生成并管理 Hub 的自签 CA 与服务端证书。
// Client 用 config 里的 hub_ca_fingerprint（证书 SHA-256 指纹）校验 Hub，
// 避免局域网内中间人。证书存在 Hub 数据目录，下次启动复用，不重复生成。
package tlscert

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Paths 是证书文件位置。
type Paths struct {
	Cert string
	Key  string
}

// Fingerprint 算一本证书的 SHA-256 指纹（小写 hex，Client 用来 pin Hub）。
func Fingerprint(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", fmt.Errorf("证书 PEM 解码失败")
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:]), nil
}

// LoadOrCreate 读现有证书，不存在则生成一对新的（CA 自签，10 年有效期）。
// 返回可直接给 tls.Config 用的证书。
//
// 注意：这是旧的自签服务端证书路径（无 CA、无客户端证书）。
// mTLS 部署请用 LoadOrCreateMTLS()：CA 签发的服务端证书 + 客户端 CA 池。
func LoadOrCreate(dataDir string) (tls.Certificate, string, error) {
	p := Paths{
		Cert: filepath.Join(dataDir, "tls", "hub.crt"),
		Key:  filepath.Join(dataDir, "tls", "hub.key"),
	}
	if err := os.MkdirAll(filepath.Dir(p.Cert), 0700); err != nil {
		return tls.Certificate{}, "", err
	}
	if _, err := os.Stat(p.Cert); err == nil {
		if _, err := os.Stat(p.Key); err == nil {
			cert, err := tls.LoadX509KeyPair(p.Cert, p.Key)
			if err != nil {
				return tls.Certificate{}, "", err
			}
			pemBytes, err := os.ReadFile(p.Cert)
			if err != nil {
				return tls.Certificate{}, "", err
			}
			fp, err := Fingerprint(pemBytes)
			if err != nil {
				return tls.Certificate{}, "", err
			}
			return cert, fp, nil
		}
	}
	// 生成新证书。
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, "", err
	}
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "open-xiaoai-hub", Organization: []string{"open-xiaoai-next"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost", "open-xiaoai-hub"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(p.Cert, certPEM, 0600); err != nil {
		return tls.Certificate{}, "", err
	}
	if err := os.WriteFile(p.Key, keyPEM, 0600); err != nil {
		return tls.Certificate{}, "", err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	fp, err := Fingerprint(certPEM)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	return cert, fp, nil
}

// ---------------------------------------------------------------------------
// mTLS（P0-3）：自有 CA + CA 签发的服务端/客户端证书。
//
// 文件布局（dataDir/tls/ 下）：
//   ca.crt / ca.key        自签 CA（10 年；私钥 0600，永不离开 NAS）
//   hub.crt / hub.key      CA 签发的服务端证书（SAN 与旧 LoadOrCreate 一致）
//   client-<cn>.crt/.key   CA 签发的客户端证书（随固件下发给音箱，见
//                          firmware-patch/scripts/gen-certs.sh 与
//                          patch_firmware.py --cert-dir）
// 旧 LoadOrCreate() 的自签 hub.crt 若已存在且不是 CA 签发，
// LoadOrCreateMTLS() 会重新签发覆盖（指纹随之变，client.toml 的
// hub_ca_fingerprint 需同步更新）。
// ---------------------------------------------------------------------------

// CA 是自有证书颁发机构。
type CA struct {
	CertPEM []byte
	KeyPEM  []byte
	Cert    *x509.Certificate
	Key     *rsa.PrivateKey
}

// CAPaths 是 CA 文件位置。
type CAPaths struct {
	Cert string
	Key  string
}

// caPaths 返回 CA 文件位置。
func caPaths(dataDir string) CAPaths {
	return CAPaths{
		Cert: filepath.Join(dataDir, "tls", "ca.crt"),
		Key:  filepath.Join(dataDir, "tls", "ca.key"),
	}
}

// LoadOrCreateCA 读现有 CA，不存在则生成一个新的自签 CA（10 年有效期）。
func LoadOrCreateCA(dataDir string) (*CA, error) {
	p := caPaths(dataDir)
	if err := os.MkdirAll(filepath.Dir(p.Cert), 0700); err != nil {
		return nil, err
	}
	if certPEM, err := os.ReadFile(p.Cert); err == nil {
		if keyPEM, err := os.ReadFile(p.Key); err == nil {
			ca, err := parseCA(certPEM, keyPEM)
			if err == nil {
				return ca, nil
			}
			// 现有 CA 文件损坏：继续往下重新生成。
		}
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "open-xiaoai-ca", Organization: []string{"open-xiaoai-next"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(p.Cert, certPEM, 0644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(p.Key, keyPEM, 0600); err != nil {
		return nil, err
	}
	return parseCA(certPEM, keyPEM)
}

// parseCA 解析 CA 证书 + 私钥，并校验两者匹配、证书确为 CA。
func parseCA(certPEM, keyPEM []byte) (*CA, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, fmt.Errorf("CA 证书 PEM 解码失败")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	if !cert.IsCA {
		return nil, fmt.Errorf("CA 证书不是 CA（BasicConstraints 缺失）")
	}
	kblock, _ := pem.Decode(keyPEM)
	if kblock == nil {
		return nil, fmt.Errorf("CA 私钥 PEM 解码失败")
	}
	// 私钥可能是 PKCS#1（Go 的 tlscert 生成）或 PKCS#8
	//（openssl genpkey / gen-certs.sh 生成），两种都接受。
	key, err := x509.ParsePKCS1PrivateKey(kblock.Bytes)
	if err != nil {
		k8, err8 := x509.ParsePKCS8PrivateKey(kblock.Bytes)
		if err8 != nil {
			return nil, err
		}
		var ok bool
		key, ok = k8.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("CA 私钥不是 RSA 私钥")
		}
	}
	// 公钥必须与证书里的公钥一致（否则是用错了的私钥）。
	pub, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok || pub.N.Cmp(key.PublicKey.N) != 0 || pub.E != key.PublicKey.E {
		return nil, fmt.Errorf("CA 私钥与证书不匹配")
	}
	return &CA{CertPEM: certPEM, KeyPEM: keyPEM, Cert: cert, Key: key}, nil
}

// Pool 返回只含该 CA 的证书池（给服务端 ClientCAs 用）。
func (c *CA) Pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(c.Cert)
	return pool
}

// issueCert 用 CA 签发一张证书（服务端或客户端），写到指定路径；
// 已存在且确由该 CA 签发时直接复用，不重签。
func (c *CA) issueCert(certPath, keyPath string, tmpl *x509.Certificate) (tls.Certificate, string, error) {
	if certPEM, err := os.ReadFile(certPath); err == nil {
		if keyPEM, err := os.ReadFile(keyPath); err == nil {
			if cert, err := tls.X509KeyPair(certPEM, keyPEM); err == nil {
				if leaf, err := x509.ParseCertificate(cert.Certificate[0]); err == nil {
					if err := leaf.CheckSignatureFrom(c.Cert); err == nil {
						fp, err := Fingerprint(certPEM)
						if err == nil {
							return cert, fp, nil
						}
					}
				}
			}
		}
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, "", err
	}
	tmpl.SerialNumber = serial
	tmpl.NotBefore = time.Now().Add(-time.Hour)
	tmpl.NotAfter = time.Now().Add(10 * 365 * 24 * time.Hour)
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.Cert, &key.PublicKey, c.Key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.MkdirAll(filepath.Dir(certPath), 0700); err != nil {
		return tls.Certificate{}, "", err
	}
	if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
		return tls.Certificate{}, "", err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return tls.Certificate{}, "", err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	fp, err := Fingerprint(certPEM)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	return cert, fp, nil
}

// IssueServerCert 签发（或复用）CA 签名的 Hub 服务端证书，写到
// dataDir/tls/hub.crt + hub.key。SAN 与旧 LoadOrCreate() 一致。
func (c *CA) IssueServerCert(dataDir string) (tls.Certificate, string, error) {
	tmpl := &x509.Certificate{
		Subject:     pkix.Name{CommonName: "open-xiaoai-hub", Organization: []string{"open-xiaoai-next"}},
		KeyUsage:    x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:    []string{"localhost", "open-xiaoai-hub"},
	}
	return c.issueCert(filepath.Join(dataDir, "tls", "hub.crt"),
		filepath.Join(dataDir, "tls", "hub.key"), tmpl)
}

// IssueClientCert 签发（或复用）一张客户端证书，写到
// dataDir/tls/client-<commonName>.crt + .key。commonName 只允许
// 字母数字、下划线与连字符（防路径穿越）。
func (c *CA) IssueClientCert(dataDir, commonName string) (string, string, error) {
	if commonName == "" {
		return "", "", fmt.Errorf("客户端证书 CN 为空")
	}
	for _, r := range commonName {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return "", "", fmt.Errorf("客户端证书 CN 不合法：%q（只允许字母数字、下划线、连字符）", commonName)
		}
	}
	tmpl := &x509.Certificate{
		Subject:     pkix.Name{CommonName: commonName, Organization: []string{"open-xiaoai-next"}},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	certPath := filepath.Join(dataDir, "tls", "client-"+commonName+".crt")
	keyPath := filepath.Join(dataDir, "tls", "client-"+commonName+".key")
	if _, _, err := c.issueCert(certPath, keyPath, tmpl); err != nil {
		return "", "", err
	}
	return certPath, keyPath, nil
}

// LoadOrCreateMTLS 一站式建 mTLS 材料：CA（没有则新建）+ CA 签发的
// 服务端证书 + 共享客户端证书（CN open-xiaoai-client，供两台音箱用；
// 要每台独立证书时再调 IssueClientCert）。
// 返回服务端证书、客户端 CA 池、服务端证书指纹。
// main.go 接线（协调人做，见 AUDIT P0-3）：
//
//	cert, pool, fp, err := tlscert.LoadOrCreateMTLS(cfg.Hub.DataDir)
//	server.SetClientCAs(pool)
//	server.Serve(ln, &cert)
func LoadOrCreateMTLS(dataDir string) (tls.Certificate, *x509.CertPool, string, error) {
	ca, err := LoadOrCreateCA(dataDir)
	if err != nil {
		return tls.Certificate{}, nil, "", err
	}
	serverCert, fp, err := ca.IssueServerCert(dataDir)
	if err != nil {
		return tls.Certificate{}, nil, "", err
	}
	if _, _, err := ca.IssueClientCert(dataDir, "open-xiaoai-client"); err != nil {
		return tls.Certificate{}, nil, "", err
	}
	return serverCert, ca.Pool(), fp, nil
}
