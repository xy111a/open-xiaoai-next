// Package router 是文本意图路由：识别文本到了之后，决定走哪条路。
//
// 顺序（先具体后通用）：
//  1. 紧急呼叫（FR-15）：“救命”“打 120”“叫救护车” → 立即通知 + 进入待确认状态：
//     可语音“取消呼叫”撤销；60s 无人取消则自动清理（待确认窗口）；
//     同一设备重复触发不重复 Notify（不同设备各自独立）。
//  2. 对讲（P1-15）：“呼叫卧室”“全屋广播” → 触发 intercom 会话；
//     房间认领（P1-39）：“我在客厅” → 记房间；“我在这里” → 确认当前房间。
//  3. 提醒（FR-14）：“每天 8 点提醒我吃药”“每 30 分钟提醒我喝水”“2 小时后提醒我关灯”“取消吃药提醒”
//  4. 场景/记忆（FR-09）：“记住空调 26 度”“我回来了”（跑 HA 场景）；“你还记得什么”读记忆
//  5. 音乐（FR-05）：“播放周杰伦的青花瓷” → 网易云搜歌 → SongURL 取地址 →
//     MPD 直播（融合 B，MA 已移除）；直连失败 → MPD 本地曲库同名歌 → 明说放不了。
//     “暂停/继续/下一首/上一首/歌词”走 MPD 队列 + Hub 内存里的当前曲。
//  6. 家电（FR-06）：“关客厅灯” → 查 HA 实体别名表 → 先查状态再调服务；“灯开着吗”只查不调
//  7. 默认：Agent 对话（按唤醒词路由多 Agent，见 P1-38）
//
// 家电别名表在 config.yaml 的 ha_entities 里配，例如：
//
//	ha_entities: {"客厅灯": "light.living_room", "卧室空调": "climate.bedroom"}
//
// 高风险设备（门锁/燃气/热水器）不在别名表里出现时，Router 拒绝执行并明说。
package router

import (
	"context"
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/huajun/open-xiaoai-next/hub/internal/agent"
	"github.com/huajun/open-xiaoai-next/hub/internal/ha"
	"github.com/huajun/open-xiaoai-next/hub/internal/music"
	"github.com/huajun/open-xiaoai-next/hub/internal/registry"
	"github.com/huajun/open-xiaoai-next/hub/internal/reminders"
)

// Deps 是 Router 依赖的下游（都可为 nil；为 nil 的路会被跳过并在回复里说明）。
type Deps struct {
	HA      *ha.Client
	Netease *music.NeteaseClient
	MPD     *music.MPDClient
	Agent   *agent.Client
	// MusicFolder 是本地曲库目录（config.music_folder）：直连 URL 失败时
	// 的本地兜底只在它存在且为目录时才尝试；不存在时点歌明说“本地曲库还没配好”。
	MusicFolder string
	Reminders   *reminders.Store
	Memory      *Memory
	// Snap 是 Snapcast 控制客户端（P1-26）：点歌成功后按配置调延迟/切流。
	// 为 nil 表示 Snapcast 未接通，点歌本身不受影响。
	Snap *music.SnapClient
	// SnapLatencies 是 Snapcast 延迟补偿（ms）：key 为客户端 ID 或显示名。
	SnapLatencies map[string]int
	// SnapGroupID / SnapStreamID：点歌成功后把该分组切到该流；GroupID 为空
	// 表示不强制切流。
	SnapGroupID  string
	SnapStreamID string
	// RoomOfDevice 查设备所在房间（认领后才有）。
	RoomOfDevice func(deviceID string) string
	// Notify 紧急呼叫的通知动作（默认走 HA fire_event + persistent_notification）。
	Notify func(ctx context.Context, text string) error
	// Intercom 是对讲管理器（P1-15）：语音“呼叫卧室”由此触发；
	// 为 nil 时对讲分支不生效（命令落到 Agent 闲聊）。
	Intercom IntercomStarter
	// DeviceByRoom 按房间名找在线设备 ID（P1-15 呼叫目标解析用）。
	DeviceByRoom func(room string) (deviceID string, ok bool)
	// RoomClaim 是语音房间认领回调（P1-39）：“我在客厅”由此写注册表；
	// 为 nil 时认领分支不生效。
	RoomClaim func(deviceID, room string)
	// Agents 是多 Agent 表（P1-38）：唤醒词 → Agent 名 → 客户端；
	// Agent 字段保留作未配表时的单大脑回退。
	Agents map[string]*agent.Client
	// WakeWordRoutes 是多唤醒词路由表（与 config.WakeWordRoutes 同源）。
	WakeWordRoutes map[string]string
}

// IntercomStarter 是 Router 依赖的对讲触发接口（P1-15）：
// 只要 Start/StartBroadcast/End 三个触发动作，不依赖 Manager 全量。
type IntercomStarter interface {
	Start(from, to string) bool
	StartBroadcast(from string) bool
	End(from string) bool
}

// Router 是意图路由器。
type Router struct {
	deps     Deps
	entities map[string]string // 别名 → HA entity_id

	// depsMu 保护运行时可替换的 deps 字段（MPD/Snap）：main.go 的
	// 后台重试在下游晚到时调 SetMPD/SetSnap 补接，music() 读时持读锁。
	depsMu sync.RWMutex

	// 紧急呼叫状态机（FR-15）：deviceID → 进行中的呼叫。
	emergencyMu    sync.Mutex
	emergencyCalls map[string]emergencyCall
	// EmergencyTimeout 是待确认窗口：触发后这么久无人取消，状态自动清理。
	// 默认 60s（New 里设）；0 表示用默认值。60s 是“误触可撤销”的窗口，
	// 不是拨出时限——PLAN 要求触发后 10s 内拨出/推送送达，那是 Notify 下游的事。
	EmergencyTimeout time.Duration

	// currentMu 保护内存里的当前曲（融合 B：播放状态归 Hub 管——MPD 队列
	// 是队列真相，current 是最近一次点歌成功的曲名/歌手，供歌词与
	// Web /api/music/status 用；不另起数据库，重启后清空）。
	currentMu sync.Mutex
	current   *currentTrack

	// wakeWords 是设备最近一次唤醒词（P1-38：多 Agent 路由用）。
	wakeMu    sync.Mutex
	wakeWords map[string]string
}

// currentTrack 是 Hub 内存里的当前曲（最近一次点歌成功的结果）。
type currentTrack struct {
	song   music.NeteaseSong
	source string // "netease" 直连 / "local" 本地曲库
}

// SetWakeWord 记录某设备最近一次唤醒词（P1-38：OnWake 调）。
func (r *Router) SetWakeWord(deviceID, word string) {
	r.wakeMu.Lock()
	defer r.wakeMu.Unlock()
	if r.wakeWords == nil {
		r.wakeWords = map[string]string{}
	}
	r.wakeWords[deviceID] = word
}

func (r *Router) wakeWordOf(deviceID string) string {
	r.wakeMu.Lock()
	defer r.wakeMu.Unlock()
	return r.wakeWords[deviceID]
}

// ha 取当前 HA 客户端（P1-27：后台看门狗可热换，读时持锁）。
func (r *Router) ha() *ha.Client {
	r.depsMu.RLock()
	defer r.depsMu.RUnlock()
	return r.deps.HA
}

// HA 返回当前 HA 客户端（P1-27：看门狗热换后，main.go 的 Notify
// 等调用方经此读最新值，不再抓启动时那个旧客户端）。
func (r *Router) HA() *ha.Client {
	return r.ha()
}

// SetHA 在 HA 断线重连后热换客户端（P1-27）。
func (r *Router) SetHA(c *ha.Client) {
	r.depsMu.Lock()
	defer r.depsMu.Unlock()
	r.deps.HA = c
}

// agentFor 按唤醒词路由选 Agent（P1-38）：先看设备最近一次唤醒词
// → WakeWordRoutes → Agents 表；未命中则按 Agent 名排序取第一个
// （确定性回退，旧代码 Go map 随机迭代 break 是假路由）；
// 表为空时回退到 Deps.Agent 单大脑。
func (r *Router) agentFor(deviceID string) *agent.Client {
	r.depsMu.RLock()
	agents := r.deps.Agents
	routes := r.deps.WakeWordRoutes
	single := r.deps.Agent
	r.depsMu.RUnlock()
	if len(agents) > 0 {
		if word := r.wakeWordOf(deviceID); word != "" {
			if name, ok := routes[word]; ok {
				if c, ok := agents[name]; ok && c != nil {
					return c
				}
			}
		}
		names := make([]string, 0, len(agents))
		for name := range agents {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if c := agents[name]; c != nil {
				return c
			}
		}
	}
	return single
}

// emergencyCall 是一次进行中的紧急呼叫。
type emergencyCall struct {
	text     string    // 触发原文（“救命”/“打 120”……）
	at       time.Time // 触发时间
	notified bool      // P1-18：通知是否真的送达过（失败时不记为已通知，可再试）
	attempts int       // 本次已尝试的 Notify 次数
}

// New 新建 Router。entities 是 HA 实体别名表。
func New(deps Deps, entities map[string]string) *Router {
	if entities == nil {
		entities = map[string]string{}
	}
	return &Router{
		deps:             deps,
		entities:         entities,
		emergencyCalls:   map[string]emergencyCall{},
		EmergencyTimeout: 60 * time.Second,
	}
}

// SetMPD 在 MPD 晚到（后台重试连上）时补接 MPD（P1-26）。
func (r *Router) SetMPD(c *music.MPDClient) {
	r.depsMu.Lock()
	defer r.depsMu.Unlock()
	r.deps.MPD = c
}

// SetSnap 在 Snapcast 晚到（后台重试连上）时补接 Snapcast（P1-26）。
func (r *Router) SetSnap(c *music.SnapClient) {
	r.depsMu.Lock()
	defer r.depsMu.Unlock()
	r.deps.Snap = c
}

// applySnapcast 在点歌成功后按配置调 Snapcast（P1-26）：先按
// SnapLatencies 表调各客户端延迟（key 先按客户端 ID 再按显示名匹配），
// 再把 SnapGroupID 分组切到 SnapStreamID 流。Snap 未接通或配置为空时
// 什么都不做；失败只记日志，不让已播出的歌再报错。
func (r *Router) applySnapcast() {
	r.depsMu.RLock()
	snap := r.deps.Snap
	latencies := r.deps.SnapLatencies
	groupID := r.deps.SnapGroupID
	streamID := r.deps.SnapStreamID
	r.depsMu.RUnlock()
	if snap == nil {
		return
	}
	if len(latencies) > 0 {
		groups, err := snap.GetGroups()
		if err != nil {
			log.Printf("snapcast: 点歌后取分组失败，延迟未校准: %v", err)
		} else {
			for _, g := range groups {
				for _, cl := range g.Clients {
					ms, ok := latencies[cl.ID]
					if !ok {
						ms, ok = latencies[cl.Name]
					}
					if !ok {
						continue
					}
					if err := snap.SetClientLatency(cl.ID, ms); err != nil {
						log.Printf("snapcast: 设 %s 延迟 %dms 失败: %v", cl.ID, ms, err)
					}
				}
			}
		}
	}
	if groupID != "" && streamID != "" {
		if err := snap.SetGroupStream(groupID, streamID); err != nil {
			log.Printf("snapcast: 分组 %s 切流 %s 失败: %v", groupID, streamID, err)
		}
	}
}

// emergencyTimeout 返回生效的超时（0 时用默认 60s）。
func (r *Router) emergencyTimeout() time.Duration {
	if r.EmergencyTimeout <= 0 {
		return 60 * time.Second
	}
	return r.EmergencyTimeout
}

// SweepEmergency 清理已过待确认窗口的紧急呼叫，返回清理掉的设备数。
// Route 每次进紧急分支时也会惰性调一次；main.go 的定时扫荡也可以调它。
func (r *Router) SweepEmergency(now time.Time) int {
	r.emergencyMu.Lock()
	defer r.emergencyMu.Unlock()
	timeout := r.emergencyTimeout()
	n := 0
	for id, c := range r.emergencyCalls {
		if now.Sub(c.at) > timeout {
			delete(r.emergencyCalls, id)
			n++
		}
	}
	return n
}

// EmergencyActive 查某设备是否有进行中的紧急呼叫（过期算无）。
func (r *Router) EmergencyActive(deviceID string) bool {
	r.SweepEmergency(time.Now())
	r.emergencyMu.Lock()
	defer r.emergencyMu.Unlock()
	_, ok := r.emergencyCalls[deviceID]
	return ok
}

// Route 路由一条识别文本，返回要播报的回复文本。
func (r *Router) Route(ctx context.Context, deviceID, text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", nil
	}
	if reply, done := r.emergency(ctx, deviceID, text); done {
		return reply, nil
	}
	if reply, done := r.intercomCall(ctx, deviceID, text); done {
		return reply, nil
	}
	if reply, done := r.roomClaim(ctx, deviceID, text); done {
		return reply, nil
	}
	if reply, done, err := r.reminder(ctx, deviceID, text); done {
		return reply, err
	}
	if reply, done, err := r.sceneMemory(ctx, deviceID, text); done {
		return reply, err
	}
	if reply, done, err := r.music(ctx, deviceID, text); done {
		return reply, err
	}
	if reply, done, err := r.homeAssistant(ctx, deviceID, text); done {
		return reply, err
	}
	// 默认：Agent 对话（P1-38：按唤醒词路由多 Agent）。
	ag := r.agentFor(deviceID)
	if ag == nil {
		return "抱歉，我现在没接大脑（Agent 未配置），只能执行家电、音乐和提醒。", nil
	}
	out, err := ag.Chat(ctx, []agent.Message{
		{Role: "system", Content: "你是家里的语音管家，回答简短口语化，一两句话。"},
		{Role: "user", Content: text},
	})
	if err != nil {
		return "", err
	}
	return out, nil
}

// ---- 1. 紧急呼叫（FR-15）----

// emergencyCoreRe 是无歧义的求救触发词；“报警”不放这里，见 isEmergencyTrigger。
var emergencyCoreRe = regexp.MustCompile(`救命|打\s*120|叫救护车|打\s*110`)
var emergencyCancelRe = regexp.MustCompile(`取消(呼叫|紧急呼叫|报警|求救)`)

// emergencyNegationRe 是“报警”的否定/非求助语境（P1-19）：
// “不要报警”“别报警”“不用报警”“不是报警”“没有报警”“误报”都不触发。
var emergencyNegationRe = regexp.MustCompile(`不要报警|别报警|不用报警|不是报警|没有报警|误报|别报|不要叫|别叫`)

// isEmergencyTrigger 判断文本是否真在求救（P1-19）：核心词直接触发；
// “报警”只有在求助语境下才触发——否定、设备词（报警器/报警声/报警系统）、
// “已经报警了”这类已处理表述都不触发。Go regexp 没有 lookahead，
// 用前置过滤实现词边界与否定排除。
func isEmergencyTrigger(text string) bool {
	if emergencyCoreRe.MatchString(text) {
		return true
	}
	if !strings.Contains(text, "报警") {
		return false
	}
	if emergencyNegationRe.MatchString(text) {
		return false
	}
	for _, w := range []string{"报警器", "报警声", "报警系统", "报警功能", "报警灯", "已经报警"} {
		if strings.Contains(text, w) {
			return false
		}
	}
	return !strings.HasSuffix(text, "报警了")
}

func (r *Router) emergency(ctx context.Context, deviceID, text string) (string, bool) {
	// 先惰性清理过期呼叫，再处理本次。
	r.SweepEmergency(time.Now())
	// 取消：只取消本设备的呼叫（别台设备的求助不能被顺手取消）。
	if emergencyCancelRe.MatchString(text) {
		r.emergencyMu.Lock()
		_, active := r.emergencyCalls[deviceID]
		if active {
			delete(r.emergencyCalls, deviceID)
		}
		r.emergencyMu.Unlock()
		if active {
			return "已经取消了本次紧急呼叫，不用担心。", true
		}
		return "没有正在进行的紧急呼叫。", true
	}
	if !isEmergencyTrigger(text) {
		return "", false
	}
	// P1-18：Notify 失败不再回“已经帮你呼叫”——最多重试 3 次，仍失败
	// 时不记为已通知，回复明确说通知失败、请手动拨打 120；下次再触发
	// 时重新尝试（不再被“已在呼叫”挡住）。
	r.emergencyMu.Lock()
	c, active := r.emergencyCalls[deviceID]
	if active && c.notified {
		r.emergencyMu.Unlock()
		return "已经在呼叫急救了，家人马上会收到通知，请保持通话。", true
	}
	if !active {
		c = emergencyCall{text: text, at: time.Now()}
	}
	r.emergencyCalls[deviceID] = c
	r.emergencyMu.Unlock()

	notifyErr := r.notifyEmergency(ctx, deviceID, text)
	r.emergencyMu.Lock()
	c = r.emergencyCalls[deviceID]
	if notifyErr == nil {
		c.notified = true
		c.attempts++
		r.emergencyCalls[deviceID] = c
		r.emergencyMu.Unlock()
		return "已经帮你呼叫急救并通知了家人，请保持通话。如果是误触，说“取消呼叫”可以撤销。", true
	}
	c.attempts++
	r.emergencyCalls[deviceID] = c
	r.emergencyMu.Unlock()
	log.Printf("router: 紧急呼叫通知失败（%s，第 %d 次）: %v", deviceID, c.attempts, notifyErr)
	return "紧急呼叫的通知没发出去，请立即手动拨打 120，也可以再说一次“救命”让我重新试。", true
}

// notifyEmergency 发一次紧急通知（带 3 次重试）；Notify 未配时直接报错。
func (r *Router) notifyEmergency(ctx context.Context, deviceID, text string) error {
	if r.deps.Notify == nil {
		return fmt.Errorf("Notify 未配置（HA 未接）")
	}
	msg := "紧急呼叫：设备 " + deviceID + " 收到求助“" + text + "”"
	var err error
	for i := 0; i < 3; i++ {
		if err = r.deps.Notify(ctx, msg); err == nil {
			return nil
		}
		if i < 2 {
			time.Sleep(100 * time.Millisecond)
		}
	}
	return err
}

// ---- 1b. 对讲（P1-15）与房间认领（P1-39）----

var (
	intercomCallRe = regexp.MustCompile(`^(?:呼叫|对讲|叫)\s*(.+?)\s*$`)
	intercomEndRe  = regexp.MustCompile(`^(?:挂断|结束对讲|结束通话)\s*$`)
	broadcastRe    = regexp.MustCompile(`^(?:全屋广播|广播一下|广播)\s*(.*)$`)
	roomClaimRe    = regexp.MustCompile(`^我(?:现在)?在(.+?)\s*$`)
	roomHereRe     = regexp.MustCompile(`^我在这里\s*$`)
)

// intercomCall 处理语音对讲触发（P1-15）：
// “呼叫卧室”/“对讲卧室” → DeviceByRoom 找目标在线设备 → Intercom.Start；
// “全屋广播” → Intercom.StartBroadcast；“挂断” → Intercom.End。
// Intercom 或 DeviceByRoom 未配时不生效，命令落到 Agent。
func (r *Router) intercomCall(_ context.Context, deviceID, text string) (string, bool) {
	if m := intercomEndRe.FindStringSubmatch(text); m != nil {
		if r.deps.Intercom == nil {
			return "", false
		}
		if r.deps.Intercom.End(deviceID) {
			return "已经挂断对讲。", true
		}
		return "现在没有正在进行的对讲。", true
	}
	if m := broadcastRe.FindStringSubmatch(text); m != nil {
		if r.deps.Intercom == nil {
			return "", false
		}
		if r.deps.Intercom.StartBroadcast(deviceID) {
			return "开始全屋广播，你说一句话就会播到所有房间。", true
		}
		return "现在正在通话中，稍后再广播。", true
	}
	if m := intercomCallRe.FindStringSubmatch(text); m != nil {
		if r.deps.Intercom == nil || r.deps.DeviceByRoom == nil {
			return "", false
		}
		room := strings.TrimSuffix(strings.TrimSpace(m[1]), "音箱")
		room = strings.TrimSuffix(room, "房间")
		room = strings.TrimSpace(room)
		if room == "" || room == deviceID {
			return "", false
		}
		to, ok := r.deps.DeviceByRoom(room)
		if !ok {
			return "找不到" + room + "的音箱，先让那边的音箱说“我在" + room + "”认领一下房间。", true
		}
		if r.deps.Intercom.Start(deviceID, to) {
			return "正在呼叫" + room + "，接通后直接说话，你说完一句话就自动挂断。", true
		}
		return room + "那边正在通话中，稍后再试。", true
	}
	return "", false
}

// roomClaim 处理语音房间认领（P1-39）：
// “我在客厅” → RoomClaim(deviceID, "客厅")；
// “我在这里” → 已知房间时确认，未知时请用户说房间。
// RoomClaim 未配时不生效，命令落到 Agent。
func (r *Router) roomClaim(_ context.Context, deviceID, text string) (string, bool) {
	if roomHereRe.MatchString(text) {
		room := ""
		if r.deps.RoomOfDevice != nil {
			room = r.deps.RoomOfDevice(deviceID)
		}
		if room != "" {
			return "你就在" + room + "，没错。", true
		}
		return "你在哪间房？说“我在客厅”我就记住了。", true
	}
	if m := roomClaimRe.FindStringSubmatch(text); m != nil {
		room := strings.TrimSpace(m[1])
		room = strings.TrimSuffix(room, "房间")
		room = strings.TrimSuffix(room, "里")
		room = strings.TrimSpace(room)
		if room == "" {
			return "", false
		}
		// P2-3：语音认领的房间名同样要合法（超长/含控制字符直接拒绝，
		// 不进注册表、配置文件与日志）。
		if err := registry.ValidateRoomName(room); err != nil {
			return "这个房间名记不下来，换个短一点的名字，比如“客厅”。", true
		}
		if r.deps.RoomClaim == nil {
			return "", false
		}
		r.deps.RoomClaim(deviceID, room)
		return "好的，你在" + room + "了。", true
	}
	return "", false
}

// ---- 2. 提醒（FR-14）----

var (
	dailyRe        = regexp.MustCompile(`每天\s*(\d{1,2})\s*[点:：]\s*(\d{1,2})?\s*分?提醒我(.+)`)
	intervalRe     = regexp.MustCompile(`每\s*(\d+)\s*分钟提醒我(.+)`)
	afterRe        = regexp.MustCompile(`(\d+)\s*小时后提醒我(.+)`)
	cancelRe       = regexp.MustCompile(`取消(.+)提醒`)
	actionHourRe   = regexp.MustCompile(`^(\d+)\s*小时后(开|关)(.+)$`)
	actionMinuteRe = regexp.MustCompile(`^(\d+)\s*分钟后(开|关)(.+)$`)
)

func (r *Router) reminder(ctx context.Context, deviceID, text string) (string, bool, error) {
	if r.deps.Reminders == nil {
		return "", false, nil
	}
	room := ""
	if r.deps.RoomOfDevice != nil {
		room = r.deps.RoomOfDevice(deviceID)
	}
	if m := cancelRe.FindStringSubmatch(text); m != nil {
		n, err := r.deps.Reminders.CancelByText(strings.TrimSpace(m[1]))
		if err != nil {
			// P2-14：取消落盘失败时明说，不要谎报已取消——
			// 内存里已删、磁盘里还在，重启后提醒会复活。
			return "取消失败，保存没存住，请稍后再试一次。", true, nil
		}
		if n == 0 {
			return "没找到“" + strings.TrimSpace(m[1]) + "”的提醒。", true, nil
		}
		return fmt.Sprintf("已取消 %d 条“%s”的提醒。", n, strings.TrimSpace(m[1])), true, nil
	}
	if m := dailyRe.FindStringSubmatch(text); m != nil {
		hour, _ := strconv.Atoi(m[1])
		min := 0
		if m[2] != "" {
			min, _ = strconv.Atoi(m[2])
		}
		// P1-25：越界时间不再进 NextDaily 报错 → 只记日志，而要回一句
		// 用户能听懂的话。
		if hour < 0 || hour > 23 || min < 0 || min > 59 {
			return fmt.Sprintf("时间不对：每天 %d 点 %d 分超出了范围，小时要在 0 到 23 之间，分钟要在 0 到 59 之间。", hour, min), true, nil
		}
		at := fmt.Sprintf("%02d:%02d", hour, min)
		next, err := reminders.NextDaily(at, time.Now())
		if err != nil {
			return "", true, err
		}
		what := strings.TrimSpace(m[3])
		if _, err := r.deps.Reminders.Add(reminders.Reminder{
			Kind: reminders.KindDaily, Text: what, Room: room, At: at, Next: next,
		}); err != nil {
			return "", true, err
		}
		return fmt.Sprintf("记下了，每天 %s 提醒你%s。", at, what), true, nil
	}
	if m := intervalRe.FindStringSubmatch(text); m != nil {
		mins, _ := strconv.Atoi(m[1])
		// P2-45 顺带：“每 0 分钟”无意义，直接拒绝。
		if mins <= 0 {
			return "间隔得大于 0 分钟，比如“每 30 分钟提醒我喝水”。", true, nil
		}
		what := strings.TrimSpace(m[2])
		next := time.Now().Add(time.Duration(mins) * time.Minute)
		if _, err := r.deps.Reminders.Add(reminders.Reminder{
			Kind: reminders.KindInterval, Text: what, Room: room, Every: mins, Next: next,
		}); err != nil {
			return "", true, err
		}
		return fmt.Sprintf("记下了，每 %d 分钟提醒你%s。", mins, what), true, nil
	}
	if m := afterRe.FindStringSubmatch(text); m != nil {
		hours, _ := strconv.Atoi(m[1])
		if hours <= 0 {
			return "时间得大于 0 小时，比如“2 小时后提醒我关灯”。", true, nil
		}
		what := strings.TrimSpace(m[2])
		next := time.Now().Add(time.Duration(hours) * time.Hour)
		if _, err := r.deps.Reminders.Add(reminders.Reminder{
			Kind: reminders.KindOnce, Text: what, Room: room, Next: next,
		}); err != nil {
			return "", true, err
		}
		return fmt.Sprintf("记下了，%d 小时后提醒你%s。", hours, what), true, nil
	}
	// P1-41：“2 小时后关灯”类定时动作——建 KindAction 提醒，触发时
	// main.go 的 onFire 把 Text 当命令走一遍 Route 执行，不只是播报。
	if reply, done, err := r.timedAction(deviceID, room, text); done {
		return reply, done, err
	}
	return "", false, nil
}

// timedAction 解析“X 小时/分钟后开/关 Y”，建 KindAction 提醒。
func (r *Router) timedAction(deviceID, room, text string) (string, bool, error) {
	var amount int
	var unit time.Duration
	var action, target string
	if m := actionHourRe.FindStringSubmatch(text); m != nil {
		amount, _ = strconv.Atoi(m[1])
		unit = time.Hour
		action, target = m[2], strings.TrimSpace(m[3])
	} else if m := actionMinuteRe.FindStringSubmatch(text); m != nil {
		amount, _ = strconv.Atoi(m[1])
		unit = time.Minute
		action, target = m[2], strings.TrimSpace(m[3])
	} else {
		return "", false, nil
	}
	if amount <= 0 {
		return "时间得大于 0，比如“2 小时后关灯”。", true, nil
	}
	// 目标没带房间名时补当前房间（“关灯”→“关客厅灯”），让 homeAssistant
	// 能命中别名表；已带房间名或无房间时原样。
	cmd := action + target
	if room != "" && !strings.Contains(target, room) {
		cmd = action + room + target
	}
	next := time.Now().Add(time.Duration(amount) * unit)
	if _, err := r.deps.Reminders.Add(reminders.Reminder{
		Kind: reminders.KindAction, Text: cmd, Room: room, Next: next,
	}); err != nil {
		return "", true, err
	}
	unitName := "小时"
	if unit == time.Minute {
		unitName = "分钟"
	}
	return fmt.Sprintf("记下了，%d %s后帮你%s。", amount, unitName, cmd), true, nil
}

// ---- 3. 场景/记忆（FR-09）----

var rememberRe = regexp.MustCompile(`^记住(.+)$`)
var recallRe = regexp.MustCompile(`^(你还记得|你记得|我说过|我让你记住的)(什么|啥|哪些|哪些事)?$`)
var homeRe = regexp.MustCompile(`我回来了|我到家了`)

// allowedVoiceDomain 是语音可直接控制的 domain 白名单（P2-49）：
// 灯/插座/空调/窗帘四类；门锁/燃气/热水器等高风险 domain 不在内。
// 家电命令与回家场景共用同一张表，场景不再是 allowlist 的旁路。
func allowedVoiceDomain(domain string) bool {
	switch domain {
	case "light", "switch", "climate", "cover":
		return true
	}
	return false
}

func (r *Router) sceneMemory(ctx context.Context, deviceID, text string) (string, bool, error) {
	// P1-41：记忆只写不读的另一半——“你还记得什么”读出来。
	if recallRe.MatchString(text) {
		if r.deps.Memory == nil {
			return "", false, nil
		}
		items := r.deps.Memory.All(deviceID)
		if len(items) == 0 {
			return "我还没记住过什么，你说“记住……”我就记下来。", true, nil
		}
		return "我记住的有：" + strings.Join(items, "；") + "。", true, nil
	}
	if m := rememberRe.FindStringSubmatch(text); m != nil {
		if r.deps.Memory == nil {
			return "", false, nil
		}
		what := strings.TrimSpace(m[1])
		if err := r.deps.Memory.Set(deviceID, what); err != nil {
			return "", true, err
		}
		return "记住了：" + what, true, nil
	}
	if homeRe.MatchString(text) {
		if r.ha() == nil {
			return "", false, nil
		}
		// P2-49：场景执行前先查场景内含的实体，逐个过 domain 白名单——
		// 场景内容由用户在 HA 侧配置，可能含门锁/燃气，一句语音即可
		// 触发。查不到场景内容（场景不存在、属性缺失、HA 故障）时
		// fail-closed：拒绝执行并明说，不猜场景内容。
		entities, err := r.ha().GetSceneEntityIDs("scene.coming_home")
		if err != nil {
			return "想帮你开回家场景，但查不到 scene.coming_home 里有什么设备，先在 HA 里确认一下场景内容。", true, nil
		}
		for _, eid := range entities {
			domain := strings.SplitN(eid, ".", 2)[0]
			if !allowedVoiceDomain(domain) {
				return "回家场景里有不能语音控制的设备（" + eid + "），先在 HA 里把它移出场景再试。", true, nil
			}
		}
		// “我回来了”跑 HA 场景 scene.coming_home（需在 HA 里建好）。
		if err := r.ha().CallService("scene", "turn_on", "scene.coming_home"); err != nil {
			return "想帮你开回家场景，但 HA 里没有 scene.coming_home。", true, nil
		}
		return "欢迎回来，灯和空调都按你的习惯开好了。", true, nil
	}
	return "", false, nil
}

// ---- 4. 音乐（FR-05）----

// 点歌与播放控制的命令词（融合 B：全部走 MPD 队列，Hub 内存记当前曲）。
var (
	playRe   = regexp.MustCompile(`^(?:播放|放一首|来一首|点一首)\s*(.+)$`)
	pauseRe  = regexp.MustCompile(`^(?:暂停|暂停音乐|暂停播放)\s*$`)
	resumeRe = regexp.MustCompile(`^(?:继续|继续播放|恢复播放)\s*$`)
	nextRe   = regexp.MustCompile(`^下一首\s*$`)
	prevRe   = regexp.MustCompile(`^上一首\s*$`)
	lyricRe  = regexp.MustCompile(`^(?:歌词|显示歌词|歌词是什么)\s*$`)
)

// music 处理点歌与播放控制。点歌降级顺序（融合 B，直连主路）：
//  1. 网易云搜歌 → 逐首取 SongURL → MPD.PlayURL（成功即播，记当前曲）；
//  2. 直连全失败 → MPD 本地曲库同名歌（search filename → PlayFile）；
//  3. 本地也没有 → 明确回复放不了（不静默失败、不落到 Agent 闲聊）。
//
// 搜歌本身失败时也先试本地兜底，再明说。
func (r *Router) music(ctx context.Context, deviceID, text string) (string, bool, error) {
	// 播放控制先于点歌：MPD 未接时明说，不要落到 Agent。
	if pauseRe.MatchString(text) {
		if err := r.MusicPause(ctx); err != nil {
			return "音乐还没接好，暂停不了：" + err.Error(), true, nil
		}
		return "音乐已暂停。", true, nil
	}
	if resumeRe.MatchString(text) {
		if err := r.MusicResume(ctx); err != nil {
			return "音乐还没接好，继续不了：" + err.Error(), true, nil
		}
		return "继续播放。", true, nil
	}
	if nextRe.MatchString(text) {
		if err := r.MusicNext(ctx); err != nil {
			return "音乐还没接好，切不了下一首：" + err.Error(), true, nil
		}
		return "下一首。", true, nil
	}
	if prevRe.MatchString(text) {
		if err := r.MusicPrevious(ctx); err != nil {
			return "音乐还没接好，切不了上一首：" + err.Error(), true, nil
		}
		return "上一首。", true, nil
	}
	if lyricRe.MatchString(text) {
		lyric, err := r.MusicLyric(ctx, 0)
		if err != nil {
			return err.Error(), true, nil
		}
		return truncateLyric(lyric), true, nil
	}
	m := playRe.FindStringSubmatch(text)
	if m == nil {
		return "", false, nil
	}
	query := strings.TrimSpace(m[1])
	if query == "" {
		return "", false, nil
	}
	// 快照运行时可替换的依赖（MPD/Snap 可能被后台重试补接）。
	r.depsMu.RLock()
	netease := r.deps.Netease
	mpd := r.deps.MPD
	r.depsMu.RUnlock()
	if netease == nil {
		return "音乐服务没接好（网易云未配置）。", true, nil
	}
	if mpd == nil {
		return "音乐服务没接好（播放器未配置）。", true, nil
	}
	songs, err := netease.Search(ctx, query, 3)
	if err != nil {
		log.Printf("netease: 搜歌 %q 失败，先试本地兜底: %v", query, err)
		if reply, ok := r.playLocal(ctx, query); ok {
			return reply, true, nil
		}
		return "网易云搜歌失败，本地曲库里也没有这首歌，请稍后再试。", true, nil
	}
	if len(songs) == 0 {
		if reply, ok := r.playLocal(ctx, query); ok {
			return reply, true, nil
		}
		return "没搜到“" + query + "”，换个歌名试试？", true, nil
	}
	var lastErr error
	for _, song := range songs {
		u, err := netease.SongURL(ctx, song.ID)
		if err != nil {
			lastErr = err
			continue
		}
		if err := mpd.PlayURL(u); err != nil {
			lastErr = err
			continue
		}
		r.setCurrent(song, "netease")
		r.applySnapcast()
		return "正在播放" + displaySongName(song) + "。", true, nil
	}
	log.Printf("netease: 直连点歌 %q 失败（%v），走本地兜底", query, lastErr)
	if reply, ok := r.playLocal(ctx, query); ok {
		return reply, true, nil
	}
	if !r.localMusicAvailable() {
		return "这首歌暂时放不了（网易云没有可播地址，本地曲库还没配好），换一首试试？", true, nil
	}
	return "这首歌暂时放不了（网易云没有可播地址，本地曲库里也没有），换一首试试？", true, nil
}

// playLocal 是点歌的本地兜底：MPD 本地曲库按文件名找同名歌，找到即播。
// 返回 (回复, 是否播出)：本地曲库没配好、搜不到、播失败都返回 false，
// 由调用方按 localMusicAvailable() 组织明确回复。
func (r *Router) playLocal(ctx context.Context, query string) (string, bool) {
	if !r.localMusicAvailable() {
		return "", false
	}
	r.depsMu.RLock()
	mpd := r.deps.MPD
	r.depsMu.RUnlock()
	if mpd == nil {
		return "", false
	}
	artist, track := splitArtistTrack(query)
	for _, term := range []string{track, query} {
		if strings.TrimSpace(term) == "" {
			continue
		}
		files, err := mpd.SearchFile(term)
		if err != nil {
			log.Printf("mpd: 本地搜歌 %q 失败: %v", term, err)
			continue
		}
		if len(files) == 0 {
			continue
		}
		if err := mpd.PlayFile(files[0]); err != nil {
			log.Printf("mpd: 本地播 %q 失败: %v", files[0], err)
			return "", false
		}
		name := track
		if artist != "" {
			name = artist + "的" + track
		}
		r.setCurrent(music.NeteaseSong{Name: track, Artist: artist}, "local")
		r.applySnapcast()
		return "正在播放本地版" + name + "。", true
	}
	return "", false
}

// localMusicAvailable 判断本地曲库是否可用：music_folder 配了且是目录。
func (r *Router) localMusicAvailable() bool {
	r.depsMu.RLock()
	folder := r.deps.MusicFolder
	r.depsMu.RUnlock()
	if folder == "" {
		return false
	}
	fi, err := os.Stat(folder)
	return err == nil && fi.IsDir()
}

// mpd 取当前 MPD 客户端（看门狗热换后是最新的；nil 表示未接通）。
func (r *Router) mpd() *music.MPDClient {
	r.depsMu.RLock()
	defer r.depsMu.RUnlock()
	return r.deps.MPD
}

// MusicPause 暂停播放（Web /api/music/pause 与语音“暂停”共用）。
func (r *Router) MusicPause(_ context.Context) error {
	mpd := r.mpd()
	if mpd == nil {
		return fmt.Errorf("MPD 未连接")
	}
	return mpd.Pause(true)
}

// MusicResume 继续播放（Web /api/music/resume 与语音“继续”共用）。
func (r *Router) MusicResume(_ context.Context) error {
	mpd := r.mpd()
	if mpd == nil {
		return fmt.Errorf("MPD 未连接")
	}
	return mpd.Resume()
}

// MusicNext 下一首（Web /api/music/next 与语音“下一首”共用）。
func (r *Router) MusicNext(_ context.Context) error {
	mpd := r.mpd()
	if mpd == nil {
		return fmt.Errorf("MPD 未连接")
	}
	return mpd.Next()
}

// MusicPrevious 上一首（Web /api/music/previous 与语音“上一首”共用）。
func (r *Router) MusicPrevious(_ context.Context) error {
	mpd := r.mpd()
	if mpd == nil {
		return fmt.Errorf("MPD 未连接")
	}
	return mpd.Previous()
}

// PlaybackState 取 Hub 侧的播放状态：MPD status + 当前曲 + 队列。
// state 是 MPD 的状态（play/pause/stop）；MPD 未接时返回错误。
func (r *Router) PlaybackState(_ context.Context) (state string, current music.MPDSong, queue []music.MPDSong, err error) {
	mpd := r.mpd()
	if mpd == nil {
		return "", music.MPDSong{}, nil, fmt.Errorf("MPD 未连接")
	}
	st, err := mpd.Status()
	if err != nil {
		return "", music.MPDSong{}, nil, err
	}
	cur, err := mpd.CurrentSong()
	if err != nil {
		return "", music.MPDSong{}, nil, err
	}
	q, err := mpd.Queue()
	if err != nil {
		return "", music.MPDSong{}, nil, err
	}
	return st.State, cur, q, nil
}

// MusicQueue 取当前队列（Web /api/music/queue 用）。
func (r *Router) MusicQueue(_ context.Context) ([]music.MPDSong, error) {
	mpd := r.mpd()
	if mpd == nil {
		return nil, fmt.Errorf("MPD 未连接")
	}
	return mpd.Queue()
}

// MusicLyric 取歌词（Web /api/music/lyric 与语音“歌词”共用）：
// id==0 时用内存里当前曲的 ID；没有在放的歌时明确报错。
func (r *Router) MusicLyric(ctx context.Context, id int64) (string, error) {
	if id == 0 {
		if song, _, ok := r.CurrentTrack(); ok {
			id = song.ID
		}
	}
	if id == 0 {
		return "", fmt.Errorf("还没在放歌，先点一首吧")
	}
	r.depsMu.RLock()
	netease := r.deps.Netease
	r.depsMu.RUnlock()
	if netease == nil {
		return "", fmt.Errorf("音乐服务没接好（网易云未配置）")
	}
	lyric, err := netease.Lyric(ctx, id)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(lyric) == "" {
		return "", fmt.Errorf("这首歌没有歌词")
	}
	return lyric, nil
}

// setCurrent 记下最近一次点歌成功的曲（内存状态，重启清空）。
func (r *Router) setCurrent(song music.NeteaseSong, source string) {
	r.currentMu.Lock()
	defer r.currentMu.Unlock()
	r.current = &currentTrack{song: song, source: source}
}

// CurrentTrack 取内存里的当前曲（ok==false 表示还没点过歌）。
func (r *Router) CurrentTrack() (music.NeteaseSong, string, bool) {
	r.currentMu.Lock()
	defer r.currentMu.Unlock()
	if r.current == nil {
		return music.NeteaseSong{}, "", false
	}
	return r.current.song, r.current.source, true
}

// displaySongName 把歌拼成“歌手 + 的 + 歌名”（歌手为空时只剩歌名）。
func displaySongName(song music.NeteaseSong) string {
	if song.Artist != "" {
		return song.Artist + "的" + song.Name
	}
	return song.Name
}

// truncateLyric 截短歌词再播报：语音回复读全文太长，只读前面一部分。
func truncateLyric(lyric string) string {
	const maxRunes = 600
	runes := []rune(lyric)
	if len(runes) <= maxRunes {
		return lyric
	}
	return string(runes[:maxRunes]) + "……（歌词太长，只读前面一部分）"
}

// splitArtistTrack 把“周杰伦的青花瓷”拆成歌手 + 歌名；
// 没“的”或任一侧为空时整句当歌名、歌手为空。
func splitArtistTrack(query string) (artist, track string) {
	i := strings.Index(query, "的")
	if i <= 0 {
		return "", query
	}
	artist = strings.TrimSpace(query[:i])
	track = strings.TrimSpace(query[i+len("的"):])
	if artist == "" || track == "" {
		return "", query
	}
	return artist, track
}

// ---- 5. 家电（FR-06）----

// homeQueryRe 是查询类命令（P1-42）：“灯开着吗”“客厅多少度”只查不调。
var homeQueryRe = regexp.MustCompile(`^(.+?)(开着吗|关着吗|开着还是关着|多少度|温度是多少|现在多少度)$`)

// homeAssistant 处理“开/关 + 别名”命令。先查 HA 状态再调服务，不编数字。
func (r *Router) homeAssistant(ctx context.Context, deviceID, text string) (string, bool, error) {
	haClient := r.ha()
	if haClient == nil {
		return "", false, nil
	}
	// P1-42：查询类命令先处理——只查状态、不调服务。
	if m := homeQueryRe.FindStringSubmatch(text); m != nil {
		alias := strings.TrimSpace(m[1])
		entityID, ok := r.entities[alias]
		if !ok {
			return "", false, nil
		}
		state, err := haClient.GetEntityState(entityID)
		if err != nil {
			return "查不到" + alias + "的状态，先不操作，请检查 HA 连接。", true, nil
		}
		if strings.Contains(m[2], "度") {
			return alias + "现在 " + state + " 度。", true, nil
		}
		switch state {
		case "on":
			return alias + "开着呢。", true, nil
		case "off":
			return alias + "关着呢。", true, nil
		default:
			return alias + "现在的状态是" + state + "。", true, nil
		}
	}
	var action, alias string
	for _, prefix := range []string{"打开", "关掉", "开启", "关闭"} {
		if strings.HasPrefix(text, prefix) {
			action = prefix
			alias = strings.TrimSpace(strings.TrimPrefix(text, prefix))
			break
		}
	}
	if action == "" {
		for _, prefix := range []string{"开", "关"} {
			if strings.HasPrefix(text, prefix) {
				action = prefix
				alias = strings.TrimSpace(strings.TrimPrefix(text, prefix))
				break
			}
		}
	}
	if action == "" || alias == "" {
		return "", false, nil
	}
	entityID, ok := r.entities[alias]
	if !ok {
		return "", false, nil
	}
	// 高风险设备拒绝：门锁/燃气/热水器不在别名表里才安全；
	// 即使配了，也只允许开关灯/插座/空调/窗帘这四类 domain
	// （与回家场景共用 allowedVoiceDomain 同一张白名单，见 P2-49）。
	domain := strings.SplitN(entityID, ".", 2)[0]
	if !allowedVoiceDomain(domain) {
		return "这个设备不能语音直接控制，请当面确认。", true, nil
	}
	turnOn := action == "开" || action == "打开" || action == "开启"
	service := "turn_off"
	verb := "关了"
	if turnOn {
		service = "turn_on"
		verb = "开了"
	}
	// 先查状态：已经是目标状态就直说，不重复调。
	// P1-42：查状态出错时不再跳过比较直接调服务——先不操作，等查得到再说。
	state, err := haClient.GetEntityState(entityID)
	if err != nil {
		return "查不到" + alias + "的状态，先不操作，请检查 HA 连接。", true, nil
	}
	{
		want := "off"
		if turnOn {
			want = "on"
		}
		if state == want {
			return alias + "已经是" + map[bool]string{true: "开着", false: "关着"}[turnOn] + "的了。", true, nil
		}
	}
	if err := haClient.CallService(domain, service, entityID); err != nil {
		return "", true, err
	}
	return alias + verb + "。", true, nil
}
