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

// TestShippedConfigRegistrationDefaults：随仓库发的 config.yaml 默认
// 走注册制（P1-44）：不自动扫描，指纹白名单默认空（注册一律拒绝，
// fail closed），且 Web 面板绑回环或明确内网 IP（2026-11-19 bai 定局域网
// 直开；只验 host，不断言端口）。
func TestShippedConfigRegistrationDefaults(t *testing.T) {
	cfg, err := config.Load("../../config.yaml")
	if err != nil {
		t.Fatalf("加载随仓库发的 config.yaml 失败: %v", err)
	}
	if cfg.Hub.ScanEnabled {
		t.Fatalf("随仓库发的 config.yaml 里扫描应默认关闭（注册制，不自动扫描）")
	}
	if len(cfg.Hub.DiscoveryFingerprints) != 0 {
		t.Fatalf("随仓库发的 config.yaml 里不应预置指纹白名单")
	}
	host, _, err := net.SplitHostPort(cfg.Hub.WebListen)
	if err != nil {
		t.Fatalf("WebListen 不是合法 host:port：%q: %v", cfg.Hub.WebListen, err)
	}
	if ip := net.ParseIP(host); ip == nil || (!ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast()) {
		t.Fatalf("WebListen 应绑回环或明确内网 IP，实际是 %q", cfg.Hub.WebListen)
	}
}
