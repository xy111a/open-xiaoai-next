package web

// 第三轮审计 P2 回归测试（Hub 面）：
// H-7 status() 走共享 CfgLock、H-11 lyric 负 id、H-12 建二维码节流。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// H-7：GET /api/status 与 PUT /api/config 并发时不得有数据竞争
// （-race 下跑本测试验证；旧代码 status 在 s.mu 下读配置，与 PUT
// 在 CfgLock 下的 *s.cfg 写是无锁读写，-race 可检出）。
func TestStatusAndConfigPutNoRace(t *testing.T) {
	s, _ := testServer(t)
	s.CfgLock = &sync.Mutex{}
	h := s.Handler()
	// 注意：不能用 time.After 通道作停止信号——它只投递一次，
	// 两个 goroutine 抢，输的那个永远等不到；用 deadline 判断。
	deadline := time.Now().Add(500 * time.Millisecond)
	var wg sync.WaitGroup
	var statusOK, putOK int
	var mu sync.Mutex
	wg.Add(2)
	go func() {
		defer wg.Done()
		for time.Now().Before(deadline) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, authedReq(http.MethodGet, "/api/status", nil))
			if rec.Code == http.StatusOK {
				mu.Lock()
				statusOK++
				mu.Unlock()
			}
		}
	}()
	go func() {
		defer wg.Done()
		for time.Now().Before(deadline) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, authedReq(http.MethodPut, "/api/config",
				[]byte(`{"wake_word":"你好小李","music_folder":"/volume1/music"}`)))
			if rec.Code == http.StatusOK {
				mu.Lock()
				putOK++
				mu.Unlock()
			}
		}
	}()
	wg.Wait()
	if statusOK == 0 {
		t.Fatal("并发期间 GET /api/status 应至少成功一次")
	}
	if putOK == 0 {
		t.Fatal("并发期间 PUT /api/config 应至少成功一次")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authedReq(http.MethodGet, "/api/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("并发后 /api/status 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// H-11：/api/music/lyric?id=-5 应回 400“id 非法”，不直通上游。
func TestMusicLyricNegativeIDRejected(t *testing.T) {
	s, _, _ := testServerFuse(t)
	h := s.Handler()
	for _, path := range []string{"/api/music/lyric?id=-5", "/api/music/lyric?id=-1"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, authedReq(http.MethodGet, path, nil))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("GET %s 应 400，实际 %d: %s", path, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "id 非法") {
			t.Fatalf("GET %s 应提示“id 非法”，实际 %s", path, rec.Body.String())
		}
	}
	// 正 id 与不带 id 仍正常（不带 id 用当前曲）。
	for _, path := range []string{"/api/music/lyric?id=12345", "/api/music/lyric"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, authedReq(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s 应 200，实际 %d: %s", path, rec.Code, rec.Body.String())
		}
	}
}

// H-12：/api/netease/login/qr 每 IP 每分钟最多新建 10 次，
// 第 11 次回 429；另一 IP 不受影响。
func TestNeteaseLoginQRThrottle(t *testing.T) {
	s, _, _ := testServerFuse(t)
	h := s.Handler()
	postQR := func(ip string) int {
		req := authedReq(http.MethodPost, "/api/netease/login/qr", nil)
		if ip != "" {
			req.RemoteAddr = ip
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for i := 0; i < maxQRPerMinute; i++ {
		if code := postQR(""); code != http.StatusOK {
			t.Fatalf("第 %d 次建二维码应 200，实际 %d", i+1, code)
		}
	}
	if code := postQR(""); code != http.StatusTooManyRequests {
		t.Fatalf("第 %d 次建二维码应 429，实际 %d", maxQRPerMinute+1, code)
	}
	// 另一 IP 不受该 IP 的节流影响。
	if code := postQR("192.0.2.99:1234"); code != http.StatusOK {
		t.Fatalf("另一 IP 建二维码应 200，实际 %d", code)
	}
}
