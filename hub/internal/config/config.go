// Package config 是 Hub 的唯一配置文件：config.yaml。
// 一个文件管房间、唤醒词、Agent、音源、HA；改配置不用改代码，Web 面板（Phase 4）
// 就是读写这个文件。房间名由语音认领写入，不用手填。
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config 是 config.yaml 的完整结构。
type Config struct {
	// WakeWord 默认唤醒词，例如 "你好小李"。换词 = 换 KWS 模型文件 + 改这里。
	WakeWord string `yaml:"wake_word"`

	// WakeWordRoutes 多唤醒词路由：唤醒词 → Agent 名。
	// 例：你好小李 → hermes（管家），你好龙虾 → openclaw（通用）。
	WakeWordRoutes map[string]string `yaml:"wake_word_routes"`

	// Devices 已发现的设备（自动发现写入；手工改也行）。
	// Key 是 Hub 分配的设备 ID，IP 由 mDNS/ARP 扫描得到。
	Devices map[string]Device `yaml:"devices"`

	// MusicFolder 音乐文件夹，默认 /volume1/music；启动时自动校验，不存在再问。
	MusicFolder string `yaml:"music_folder"`

	// HomeAssistant 地址，例如 ws://192.168.1.10:8123，Token 走环境变量 HASS_TOKEN。
	HomeAssistant HomeAssistant `yaml:"home_assistant"`

	// Agents 可用的 Agent（OpenAI 兼容 /v1/chat/completions）。
	Agents map[string]Agent `yaml:"agents"`

	// Netease 网易云：Music Assistant 地址 + NeteaseCloudMusicApi 地址。
	Netease Netease `yaml:"netease"`

	// HAEntities 是 HA 实体别名表：{"客厅灯": "light.living_room"}。
	// 语音家电命令只认这里配了的别名；门锁/燃气/热水器不配进来。
	HAEntities map[string]string `yaml:"ha_entities"`

	// Hub 是 Hub 自身的运行参数（监听、鉴权、TLS、下游服务地址）。
	Hub Hub `yaml:"hub"`

	// KWS 是服务端 KWS 边车（kws-sidecar）的连接配置。
	// Enabled=false（默认）时 Hub 不连边车，语音唤醒不可用，/api/status
	// 报告 kws_status=disabled；Enabled=true 时 URL 与 Keywords 必填。
	KWS KWS `yaml:"kws"`
}

// KWS 是服务端 KWS 边车的连接配置（Hub 侧只接线，不跑模型）。
type KWS struct {
	// Enabled 是否启用服务端唤醒（默认 false；显式写 true 才连边车）。
	Enabled bool `yaml:"enabled"`
	// URL 是边车 WS 地址，例如 ws://127.0.0.1:10400/v1/kws（部署默认：
	// Hub 与边车都走 host 网络，见 deploy/docker-compose.yml，用回环地址）。
	URL string `yaml:"url"`
	// Keywords 是接受的唤醒词表，默认应含 WakeWord（如“你好小李”）。
	Keywords []string `yaml:"keywords"`
}

// Device 是一台音箱。
type Device struct {
	Model string `yaml:"model"` // lx06 / oh2p（只支持这两款）
	IP    string `yaml:"ip"`    // 自动发现填入
	Room  string `yaml:"room"`  // 语音认领写入，例如 "客厅"；未认领为空
}

// HomeAssistant 是 HA 连接参数。
type HomeAssistant struct {
	URL string `yaml:"url"`
}

// Agent 是一个 OpenAI 兼容的 Agent/LLM。
type Agent struct {
	BaseURL string `yaml:"base_url"` // 例如 http://hermes:8642/v1
	Model   string `yaml:"model"`    // 例如 deepseek-chat
}

// Netease 是网易云接入参数。
type Netease struct {
	MusicAssistant string `yaml:"music_assistant"` // 例如 http://music-assistant:8099
	API            string `yaml:"api"`             // 例如 http://netease-api:3000
}

// Hub 是 Hub 自身的运行参数。
type Hub struct {
	// Listen 是 Client 接入地址，例如 ":4399"。
	Listen string `yaml:"listen"`
	// TLS 是否启用 wss（生产必须 true；本地测试可设 false 走纯 ws）。
	// 用 *bool：没写时默认 true，只有显式写 false 才关闭。
	TLS *bool `yaml:"tls"`
	// DataDir 是 Hub 数据目录（TLS 证书、提醒持久化等），默认 ./hub-data。
	DataDir string `yaml:"data_dir"`
	// STT 是 Wyoming STT 地址，例如 localhost:10300。
	STT string `yaml:"stt"`
	// TTS 是 Wyoming TTS 地址，例如 localhost:10200。
	TTS string `yaml:"tts"`
	// TTSVoice 是 Piper 音色，例如 zh_CN-huayan-medium。
	TTSVoice string `yaml:"tts_voice"`
	// Snapcast 是 Snapcast JSON-RPC 地址，例如 localhost:1705。
	Snapcast string `yaml:"snapcast"`
	// MPD 是 MPD 地址，例如 localhost:6600（网易云 URL 直播降级路径用，P1-26）。
	MPD string `yaml:"mpd"`
	// SnapcastLatencies 是 Snapcast 延迟补偿（ms，P1-26）：key 为 Snapcast
	// 客户端 ID，未知 ID 时也可按客户端显示名（如“客厅”）配；点歌成功后
	// 按此表调 SetClientLatency。
	SnapcastLatencies map[string]int `yaml:"snapcast_latencies"`
	// SnapcastGroup 是点歌成功后要切流的分组 ID（P1-26）；为空表示不强制切流。
	// 分组 ID 见 Snapcast Server.GetStatus 的返回。
	SnapcastGroup string `yaml:"snapcast_group"`
	// SnapcastStream 是点歌成功后分组要切到的流 ID（P1-26），如 "NAS音乐"。
	SnapcastStream string `yaml:"snapcast_stream"`
	// WebListen 是 Web 面板监听地址，例如 ":8080"（Phase 4）。
	WebListen string `yaml:"web_listen"`
	// ScanCIDR 是自动发现扫描的网段，例如 "192.168.1.0/24"；为空则自动取本机网段。
	ScanCIDR string `yaml:"scan_cidr"`
	// ScanEnabled 是自动发现总开关（P0-4）：默认 false，启动不扫描；
	// 只有显式写 true 才在启动时扫一次。光开开关不够，还须配
	// DiscoveryFingerprints 白名单，否则扫描拒绝发 Token。
	ScanEnabled bool `yaml:"scan_enabled"`
	// DiscoveryFingerprints 是自动发现的设备证书 SHA-256 指纹白名单（P0-4），
	// 小写 hex，冒号可有可无。为空时扫描拒绝发 Token。
	DiscoveryFingerprints []string `yaml:"discovery_fingerprints"`
	// DoNotDisturb 是勿扰时段（FR-10 主动播报跳过该时段），格式 "23:00-07:00"。
	DoNotDisturb string `yaml:"do_not_disturb"`
}

// RoomNames 返回已认领的房间名列表。
func (c *Config) RoomNames() []string {
	var rooms []string
	for _, d := range c.Devices {
		if d.Room != "" {
			rooms = append(rooms, d.Room)
		}
	}
	return rooms
}

// TLSEnabled 返回是否启用 wss。
func (c *Config) TLSEnabled() bool {
	return c.Hub.TLS != nil && *c.Hub.TLS
}

// Validate 检查必填项与取值范围，并收紧下游地址：
//   - 下游 URL 只许 http(s)（HA 另许 ws(s)），且 host 必须是内网地址
//     （私有/回环/链路本地 IP、localhost、*.local 等内网后缀、或无点的
//     单标签主机名如 Docker 服务名）；拒绝公网 IP/域名与 file:// 等 scheme。
//   - hub.stt/tts/snapcast（host:port 形式）同样只许内网 host。
//   - scan_cidr 非空时必须是可解析的内网网段。
//
// 注意：hub.tls=false 本身不由 Validate 拒绝（本地测试允许）；
// “不许经 Web 改为 false”由 web 层在 PUT 时对照旧配置拦截。
func (c *Config) Validate() error {
	if c.WakeWord == "" {
		return fmt.Errorf("wake_word 为空")
	}
	if c.Hub.Listen == "" {
		return fmt.Errorf("hub.listen 为空")
	}
	if err := validateListenAddr("hub.listen", c.Hub.Listen); err != nil {
		return err
	}
	if err := validateListenAddr("hub.web_listen", c.Hub.WebListen); err != nil {
		return err
	}
	for id, d := range c.Devices {
		if d.Model != "lx06" && d.Model != "oh2p" {
			return fmt.Errorf("devices[%s].model 必须是 lx06 或 oh2p，实际是 %q", id, d.Model)
		}
		if d.IP != "" {
			if ip := net.ParseIP(d.IP); ip == nil || !(ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()) {
				return fmt.Errorf("devices[%s].ip 必须是内网 IP，实际是 %q", id, d.IP)
			}
		}
	}
	if c.MusicFolder == "" {
		return fmt.Errorf("music_folder 为空")
	}
	// 下游地址：只许内网。
	if err := validateDownstreamURL("home_assistant.url", c.HomeAssistant.URL, "ws", "wss", "http", "https"); err != nil {
		return err
	}
	for name, a := range c.Agents {
		if err := validateDownstreamURL("agents["+name+"].base_url", a.BaseURL, "http", "https"); err != nil {
			return err
		}
	}
	if err := validateDownstreamURL("netease.music_assistant", c.Netease.MusicAssistant, "http", "https"); err != nil {
		return err
	}
	if err := validateDownstreamURL("netease.api", c.Netease.API, "http", "https"); err != nil {
		return err
	}
	for _, hp := range []struct{ name, addr string }{
		{"hub.stt", c.Hub.STT}, {"hub.tts", c.Hub.TTS}, {"hub.snapcast", c.Hub.Snapcast},
	} {
		if hp.addr == "" {
			continue
		}
		host, _, err := net.SplitHostPort(hp.addr)
		if err != nil {
			return fmt.Errorf("%s 必须是 host:port 形式，实际是 %q", hp.name, hp.addr)
		}
		if !isPrivateHost(host) {
			return fmt.Errorf("%s 的 host 必须是内网地址，实际是 %q", hp.name, hp.addr)
		}
	}
	if c.Hub.ScanCIDR != "" {
		ip, ipnet, err := net.ParseCIDR(c.Hub.ScanCIDR)
		if err != nil {
			return fmt.Errorf("hub.scan_cidr 不是合法 CIDR：%q", c.Hub.ScanCIDR)
		}
		if !(ip.IsPrivate() || ip.IsLoopback()) && !ipnet.IP.IsPrivate() && !ipnet.IP.IsLoopback() {
			return fmt.Errorf("hub.scan_cidr 必须是内网网段，实际是 %q", c.Hub.ScanCIDR)
		}
	}
	// KWS 边车：启用时 URL 与 Keywords 必填，且 URL 只许 ws/wss + 内网 host
	//（与下游地址同一白名单规则）；缺配置或配错地址时启动即报错，
	// 不要静默连错地址或静默关闭唤醒。未启用时 URL/Keywords 不校验（忽略）。
	if c.KWS.Enabled {
		if err := validateKWSURL(c.KWS.URL); err != nil {
			return err
		}
		if len(c.KWS.Keywords) == 0 {
			return fmt.Errorf("kws.keywords 为空（kws.enabled=true 时至少配一个唤醒词，如“你好小李”）")
		}
		for _, kw := range c.KWS.Keywords {
			if strings.TrimSpace(kw) == "" {
				return fmt.Errorf("kws.keywords 里有空唤醒词")
			}
		}
	}
	return nil
}

// validateKWSURL 校验边车 WS 地址：ws/wss scheme、无 userinfo、内网 host、带路径。
func validateKWSURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("kws.url 为空（kws.enabled=true 时必须配置边车地址，如 ws://127.0.0.1:10400/v1/kws）")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("kws.url 不是合法 URL：%q", raw)
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return fmt.Errorf("kws.url 的 scheme 必须是 ws 或 wss，实际是 %q", u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("kws.url 不许带 userinfo（可能泄露凭据）")
	}
	if u.Host == "" || !isPrivateHost(u.Hostname()) {
		return fmt.Errorf("kws.url 的 host 必须是内网地址，实际是 %q", raw)
	}
	if u.Path == "" || u.Path == "/" {
		return fmt.Errorf("kws.url 必须带路径（边车 WS 路径，如 /v1/kws），实际是 %q", raw)
	}
	return nil
}

// validateListenAddr 校验 listen 地址可解析为 host:port（":4399" 这种全接口形式也合法）。
func validateListenAddr(name, addr string) error {
	if addr == "" {
		return nil // 空值由 ApplyDefaults 补齐；Validate 的必填检查在别处。
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return fmt.Errorf("%s 不是合法监听地址：%q", name, addr)
	}
	return nil
}

// validateDownstreamURL 校验下游 URL：scheme 在白名单内、无 userinfo、host 为内网地址。
// 空值跳过（该下游未配置）。
func validateDownstreamURL(name, raw string, schemes ...string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s 不是合法 URL：%q", name, raw)
	}
	okScheme := false
	for _, s := range schemes {
		if u.Scheme == s {
			okScheme = true
			break
		}
	}
	if !okScheme {
		return fmt.Errorf("%s 的 scheme 必须是 %v，实际是 %q", name, schemes, u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("%s 不许带 userinfo（可能泄露凭据）", name)
	}
	if u.Host == "" || !isPrivateHost(u.Hostname()) {
		return fmt.Errorf("%s 的 host 必须是内网地址，实际是 %q", name, raw)
	}
	return nil
}

// IsPrivateHost 供 kws 等包复用同一内网判定：私有/回环/链路本地 IP、
// localhost、内网后缀（.local/.lan/.internal/.home.arpa）、或无点的单标签
// 主机名（Docker 服务名，如 kws-sidecar）。带点的公网域名一律拒绝。
func IsPrivateHost(host string) bool {
	return isPrivateHost(host)
}

// isPrivateHost 判断 host 是否为内网地址：私有/回环/链路本地 IP、localhost、
// 内网后缀（.local/.lan/.internal/.home.arpa）、或无点的单标签主机名
// （Docker 服务名，如 hermes）。带点的公网域名一律拒绝。
func isPrivateHost(host string) bool {
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()
	}
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "localhost" {
		return true
	}
	for _, suffix := range []string{".local", ".lan", ".internal", ".home.arpa"} {
		if strings.HasSuffix(h, suffix) {
			return true
		}
	}
	// 无点的单标签主机名：本机/Docker 内网解析，不可能是公网域名。
	if !strings.Contains(h, ".") {
		return true
	}
	return false
}

// Load 读 config.yaml；缺失的 Hub 参数用默认值补齐。
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	cfg.applyDefaults()
	return &cfg, nil
}

// ApplyDefaults 补齐缺失的 Hub 参数（Web 面板 PUT 后保存前调）。
func (c *Config) ApplyDefaults() {
	c.applyDefaults()
}

func (c *Config) applyDefaults() {
	if c.Hub.Listen == "" {
		c.Hub.Listen = ":4399"
	}
	// TLS 没写时默认开启，只有显式写 false 才关闭。
	if c.Hub.TLS == nil {
		t := true
		c.Hub.TLS = &t
	}
	if c.Hub.DataDir == "" {
		c.Hub.DataDir = "./hub-data"
	}
	if c.Hub.STT == "" {
		c.Hub.STT = "localhost:10300"
	}
	if c.Hub.TTS == "" {
		c.Hub.TTS = "localhost:10200"
	}
	if c.Hub.TTSVoice == "" {
		c.Hub.TTSVoice = "zh_CN-huayan-medium"
	}
	if c.Hub.Snapcast == "" {
		c.Hub.Snapcast = "localhost:1705"
	}
	if c.Hub.MPD == "" {
		c.Hub.MPD = "localhost:6600"
	}
	if c.Hub.SnapcastStream == "" {
		c.Hub.SnapcastStream = "NAS音乐"
	}
	if c.Hub.WebListen == "" {
		// P0-1：Web 面板默认只绑回环地址，远程管理走 SSH 隧道。
		c.Hub.WebListen = "127.0.0.1:8080"
	}
	if c.HAEntities == nil {
		c.HAEntities = map[string]string{}
	}
	if c.Devices == nil {
		c.Devices = map[string]Device{}
	}
	if c.WakeWordRoutes == nil {
		c.WakeWordRoutes = map[string]string{}
	}
}

// Save 把当前配置写回文件（房间认领、Web 面板改配置时用）。
// 先写临时文件再改名，避免写一半断电损坏原文件。
func (c *Config) Save(path string) error {
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Token 从环境变量 OPEN_XIAOAI_TOKEN 读 Client 鉴权 Token。
func Token() string {
	return os.Getenv("OPEN_XIAOAI_TOKEN")
}

// HASSToken 从环境变量 HASS_TOKEN 读 HA 长期访问令牌。
func HASSToken() string {
	return os.Getenv("HASS_TOKEN")
}

// LLMAPIKey 从环境变量 LLM_API_KEY 读 LLM 密钥。
func LLMAPIKey() string {
	return os.Getenv("LLM_API_KEY")
}
