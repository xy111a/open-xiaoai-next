package tlscert

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// P2-1：新证书有效期 2 年（不是 10 年），SAN 含回环地址。
func TestNewCertValidityTwoYearsP2(t *testing.T) {
	dir := t.TempDir()
	_, _, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes, err := os.ReadFile(filepath.Join(dir, "tls", "hub.crt"))
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := parseLeafCert(pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	validity := leaf.NotAfter.Sub(leaf.NotBefore)
	want := 2 * 365 * 24 * time.Hour
	if validity < want-time.Hour || validity > want+time.Hour {
		t.Fatalf("新证书有效期应为 2 年，实际 %v", validity)
	}
	foundLoopback := false
	for _, ip := range leaf.IPAddresses {
		if ip.Equal(net.ParseIP("127.0.0.1")) {
			foundLoopback = true
		}
	}
	if !foundLoopback {
		t.Fatalf("SAN 应含 127.0.0.1，实际 %v", leaf.IPAddresses)
	}
	// 证书与私钥文件 0600（P2-4）。
	for _, f := range []string{"tls/hub.crt", "tls/hub.key"} {
		fi, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0600 {
			t.Fatalf("%s 权限应为 0600，实际 %o", f, fi.Mode().Perm())
		}
	}
}

// P2-1：损坏的证书备份后重签，Hub 不再因坏证书无法启动。
func TestCorruptCertBackedUpAndRegeneratedP2(t *testing.T) {
	dir := t.TempDir()
	_, fp1, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(dir, "tls", "hub.crt")
	if err := os.WriteFile(certPath, []byte("not a cert"), 0600); err != nil {
		t.Fatal(err)
	}
	_, fp2, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("损坏证书应备份后重签，而不是报错: %v", err)
	}
	if fp1 == fp2 {
		t.Fatal("重签后指纹应变化")
	}
	matches, _ := filepath.Glob(certPath + ".bak.*")
	if len(matches) != 1 {
		t.Fatalf("应留 1 个 .bak.* 备份，实际 %v", matches)
	}
}

// P2-1：已过期证书不复用，备份后重签。
func TestExpiredCertRegeneratedP2(t *testing.T) {
	dir := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "open-xiaoai-hub"},
		NotBefore:    time.Now().Add(-48 * time.Hour),
		NotAfter:     time.Now().Add(-time.Hour), // 已过期
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	tlsDir := filepath.Join(dir, "tls")
	if err := os.MkdirAll(tlsDir, 0700); err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(filepath.Join(tlsDir, "hub.crt"), certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tlsDir, "hub.key"), keyPEM, 0600); err != nil {
		t.Fatal(err)
	}
	_, _, err = LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("过期证书应备份后重签，而不是报错: %v", err)
	}
	pemBytes, err := os.ReadFile(filepath.Join(tlsDir, "hub.crt"))
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := parseLeafCert(pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	if time.Now().After(leaf.NotAfter) {
		t.Fatal("重签后的证书不应已过期")
	}
}
