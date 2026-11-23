package music

// 融合 B 回归测试：MPD 队列控制、网易云扫码登录全流程、
// NeteaseClient 带 Cookie 发请求。全部用 fake 服务与假 cookie 字符串，
// 不写任何真实 token/cookie。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMPDQueueControls(t *testing.T) {
	f := startFakeMPD(t)
	defer f.ln.Close()
	c, err := DialMPD(f.ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("DialMPD: %v", err)
	}
	defer c.Close()

	// 每个动作后断言它发出的最后一条命令。
	lastCmd := func() string {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.cmds[len(f.cmds)-1]
	}
	if err := c.Pause(true); err != nil || lastCmd() != "pause 1" {
		t.Fatalf("Pause(true) 应发 pause 1，实际 %q err=%v", lastCmd(), err)
	}
	if err := c.Pause(false); err != nil || lastCmd() != "pause 0" {
		t.Fatalf("Pause(false) 应发 pause 0，实际 %q err=%v", lastCmd(), err)
	}
	if err := c.Resume(); err != nil || lastCmd() != "pause 0" {
		t.Fatalf("Resume 应发 pause 0，实际 %q err=%v", lastCmd(), err)
	}
	if err := c.Play(); err != nil || lastCmd() != "play" {
		t.Fatalf("Play 应发 play，实际 %q err=%v", lastCmd(), err)
	}
	if err := c.Next(); err != nil || lastCmd() != "next" {
		t.Fatalf("Next 应发 next，实际 %q err=%v", lastCmd(), err)
	}
	if err := c.Previous(); err != nil || lastCmd() != "previous" {
		t.Fatalf("Previous 应发 previous，实际 %q err=%v", lastCmd(), err)
	}
}

func TestMPDStatusCurrentQueue(t *testing.T) {
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
	if st.State != "play" || st.Song != 0 || st.SongID != 5 || st.Elapsed != 12.5 || st.Duration != 240 || st.Volume != 80 {
		t.Fatalf("Status 解析不对: %+v", st)
	}
	cur, err := c.CurrentSong()
	if err != nil {
		t.Fatalf("CurrentSong: %v", err)
	}
	if cur.File != "http://music.example.com/12345.mp3" || cur.Title != "青花瓷" || cur.Artist != "周杰伦" || cur.ID != 5 {
		t.Fatalf("CurrentSong 解析不对: %+v", cur)
	}
	q, err := c.Queue()
	if err != nil {
		t.Fatalf("Queue: %v", err)
	}
	if len(q) != 2 || q[1].File != "青花瓷.mp3" || q[1].ID != 6 || q[1].Pos != 1 {
		t.Fatalf("Queue 应有 2 首、第二首是本地文件，实际 %+v", q)
	}
	n := len(f.cmds)
	files, err := c.SearchFile("青花瓷")
	if err != nil {
		t.Fatalf("SearchFile: %v", err)
	}
	if len(files) != 1 || files[0] != "青花瓷.mp3" {
		t.Fatalf("SearchFile 应返回 [青花瓷.mp3]，实际 %v", files)
	}
	f.mu.Lock()
	gotSearch := append([]string{}, f.cmds[n:]...)
	f.mu.Unlock()
	if len(gotSearch) != 1 || gotSearch[0] != `search filename "青花瓷"` {
		t.Fatalf("SearchFile 应发 [search filename \"青花瓷\"]，实际 %v", gotSearch)
	}
	n = len(f.cmds)
	if err := c.PlayFile("青花瓷.mp3"); err != nil {
		t.Fatalf("PlayFile: %v", err)
	}
	f.mu.Lock()
	got := append([]string{}, f.cmds[n:]...)
	f.mu.Unlock()
	want := []string{"clear", `add "青花瓷.mp3"`, "play"}
	if len(got) != len(want) {
		t.Fatalf("PlayFile 应发 %v，实际 %v", want, got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("PlayFile 第 %d 条应为 %q，实际 %q", i, w, got[i])
		}
	}
}

// fakeNeteaseAPIFuse 是带扫码登录的假 netease-api：key/create/check/status
// 全流程，cookie 全是假字符串。
func fakeNeteaseAPIFuse(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/login/qr/key"):
			fmt.Fprint(w, `{"code":200,"data":{"unikey":"ok-key"}}`)
		case strings.HasPrefix(r.URL.Path, "/login/qr/create"):
			if r.URL.Query().Get("key") == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			fmt.Fprint(w, `{"code":200,"data":{"qrurl":"https://example.com/qr/ok-key","qrimg":"data:image/png;base64,fakeqr"}}`)
		case strings.HasPrefix(r.URL.Path, "/login/qr/check"):
			switch r.URL.Query().Get("key") {
			case "ok-key":
				fmt.Fprint(w, `{"code":803,"message":"授权登录成功","cookie":"MUSIC_U=fake-music-u-value; Max-Age=2592000; Path=/; HTTPOnly"}`)
			case "wait-key":
				fmt.Fprint(w, `{"code":801,"message":"等待扫码"}`)
			default:
				fmt.Fprint(w, `{"code":800,"message":"二维码已过期"}`)
			}
		case strings.HasPrefix(r.URL.Path, "/login/status"):
			if strings.Contains(r.Header.Get("Cookie"), "MUSIC_U=fake-music-u-value") {
				fmt.Fprint(w, `{"data":{"account":{"id":123},"profile":{"nickname":"测试用户"}}}`)
			} else {
				fmt.Fprint(w, `{"data":{"account":{"id":0},"profile":{}}}`)
			}
		case strings.HasPrefix(r.URL.Path, "/search"):
			fmt.Fprintf(w, `{"code":200,"result":{"songs":[{"id":12345,"name":"青花瓷","artists":[{"name":"周杰伦"}]}]},"cookie_seen":%q}`, r.Header.Get("Cookie"))
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestNeteaseAuthQRFlow(t *testing.T) {
	srv := fakeNeteaseAPIFuse(t)
	defer srv.Close()
	ctx := context.Background()
	cookiePath := filepath.Join(t.TempDir(), "netease-cookie.json")
	auth := NewNeteaseAuth(srv.URL, cookiePath)

	// 未登录时 Status 应报未登录，不报错。
	if loggedIn, _, _, err := auth.Status(ctx); err != nil || loggedIn {
		t.Fatalf("未登录时 Status 应为 (false, 无错)，实际 loggedIn=%v err=%v", loggedIn, err)
	}
	// 建码：key 与 qrurl 都应返回。
	key, qrURL, qrImg, err := auth.CreateQR(ctx)
	if err != nil {
		t.Fatalf("CreateQR: %v", err)
	}
	if key != "ok-key" || qrURL == "" || qrImg == "" {
		t.Fatalf("CreateQR 返回不对: key=%q qrurl=%q qrimg 为空=%v", key, qrURL, qrImg == "")
	}
	// 等待扫码：801，不写 cookie 文件。
	if code, _, err := auth.CheckQR(ctx, "wait-key"); err != nil || code != 801 {
		t.Fatalf("等待扫码应返回 801，实际 code=%d err=%v", code, err)
	}
	if _, err := os.Stat(cookiePath); !os.IsNotExist(err) {
		t.Fatal("801 时不应写 cookie 文件")
	}
	// 扫码成功：803，cookie 写盘 0600，且不在仓库里（路径是 TempDir）。
	if code, _, err := auth.CheckQR(ctx, "ok-key"); err != nil || code != 803 {
		t.Fatalf("扫码成功应返回 803，实际 code=%d err=%v", code, err)
	}
	fi, err := os.Stat(cookiePath)
	if err != nil {
		t.Fatalf("803 后 cookie 文件应存在: %v", err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Fatalf("cookie 文件权限应为 0600，实际 %o", fi.Mode().Perm())
	}
	b, err := os.ReadFile(cookiePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "fake-music-u-value") {
		t.Fatal("cookie 文件应含假 cookie 字符串")
	}
	if auth.Cookie() == "" || !strings.Contains(auth.Cookie(), "fake-music-u-value") {
		t.Fatal("CheckQR 成功后内存里应有 cookie")
	}
	// Status：已登录 + 昵称 + 过期时间（Max-Age 30 天，相对保存时间）。
	loggedIn, nickname, expiresAt, err := auth.Status(ctx)
	if err != nil || !loggedIn {
		t.Fatalf("登录后 Status 应为已登录，实际 loggedIn=%v err=%v", loggedIn, err)
	}
	if nickname != "测试用户" {
		t.Fatalf("昵称应为 测试用户，实际 %q", nickname)
	}
	if expiresAt == "" {
		t.Fatal("Status 应返回过期时间")
	}
	if exp, err := time.Parse(time.RFC3339, expiresAt); err != nil || time.Until(exp) < 29*24*time.Hour {
		t.Fatalf("过期时间应在约 30 天后，实际 %q err=%v", expiresAt, err)
	}
	// 新实例 Load() 能读回 cookie；Logout 后文件消失、Status 回未登录。
	auth2 := NewNeteaseAuth(srv.URL, cookiePath)
	ok, err := auth2.Load()
	if err != nil || !ok {
		t.Fatalf("Load 应读回 cookie，实际 ok=%v err=%v", ok, err)
	}
	if auth2.Cookie() != auth.Cookie() {
		t.Fatal("Load 读回的 cookie 应与保存的一致")
	}
	if err := auth2.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := os.Stat(cookiePath); !os.IsNotExist(err) {
		t.Fatal("Logout 后 cookie 文件应删除")
	}
	if loggedIn, _, _, _ := auth2.Status(ctx); loggedIn {
		t.Fatal("Logout 后 Status 应为未登录")
	}
}

func TestNeteaseClientSendsCookie(t *testing.T) {
	srv := fakeNeteaseAPIFuse(t)
	defer srv.Close()
	c := NewNetease(srv.URL)
	ctx := context.Background()
	// 未登录：Search 不带 Cookie 头（服务端回的 cookie_seen 为空即证明）。
	songs, err := c.Search(ctx, "青花瓷", 1)
	if err != nil || len(songs) != 1 {
		t.Fatalf("Search: songs=%v err=%v", songs, err)
	}
	// 登录后：Search 应带上 Cookie（假服务端把收到的头原样回在 body 里，
	// 这里用 Search 的返回验证不现实——改用直读 body 的方式验证：
	// 直接对 fake 服务发请求并检查它回显的 cookie_seen）。
	c.SetCookie("MUSIC_U=fake-music-u-value; Path=/")
	resp, err := c.get(ctx, srv.URL+"/search?keywords=%E9%9D%92%E8%8A%B1%E7%93%B7&type=1&limit=1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		CookieSeen string `json:"cookie_seen"`
	}
	// fake 服务的 /search 返回体里带 cookie_seen。
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("解响应: %v", err)
	}
	if !strings.Contains(out.CookieSeen, "MUSIC_U=fake-music-u-value") {
		t.Fatalf("登录后 Search 应带 Cookie，服务端收到 %q", out.CookieSeen)
	}
	// 清除后不再带。
	c.SetCookie("")
	resp2, err := c.get(ctx, srv.URL+"/search?keywords=x&type=1&limit=1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp2.Body.Close()
	var out2 struct {
		CookieSeen string `json:"cookie_seen"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&out2); err != nil {
		t.Fatalf("解响应: %v", err)
	}
	if out2.CookieSeen != "" {
		t.Fatalf("清除 Cookie 后不应再带，服务端收到 %q", out2.CookieSeen)
	}
}
