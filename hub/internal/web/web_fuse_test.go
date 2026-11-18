package web

// 融合 B 回归测试：网易云扫码登录全流程（fake netease-api，cookie 写盘
// 0600 且在临时目录里）、音乐端点（pause/resume/next/previous/status/
// queue/lyric）、鉴权复用（无 Token 一律 401）、旧配置字段经 PUT 拒绝。
// 全部用 fake 服务与假 cookie 字符串，不写真实 token/cookie。

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

	"github.com/huajun/open-xiaoai-next/hub/internal/music"
)

// stubMusicBackend 是 MusicBackend 的测试替身：记录控制调用，
// 返回 canned 的播放状态/队列/歌词。
type stubMusicBackend struct {
	calls    []string
	state    string
	current  music.MPDSong
	queue    []music.MPDSong
	lyric    string
	track    music.NeteaseSong
	source   string
	hasTrack bool
	err      error
}

func (s *stubMusicBackend) MusicPause(_ context.Context) error {
	s.calls = append(s.calls, "pause")
	return s.err
}

func (s *stubMusicBackend) MusicResume(_ context.Context) error {
	s.calls = append(s.calls, "resume")
	return s.err
}

func (s *stubMusicBackend) MusicNext(_ context.Context) error {
	s.calls = append(s.calls, "next")
	return s.err
}

func (s *stubMusicBackend) MusicPrevious(_ context.Context) error {
	s.calls = append(s.calls, "previous")
	return s.err
}

func (s *stubMusicBackend) PlaybackState(_ context.Context) (string, music.MPDSong, []music.MPDSong, error) {
	return s.state, s.current, s.queue, s.err
}

func (s *stubMusicBackend) MusicQueue(_ context.Context) ([]music.MPDSong, error) {
	return s.queue, s.err
}

func (s *stubMusicBackend) MusicLyric(_ context.Context, id int64) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	if id == 0 && !s.hasTrack {
		return "", fmt.Errorf("还没在放歌，先点一首吧")
	}
	return s.lyric, nil
}

func (s *stubMusicBackend) CurrentTrack() (music.NeteaseSong, string, bool) {
	return s.track, s.source, s.hasTrack
}

func (s *stubMusicBackend) saw(call string) bool {
	for _, c := range s.calls {
		if c == call {
			return true
		}
	}
	return false
}

// fakeNeteaseAPIFuse 是带扫码登录的假 netease-api（与 music 包的测试替身
// 同款协议：key/create/check/status，cookie 全是假字符串）。
func fakeNeteaseAPIFuse(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/login/qr/key"):
			fmt.Fprint(w, `{"code":200,"data":{"unikey":"ok-key"}}`)
		case strings.HasPrefix(r.URL.Path, "/login/qr/create"):
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
		default:
			http.NotFound(w, r)
		}
	}))
}

func testServerFuse(t *testing.T) (*Server, *stubMusicBackend, string) {
	t.Helper()
	s, _ := testServer(t)
	stub := &stubMusicBackend{
		state:   "play",
		current: music.MPDSong{ID: 5, Pos: 0, File: "http://music.example.com/12345.mp3", Title: "青花瓷", Artist: "周杰伦"},
		queue: []music.MPDSong{
			{ID: 5, Pos: 0, File: "http://music.example.com/12345.mp3", Title: "青花瓷", Artist: "周杰伦"},
			{ID: 6, Pos: 1, File: "青花瓷.mp3", Title: "青花瓷", Artist: "周杰伦"},
		},
		lyric:    "[00:00.00]素胚勾勒出青花",
		track:    music.NeteaseSong{ID: 12345, Name: "青花瓷", Artist: "周杰伦"},
		source:   "netease",
		hasTrack: true,
	}
	api := fakeNeteaseAPIFuse(t)
	t.Cleanup(api.Close)
	cookiePath := filepath.Join(t.TempDir(), "netease-cookie.json")
	s.Music = stub
	s.Netease = music.NewNetease(api.URL)
	s.NeteaseAuth = music.NewNeteaseAuth(api.URL, cookiePath)
	return s, stub, cookiePath
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.NewDecoder(rec.Body).Decode(out); err != nil {
		t.Fatalf("解响应失败: %v（body: %s）", err, rec.Body.String())
	}
}

func TestWebMusicEndpointsRequireAuth(t *testing.T) {
	s, _, _ := testServerFuse(t)
	for _, path := range []string{"/api/music/status", "/api/music/queue", "/api/netease/status", "/api/music/lyric?id=1"} {
		req := httptest.NewRequest(http.MethodGet, path, nil) // 不带 Bearer
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s 无 Token 应 401，实际 %d", path, rec.Code)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/api/music/pause", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /api/music/pause 无 Token 应 401，实际 %d", rec.Code)
	}
}

func TestWebNeteaseLoginFlow(t *testing.T) {
	s, _, cookiePath := testServerFuse(t)
	h := s.Handler()

	// 未登录：status 报 logged_in=false、needs_rescan=true。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authedReq(http.MethodGet, "/api/netease/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var st map[string]any
	decodeBody(t, rec, &st)
	if st["logged_in"] != false || st["needs_rescan"] != true {
		t.Fatalf("未登录时 status 应为 logged_in=false/needs_rescan=true，实际 %v", st)
	}

	// 建二维码：应返回 key 与 qrurl。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, authedReq(http.MethodPost, "/api/netease/login/qr", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("建二维码应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var qr map[string]any
	decodeBody(t, rec, &qr)
	if qr["key"] != "ok-key" || qr["qrurl"] == "" || qr["qrimg"] == "" {
		t.Fatalf("建二维码返回不对: %v", qr)
	}

	// 轮询等待扫码：801，未登录，不写 cookie。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, authedReq(http.MethodGet, "/api/netease/login/qr/check?key=wait-key", nil))
	var chk map[string]any
	decodeBody(t, rec, &chk)
	if chk["code"] != float64(801) || chk["logged_in"] != false {
		t.Fatalf("等待扫码应为 code=801/logged_in=false，实际 %v", chk)
	}
	if _, err := os.Stat(cookiePath); !os.IsNotExist(err) {
		t.Fatal("801 时不应写 cookie 文件")
	}

	// 轮询成功：803，cookie 写盘 0600（临时目录，不在仓库里）。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, authedReq(http.MethodGet, "/api/netease/login/qr/check?key=ok-key", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("check 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	decodeBody(t, rec, &chk)
	if chk["code"] != float64(803) || chk["logged_in"] != true {
		t.Fatalf("扫码成功应为 code=803/logged_in=true，实际 %v", chk)
	}
	fi, err := os.Stat(cookiePath)
	if err != nil {
		t.Fatalf("803 后 cookie 文件应存在: %v", err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Fatalf("cookie 文件权限应为 0600，实际 %o", fi.Mode().Perm())
	}
	if b, err := os.ReadFile(cookiePath); err != nil || !strings.Contains(string(b), "fake-music-u-value") {
		t.Fatalf("cookie 文件应含假 cookie 字符串，err=%v", err)
	}

	// 登录后 status：已登录 + 昵称 + 过期时间。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, authedReq(http.MethodGet, "/api/netease/status", nil))
	decodeBody(t, rec, &st)
	if st["logged_in"] != true || st["nickname"] != "测试用户" || st["expires_at"] == "" {
		t.Fatalf("登录后 status 应为已登录/昵称/过期时间，实际 %v", st)
	}

	// 登出：文件删除，status 回未登录。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, authedReq(http.MethodPost, "/api/netease/logout", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("logout 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(cookiePath); !os.IsNotExist(err) {
		t.Fatal("logout 后 cookie 文件应删除")
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, authedReq(http.MethodGet, "/api/netease/status", nil))
	decodeBody(t, rec, &st)
	if st["logged_in"] != false || st["needs_rescan"] != true {
		t.Fatalf("登出后 status 应回未登录，实际 %v", st)
	}
}

func TestWebMusicControlAndStatus(t *testing.T) {
	s, stub, _ := testServerFuse(t)
	h := s.Handler()

	// 状态：当前曲用 Hub 内存里的网易云曲名，队列长度 2。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authedReq(http.MethodGet, "/api/music/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("music status 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var st map[string]any
	decodeBody(t, rec, &st)
	if st["state"] != "play" || st["queue_len"] != float64(2) {
		t.Fatalf("music status 不对: %v", st)
	}
	cur, _ := st["current"].(map[string]any)
	if cur["name"] != "青花瓷" || cur["artist"] != "周杰伦" || cur["source"] != "netease" {
		t.Fatalf("当前曲应为网易云版青花瓷，实际 %v", cur)
	}

	// 四个控制端点：200 且后端收到调用。
	for path, call := range map[string]string{
		"/api/music/pause": "pause", "/api/music/resume": "resume",
		"/api/music/next": "next", "/api/music/previous": "previous",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, authedReq(http.MethodPost, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("POST %s 应 200，实际 %d: %s", path, rec.Code, rec.Body.String())
		}
		if !stub.saw(call) {
			t.Fatalf("POST %s 后后端应收到 %s，实际 %v", path, call, stub.calls)
		}
	}

	// 队列：2 首。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, authedReq(http.MethodGet, "/api/music/queue", nil))
	var q map[string]any
	decodeBody(t, rec, &q)
	items, _ := q["queue"].([]any)
	if len(items) != 2 {
		t.Fatalf("队列应有 2 首，实际 %v", q)
	}

	// 歌词：带 id 与不带 id（用当前曲）都应返回。
	for _, path := range []string{"/api/music/lyric?id=12345", "/api/music/lyric"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, authedReq(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s 应 200，实际 %d: %s", path, rec.Code, rec.Body.String())
		}
		var ly map[string]any
		decodeBody(t, rec, &ly)
		if !strings.Contains(ly["lyric"].(string), "素胚勾勒出青花") {
			t.Fatalf("GET %s 歌词不对: %v", path, ly)
		}
	}
}

func TestWebMusicUnwired(t *testing.T) {
	s, _ := testServer(t) // 不接 Music / NeteaseAuth
	h := s.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authedReq(http.MethodGet, "/api/music/status", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("音乐未接线时 status 应 503，实际 %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, authedReq(http.MethodGet, "/api/netease/status", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("登录未接线时 status 应 503，实际 %d", rec.Code)
	}
}

func TestWebConfigPutRejectsMusicAssistant(t *testing.T) {
	s, _ := testServer(t)
	rec := httptest.NewRecorder()
	h := s.Handler()
	h.ServeHTTP(rec, authedReq(http.MethodPut, "/api/config", []byte(`{"netease":{"music_assistant":"http://music-assistant:8095"}}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT 含 music_assistant 应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "netease.music_assistant 已废弃") {
		t.Fatalf("拒绝信息应明说 music_assistant 已废弃，实际 %s", rec.Body.String())
	}
}
