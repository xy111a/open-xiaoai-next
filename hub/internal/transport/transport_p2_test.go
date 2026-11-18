package transport

import (
	"testing"
	"time"

	"github.com/huajun/open-xiaoai-next/hub/internal/registry"
)

// P2-2：hello 拒绝原因统一，不可区分枚举——
// Token 错与型号错回同一个 reason。
func TestHelloRejectReasonUnifiedP2(t *testing.T) {
	_, url, _ := startServer(t, "test-token-12345678")
	rejectReason := func(hello map[string]any) string {
		c := dial(t, url)
		defer c.Close()
		if err := c.WriteJSON(hello); err != nil {
			t.Fatalf("发 hello: %v", err)
		}
		var resp map[string]string
		if err := c.ReadJSON(&resp); err != nil {
			t.Fatalf("读 hello_reject: %v", err)
		}
		if resp["type"] != "hello_reject" {
			t.Fatalf("应收到 hello_reject，实际 %v", resp)
		}
		return resp["reason"]
	}
	badToken := rejectReason(map[string]any{"type": "hello", "model": "lx06", "token": "wrong-token", "ram_mb": 256})
	badModel := rejectReason(map[string]any{"type": "hello", "model": "lx05", "token": "test-token-12345678", "ram_mb": 256})
	if badToken != badModel {
		t.Fatalf("拒绝原因应统一，Token 错回 %q、型号错回 %q", badToken, badModel)
	}
	if badToken == "bad token" || badToken == "unsupported model" {
		t.Fatalf("拒绝原因仍可区分枚举：%q", badToken)
	}
}

// P2-51：hello 的 room 不得覆盖语音认领的房间；
// P2-3：hello 的超长 Version 被丢弃。
func TestHelloRoomDoesNotOverrideClaimP2(t *testing.T) {
	srv, url, _ := startServer(t, "test-token-12345678")
	reg := srv.reg
	// 先用认领写入房间（模拟语音认领）。
	deviceID := "lx06-127-0-0-1"
	reg.Upsert(registry.Device{ID: deviceID, Model: "lx06", IP: "127.0.0.1"})
	reg.SetRoom(deviceID, "客厅")
	reg.SetOnline(deviceID, true)

	c := dial(t, url)
	defer c.Close()
	longVersion := ""
	for i := 0; i < 100; i++ {
		longVersion += "x"
	}
	if err := c.WriteJSON(map[string]any{
		"type": "hello", "model": "lx06", "token": "test-token-12345678",
		"ram_mb": 256, "room": "卧室", "version": longVersion,
	}); err != nil {
		t.Fatal(err)
	}
	var ok map[string]any
	if err := c.ReadJSON(&ok); err != nil || ok["type"] != "hello_ok" {
		t.Fatalf("应收到 hello_ok，实际 %v / %v", ok, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		d, found := reg.Get(deviceID)
		if found && d.Room == "客厅" {
			if d.Version != "" {
				t.Fatalf("超长 Version 应被丢弃，实际 %q", d.Version)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("hello 后房间应仍为客厅，实际 %+v", d)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// P2-3：hello 里的非法房间名（含换行）被忽略，不进注册表。
func TestHelloBadRoomIgnoredP2(t *testing.T) {
	srv, url, _ := startServer(t, "test-token-12345678")
	reg := srv.reg
	c := dial(t, url)
	defer c.Close()
	if err := c.WriteJSON(map[string]any{
		"type": "hello", "model": "lx06", "token": "test-token-12345678",
		"ram_mb": 256, "room": "客厅\n伪造",
	}); err != nil {
		t.Fatal(err)
	}
	var ok map[string]any
	if err := c.ReadJSON(&ok); err != nil || ok["type"] != "hello_ok" {
		t.Fatalf("应收到 hello_ok，实际 %v / %v", ok, err)
	}
	d, found := reg.Get("lx06-127-0-0-1")
	if !found {
		t.Fatal("设备应已注册")
	}
	if d.Room != "" {
		t.Fatalf("非法房间名应被忽略，实际 %q", d.Room)
	}
}
