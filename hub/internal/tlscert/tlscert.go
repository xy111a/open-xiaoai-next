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
	"log"
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

// serverCertValidity 是服务端/客户端证书的有效期（P2-1）：
// 旧代码 10 年——私钥一旦泄露可冒充 Hub 长达 10 年，且指纹 pin
// 不会过期失效。改 2 年，到期前 30 天启动时自动重签（见 reuse 检查）。
// CA 本身仍 10 年（CA 轮换会废掉全部客户端证书，另行规划）。
const serverCertValidity = 2 * 365 * 24 * time.Hour

// renewBefore 是证书剩余有效期低于该值时启动即重签的阈值。
const renewBefore = 30 * 24 * time.Hour

// lanSANs 取本机局域网 IP 与主机名，供新证书的 SAN 用（P2-1：
// 旧 SAN 只有 127.0.0.1/localhost/open-xiaoai-hub，非 pin 客户端
// 校验局域网地址会失败）。取不到时只返回回环地址，不阻塞签发。
func lanSANs() ([]net.IP, []string) {
	ips := []net.IP{net.ParseIP("127.0.0.1")}
	names := []string{"localhost", "open-xiaoai-hub"}
	if hn, err := os.Hostname(); err == nil && hn != "" {
		names = append(names, hn)
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return ips, names
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.To4() == nil {
				continue
			}
			if !ip.IsPrivate() {
				continue
			}
			dup := false
			for _, have := range ips {
				if have.Equal(ip) {
					dup = true
					break
				}
			}
			if !dup {
				ips = append(ips, ip)
			}
		}
	}
	return ips, names
}

// parseLeafCert 解析 PEM 证书文件里的叶子证书（校验有效期用）。
func parseLeafCert(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, fmt.Errorf("证书 PEM 解码失败")
	}
	return x509.ParseCertificate(block.Bytes)
}

// certNeedsRenew 判断证书是否该重签：已过期、30 天内过期、
// 或尚未生效（时钟 skew 之外）都算该重签。
func certNeedsRenew(cert *x509.Certificate, now time.Time) bool {
	if now.After(cert.NotAfter.Add(-renewBefore)) {
		return true
	}
	if now.Before(cert.NotBefore) {
		return true
	}
	return false
}

// backupDamaged 把损坏/过期证书与私钥改名备份（不直接删，
// 指纹变了要能找回旧证书比对），返回备份路径。
func backupDamaged(certPath, keyPath string) string {
	stamp := time.Now().Format("20060102-150405")
	_ = os.Rename(certPath, certPath+".bak."+stamp)
	_ = os.Rename(keyPath, keyPath+".bak."+stamp)
	return certPath + ".bak." + stamp
}

// LoadOrCreate 读现有证书，不存在则生成一对新的（2 年有效期，P2-1）。
// 返回可直接给 tls.Config 用的证书。
//
// 现有证书复用前校验有效期：已过期或 30 天内过期即备份旧文件
// （.bak.<时间戳>）后重签；损坏（解析/加载失败）同样备份后重签，
// 不再让 Hub 因一张坏证书无法启动（旧代码直接返回错误 → main Fatal）。
// 指纹变了要同步更新 client.toml 的 hub_ca_fingerprint，
// 重签时记一条日志提醒操作员。
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
	if pemBytes, err := os.ReadFile(p.Cert); err == nil {
		if _, err := os.ReadFile(p.Key); err == nil {
			cert, err := tls.LoadX509KeyPair(p.Cert, p.Key)
			if err == nil {
				if leaf, err := parseLeafCert(pemBytes); err == nil {
					if !certNeedsRenew(leaf, time.Now()) {
						fp, err := Fingerprint(pemBytes)
						if err == nil {
							return cert, fp, nil
						}
					} else {
						log.Printf("tlscert: 证书将于 %s 过期（剩余有效期不足 30 天），备份后重签", leaf.NotAfter.Format("2006-01-02"))
					}
				}
			}
			// 加载失败 / 解析失败 / 该重签：备份旧文件后往下重签。
			backupDamaged(p.Cert, p.Key)
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
	ips, names := lanSANs()
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "open-xiaoai-hub", Organization: []string{"open-xiaoai-next"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(serverCertValidity),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  ips,
		DNSNames:     names,
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
//   hub.crt / hub.key      CA 签发的服务端证书（2 年有效期，SAN 含局域网地址）
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
// H2：任一 CA 文件已存在但成对解析失败 = 损坏，先备份再明确报错拒绝启动，
// 不再静默重建（重建会废掉全部客户端证书与指纹 pin，事后无从找回旧 CA 比对）。
// H1：两文件都不存在时才是首次启动，生成全新 CA 并记一条醒目 warn 日志
// （含新指纹与有效期）；部署应以 gen-certs.sh 预生成并先拷入 hub-data/tls，
// 单一 CA 来源由部署清单保证（见 release/flash-kit/CHECKLIST.md）。
func LoadOrCreateCA(dataDir string) (*CA, error) {
	p := caPaths(dataDir)
	if err := os.MkdirAll(filepath.Dir(p.Cert), 0700); err != nil {
		return nil, err
	}
	certPEM, certErr := os.ReadFile(p.Cert)
	keyPEM, keyErr := os.ReadFile(p.Key)
	if certErr == nil || keyErr == nil {
		ca, err := parseCA(certPEM, keyPEM)
		if err == nil {
			return ca, nil
		}
		bak := backupDamaged(p.Cert, p.Key)
		log.Printf("tlscert: CA 文件损坏（%v），已备份到 %s；拒绝自动重建", err, bak)
		return nil, fmt.Errorf("CA 文件损坏（%v），已备份到 %s；请恢复备份或按部署清单重建 CA（不要直接删卷重建，旧 CA 签发的客户端证书会失效）", err, bak)
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
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	// P2-4：CA 证书也 0600（旧代码 0644）。
	if err := os.WriteFile(p.Cert, certPEM, 0600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(p.Key, keyPEM, 0600); err != nil {
		return nil, err
	}
	ca, err := parseCA(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	// H1：全新 CA 生成是全断事件（旧 CA 签发的所有客户端证书失效，
	// client.toml 的 hub_ca_fingerprint 必须同步），记醒目 warn 日志。
	fp, _ := Fingerprint(certPEM)
	log.Printf("警告：tlscert: hub-data/tls 下无可用 CA，已生成全新 CA（指纹：%s，有效期至 %s）；旧 CA 签发的客户端证书已失效，需重发客户端证书并同步 client.toml 的 hub_ca_fingerprint", fp, ca.Cert.NotAfter.Format("2006-01-02"))
	return ca, nil
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
// 已存在、确由该 CA 签发且剩余有效期 >30 天时直接复用，不重签。
// 过期/将过期/损坏时备份旧文件后重签（P2-1：旧代码只认签名不认有效期，
// 一张 10 年证书过期后仍被复用，Hub 带着过期证书启动）。
func (c *CA) issueCert(certPath, keyPath string, tmpl *x509.Certificate) (tls.Certificate, string, error) {
	if certPEM, err := os.ReadFile(certPath); err == nil {
		if keyPEM, err := os.ReadFile(keyPath); err == nil {
			if cert, err := tls.X509KeyPair(certPEM, keyPEM); err == nil {
				if leaf, err := x509.ParseCertificate(cert.Certificate[0]); err == nil {
					if err := leaf.CheckSignatureFrom(c.Cert); err == nil && !certNeedsRenew(leaf, time.Now()) {
						fp, err := Fingerprint(certPEM)
						if err == nil {
							return cert, fp, nil
						}
					} else if err == nil {
						// 有效期不足：重签会换指纹，记日志提醒同步 client.toml。
						log.Printf("tlscert: %s 剩余有效期不足 30 天，备份后重签（指纹会变，需同步更新 client.toml 的 hub_ca_fingerprint）", certPath)
					}
				}
			}
			backupDamaged(certPath, keyPath)
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
	tmpl.NotAfter = time.Now().Add(serverCertValidity)
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.Cert, &key.PublicKey, c.Key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.MkdirAll(filepath.Dir(certPath), 0700); err != nil {
		return tls.Certificate{}, "", err
	}
	// P2-4：证书文件也 0600（旧代码 0644，同 NAS 多用户可读）。
	if err := os.WriteFile(certPath, certPEM, 0600); err != nil {
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
// dataDir/tls/hub.crt + hub.key。SAN 含本机局域网 IP/主机名（P2-1）。
func (c *CA) IssueServerCert(dataDir string) (tls.Certificate, string, error) {
	ips, names := lanSANs()
	tmpl := &x509.Certificate{
		Subject:     pkix.Name{CommonName: "open-xiaoai-hub", Organization: []string{"open-xiaoai-next"}},
		KeyUsage:    x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: ips,
		DNSNames:    names,
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
	// H1：启动日志输出 CA 指纹与有效期（部署清单门禁：与 client.toml、
	// flash-kit 的 hub_ca_fingerprint 三方比对）。
	caFP, _ := Fingerprint(ca.CertPEM)
	log.Printf("tlscert: CA 指纹：%s，有效期至 %s", caFP, ca.Cert.NotAfter.Format("2006-01-02"))
	return serverCert, ca.Pool(), fp, nil
}
