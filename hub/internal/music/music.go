// Package music 是音乐平面：Snapcast 融进 Hub（分组 + 延迟校准），
// 音源唯一指定网易云：Hub 直连 NeteaseCloudMusicApi，播到 MPD →
// snapfifo → Snapcast（融合 B，Music Assistant 已移除）。
//
// 与语音平面分离：语音走 Opus 16kHz 低延迟；音乐走 Snapcast 48kHz 同步。
// 语音点歌（“播放周杰伦的青花瓷”）3s 内出声；歌词/暂停/下一首可用。
//
// 分组：Snapcast 的 group 机制天然支持一台设备进多个组
// （楼上组 / 楼下组 / 全屋组），验收时测“同一台进两个组”。
// 延迟校准：起始值客厅 0ms / 卧室 +15ms，实测后自动校准，目标延迟差 ≤10ms。
//
// 网易云登录：扫码登录走 netease-api 的 /login/qr/*（见 netease_auth.go），
// cookie 只存在 Hub 数据卷，不进仓库/配置/日志；VIP 歌取地址时带上它。
package music

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
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
	// Host 是 Snapcast 上报的客户端主机信息（follow-me 用它把
	// Snapcast 客户端映射回 Hub 设备：Host.IP 与注册表的设备 IP 对上
	// 即同一台音箱；config.name/config.latency 两个扁平 tag 是历史
	// 写法——真服务端发的是嵌套 config 对象，它们在生产上恒为空，
	// 这里不动它们，只新增 Host）。
	Host SnapHost `json:"host"`
}

// SnapHost 是 Snapcast 客户端的主机信息（Server.GetStatus 原样返回）。
type SnapHost struct {
	Name string `json:"name"` // 客户端主机名
	IP   string `json:"ip"`   // 客户端 IP（与 Hub 注册表设备 IP 对照用）
	MAC  string `json:"mac"`
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

// maxSnapLineBytes 是 Snapcast 单行 JSON-RPC 的上限（N-P1-1）：
// 服务端异步通知与正常响应都是一行 JSON；无界行可打满内存。
const maxSnapLineBytes = 64 << 10

// maxSnapNotifications 是单次 call() 最多跳过的异步通知行数：
// 超过即报错，避免异常服务端用通知流把调用拖死。
const maxSnapNotifications = 10

// maxUpstreamBody 是网易云上游响应体的读取上限（P1-6/P2-7）：
// 超过即截断，错误信息里只拼截断后的摘要，永不全文回显。
const maxUpstreamBody = 1 << 20

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
	// N-P1-1：循环读行直到读到 id 与本次 seq 匹配的响应；
	// Snapcast 服务端会主动推送无 id 的 JSON-RPC 通知（连接/断开、音量变化），
	// 旧代码只读一行就把通知当成“ID 0、无错误、空结果”的正常响应返回，
	// 真响应留在缓冲里被下一次 call() 读走，两次调用结果错配。
	// 每轮都续期读 deadline，避免通知流把调用拖过超时。
	for skipped := 0; ; skipped++ {
		_ = s.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		line, err := readLimitedLine(s.r, maxSnapLineBytes)
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
		if resp.ID != s.seq {
			// 异步通知行（或上一轮残留的错配响应）：丢弃，不报错不断开。
			if skipped >= maxSnapNotifications {
				return nil, fmt.Errorf("snapcast: 跳过 %d 条通知仍未等到 id=%d 的响应", skipped, s.seq)
			}
			continue
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("snapcast: %s", resp.Error.Message)
		}
		return resp.Result, nil
	}
}

// readLimitedLine 限长读一行（含换行符）：超过上限即报错，
// 不让异常服务端用无界长行打满内存。Snapcast 与 MPD 共用。
func readLimitedLine(r *bufio.Reader, maxBytes int) ([]byte, error) {
	var line []byte
	for {
		frag, err := r.ReadSlice('\n')
		line = append(line, frag...)
		if len(line) > maxBytes {
			return nil, fmt.Errorf("响应行超过 %d 字节上限", maxBytes)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return nil, err
		}
		return line, nil
	}
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

// SetGroupClients 设置一个分组的客户端列表（follow-me 用它搬音箱）：
// 列表里新出现的客户端会被服务端从原分组移过来，列表里消失的客户端
// 由服务端另行安置（新分组/销毁旧分组，见 snapweb 的同款用法）；
// 搬完后调用方用 GetGroups 再核对一次，不要假设一次调用即生效。
func (s *SnapClient) SetGroupClients(groupID string, clientIDs []string) error {
	if clientIDs == nil {
		clientIDs = []string{}
	}
	_, err := s.call("Group.SetClients", map[string]any{"id": groupID, "clients": clientIDs})
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
	// cookie 是扫码登录后的网易云 Cookie（netease_auth.go 管存取）：
	// VIP 歌取地址时带上；为空表示未登录，只能放免费歌。
	cookieMu sync.RWMutex
	cookie   string
}

// NewNetease 新建客户端。
func NewNetease(baseURL string) *NeteaseClient {
	return &NeteaseClient{baseURL: strings.TrimSuffix(baseURL, "/"), http: &http.Client{Timeout: 15 * time.Second}}
}

// SetCookie 设置登录 Cookie（空字符串表示清除，登出时用）。
func (c *NeteaseClient) SetCookie(cookie string) {
	c.cookieMu.Lock()
	defer c.cookieMu.Unlock()
	c.cookie = cookie
}

// get 发一个带登录 Cookie（有的话）的 GET 请求。
func (c *NeteaseClient) get(ctx context.Context, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	c.cookieMu.RLock()
	cookie := c.cookie
	c.cookieMu.RUnlock()
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	return c.http.Do(req)
}

// Search 搜歌（type=1 单曲），返回前 limit 首。
func (c *NeteaseClient) Search(ctx context.Context, keywords string, limit int) ([]NeteaseSong, error) {
	u := fmt.Sprintf("%s/search?keywords=%s&type=1&limit=%d", c.baseURL, url.QueryEscape(keywords), limit)
	resp, err := c.get(ctx, u)
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
	if err := cappedBodyDecoder(resp).Decode(&out); err != nil {
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

// SongURL 取一首歌的可播地址（br=320000；VIP 歌需 VIP 账号 Cookie）。
// 2026-11-23 彩排 P0-3：/song/url/v1 带 Cookie 请求触发 -460 风控，
// 改用老端点 /song/url（老系统一直在用的路径；NAS 实测返回直链）。
func (c *NeteaseClient) SongURL(ctx context.Context, id int64) (string, error) {
	u := fmt.Sprintf("%s/song/url?id=%d&br=320000", c.baseURL, id)
	resp, err := c.get(ctx, u)
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
	if err := cappedBodyDecoder(resp).Decode(&out); err != nil {
		return "", err
	}
	if len(out.Data) == 0 || out.Data[0].URL == "" {
		return "", fmt.Errorf("netease: 歌曲 %d 无可播地址（可能需 VIP）", id)
	}
	// H-3：上游返回的 URL 先验白名单再交出去；非法 URL 按“无可播地址”
	// 处理，调用方（router）即走本地兜底，不把非法地址交给 MPD。
	if err := validatePlayableURL(out.Data[0].URL); err != nil {
		return "", fmt.Errorf("netease: 歌曲 %d 无可播地址（URL 非法）: %w", id, err)
	}
	return out.Data[0].URL, nil
}

// validatePlayableURL 校验可播地址（H-3，SongURL 与 PlayURL 共用）：
// 只许 http(s)，拒绝 file:// 等 scheme、localhost/单标签主机名、
// 以及任何裸 IP（上游是域名 CDN，不用裸 IP；内网 IP 与公网裸 IP
// 一律拒绝，堵 MPD 代为请求内网地址的 SSRF 面）。
// 注意：这里只做 scheme/host 形状校验，不做 DNS 解析——域名解析到
// 内网 IP 的 DNS 重绑定不在本项范围。
func validatePlayableURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("mpd: URL 为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("mpd: URL 解析失败")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("mpd: URL 只许 http/https，实际是 %q", u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("mpd: URL 不许带 userinfo")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return fmt.Errorf("mpd: URL 缺少 host")
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return fmt.Errorf("mpd: URL host 不许是 localhost")
	}
	if ip := net.ParseIP(host); ip != nil {
		return fmt.Errorf("mpd: URL host 不许是裸 IP")
	}
	if !strings.Contains(host, ".") {
		return fmt.Errorf("mpd: URL host 不许是单标签主机名")
	}
	return nil
}

// cappedBodyDecoder 限长解 JSON（网易云上游响应体上限，P2-7 顺带）。
func cappedBodyDecoder(resp *http.Response) *json.Decoder {
	return json.NewDecoder(io.LimitReader(resp.Body, maxUpstreamBody))
}

// Lyric 取歌词。
func (c *NeteaseClient) Lyric(ctx context.Context, id int64) (string, error) {
	u := fmt.Sprintf("%s/lyric?id=%d", c.baseURL, id)
	resp, err := c.get(ctx, u)
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
	if err := cappedBodyDecoder(resp).Decode(&out); err != nil {
		return "", err
	}
	return out.Lrc.Lyric, nil
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
	// N-P1-2：读欢迎行 "OK MPD ..." 之前先设读 deadline——timeout 参数
	// 只管 TCP 建连，不管欢迎行；MPD 进程存活但卡住/不发问候时旧代码
	// 在 ReadString 上永久阻塞，Hub 停在启动中途且无任何日志。
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	line, err := c.r.ReadString('\n')
	_ = conn.SetReadDeadline(time.Time{})
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
	_, err := c.query(format, args...)
	return err
}

// maxMPDLineBytes 是 MPD 单行响应的上限（H-2，与 Snapcast 侧同款 64KB）。
const maxMPDLineBytes = 64 << 10

// maxMPDResponseLines / maxMPDResponseBytes 是单次 query() 响应的总上限
// （H-2）：异常/卡住的 MPD 用行流打满内存的缺口。正常响应（status、
// currentsong、playlistinfo 几百首队列）远小于此；超限即报错并断开
// 连接，看门狗会重拨 MPD。
const (
	maxMPDResponseLines = 10000
	maxMPDResponseBytes = 4 << 20
)

// query 发一条 MPD 命令，返回 OK 之前的所有响应行（ACK 即报错）。
// cmd() 是它的丢弃响应版；status/currentsong/playlistinfo/search 用它取值。
func (c *MPDClient) query(format string, args ...any) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(c.conn, format+"\n", args...); err != nil {
		return nil, err
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var lines []string
	totalBytes := 0
	for {
		raw, err := readLimitedLine(c.r, maxMPDLineBytes)
		if err != nil {
			// 读错即断开：超长行可能只读了一半进缓冲，下次 query
			// 会把残行当新响应误解析；看门狗会重拨。
			_ = c.conn.Close()
			return nil, err
		}
		totalBytes += len(raw)
		if totalBytes > maxMPDResponseBytes {
			_ = c.conn.Close()
			return nil, fmt.Errorf("mpd: 响应超过 %d 字节上限", maxMPDResponseBytes)
		}
		line := strings.TrimRight(string(raw), "\r\n")
		if strings.HasPrefix(line, "OK") {
			return lines, nil
		}
		if strings.HasPrefix(line, "ACK") {
			return nil, fmt.Errorf("mpd: %s", line)
		}
		lines = append(lines, line)
		if len(lines) > maxMPDResponseLines {
			_ = c.conn.Close()
			return nil, fmt.Errorf("mpd: 响应超过 %d 行上限", maxMPDResponseLines)
		}
	}
}

// PlayURL 清空队列、加入 URL 并播放。
// P1-7：MPD 是行协议，URL 里的 \r/\n/控制字符会换行注入任意命令
// （如 "x\nclear\n"），先校验再发。
func (c *MPDClient) PlayURL(url string) error {
	if err := validateMPDURL(url); err != nil {
		return err
	}
	if err := c.cmd("clear"); err != nil {
		return err
	}
	if err := c.cmd("add %s", quoteMPDArg(url)); err != nil {
		return err
	}
	return c.cmd("play")
}

// validateMPDURL 拒绝含换行/控制字符的 URL（MPD 换行注入防护，P1-7），
// 再过 H-3 的可播地址白名单（只许 http(s)，拒绝 file://、内网 IP、localhost）。
func validateMPDURL(url string) error {
	if url == "" {
		return fmt.Errorf("mpd: URL 为空")
	}
	if err := validateMPDArg(url); err != nil {
		return err
	}
	return validatePlayableURL(url)
}

// quoteMPDArg 把 MPD 命令参数包进双引号，里面的反斜杠与双引号转义：
// MPD 按空白切分未加引号的参数，含空格的路径会被拒收
// （add 命令报 wrong number of arguments，2026-11-23 彩排复测实测）。
// 必须在 validateMPDArg 之后调用：控制字符先被拒掉，引号化本身
// 不改变注入面——参数里的 " 与 \ 都已转义，无法提前闭合引号。
func quoteMPDArg(s string) string {
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
	return `"` + esc + `"`
}

// validateMPDArg 拒绝含换行/控制字符的 MPD 参数（URL 或本地文件路径共用）：
// MPD 是行协议，参数里的换行会注入任意命令。
func validateMPDArg(s string) error {
	for _, r := range s {
		if r == '\r' || r == '\n' || r < 0x20 || r == 0x7f {
			return fmt.Errorf("mpd: 参数含非法控制字符 %q", r)
		}
	}
	return nil
}

// Ping 探活（P1-27 看门狗用）：MPD 收到 "ping" 回 OK。
func (c *MPDClient) Ping() error {
	return c.cmd("ping")
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

// Play 从当前曲目开始播放（暂停后继续也可用它；明确“继续”语义时用 Resume）。
func (c *MPDClient) Play() error {
	return c.cmd("play")
}

// Resume 继续播放（pause 0；暂停后恢复用它，不是从头播）。
func (c *MPDClient) Resume() error {
	return c.Pause(false)
}

// Previous 上一首。
func (c *MPDClient) Previous() error {
	return c.cmd("previous")
}

// MPDStatus 是 MPD 的播放状态（status 命令）。
type MPDStatus struct {
	State    string  `json:"state"`    // play / pause / stop
	Song     int     `json:"song"`     // 当前曲在队列里的位置，无曲时 -1
	SongID   int     `json:"songid"`   // 当前曲 ID，无曲时 -1
	Elapsed  float64 `json:"elapsed"`  // 已播秒数
	Duration float64 `json:"duration"` // 总时长秒数
	Volume   int     `json:"volume"`   // 0–100，未知时 -1
	Repeat   bool    `json:"repeat"`
	Random   bool    `json:"random"`
}

// MPDSong 是队列里的一首歌（currentsong / playlistinfo 的一项）。
type MPDSong struct {
	ID     int    `json:"id"`
	Pos    int    `json:"pos"`
	File   string `json:"file"`
	Title  string `json:"title"`
	Artist string `json:"artist"`
	Name   string `json:"name"`
}

// Status 取播放状态。
func (c *MPDClient) Status() (MPDStatus, error) {
	lines, err := c.query("status")
	if err != nil {
		return MPDStatus{}, err
	}
	st := MPDStatus{Song: -1, SongID: -1, Volume: -1}
	kv := parseMPDKV(lines)
	st.State = kv["state"]
	if v, ok := kv["song"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			st.Song = n
		}
	}
	if v, ok := kv["songid"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			st.SongID = n
		}
	}
	if v, ok := kv["elapsed"]; ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			st.Elapsed = f
		}
	}
	if v, ok := kv["duration"]; ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			st.Duration = f
		}
	}
	if v, ok := kv["volume"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			st.Volume = n
		}
	}
	st.Repeat = kv["repeat"] == "1"
	st.Random = kv["random"] == "1"
	return st, nil
}

// CurrentSong 取当前曲目（没在放时 File 为空）。
func (c *MPDClient) CurrentSong() (MPDSong, error) {
	lines, err := c.query("currentsong")
	if err != nil {
		return MPDSong{}, err
	}
	songs := parseMPDSongs(lines)
	if len(songs) == 0 {
		return MPDSong{}, nil
	}
	return songs[0], nil
}

// Queue 取当前队列（playlistinfo）。
func (c *MPDClient) Queue() ([]MPDSong, error) {
	lines, err := c.query("playlistinfo")
	if err != nil {
		return nil, err
	}
	return parseMPDSongs(lines), nil
}

// SearchFile 按文件名在 MPD 本地曲库里找歌（子串匹配），返回文件路径列表
// （相对 MPD music 目录）。直连 URL 失败时的本地兜底用它。
func (c *MPDClient) SearchFile(query string) ([]string, error) {
	if err := validateMPDArg(query); err != nil {
		return nil, err
	}
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("mpd: 搜索词为空")
	}
	// MPD search 参数用双引号包起来，里面的反斜杠与双引号要转义。
	lines, err := c.query(`search filename %s`, quoteMPDArg(query))
	if err != nil {
		return nil, err
	}
	var files []string
	for _, l := range lines {
		if v, ok := strings.CutPrefix(l, "file: "); ok {
			files = append(files, v)
		}
	}
	return files, nil
}

// PlayFile 清空队列、加入本地曲库文件并播放（直连失败时的本地兜底）。
func (c *MPDClient) PlayFile(file string) error {
	if file == "" {
		return fmt.Errorf("mpd: 文件路径为空")
	}
	if err := validateMPDArg(file); err != nil {
		return err
	}
	if err := c.cmd("clear"); err != nil {
		return err
	}
	if err := c.cmd("add %s", quoteMPDArg(file)); err != nil {
		return err
	}
	return c.cmd("play")
}

// parseMPDKV 把 "key: value" 行解析成 map（单块响应，如 status）。
func parseMPDKV(lines []string) map[string]string {
	kv := make(map[string]string, len(lines))
	for _, l := range lines {
		if k, v, ok := strings.Cut(l, ": "); ok {
			kv[k] = v
		}
	}
	return kv
}

// parseMPDSongs 把 currentsong / playlistinfo 的响应解析成歌列表：
// 每首歌以 "file: ..." 开头，后面跟 Title/Artist/Name/Id/Pos 等行。
func parseMPDSongs(lines []string) []MPDSong {
	var songs []MPDSong
	var cur *MPDSong
	flush := func() {
		if cur != nil {
			songs = append(songs, *cur)
			cur = nil
		}
	}
	for _, l := range lines {
		k, v, ok := strings.Cut(l, ": ")
		if !ok {
			continue
		}
		switch k {
		case "file":
			flush()
			cur = &MPDSong{File: v}
		case "Title":
			if cur != nil {
				cur.Title = v
			}
		case "Artist":
			if cur != nil {
				cur.Artist = v
			}
		case "Name":
			if cur != nil {
				cur.Name = v
			}
		case "Id":
			if cur != nil {
				if n, err := strconv.Atoi(v); err == nil {
					cur.ID = n
				}
			}
		case "Pos":
			if cur != nil {
				if n, err := strconv.Atoi(v); err == nil {
					cur.Pos = n
				}
			}
		}
	}
	flush()
	return songs
}
