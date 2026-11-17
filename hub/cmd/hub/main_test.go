// Package main 的接线测试：只验 main.go 的启动接线本身——
// discovery 端口取自配置、扫描默认关闭、Web 用带超时的 Server。
// 刻意不断言任何具体端口值：端口只验“输入什么、解析出什么”的往返，
// 默认值只验“关着/绑着回环”这类安全属性，不把 4399/8080 写进断言。
package main

import (
	"net"
	"net/http"
	"testing"

	"github.com/huajun/open-xiaoai-next/hub/internal/config"
	"github.com/huajun/open-xiaoai-next/hub/internal/ota"
	"github.com/huajun/open-xiaoai-next/hub/internal/registry"
	"github.com/huajun/open-xiaoai-next/hub/internal/web"
)

func boolPtr(b bool) *bool { return &b }

// TestDiscoveryPort：端口从 Listen 地址解析（往返值，不硬编码生产端口）。
func TestDiscoveryPort(t *testing.T) {
	cases := []struct {
		listen string
		want   int
	}{
		{"127.0.0.1:14567", 14567},
		{"[::1]:14601", 14601},
		{"localhost:10499", 10499},
	}
	for _, c := range cases {
		got, err := discoveryPort(c.listen)
		if err != nil {
			t.Fatalf("discoveryPort(%q) 报错: %v", c.listen, err)
		}
		if got != c.want {
			t.Fatalf("discoveryPort(%q) = %d， want %d", c.listen, got, c.want)
		}
	}
	for _, bad := range []string{"", "not-an-addr", "127.0.0.1:0", "127.0.0.1:99999", "127.0.0.1:notaport"} {
		if _, err := discoveryPort(bad); err == nil {
			t.Fatalf("discoveryPort(%q) 应报错，实际通过", bad)
		}
	}
}

// TestNewDiscoveryScannerDefaultOff：默认配置下扫描关闭（P0-4）。
func TestNewDiscoveryScannerDefaultOff(t *testing.T) {
	cfg := &config.Config{}
	cfg.Hub.Listen = "127.0.0.1:14567"
	cfg.Hub.TLS = boolPtr(true)
	if s := newDiscoveryScanner(cfg, "test-token"); s != nil {
		t.Fatalf("默认配置下 newDiscoveryScanner 应返回 nil（扫描默认关闭），实际返回 %+v", s)
	}
	// 显式开启但未配指纹白名单：fail closed，同样不扫描。
	cfg.Hub.ScanEnabled = true
	if s := newDiscoveryScanner(cfg, "test-token"); s != nil {
		t.Fatalf("未配指纹白名单时 newDiscoveryScanner 应返回 nil，实际返回 %+v", s)
	}
	// TLS 未开：拒绝明文扫描。
	cfg.Hub.DiscoveryFingerprints = []string{"aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"}
	cfg.Hub.TLS = boolPtr(false)
	if s := newDiscoveryScanner(cfg, "test-token"); s != nil {
		t.Fatalf("TLS 未开时 newDiscoveryScanner 应返回 nil，实际返回 %+v", s)
	}
}

// TestNewDiscoveryScannerEnabled：显式开启 + 指纹白名单后，扫描器拿到
// 配置里的端口（往返值）与白名单，且 TLS 走 wss。
func TestNewDiscoveryScannerEnabled(t *testing.T) {
	cfg := &config.Config{}
	cfg.Hub.Listen = "127.0.0.1:14567"
	cfg.Hub.TLS = boolPtr(true)
	cfg.Hub.ScanEnabled = true
	fps := []string{"aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"}
	cfg.Hub.DiscoveryFingerprints = fps
	s := newDiscoveryScanner(cfg, "test-token")
	if s == nil {
		t.Fatalf("显式开启 + 配好白名单后 newDiscoveryScanner 不应返回 nil")
	}
	if !s.Enabled {
		t.Fatalf("扫描器 Enabled 应为 true")
	}
	if !s.TLS {
		t.Fatalf("扫描器 TLS 应为 true（只走 wss + 指纹校验）")
	}
	if s.Port != 14567 {
		t.Fatalf("扫描器 Port = %d，应与 Listen 地址里的端口一致", s.Port)
	}
	if len(s.ExpectedFingerprints) != len(fps) || s.ExpectedFingerprints[0] != fps[0] {
		t.Fatalf("扫描器 ExpectedFingerprints = %v，应原样传入配置值", s.ExpectedFingerprints)
	}
}

// TestNewHTTPServerTimeouts：Web 用带超时的 Server（P0-1），
// 超时须全部大于 0（不断言具体毫秒数）；地址用配置值（往返），
// 未配时有默认地址（不断言默认值）。
func TestNewHTTPServerTimeouts(t *testing.T) {
	cfg := &config.Config{}
	cfg.Hub.WebListen = "127.0.0.1:18080"
	srv := web.New("config.yaml", cfg, registry.New(), ota.New()).NewHTTPServer()
	if srv == nil {
		t.Fatalf("NewHTTPServer 不应返回 nil")
	}
	if srv.Addr != "127.0.0.1:18080" {
		t.Fatalf("NewHTTPServer Addr = %q，应与配置的 WebListen 一致", srv.Addr)
	}
	checkTimeouts(t, srv)

	cfg2 := &config.Config{}
	srv2 := web.New("config.yaml", cfg2, registry.New(), ota.New()).NewHTTPServer()
	if srv2.Addr == "" {
		t.Fatalf("WebListen 未配时 NewHTTPServer 应有默认地址")
	}
	checkTimeouts(t, srv2)
}

// checkTimeouts 断言 Server 的四个超时都大于 0（不断言具体毫秒数）。
func checkTimeouts(t *testing.T, srv *http.Server) {
	t.Helper()
	if srv.ReadHeaderTimeout <= 0 {
		t.Fatalf("NewHTTPServer ReadHeaderTimeout 应 > 0，实际 %v", srv.ReadHeaderTimeout)
	}
	if srv.ReadTimeout <= 0 {
		t.Fatalf("NewHTTPServer ReadTimeout 应 > 0，实际 %v", srv.ReadTimeout)
	}
	if srv.WriteTimeout <= 0 {
		t.Fatalf("NewHTTPServer WriteTimeout 应 > 0，实际 %v", srv.WriteTimeout)
	}
	if srv.IdleTimeout <= 0 {
		t.Fatalf("NewHTTPServer IdleTimeout 应 > 0，实际 %v", srv.IdleTimeout)
	}
}

// TestShippedConfigScanDefaultsOff：随仓库发的 config.yaml 默认不扫描，
// 且 Web 面板绑回环地址（只验 host 是回环，不断言端口）。
func TestShippedConfigScanDefaultsOff(t *testing.T) {
	cfg, err := config.Load("../../config.yaml")
	if err != nil {
		t.Fatalf("加载随仓库发的 config.yaml 失败: %v", err)
	}
	if cfg.Hub.ScanEnabled {
		t.Fatalf("随仓库发的 config.yaml 里扫描应默认关闭")
	}
	if len(cfg.Hub.DiscoveryFingerprints) != 0 {
		t.Fatalf("随仓库发的 config.yaml 里不应预置指纹白名单")
	}
	host, _, err := net.SplitHostPort(cfg.Hub.WebListen)
	if err != nil {
		t.Fatalf("WebListen 不是合法 host:port：%q: %v", cfg.Hub.WebListen, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		t.Fatalf("WebListen 应绑回环地址，实际是 %q", cfg.Hub.WebListen)
	}
}
