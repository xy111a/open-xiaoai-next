// Package router 是文本意图路由：识别文本到了之后，决定走哪条路。
//
// 顺序（先具体后通用）：
//  1. 紧急呼叫（FR-15）：“救命”“打 120”“叫救护车” → 立即通知 + 进入待确认状态：
//     可语音“取消呼叫”撤销；60s 无人取消则自动清理（待确认窗口）；
//     同一设备重复触发不重复 Notify（不同设备各自独立）。
//  2. 提醒（FR-14）：“每天 8 点提醒我吃药”“每 30 分钟提醒我喝水”“2 小时后提醒我关灯”“取消吃药提醒”
//  3. 场景/记忆（FR-09）：“记住空调 26 度”“我回来了”（跑 HA 场景）
//  4. 音乐（FR-05）：“播放周杰伦的青花瓷” → 网易云搜歌 → MA 播（MA 不可用时走 MPD 直播 URL）
//  5. 家电（FR-06）：“关客厅灯” → 查 HA 实体别名表 → 先查状态再调服务
//  6. 默认：Agent 对话（Hermes/OpenClaw/DeepSeek，OpenAI 兼容接口）
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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/huajun/open-xiaoai-next/hub/internal/agent"
	"github.com/huajun/open-xiaoai-next/hub/internal/ha"
	"github.com/huajun/open-xiaoai-next/hub/internal/music"
	"github.com/huajun/open-xiaoai-next/hub/internal/reminders"
)

// Deps 是 Router 依赖的下游（都可为 nil；为 nil 的路会被跳过并在回复里说明）。
type Deps struct {
	HA        *ha.Client
	MA        *music.MAClient
	Netease   *music.NeteaseClient
	MPD       *music.MPDClient
	Agent     *agent.Client
	Reminders *reminders.Store
	Memory    *Memory
	// QueueID 是 MA 播放队列（未分组时等于 player_id）。
	QueueID string
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
}

// Router 是意图路由器。
type Router struct {
	deps     Deps
	entities map[string]string // 别名 → HA entity_id

	// depsMu 保护运行时可替换的 deps 字段（MPD/Snap/QueueID）：main.go 的
	// 后台重试在下游晚到时调 SetMPD/SetSnap 补接，music() 读时持读锁。
	depsMu sync.RWMutex

	// 紧急呼叫状态机（FR-15）：deviceID → 进行中的呼叫。
	emergencyMu    sync.Mutex
	emergencyCalls map[string]emergencyCall
	// EmergencyTimeout 是待确认窗口：触发后这么久无人取消，状态自动清理。
	// 默认 60s（New 里设）；0 表示用默认值。60s 是“误触可撤销”的窗口，
	// 不是拨出时限——PLAN 要求触发后 10s 内拨出/推送送达，那是 Notify 下游的事。
	EmergencyTimeout time.Duration
}

// emergencyCall 是一次进行中的紧急呼叫。
type emergencyCall struct {
	text string    // 触发原文（“救命”/“打 120”……）
	at   time.Time // 触发时间
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

// DefaultQueueID 查 MA 的 Players() 取默认播放器 ID 作 QueueID（P1-26，
// main.go 启动时调）：MA 未配置、查不到播放器、一个播放器都没有都返回
// “MA 未就绪”错误，调用方记日志后点歌走 MPD 降级路径。
func DefaultQueueID(ctx context.Context, ma *music.MAClient) (string, error) {
	if ma == nil {
		return "", fmt.Errorf("MA 未就绪：Music Assistant 未配置")
	}
	players, err := ma.Players(ctx)
	if err != nil {
		return "", fmt.Errorf("MA 未就绪：查播放器失败: %w", err)
	}
	if len(players) == 0 {
		return "", fmt.Errorf("MA 未就绪：没有可用播放器")
	}
	return players[0].PlayerID, nil
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

// SetQueueID 在 MA 播放器晚到时补设 QueueID（P1-26；main.go 暂未用后台
// 重试 MA，见 P1-27，留给以后）。
func (r *Router) SetQueueID(queueID string) {
	r.depsMu.Lock()
	defer r.depsMu.Unlock()
	r.deps.QueueID = queueID
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
	// 默认：Agent 对话。
	if r.deps.Agent == nil {
		return "抱歉，我现在没接大脑（Agent 未配置），只能执行家电、音乐和提醒。", nil
	}
	out, err := r.deps.Agent.Chat(ctx, []agent.Message{
		{Role: "system", Content: "你是家里的语音管家，回答简短口语化，一两句话。"},
		{Role: "user", Content: text},
	})
	if err != nil {
		return "", err
	}
	return out, nil
}

// ---- 1. 紧急呼叫（FR-15）----

var emergencyRe = regexp.MustCompile(`救命|打\s*120|叫救护车|打\s*110|报警`)
var emergencyCancelRe = regexp.MustCompile(`取消(呼叫|紧急呼叫|报警|求救)`)

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
	if !emergencyRe.MatchString(text) {
		return "", false
	}
	// 重复触发：不重复 Notify，只提醒已经在呼叫了。
	r.emergencyMu.Lock()
	_, active := r.emergencyCalls[deviceID]
	if active {
		r.emergencyMu.Unlock()
		return "已经在呼叫急救了，家人马上会收到通知，请保持通话。", true
	}
	r.emergencyCalls[deviceID] = emergencyCall{text: text, at: time.Now()}
	r.emergencyMu.Unlock()
	msg := "紧急呼叫：设备 " + deviceID + " 收到求助“" + text + "”"
	if r.deps.Notify != nil {
		_ = r.deps.Notify(ctx, msg)
	}
	return "已经帮你呼叫急救并通知了家人，请保持通话。如果是误触，说“取消呼叫”可以撤销。", true
}

// ---- 2. 提醒（FR-14）----

var (
	dailyRe    = regexp.MustCompile(`每天\s*(\d{1,2})\s*[点:：]\s*(\d{1,2})?分?提醒我(.+)`)
	intervalRe = regexp.MustCompile(`每\s*(\d+)\s*分钟提醒我(.+)`)
	afterRe    = regexp.MustCompile(`(\d+)\s*小时后提醒我(.+)`)
	cancelRe   = regexp.MustCompile(`取消(.+)提醒`)
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
		n := r.deps.Reminders.CancelByText(strings.TrimSpace(m[1]))
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
		what := strings.TrimSpace(m[2])
		next := time.Now().Add(time.Duration(hours) * time.Hour)
		if _, err := r.deps.Reminders.Add(reminders.Reminder{
			Kind: reminders.KindOnce, Text: what, Room: room, Next: next,
		}); err != nil {
			return "", true, err
		}
		return fmt.Sprintf("记下了，%d 小时后提醒你%s。", hours, what), true, nil
	}
	return "", false, nil
}

// ---- 3. 场景/记忆（FR-09）----

var rememberRe = regexp.MustCompile(`^记住(.+)$`)
var homeRe = regexp.MustCompile(`我回来了|我到家了`)

func (r *Router) sceneMemory(ctx context.Context, deviceID, text string) (string, bool, error) {
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
		if r.deps.HA == nil {
			return "", false, nil
		}
		// “我回来了”跑 HA 场景 scene.coming_home（需在 HA 里建好）。
		if err := r.deps.HA.CallService("scene", "turn_on", "scene.coming_home"); err != nil {
			return "想帮你开回家场景，但 HA 里没有 scene.coming_home。", true, nil
		}
		return "欢迎回来，灯和空调都按你的习惯开好了。", true, nil
	}
	return "", false, nil
}

// ---- 4. 音乐（FR-05）----

var playRe = regexp.MustCompile(`^(?:播放|放一首|来一首|点一首)\s*(.+)$`)

func (r *Router) music(ctx context.Context, deviceID, text string) (string, bool, error) {
	m := playRe.FindStringSubmatch(text)
	if m == nil {
		return "", false, nil
	}
	query := strings.TrimSpace(m[1])
	if query == "" {
		return "", false, nil
	}
	// 快照运行时可替换的依赖（MPD/Snap/QueueID 可能被后台重试补接）。
	r.depsMu.RLock()
	ma := r.deps.MA
	queueID := r.deps.QueueID
	netease := r.deps.Netease
	mpd := r.deps.MPD
	r.depsMu.RUnlock()
	// 优先 MA（有队列/歌词/收藏）；MA 不可用时降级 MPD 直播网易云 URL。
	if ma != nil && queueID != "" {
		if err := ma.PlayTrackByName(ctx, queueID, query, ""); err == nil {
			r.applySnapcast()
			return "正在播放" + query + "。", true, nil
		}
		// MA 失败则继续走降级路径，不直接报错。
	}
	// MA 已配但没有可用播放器（启动时 Players() 查不到）：MPD 也没有时
	// 明确报“MA 未就绪”，而不是含糊的“播放器未配置”；MPD 在时走降级。
	if ma != nil && queueID == "" && mpd == nil {
		return "MA 未就绪（Music Assistant 没有可用播放器），请检查 MA 与播放器。", true, nil
	}
	if netease == nil {
		return "音乐服务没接好（网易云未配置）。", true, nil
	}
	songs, err := netease.Search(ctx, query, 1)
	if err != nil {
		return "", true, err
	}
	if len(songs) == 0 {
		return "没搜到“" + query + "”，换个歌名试试？", true, nil
	}
	song := songs[0]
	if mpd != nil {
		u, err := netease.SongURL(ctx, song.ID)
		if err != nil {
			return "", true, err
		}
		if err := mpd.PlayURL(u); err != nil {
			return "", true, err
		}
		r.applySnapcast()
		name := song.Name
		if song.Artist != "" {
			name = song.Artist + "的" + song.Name
		}
		return "正在播放" + name + "。", true, nil
	}
	return "音乐服务没接好（播放器未配置）。", true, nil
}

// ---- 5. 家电（FR-06）----

// homeAssistant 处理“开/关 + 别名”命令。先查 HA 状态再调服务，不编数字。
func (r *Router) homeAssistant(ctx context.Context, deviceID, text string) (string, bool, error) {
	if r.deps.HA == nil {
		return "", false, nil
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
	// 即使配了，也只允许开关灯/插座/空调/窗帘这四类 domain。
	domain := strings.SplitN(entityID, ".", 2)[0]
	switch domain {
	case "light", "switch", "climate", "cover":
	default:
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
	if state, err := r.deps.HA.GetEntityState(entityID); err == nil {
		want := "off"
		if turnOn {
			want = "on"
		}
		if state == want {
			return alias + "已经是" + map[bool]string{true: "开着", false: "关着"}[turnOn] + "的了。", true, nil
		}
	}
	if err := r.deps.HA.CallService(domain, service, entityID); err != nil {
		return "", true, err
	}
	return alias + verb + "。", true, nil
}
