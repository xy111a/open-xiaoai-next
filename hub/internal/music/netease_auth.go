// Package music 的网易云扫码登录：对着 netease-api（NeteaseCloudMusicApi）
// 的 /login/qr/key → /login/qr/create → /login/qr/check 走二维码登录。
//
// 流程（与 Binaryify 的 netease_cloud_music_api 一致）：
//  1. GET /login/qr/key 拿 unikey；
//  2. GET /login/qr/create?key=...&qrimg=true 拿 qrurl（扫码地址）与 qrimg；
//  3. 轮询 GET /login/qr/check?key=...：800 二维码过期、801 等待扫码、
//     802 待确认、803 成功（响应里带 cookie）。
//
// 成功后 cookie 只写 Hub 数据卷 <data_dir>/netease-cookie.json（0600），
// 绝不进仓库、配置文件、测试、文档、日志；NeteaseClient 取 VIP 歌地址时
// 带上它。Cookie 约 1–2 个月过期，过期后 /api/netease/status 明确返回
// 需重扫（见 Status）。
package music

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// NeteaseAuth 管网易云扫码登录与 cookie 存取。
type NeteaseAuth struct {
	baseURL    string
	cookiePath string
	http       *http.Client

	mu      sync.Mutex
	cookie  string
	savedAt time.Time // cookie 保存时间（Max-Age 算过期用）
}

// NewNeteaseAuth 新建登录管理器。cookiePath 是 cookie 文件的完整路径，
// 由 main 传 <data_dir>/netease-cookie.json。
func NewNeteaseAuth(baseURL, cookiePath string) *NeteaseAuth {
	return &NeteaseAuth{
		baseURL:    strings.TrimSuffix(baseURL, "/"),
		cookiePath: cookiePath,
		http:       &http.Client{Timeout: 15 * time.Second},
	}
}

// Cookie 返回当前登录 Cookie（未登录为空）。只在 Hub 进程内用，永不记日志。
func (a *NeteaseAuth) Cookie() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cookie
}

type neteaseCookieFile struct {
	Cookie  string `json:"cookie"`
	SavedAt string `json:"saved_at"` // RFC3339
}

// Load 从数据卷读回上次保存的 cookie（文件不存在表示从未登录过，
// 返回 false, nil；文件损坏返回错误，由调用方记日志并当未登录处理）。
func (a *NeteaseAuth) Load() (bool, error) {
	b, err := os.ReadFile(a.cookiePath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	var f neteaseCookieFile
	if err := json.Unmarshal(b, &f); err != nil {
		return false, fmt.Errorf("netease cookie 文件损坏: %w", err)
	}
	if strings.TrimSpace(f.Cookie) == "" {
		return false, nil
	}
	savedAt, _ := time.Parse(time.RFC3339, f.SavedAt)
	a.mu.Lock()
	a.cookie = f.Cookie
	a.savedAt = savedAt
	a.mu.Unlock()
	return true, nil
}

// save 把 cookie 写进数据卷：先写同目录临时文件（0600）再改名，
// 目录不存在时建出来。调用方须已确认 cookie 非空。
func (a *NeteaseAuth) save(cookie string) error {
	now := time.Now()
	f := neteaseCookieFile{Cookie: cookie, SavedAt: now.Format(time.RFC3339)}
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	dir := filepath.Dir(a.cookiePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".netease-cookie-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0600); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, a.cookiePath); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	a.mu.Lock()
	a.cookie = cookie
	a.savedAt = now
	a.mu.Unlock()
	return nil
}

// Logout 清除登录：内存 cookie 清空，数据卷文件删除（不存在也算成功）。
func (a *NeteaseAuth) Logout() error {
	a.mu.Lock()
	a.cookie = ""
	a.savedAt = time.Time{}
	a.mu.Unlock()
	if err := os.Remove(a.cookiePath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// qrTimestamp 是 netease-api 防缓存用的毫秒时间戳参数。
func qrTimestamp() string {
	return strconv.FormatInt(time.Now().UnixMilli(), 10)
}

func (a *NeteaseAuth) getJSON(ctx context.Context, u string, cookie string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("netease-api 返回 %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// CreateQR 建一个登录二维码：返回 key（轮询用）、qrurl（扫码地址）、
// qrimg（二维码图片 data URL，前端可直接展示）。
func (a *NeteaseAuth) CreateQR(ctx context.Context) (key, qrURL, qrImg string, err error) {
	var keyOut struct {
		Code int `json:"code"`
		Data struct {
			Unikey string `json:"unikey"`
		} `json:"data"`
	}
	if err := a.getJSON(ctx, a.baseURL+"/login/qr/key?timestamp="+qrTimestamp(), "", &keyOut); err != nil {
		return "", "", "", err
	}
	if keyOut.Data.Unikey == "" {
		return "", "", "", fmt.Errorf("netease-api /login/qr/key 未返回 unikey")
	}
	key = keyOut.Data.Unikey
	var createOut struct {
		Code int `json:"code"`
		Data struct {
			QRURL string `json:"qrurl"`
			QRImg string `json:"qrimg"`
		} `json:"data"`
	}
	u := a.baseURL + "/login/qr/create?key=" + url.QueryEscape(key) + "&qrimg=true&timestamp=" + qrTimestamp()
	if err := a.getJSON(ctx, u, "", &createOut); err != nil {
		return "", "", "", err
	}
	if createOut.Data.QRURL == "" {
		return "", "", "", fmt.Errorf("netease-api /login/qr/create 未返回 qrurl")
	}
	return key, createOut.Data.QRURL, createOut.Data.QRImg, nil
}

// QRCheckCode 是 /login/qr/check 返回的状态码。
const (
	QRCodeExpired        = 800 // 二维码过期，需重新生成
	QRCodeWaitingScan    = 801 // 等待扫码
	QRCodeWaitingConfirm = 802 // 待确认（手机上点了确认）
	QRCodeSuccess        = 803 // 授权成功，响应带 cookie
)

// CheckQR 轮询一次扫码结果。返回 code 与 message；code==803 时
// cookie 已写盘并记在内存里，调用方再调 Cookie() 交给 NeteaseClient。
func (a *NeteaseAuth) CheckQR(ctx context.Context, key string) (code int, message string, err error) {
	if strings.TrimSpace(key) == "" {
		return 0, "", fmt.Errorf("qr key 为空")
	}
	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Cookie  string `json:"cookie"`
	}
	u := a.baseURL + "/login/qr/check?key=" + url.QueryEscape(key) + "&timestamp=" + qrTimestamp()
	if err := a.getJSON(ctx, u, "", &out); err != nil {
		return 0, "", err
	}
	if out.Code == QRCodeSuccess {
		if strings.TrimSpace(out.Cookie) == "" {
			return out.Code, out.Message, fmt.Errorf("netease-api 扫码成功但未返回 cookie")
		}
		if err := a.save(out.Cookie); err != nil {
			return out.Code, out.Message, fmt.Errorf("保存网易云 cookie 失败: %w", err)
		}
	}
	return out.Code, out.Message, nil
}

// Status 查登录状态：无 cookie 即未登录；有 cookie 时调 /login/status
// 验证（account.id 为 0 或失败都算未登录，需重扫）。
// 返回 loggedIn、昵称、过期时间（RFC3339，算不出时为空）。
func (a *NeteaseAuth) Status(ctx context.Context) (loggedIn bool, nickname, expiresAt string, err error) {
	a.mu.Lock()
	cookie := a.cookie
	savedAt := a.savedAt
	a.mu.Unlock()
	if cookie == "" {
		return false, "", "", nil
	}
	var out struct {
		Data struct {
			Account struct {
				ID int64 `json:"id"`
			} `json:"account"`
			Profile struct {
				Nickname string `json:"nickname"`
			} `json:"profile"`
		} `json:"data"`
	}
	if err := a.getJSON(ctx, a.baseURL+"/login/status?timestamp="+qrTimestamp(), cookie, &out); err != nil {
		return false, "", "", err
	}
	if out.Data.Account.ID == 0 {
		return false, "", "", nil
	}
	return true, out.Data.Profile.Nickname, parseCookieExpiry(cookie, savedAt), nil
}

// parseCookieExpiry 从 cookie 串里算过期时间：优先 Expires=（HTTP 日期），
// 其次 Max-Age=（相对保存时间），都没有返回 ""（未知，按 1–2 个月估算，
// 过期后 Status 会因 /login/status 失败而报需重扫）。
func parseCookieExpiry(cookie string, savedAt time.Time) string {
	for _, part := range strings.Split(cookie, ";") {
		part = strings.TrimSpace(part)
		if v, ok := strings.CutPrefix(part, "Expires="); ok {
			if t, err := http.ParseTime(strings.TrimSpace(v)); err == nil {
				return t.Format(time.RFC3339)
			}
		}
		if v, ok := strings.CutPrefix(part, "Max-Age="); ok {
			if secs, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && !savedAt.IsZero() {
				return savedAt.Add(time.Duration(secs) * time.Second).Format(time.RFC3339)
			}
		}
	}
	return ""
}
