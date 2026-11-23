package main

// 房间认领的集成测试（P0-2）：真 transport 服务 + 真 hubHandler + 模拟 Client。
// Save 成功时设备应收到 room_ok、磁盘落盘；Save 失败时应收到 room_reject、
// 不收到 room_ok，且注册表与配置里的认领被回滚。

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/huajun/open-xiaoai-next/hub/internal/config"
	"github.com/huajun/open-xiaoai-next/hub/internal/intercom"
	"github.com/huajun/open-xiaoai-next/hub/internal/registry"
	"github.com/huajun/open-xiaoai-next/hub/internal/transport"
)

// newRoomClaimServer 建一套带 cfg/cfgPath 的真 Hub（复用 barge_in 测试的
// dial/startBargeInReader/waitForMsgType 助手），返回 handler 与 ws:// URL。
func newRoomClaimServer(t *testing.T, cfgPath string) (*hubHandler, string) {
	t.Helper()
	reg := registry.New()
	h := &hubHandler{
		cfg:      &config.Config{Devices: map[string]config.Device{}},
		cfgPath:  cfgPath,
		reg:      reg,
		intercom: intercom.New(func(string, []byte) error { return nil }),
		sessions: map[string]time.Time{},
		states:   map[string]DeviceState{},
		playGen:  map[string]uint64{},
	}
	srv := transport.New("secret", reg, h)
	h.server = srv
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() { _ = srv.Serve(ln, nil) }()
	return h, "ws://" + ln.Addr().String() + "/ws"
}

// TestRoomClaimSaveSuccess：Save 成功时收 room_ok，磁盘与注册表都有房间。
func TestRoomClaimSaveSuccess(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	h, url := newRoomClaimServer(t, cfgPath)
	c, deviceID := dialBargeInClient(t, url, "oh2p")
	defer c.Close()
	msgs := startBargeInReader(t, c)

	h.OnRoomClaim(deviceID, "客厅")
	waitForMsgType(t, msgs, "room_ok", 2*time.Second)

	got, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Save 后重读配置失败: %v", err)
	}
	if d, ok := got.Devices[deviceID]; !ok || d.Room != "客厅" {
		t.Fatalf("磁盘上的认领应为客厅，实际 %+v", got.Devices)
	}
	if d, ok := h.reg.Get(deviceID); !ok || d.Room != "客厅" {
		t.Fatalf("注册表里的认领应为客厅，实际 %+v", d)
	}
}

// TestRoomClaimChangeRoomSaveFailureRestoresPrev：换房间认领失败时，
// 配置与注册表里的旧房间都要恢复（M1，PR #17 审查必修项），而不是
// 把旧条目删掉。
func TestRoomClaimChangeRoomSaveFailureRestoresPrev(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	h, url := newRoomClaimServer(t, cfgPath)
	c, deviceID := dialBargeInClient(t, url, "oh2p")
	defer c.Close()
	msgs := startBargeInReader(t, c)

	// 先在客厅认领成功，建立旧条目。
	h.OnRoomClaim(deviceID, "客厅")
	waitForMsgType(t, msgs, "room_ok", 2*time.Second)
	if d, ok := h.cfg.Devices[deviceID]; !ok || d.Room != "客厅" {
		t.Fatalf("首次认领后配置应为客厅，实际 %+v", h.cfg.Devices)
	}

	// 删掉配置所在目录，让第二次 Save 一定失败。
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	h.OnRoomClaim(deviceID, "卧室")
	waitForMsgType(t, msgs, "room_reject", 2*time.Second)
	select {
	case typ := <-msgs:
		if typ == "room_ok" {
			t.Fatal("Save 失败时不应回 room_ok")
		}
	case <-time.After(200 * time.Millisecond):
	}

	if d, ok := h.reg.Get(deviceID); !ok {
		t.Fatal("设备应在注册表里")
	} else if d.Room != "客厅" {
		t.Fatalf("换房间失败后注册表应恢复客厅，实际 %q", d.Room)
	}
	if d, ok := h.cfg.Devices[deviceID]; !ok || d.Room != "客厅" {
		t.Fatalf("换房间失败后配置应恢复客厅旧条目，实际 %+v", h.cfg.Devices)
	}
}
// TestRoomClaimSaveFailureRejects：Save 失败时收 room_reject、不收 room_ok，
// 且注册表与配置里的认领被回滚（不留痕迹）。
func TestRoomClaimSaveFailureRejects(t *testing.T) {
	// cfgPath 的父目录不存在：CreateTemp 失败，Save 一定失败。
	cfgPath := filepath.Join(t.TempDir(), "no-such-dir", "config.yaml")
	h, url := newRoomClaimServer(t, cfgPath)
	c, deviceID := dialBargeInClient(t, url, "oh2p")
	defer c.Close()
	msgs := startBargeInReader(t, c)

	h.OnRoomClaim(deviceID, "客厅")
	waitForMsgType(t, msgs, "room_reject", 2*time.Second)
	// 已收到 room_reject，之后不应再有 room_ok（OnRoomClaim 只发一条消息）。
	select {
	case typ := <-msgs:
		if typ == "room_ok" {
			t.Fatal("Save 失败时不应回 room_ok")
		}
	case <-time.After(200 * time.Millisecond):
	}

	if d, ok := h.reg.Get(deviceID); !ok {
		t.Fatal("设备应在注册表里")
	} else if d.Room != "" {
		t.Fatalf("Save 失败后注册表房间应被回滚为空，实际 %q", d.Room)
	}
	if _, ok := h.cfg.Devices[deviceID]; ok {
		t.Fatal("Save 失败后配置里的设备条目应被回滚删除")
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Fatalf("Save 失败时不应写出配置文件，stat 结果: %v", err)
	}
}
