package music

// N-P1-1 / N-P1-2 / P1-7 回归测试。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// notifySnap 是先发一条异步通知、再发真响应的 Snapcast 假服务器。
type notifySnap struct {
	ln net.Listener
}

func startNotifySnap(t *testing.T) *notifySnap {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &notifySnap{ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				r := bufio.NewReader(conn)
				for {
					line, err := r.ReadBytes('\n')
					if err != nil {
						return
					}
					var req struct {
						ID     int    `json:"id"`
						Method string `json:"method"`
					}
					if err := json.Unmarshal(line, &req); err != nil {
						return
					}
					// 先推一条无 id 的异步通知（音量变化），再回真响应。
					_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","method":"Client.OnVolumeChanged","params":{}}` + "\n"))
					_, _ = conn.Write([]byte(fmt.Sprintf(`{"id":%d,"jsonrpc":"2.0","result":{"server":{"groups":[]}}}`+"\n", req.ID)))
				}
			}(conn)
		}
	}()
	return f
}

func TestSnapCallSkipsNotifications(t *testing.T) {
	f := startNotifySnap(t)
	defer f.ln.Close()
	c, err := DialSnap(f.ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("DialSnap: %v", err)
	}
	defer c.Close()
	// 两次调用都不应被通知错配：第一次拿到的必须是本次的响应（空分组），
	// 第二次同样如此（真响应不能被上一轮“偷走”）。
	for i := 0; i < 2; i++ {
		groups, err := c.GetGroups()
		if err != nil {
			t.Fatalf("第 %d 次 GetGroups: %v", i+1, err)
		}
		if len(groups) != 0 {
			t.Fatalf("第 %d 次 GetGroups 应返回空分组，实际 %+v", i+1, groups)
		}
	}
}

func TestSnapCallRejectsOversizeLine(t *testing.T) {
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
		r := bufio.NewReader(conn)
		if _, err := r.ReadBytes('\n'); err != nil {
			return
		}
		// 发一行超过 64KB 的“响应”，不带换行符拖住读端。
		_, _ = conn.Write([]byte(`{"id":1,"result":` + strings.Repeat("x", maxSnapLineBytes+1)))
		time.Sleep(2 * time.Second)
	}()
	c, err := DialSnap(ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("DialSnap: %v", err)
	}
	defer c.Close()
	if _, err := c.GetGroups(); err == nil {
		t.Fatal("超长响应行应报错")
	} else if !strings.Contains(err.Error(), "超过") {
		t.Fatalf("错误应明说超长，实际: %v", err)
	}
}

func TestDialMPDWelcomeTimeout(t *testing.T) {
	// 只建连不发问候的 MPD：DialMPD 必须在 timeout 内返回，不许永久挂起。
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
	_, err = DialMPD(ln.Addr().String(), 500*time.Millisecond)
	if err == nil {
		t.Fatal("不发问候的 MPD 应 Dial 失败")
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("DialMPD 挂起 %v，远超 500ms 超时", took)
	}
}

func TestPlayURLRejectsControlChars(t *testing.T) {
	f := startFakeMPD(t)
	defer f.ln.Close()
	c, err := DialMPD(f.ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("DialMPD: %v", err)
	}
	defer c.Close()
	for _, bad := range []string{
		"http://x/1.mp3\nclear\n",
		"http://x/1.mp3\rclear\r",
		"http://x/1.mp3\x00",
		"",
	} {
		if err := c.PlayURL(bad); err == nil {
			t.Fatalf("PlayURL(%q) 应拒绝", bad)
		}
	}
	f.mu.Lock()
	n := len(f.cmds)
	f.mu.Unlock()
	if n != 0 {
		t.Fatalf("非法 URL 不应发任何 MPD 命令，实际发了 %d 条", n)
	}
	if err := c.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}
