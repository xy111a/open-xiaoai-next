package ha

// P1-5 / P1-21 回归测试。

import (
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func fakeHAURL(t *testing.T) string {
	t.Helper()
	f, url := startFakeHA(t)
	_ = f
	return url
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func TestDialEmptyTokenRejected(t *testing.T) {
	// 空 token 应在拨号前就被拒绝，不发任何 auth。
	url := fakeHAURL(t)
	if _, err := Dial(url, "", 5*time.Second); err == nil {
		t.Fatal("空 token 的 Dial 应返回错误")
	} else if !contains(err.Error(), "HASS_TOKEN") {
		t.Fatalf("错误应明说 HASS_TOKEN 为空，实际: %v", err)
	}
	if _, err := Dial(url, "   ", 5*time.Second); err == nil {
		t.Fatal("空白 token 的 Dial 应返回错误")
	}
}

func TestDialSilentServerTimesOut(t *testing.T) {
	// 只建连不发 auth_required 的 HA：Dial 必须在 timeout 内返回，不许永久挂起。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		time.Sleep(5 * time.Second)
	}()
	start := time.Now()
	_, err = Dial("ws://"+ln.Addr().String()+"/api/websocket", "tok", 500*time.Millisecond)
	if err == nil {
		t.Fatal("静默 HA 应 Dial 失败")
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("Dial 挂起 %v，远超 500ms 超时", took)
	}
}

func TestConcurrentCallsNoRace(t *testing.T) {
	// P1-21：多 goroutine 并发 call() + SubscribeEvents() 不得并发写
	// 同一连接（-race 下验证；无写锁时 fakeHA 侧也会 panic）。
	c, err := Dial(fakeHAURL(t), "hass-token", 5*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.GetEntityState("light.living_room")
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.SubscribeEvents("state_changed", func(map[string]any) {})
		}()
	}
	wg.Wait()
}

func TestNormalizeHAURL(t *testing.T) {
	// 2026-11-23 彩排 P0-2：裸地址缺 /api/websocket，HA 握手直接失败。
	// startFakeHA 不校验请求路径，Dial 层面的测试证明不了归一化，
	// 所以直接测 helper 本身。
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"裸 ws", "ws://192.168.88.247:8123", "ws://192.168.88.247:8123/api/websocket"},
		{"裸 wss", "wss://192.168.88.247:8123", "wss://192.168.88.247:8123/api/websocket"},
		{"根路径 /", "ws://192.168.88.247:8123/", "ws://192.168.88.247:8123/api/websocket"},
		{"已带路径不动", "ws://192.168.88.247:8123/api/websocket", "ws://192.168.88.247:8123/api/websocket"},
		{"其他路径不动", "ws://192.168.88.247:8123/custom", "ws://192.168.88.247:8123/custom"},
		{"带端口与空白", "  ws://127.0.0.1:8123  ", "ws://127.0.0.1:8123/api/websocket"},
		{"非法 URL 原样返回", "://bad url", "://bad url"},
		{"空串原样返回", "", ""},
		{"无 host 原样返回", "ws:///api/websocket", "ws:///api/websocket"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeHAURL(tc.in); got != tc.want {
				t.Fatalf("normalizeHAURL(%q) = %q，想要 %q", tc.in, got, tc.want)
			}
		})
	}
}
