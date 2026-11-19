package music

// 第三轮审计 P2 回归测试（Hub 面）：
// H-2 MPD query() 限长、H-3 可播 URL 白名单、
// H-4 Status() 401/403 映射为未登录、H-9 save() 并发一致性。

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// startRawMPD 起一个只按 handle 回包的 MPD 假服务器（H-2 用）：
// 先发欢迎行，读到一条命令后调 handle，由 handle 决定回什么。
func startRawMPD(t *testing.T, handle func(conn net.Conn, cmd string)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_, _ = conn.Write([]byte("OK MPD 0.23.0\n"))
				r := bufio.NewReader(conn)
				line, err := r.ReadString('\n')
				if err != nil {
					return
				}
				handle(conn, strings.TrimSpace(line))
				// 保持连接开一会儿，让客户端读完/超时（超限错误
				// 本身不依赖这段等待）。
				time.Sleep(2 * time.Second)
			}(conn)
		}
	}()
	return ln.Addr().String()
}

// H-2：单行超过 64KB 的 MPD 响应应报错，不许无界读行。
func TestMPDQueryRejectsOversizeLine(t *testing.T) {
	addr := startRawMPD(t, func(conn net.Conn, cmd string) {
		_, _ = conn.Write([]byte(strings.Repeat("x", maxMPDLineBytes+1) + "\nOK\n"))
	})
	c, err := DialMPD(addr, 5*time.Second)
	if err != nil {
		t.Fatalf("DialMPD: %v", err)
	}
	defer c.Close()
	if _, err := c.Status(); err == nil {
		t.Fatal("超长响应行应报错")
	} else if !strings.Contains(err.Error(), "超过") {
		t.Fatalf("错误应明说超长，实际: %v", err)
	}
}

// H-2：总行数超过上限的 MPD 响应应报错并断开连接。
func TestMPDQueryRejectsTooManyLines(t *testing.T) {
	addr := startRawMPD(t, func(conn net.Conn, cmd string) {
		var b strings.Builder
		for i := 0; i < maxMPDResponseLines+1; i++ {
			b.WriteString("file: song.mp3\n")
		}
		b.WriteString("OK\n")
		_, _ = conn.Write([]byte(b.String()))
	})
	c, err := DialMPD(addr, 5*time.Second)
	if err != nil {
		t.Fatalf("DialMPD: %v", err)
	}
	defer c.Close()
	if _, err := c.Queue(); err == nil {
		t.Fatal("超行数响应应报错")
	} else if !strings.Contains(err.Error(), "超过") {
		t.Fatalf("错误应明说超限，实际: %v", err)
	}
	// 超限后连接已断开（看门狗会重拨，客户端侧后续调用应失败）。
	if err := c.Ping(); err == nil {
		t.Fatal("超限断开后 Ping 应失败")
	}
}

// H-2：正常小响应不受上限影响（status 仍能读到）。
func TestMPDQuerySmallResponseOK(t *testing.T) {
	f := startFakeMPD(t)
	defer f.ln.Close()
	c, err := DialMPD(f.ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("DialMPD: %v", err)
	}
	defer c.Close()
	st, err := c.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != "play" {
		t.Fatalf("状态应为 play，实际 %q", st.State)
	}
}

// H-3：可播 URL 白名单（validatePlayableURL）。
func TestValidatePlayableURL(t *testing.T) {
	valid := []string{
		"http://m801.music.126.net/abc123.mp3",
		"https://m701.music.126.net/abc123.mp3",
		"http://music.example.com/12345.mp3",
		"https://music.example.com/12345.mp3",
	}
	for _, u := range valid {
		if err := validatePlayableURL(u); err != nil {
			t.Fatalf("PlayableURL(%q) 应通过，实际: %v", u, err)
		}
	}
	invalid := []string{
		"",
		"file:///etc/passwd",
		"file://music.example.com/x.mp3",
		"ftp://music.example.com/x.mp3",
		"http://localhost:6600/x.mp3",
		"http://localhost/x.mp3",
		"https://foo.localhost/x.mp3",
		"http://127.0.0.1/x.mp3",
		"http://192.168.88.247/x.mp3",
		"http://10.0.0.5/x.mp3",
		"http://172.16.0.1/x.mp3",
		"http://[::1]/x.mp3",
		"http://[fe80::1]/x.mp3",
		"http://8.8.8.8/x.mp3",
		"https://user:pass@music.example.com/x.mp3",
		"http://x/1.mp3",
		"music.example.com/x.mp3",
		"//music.example.com/x.mp3",
	}
	for _, u := range invalid {
		if err := validatePlayableURL(u); err == nil {
			t.Fatalf("PlayableURL(%q) 应拒绝", u)
		}
	}
}

// H-3：PlayURL 拒绝非法 URL，且不发任何 MPD 命令。
func TestPlayURLRejectsDisallowedURL(t *testing.T) {
	f := startFakeMPD(t)
	defer f.ln.Close()
	c, err := DialMPD(f.ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("DialMPD: %v", err)
	}
	defer c.Close()
	for _, bad := range []string{
		"file:///etc/passwd",
		"http://127.0.0.1/x.mp3",
		"http://192.168.88.247/x.mp3",
		"http://localhost/x.mp3",
		"ftp://music.example.com/x.mp3",
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

// H-3：SongURL 收到非法 URL 时按“无可播地址”报错（调用方走本地兜底），
// 而不是把非法地址交出去。
func TestSongURLRejectsDisallowedURL(t *testing.T) {
	for _, badURL := range []string{
		"file:///etc/passwd",
		"http://192.168.88.247/secret.mp3",
		"http://localhost:3000/x.mp3",
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"code":200,"data":[{"id":12345,"url":%q}]}`, badURL)
		}))
		c := NewNetease(srv.URL)
		_, err := c.SongURL(context.Background(), 12345)
		srv.Close()
		if err == nil {
			t.Fatalf("SongURL 返回 %q 时应报错", badURL)
		} else if !strings.Contains(err.Error(), "无可播地址") {
			t.Fatalf("SongURL 非法 URL 应报“无可播地址”，实际: %v", err)
		}
	}
}

// H-4：Status() 把上游 401/403 映射为未登录（不返回 err），
// 仅网络错误/5xx 才返回 err（Web 层据此 502）。
func TestNeteaseStatusMaps401403ToLoggedOut(t *testing.T) {
	var code int
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if code != http.StatusOK {
			w.WriteHeader(code)
		}
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	a := NewNeteaseAuth(srv.URL, filepath.Join(t.TempDir(), "netease-cookie.json"))
	if err := a.save("MUSIC_U=fake-cookie; Max-Age=2592000"); err != nil {
		t.Fatalf("save: %v", err)
	}
	ctx := context.Background()

	// 401 → 未登录，无 err。
	code = http.StatusUnauthorized
	body = `{"code":401}`
	if loggedIn, _, _, err := a.Status(ctx); err != nil || loggedIn {
		t.Fatalf("401 应为未登录且无 err，实际 loggedIn=%v err=%v", loggedIn, err)
	}
	// 403 → 未登录，无 err。
	code = http.StatusForbidden
	body = `{"code":403}`
	if loggedIn, _, _, err := a.Status(ctx); err != nil || loggedIn {
		t.Fatalf("403 应为未登录且无 err，实际 loggedIn=%v err=%v", loggedIn, err)
	}
	// 500 → 返回 err（Web 层 502）。
	code = http.StatusInternalServerError
	body = `{"code":500}`
	if _, _, _, err := a.Status(ctx); err == nil {
		t.Fatal("500 应返回 err")
	}
	// 200 + account.id=0 → 未登录，无 err（旧行为保持）。
	code = http.StatusOK
	body = `{"data":{"account":{"id":0},"profile":{}}}`
	if loggedIn, _, _, err := a.Status(ctx); err != nil || loggedIn {
		t.Fatalf("200 且 id=0 应为未登录且无 err，实际 loggedIn=%v err=%v", loggedIn, err)
	}
	// 200 + 正常账号 → 已登录。
	body = `{"data":{"account":{"id":123},"profile":{"nickname":"测试用户"}}}`
	if loggedIn, nick, _, err := a.Status(ctx); err != nil || !loggedIn || nick != "测试用户" {
		t.Fatalf("200 且 id=123 应为已登录，实际 loggedIn=%v nick=%q err=%v", loggedIn, nick, err)
	}
}

// H-9：并发 save() 后，落盘文件与内存 cookie 必须一致
// （旧代码先写盘后持锁更新内存，交错后两者可能各执一个 cookie）。
func TestNeteaseSaveConcurrentConsistent(t *testing.T) {
	dir := t.TempDir()
	cookiePath := filepath.Join(dir, "netease-cookie.json")
	a := NewNeteaseAuth("http://127.0.0.1:1", cookiePath)
	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := a.save(fmt.Sprintf("MUSIC_U=cookie-%02d", i)); err != nil {
				t.Errorf("save %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	b, err := os.ReadFile(cookiePath)
	if err != nil {
		t.Fatalf("读 cookie 文件: %v", err)
	}
	var f neteaseCookieFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("解析 cookie 文件: %v", err)
	}
	if f.Cookie != a.Cookie() {
		t.Fatalf("文件与内存 cookie 不一致：文件 %q、内存 %q", f.Cookie, a.Cookie())
	}
	if f.SavedAt == "" {
		t.Fatal("cookie 文件应有 saved_at")
	}
	// 成功保存后不应残留临时文件。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("保存后目录应只有 cookie 文件，实际 %d 个: %v", len(entries), entries)
	}
}
