package tlscert

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadOrCreateReuse(t *testing.T) {
	dir := t.TempDir()
	cert1, fp1, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("首次 LoadOrCreate: %v", err)
	}
	if cert1.Leaf == nil && len(cert1.Certificate) == 0 {
		t.Fatal("证书为空")
	}
	if len(fp1) != 64 {
		t.Fatalf("指纹应为 64 位 hex，实际 %q", fp1)
	}
	// 第二次应复用同一张证书（指纹不变），而不是每次重签。
	_, fp2, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("第二次 LoadOrCreate: %v", err)
	}
	if fp1 != fp2 {
		t.Fatalf("证书应复用，指纹变了: %s → %s", fp1, fp2)
	}
	// 证书文件应真实落盘（dataDir/tls/hub.crt + hub.key）。
	if _, err := os.Stat(filepath.Join(dir, "tls", "hub.crt")); err != nil {
		t.Fatalf("tls/hub.crt 未落盘: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tls", "hub.key")); err != nil {
		t.Fatalf("tls/hub.key 未落盘: %v", err)
	}
}

func TestFingerprintBadInput(t *testing.T) {
	if _, err := Fingerprint([]byte("not a pem")); err == nil {
		t.Fatal("非法 PEM 应返回错误")
	}
}

func TestLoadOrCreateMTLS(t *testing.T) {
	dir := t.TempDir()
	cert, pool, fp1, err := LoadOrCreateMTLS(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateMTLS: %v", err)
	}
	if len(cert.Certificate) == 0 {
		t.Fatal("服务端证书为空")
	}
	if len(fp1) != 64 {
		t.Fatalf("服务端证书指纹应为 64 位 hex，实际 %q", fp1)
	}
	// CA 与证书文件应落盘；私钥 0600。
	for _, f := range []string{"tls/ca.crt", "tls/ca.key", "tls/hub.crt", "tls/hub.key",
		"tls/client-open-xiaoai-client.crt", "tls/client-open-xiaoai-client.key"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("%s 未落盘: %v", f, err)
		}
	}
	if st, _ := os.Stat(filepath.Join(dir, "tls/ca.key")); st.Mode().Perm() != 0600 {
		t.Fatalf("ca.key 权限应为 0600，实际 %o", st.Mode().Perm())
	}
	if st, _ := os.Stat(filepath.Join(dir, "tls/client-open-xiaoai-client.key")); st.Mode().Perm() != 0600 {
		t.Fatalf("client key 权限应为 0600，实际 %o", st.Mode().Perm())
	}
	// 服务端证书确由该 CA 签发（x509 链校验，ServerAuth）。
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("解析服务端证书: %v", err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "localhost",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatalf("服务端证书应用 CA 链校验通过: %v", err)
	}
	// 客户端证书确由该 CA 签发（ClientAuth）。
	clientCertPEM, err := os.ReadFile(filepath.Join(dir, "tls/client-open-xiaoai-client.crt"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(clientCertPEM)
	if block == nil {
		t.Fatal("客户端证书 PEM 解码失败")
	}
	clientLeaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("解析客户端证书: %v", err)
	}
	if _, err := clientLeaf.Verify(x509.VerifyOptions{Roots: pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("客户端证书应用 CA 链校验通过: %v", err)
	}
	// 第二次调用应复用同一套材料（指纹不变，不重签）。
	_, _, fp2, err := LoadOrCreateMTLS(dir)
	if err != nil {
		t.Fatalf("第二次 LoadOrCreateMTLS: %v", err)
	}
	if fp1 != fp2 {
		t.Fatalf("证书应复用，指纹变了: %s → %s", fp1, fp2)
	}
}

func TestIssueClientCertRejectsBadCN(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	for _, bad := range []string{"../evil", "a/b", "a b", "", "lx06;rm"} {
		if _, _, err := ca.IssueClientCert(dir, bad); err == nil {
			t.Fatalf("非法 CN %q 应被拒绝", bad)
		}
	}
	if _, _, err := ca.IssueClientCert(dir, "lx06-01"); err != nil {
		t.Fatalf("合法 CN 应通过: %v", err)
	}
}

func TestLoadOrCreateCAReuseAndMismatch(t *testing.T) {
	dir := t.TempDir()
	ca1, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("首次 LoadOrCreateCA: %v", err)
	}
	ca2, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("第二次 LoadOrCreateCA: %v", err)
	}
	if ca1.Cert.SerialNumber.Cmp(ca2.Cert.SerialNumber) != 0 {
		t.Fatal("CA 应复用，序列号变了")
	}
	// 证书与私钥不匹配时应报错（换一把私钥进去）。
	other, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseCA(ca1.CertPEM, other.KeyPEM); err == nil {
		t.Fatal("CA 私钥与证书不匹配时应报错")
	}
}

// H2：CA 文件损坏时先备份再明确报错拒绝启动，不再静默重建。
func TestLoadOrCreateCADamagedRefusesStartup(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadOrCreateCA(dir); err != nil {
		t.Fatalf("首次 LoadOrCreateCA: %v", err)
	}
	// 写坏 ca.crt：解析失败时应备份并拒绝启动，而不是静默重建。
	caCert := filepath.Join(dir, "tls", "ca.crt")
	if err := os.WriteFile(caCert, []byte("not a pem"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateCA(dir); err == nil {
		t.Fatal("CA 损坏时应拒绝启动")
	} else if !strings.Contains(err.Error(), "CA 文件损坏") {
		t.Fatalf("错误应说明 CA 损坏，实际：%v", err)
	}
	// 损坏的 CA 应备份到 .bak.<时间戳>，原路径文件已被移走。
	matches, _ := filepath.Glob(caCert + ".bak.*")
	if len(matches) == 0 {
		t.Fatal("损坏的 CA 应备份到 .bak.<时间戳>")
	}
	if _, err := os.Stat(caCert); !os.IsNotExist(err) {
		t.Fatal("损坏的 CA 文件应已被移走备份")
	}
}

func TestLoadOrCreateCAReadsOpenSSLCA(t *testing.T) {
	// gen-certs.sh（openssl genpkey）产 PKCS#8 私钥；Go 的 parseCA 必须能读，
	// 且用该 CA 签发的服务端证书应被复用（不重签）。
	dir := t.TempDir()
	tlsDir := filepath.Join(dir, "tls")
	if err := os.MkdirAll(tlsDir, 0700); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := exec.Command("openssl", args...)
		cmd.Dir = tlsDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("openssl %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(tlsDir, "hub.ext"),
		[]byte("subjectAltName=DNS:localhost,DNS:open-xiaoai-hub,IP:127.0.0.1\nextendedKeyUsage=serverAuth\nkeyUsage=digitalSignature,keyEncipherment\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("genpkey", "-algorithm", "RSA", "-pkeyopt", "rsa_keygen_bits:2048", "-out", "ca.key")
	run("req", "-new", "-x509", "-key", "ca.key", "-out", "ca.crt", "-days", "365",
		"-subj", "/CN=open-xiaoai-ca/O=open-xiaoai-next",
		"-addext", "basicConstraints=critical,CA:TRUE",
		"-addext", "keyUsage=critical,keyCertSign,cRLSign,digitalSignature")
	run("genpkey", "-algorithm", "RSA", "-pkeyopt", "rsa_keygen_bits:2048", "-out", "hub.key")
	run("req", "-new", "-key", "hub.key", "-out", "hub.csr",
		"-subj", "/CN=open-xiaoai-hub/O=open-xiaoai-next",
		"-addext", "subjectAltName=DNS:localhost,DNS:open-xiaoai-hub,IP:127.0.0.1")
	run("x509", "-req", "-in", "hub.csr", "-CA", "ca.crt", "-CAkey", "ca.key",
		"-CAcreateserial", "-out", "hub.crt", "-days", "365",
		"-extfile", "hub.ext")

	ca, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateCA 读 openssl CA 失败: %v", err)
	}
	if ca.Cert.Subject.CommonName != "open-xiaoai-ca" {
		t.Fatalf("CA CN 不对: %q", ca.Cert.Subject.CommonName)
	}
	// openssl 签发的服务端证书应被 IssueServerCert 复用（指纹与 openssl 的一致）。
	before, err := os.ReadFile(filepath.Join(tlsDir, "hub.crt"))
	if err != nil {
		t.Fatal(err)
	}
	fpBefore, err := Fingerprint(before)
	if err != nil {
		t.Fatal(err)
	}
	_, fpAfter, err := ca.IssueServerCert(dir)
	if err != nil {
		t.Fatalf("IssueServerCert: %v", err)
	}
	if fpBefore != fpAfter {
		t.Fatalf("openssl 签发的 hub.crt 应被复用，指纹变了: %s → %s", fpBefore, fpAfter)
	}
}
