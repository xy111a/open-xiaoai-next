// Package music 是音乐平面：Snapcast 融进 Hub（分组 + 延迟校准），
// 音源唯一指定网易云：Music Assistant + NeteaseCloudMusicApi。
//
// 与语音平面分离：语音走 Opus 16kHz 低延迟；音乐走 Snapcast 48kHz 同步。
// 语音点歌（“播放周杰伦的青花瓷”）3s 内出声；歌词/暂停/下一首可用。
//
// 分组：Snapcast 的 group 机制天然支持一台设备进多个组
// （楼上组 / 楼下组 / 全屋组），验收时测“同一台进两个组”。
// 延迟校准：起始值客厅 0ms / 卧室 +15ms，实测后自动校准，目标延迟差 ≤10ms。
//
// Music Assistant API（已核实，music-assistant.io/api 与实测记录）：
//
//	HTTP POST http://<host>:8095/api，Bearer Token，body {"command": "...", "message_id": N, ...}
//	命令：players/all、player_queues/play_media（queue_id + media[uri] + option）、
//	player_queues/play、player_queues/pause、music/track_by_name。
//	注意：MA 默认端口是 8095；WS 版 auth 是 in-band，不走 Authorization 头。
//	本客户端走 HTTP 版（无状态，好测），queue_id 未分组时等于 player_id。
package music

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ==================== Snapcast JSON-RPC ====================

// SnapClient 是 Snapcast JSON-RPC 客户端（TCP :1705，换行分隔的 JSON-RPC 2.0）。
type SnapClient struct {
	addr string
	mu   sync.Mutex
	conn net.Conn
	seq  int
	r    *bufio.Reader
}

// SnapGroup 是 Snapcast 的一个分组。
type SnapGroup struct {
	ID       string       `json:"id"`
	Name     string       `json:"name"`
	StreamID string       `json:"stream_id"`
	Muted    bool         `json:"muted"`
	Clients  []SnapDevice `json:"clients"`
}

// SnapDevice 是分组里的一台设备。
type SnapDevice struct {
	ID        string `json:"id"`
	Connected bool   `json:"connected"`
	Name      string `json:"config.name"`
	Latency   int    `json:"config.latency"`
}

// DialSnap 连上 Snapcast。
func DialSnap(addr string, timeout time.Duration) (*SnapClient, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, err
	}
	return &SnapClient{addr: addr, conn: conn, r: bufio.NewReader(conn)}, nil
}

// Close 关闭连接。
func (s *SnapClient) Close() error {
	return s.conn.Close()
}

func (s *SnapClient) call(method string, params map[string]any) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	req := map[string]any{"id": s.seq, "jsonrpc": "2.0", "method": method}
	if params != nil {
		req["params"] = params
	}
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	_ = s.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := s.conn.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	_ = s.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := s.r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var resp struct {
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("snapcast: %s", resp.Error.Message)
	}
	return resp.Result, nil
}

// GetGroups 取全部分组（含设备与延迟）。
func (s *SnapClient) GetGroups() ([]SnapGroup, error) {
	raw, err := s.call("Server.GetStatus", nil)
	if err != nil {
		return nil, err
	}
	var status struct {
		Server struct {
			Groups []SnapGroup `json:"groups"`
		} `json:"server"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return nil, err
	}
	return status.Server.Groups, nil
}

// SetClientLatency 设置一台设备的延迟补偿（ms）。
func (s *SnapClient) SetClientLatency(clientID string, latencyMs int) error {
	_, err := s.call("Client.SetLatency", map[string]any{"id": clientID, "latency": latencyMs})
	return err
}

// SetGroupStream 设置分组播放的流。
func (s *SnapClient) SetGroupStream(groupID, streamID string) error {
	_, err := s.call("Group.SetStream", map[string]any{"id": groupID, "stream_id": streamID})
	return err
}

// SetGroupMute 设置分组静音。
func (s *SnapClient) SetGroupMute(groupID string, mute bool) error {
	_, err := s.call("Group.SetMute", map[string]any{"id": groupID, "mute": mute})
	return err
}

// SetClientVolume 设置一台设备的音量（0–100）。
func (s *SnapClient) SetClientVolume(clientID string, volume int) error {
	_, err := s.call("Client.SetVolume", map[string]any{"id": clientID, "volume": volume})
	return err
}

// ==================== 网易云 API ====================

// NeteaseSong 是一首歌。
type NeteaseSong struct {
	ID     int64
	Name   string
	Artist string
}

// NeteaseClient 是 NeteaseCloudMusicApi 的 HTTP 客户端。
type NeteaseClient struct {
	baseURL string
	http    *http.Client
}

// NewNetease 新建客户端。
func NewNetease(baseURL string) *NeteaseClient {
	return &NeteaseClient{baseURL: strings.TrimSuffix(baseURL, "/"), http: &http.Client{Timeout: 15 * time.Second}}
}

// Search 搜歌（type=1 单曲），返回前 limit 首。
func (c *NeteaseClient) Search(ctx context.Context, keywords string, limit int) ([]NeteaseSong, error) {
	u := fmt.Sprintf("%s/search?keywords=%s&type=1&limit=%d", c.baseURL, url.QueryEscape(keywords), limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("netease search 返回 %d", resp.StatusCode)
	}
	var out struct {
		Result struct {
			Songs []struct {
				ID      int64  `json:"id"`
				Name    string `json:"name"`
				Artists []struct {
					Name string `json:"name"`
				} `json:"artists"`
			} `json:"songs"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	songs := make([]NeteaseSong, 0, len(out.Result.Songs))
	for _, s := range out.Result.Songs {
		artist := ""
		if len(s.Artists) > 0 {
			artist = s.Artists[0].Name
		}
		songs = append(songs, NeteaseSong{ID: s.ID, Name: s.Name, Artist: artist})
	}
	return songs, nil
}

// SongURL 取一首歌的可播地址（level=standard；VIP 歌需 VIP 账号 Cookie）。
func (c *NeteaseClient) SongURL(ctx context.Context, id int64) (string, error) {
	u := fmt.Sprintf("%s/song/url/v1?id=%d&level=standard", c.baseURL, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("netease song/url 返回 %d", resp.StatusCode)
	}
	var out struct {
		Data []struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if len(out.Data) == 0 || out.Data[0].URL == "" {
		return "", fmt.Errorf("netease: 歌曲 %d 无可播地址（可能需 VIP）", id)
	}
	return out.Data[0].URL, nil
}

// Lyric 取歌词。
func (c *NeteaseClient) Lyric(ctx context.Context, id int64) (string, error) {
	u := fmt.Sprintf("%s/lyric?id=%d", c.baseURL, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("netease lyric 返回 %d", resp.StatusCode)
	}
	var out struct {
		Lrc struct {
			Lyric string `json:"lyric"`
		} `json:"lrc"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Lrc.Lyric, nil
}

// ==================== Music Assistant HTTP API ====================

// MAClient 是 Music Assistant 的 HTTP 客户端（POST /api，Bearer Token）。
type MAClient struct {
	baseURL string
	token   string
	http    *http.Client
	seq     int
	mu      sync.Mutex
}

// NewMA 新建客户端。token 为空表示无认证（局域网默认）。
func NewMA(baseURL, token string) *MAClient {
	return &MAClient{baseURL: strings.TrimSuffix(baseURL, "/"), token: token, http: &http.Client{Timeout: 15 * time.Second}}
}

func (c *MAClient) command(ctx context.Context, command string, args map[string]any) (json.RawMessage, error) {
	c.mu.Lock()
	c.seq++
	id := c.seq
	c.mu.Unlock()
	body := map[string]any{"command": command, "message_id": id}
	for k, v := range args {
		body[k] = v
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ma: %s 返回 %d: %s", command, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		MessageID int             `json:"message_id"`
		Result    json.RawMessage `json:"result"`
		Error     *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		return nil, fmt.Errorf("ma: %s: %s", out.Error.Code, out.Error.Message)
	}
	return out.Result, nil
}

// Player 是 MA 的一个播放器。
type Player struct {
	PlayerID string `json:"player_id"`
	Name     string `json:"name"`
}

// Players 取全部播放器。
func (c *MAClient) Players(ctx context.Context) ([]Player, error) {
	raw, err := c.command(ctx, "players/all", nil)
	if err != nil {
		return nil, err
	}
	var players []Player
	if err := json.Unmarshal(raw, &players); err != nil {
		return nil, err
	}
	return players, nil
}

// PlayMedia 在指定 queue 上播媒体（media 是 library:// URI 列表）。
// option: replace（替换队列）/ play / next / add。
func (c *MAClient) PlayMedia(ctx context.Context, queueID string, media []string, option string) error {
	_, err := c.command(ctx, "player_queues/play_media", map[string]any{
		"queue_id": queueID, "media": media, "option": option,
	})
	return err
}

// PlayTrackByName 按歌名找歌并播（先 music/track_by_name 拿 URI，再 play_media）。
func (c *MAClient) PlayTrackByName(ctx context.Context, queueID, track, artist string) error {
	raw, err := c.command(ctx, "music/track_by_name", map[string]any{
		"track": track, "artist": artist,
	})
	if err != nil {
		return err
	}
	var item struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return err
	}
	if item.URI == "" {
		return fmt.Errorf("ma: 找不到歌曲 %s %s", track, artist)
	}
	return c.PlayMedia(ctx, queueID, []string{item.URI}, "replace")
}

// QueuePlay / QueuePause / QueueNext 控制队列。
func (c *MAClient) QueuePlay(ctx context.Context, queueID string) error {
	_, err := c.command(ctx, "player_queues/play", map[string]any{"queue_id": queueID})
	return err
}

// QueuePause 暂停队列。
func (c *MAClient) QueuePause(ctx context.Context, queueID string) error {
	_, err := c.command(ctx, "player_queues/pause", map[string]any{"queue_id": queueID})
	return err
}

// QueueNext 下一首。
func (c *MAClient) QueueNext(ctx context.Context, queueID string) error {
	_, err := c.command(ctx, "player_queues/next", map[string]any{"queue_id": queueID})
	return err
}

// ==================== MPD ====================

// MPDClient 是 MPD 的最小 TCP 客户端（纯文本协议，命令以 \n 结尾，OK 结尾）。
// 用途：网易云 URL 直播到 MPD → snapfifo → Snapcast（MA 不可用时的降级路径）。
type MPDClient struct {
	addr string
	conn net.Conn
	r    *bufio.Reader
	mu   sync.Mutex
}

// DialMPD 连上 MPD。
func DialMPD(addr string, timeout time.Duration) (*MPDClient, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, err
	}
	c := &MPDClient{addr: addr, conn: conn, r: bufio.NewReader(conn)}
	// 读欢迎行 "OK MPD ...".
	line, err := c.r.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, err
	}
	if !strings.HasPrefix(line, "OK") {
		conn.Close()
		return nil, fmt.Errorf("mpd: 欢迎行异常: %s", strings.TrimSpace(line))
	}
	return c, nil
}

// Close 关闭连接。
func (c *MPDClient) Close() error {
	return c.conn.Close()
}

func (c *MPDClient) cmd(format string, args ...any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(c.conn, format+"\n", args...); err != nil {
		return err
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return err
		}
		if strings.HasPrefix(line, "OK") {
			return nil
		}
		if strings.HasPrefix(line, "ACK") {
			return fmt.Errorf("mpd: %s", strings.TrimSpace(line))
		}
	}
}

// PlayURL 清空队列、加入 URL 并播放。
func (c *MPDClient) PlayURL(url string) error {
	if err := c.cmd("clear"); err != nil {
		return err
	}
	if err := c.cmd("add %s", url); err != nil {
		return err
	}
	return c.cmd("play")
}

// Pause 暂停（true）/继续（false）。
func (c *MPDClient) Pause(pause bool) error {
	if pause {
		return c.cmd("pause 1")
	}
	return c.cmd("pause 0")
}

// Next 下一首。
func (c *MPDClient) Next() error {
	return c.cmd("next")
}

// Stop 停止。
func (c *MPDClient) Stop() error {
	return c.cmd("stop")
}
