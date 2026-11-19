// Hub 把音箱报给 HA 做 media_player 实体（HA 融合 b）。
//
// 做法说明——为什么走 REST 而不是复用 ha.Client 的 WebSocket：
// HA 的 WebSocket API 没有“设置任意实体状态”的命令（get_states /
// call_service / fire_event 都有，set_state 没有），而 REST API 的
// POST /api/states/<entity_id> 正是为这类外部上报设计的：
// 同一个长期访问令牌（HASS_TOKEN），同一台 HA，就能创建/更新
// 实体状态。实体出现在 HA 的状态表里，可进 UI、自动化与模板；
// 它不属于任何集成配置项（没有设备页），这是 REST 上报的固有限制，
// 想要原生设备页需另写自定义集成（见 docs/HA_MEDIA_PLAYER.md）。
//
// 上报范围（本功能只做状态上报）：state（playing/paused/idle/
// unavailable）、volume_level、media_title、media_artist。
// 不做 HA → Hub 的控制命令（HA 里点播放/暂停/音量现在还不会动
// 音箱）；控制是自然的后续工作，见 docs/HA_MEDIA_PLAYER.md。
//
// 状态来源：MPD 是全屋唯一的播放源（Snapcast 同步播到所有音箱），
// 所以全部音箱共享同一份 MPD 状态（state/音量/当前曲）；每台音箱
// 的实体只在 friendly_name（房间）与 availability（在线/离线）
// 上不同。离线音箱报 unavailable，不报过期的 playing。
//
// 去重：同一实体的 state/音量/曲名都没变时不发请求（Hub 每 15 秒
// 同步一次，不重复打 HA）；变化才推。
package ha

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// DeviceInfo 是上报需要的一台音箱的最小信息（调用方从注册表填）。
type DeviceInfo struct {
	ID     string // 注册表设备 ID，如 lx06-192-168-1-20
	Room   string // 语音认领的房间，未认领为空
	Model  string // lx06 / oh2p
	Online bool
}

// SpeakerPlayback 是一台音箱在某一时刻的播放快照
// （MPD 全屋同播：State/Volume/Title/Artist 对所有音箱相同，
// 只有设备本身的 ID/Room/Online 不同）。
type SpeakerPlayback struct {
	DeviceID string
	Room     string
	Model    string
	Online   bool
	// Volume 是 MPD 音量 0–100；-1 表示未知（不上报 volume_level）。
	Volume int
	// MPDState 是 MPD 的 state：play / pause / stop（空表示未知，按 idle 计）。
	MPDState string
	Title    string
	Artist   string
}

// MediaPlayerState 是一台音箱对应的 HA media_player 实体状态。
type MediaPlayerState struct {
	EntityID     string
	State        string // playing / paused / idle / unavailable
	FriendlyName string
	VolumeLevel  float64 // 0–1；HasVolume=false 时不上报
	HasVolume    bool
	Title        string
	Artist       string
}

// EntityID 把设备 ID 映射成 HA 实体 ID：
// media_player.open_xiaoai_<slug>，slug 是设备 ID 小写后、
// 非 [a-z0-9] 换成下划线（HA 实体 ID 只许小写字母/数字/下划线）。
// 如 lx06-192-168-1-20 → media_player.open_xiaoai_lx06_192_168_1_20。
func EntityID(deviceID string) string {
	return "media_player.open_xiaoai_" + slugify(deviceID)
}

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastUnderscore := true // 开头不留 _
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastUnderscore = false
		} else if !lastUnderscore {
			b.WriteRune('_')
			lastUnderscore = true
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		out = "speaker"
	}
	if len(out) > 64 {
		out = strings.Trim(out[:64], "_")
	}
	return out
}

// FriendlyName 是实体在 HA UI 里显示的名字：有房间用“客厅音箱”，
// 未认领用设备 ID（房间认领后下次同步自动改名）。
func FriendlyName(deviceID, room string) string {
	if strings.TrimSpace(room) != "" {
		return strings.TrimSpace(room) + "音箱"
	}
	return "open-xiaoai " + deviceID
}

// StateFor 把一台音箱的快照映射成 HA media_player 的 state：
// 离线 → unavailable；MPD play → playing；pause → paused；
// 其余（stop/未知）→ idle。
func StateFor(sp SpeakerPlayback) MediaPlayerState {
	st := MediaPlayerState{
		EntityID:     EntityID(sp.DeviceID),
		FriendlyName: FriendlyName(sp.DeviceID, sp.Room),
		Title:        sp.Title,
		Artist:       sp.Artist,
	}
	switch {
	case !sp.Online:
		st.State = "unavailable"
	case sp.MPDState == "play":
		st.State = "playing"
	case sp.MPDState == "pause":
		st.State = "paused"
	default:
		st.State = "idle"
	}
	if sp.Volume >= 0 && sp.Volume <= 100 {
		st.VolumeLevel = float64(sp.Volume) / 100
		st.HasVolume = true
	}
	return st
}

// BuildPlayback 把注册表设备表 + 一份 MPD 全屋状态拼成每台音箱的快照：
// 设备按 ID 排序（确定性：map 遍历顺序随机，排序后上报顺序稳定，
// 日志与测试都可复现）。
func BuildPlayback(devices []DeviceInfo, mpdState string, volume int, title, artist string) []SpeakerPlayback {
	sorted := append([]DeviceInfo(nil), devices...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	out := make([]SpeakerPlayback, 0, len(sorted))
	for _, d := range sorted {
		out = append(out, SpeakerPlayback{
			DeviceID: d.ID, Room: d.Room, Model: d.Model, Online: d.Online,
			Volume: volume, MPDState: mpdState, Title: title, Artist: artist,
		})
	}
	return out
}

// RESTBaseURL 把 HA 的 WebSocket 地址换算成 REST 根地址：
// ws://host:8123/api/websocket → http://host:8123，
// wss://… → https://…。路径一律丢弃（REST 路径由 Reporter 自己拼）。
func RESTBaseURL(wsURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(wsURL))
	if err != nil {
		return "", fmt.Errorf("ha: home_assistant.url 解析失败：%q", wsURL)
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	case "http", "https":
		// 已是 REST 地址：照样接受，只清路径。
	default:
		return "", fmt.Errorf("ha: home_assistant.url 的 scheme 必须是 ws/wss/http/https，实际是 %q", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("ha: home_assistant.url 缺少 host：%q", wsURL)
	}
	u.Path = ""
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

// Reporter 是 HA media_player 状态上报器：走 HA REST API
// POST /api/states/<entity_id>，与 ha.Client 共用同一个长期访问
// 令牌（HASS_TOKEN），但不共用 WebSocket 连接（REST 不依赖 WS
// 是否连通；WS 断线时上报仍可工作）。
type Reporter struct {
	baseURL string // 如 http://192.168.1.10:8123
	token   string
	http    *http.Client

	mu   sync.Mutex
	last map[string]MediaPlayerState // 上次已上报的状态（去重用）
}

// NewReporter 新建 Reporter：baseURL 是 HA REST 根地址
// （如 http://192.168.1.10:8123），token 是长期访问令牌；
// 空 token 直接报错，不发无鉴权请求。
func NewReporter(baseURL, token string) (*Reporter, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("ha: HASS_TOKEN 为空，拒绝无鉴权上报（HA 须配长期访问令牌）")
	}
	baseURL = strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("ha: HA 地址为空，无法上报 media_player")
	}
	if _, err := url.Parse(baseURL); err != nil {
		return nil, fmt.Errorf("ha: HA 地址解析失败：%q", baseURL)
	}
	return &Reporter{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Timeout: 10 * time.Second},
		last:    map[string]MediaPlayerState{},
	}, nil
}

// NewReporterFromWSURL 从 config 里的 home_assistant.url
// （WebSocket 地址）直接建 Reporter：内部先换算成 REST 根地址。
func NewReporterFromWSURL(wsURL, token string) (*Reporter, error) {
	base, err := RESTBaseURL(wsURL)
	if err != nil {
		return nil, err
	}
	return NewReporter(base, token)
}

// Push 上报一台音箱的状态。state/音量/曲名与上次上报完全相同
// 时跳过请求、返回 changed=false；否则 POST 到 HA 并返回
// changed=true。HA 返回非 2xx 时返回错误（且不记为已上报，
// 下次同步会重试）。
func (r *Reporter) Push(ctx context.Context, sp SpeakerPlayback) (changed bool, err error) {
	st := StateFor(sp)
	r.mu.Lock()
	prev, seen := r.last[st.EntityID]
	r.mu.Unlock()
	if seen && prev == st {
		return false, nil
	}
	if err := r.postState(ctx, st, sp.DeviceID); err != nil {
		return false, err
	}
	r.mu.Lock()
	r.last[st.EntityID] = st
	r.mu.Unlock()
	return true, nil
}

// PushAll 上报全部音箱（每台独立去重、独立成败）：
// 全部尝试完后返回 errors.Join 的失败集合；nil 表示全部成功
// （或全部因无变化而跳过）。
func (r *Reporter) PushAll(ctx context.Context, speakers []SpeakerPlayback) error {
	var errs []error
	for _, sp := range speakers {
		if _, err := r.Push(ctx, sp); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", sp.DeviceID, err))
		}
	}
	return errors.Join(errs...)
}

// postState 发一次 POST /api/states/<entity_id>。
func (r *Reporter) postState(ctx context.Context, st MediaPlayerState, deviceID string) error {
	attrs := map[string]any{
		"friendly_name":      st.FriendlyName,
		"supported_features": 0, // 本功能只上报状态，不接控制命令
		"open_xiaoai_device": deviceID,
	}
	if st.HasVolume {
		attrs["volume_level"] = st.VolumeLevel
	}
	if st.Title != "" {
		attrs["media_title"] = st.Title
	}
	if st.Artist != "" {
		attrs["media_artist"] = st.Artist
	}
	body, err := json.Marshal(map[string]any{"state": st.State, "attributes": attrs})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/api/states/"+st.EntityID, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.http.Do(req)
	if err != nil {
		return fmt.Errorf("ha: 上报 %s 失败：%w", st.EntityID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("ha: 上报 %s 被 HA 拒绝（HTTP %d）：%s", st.EntityID, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
