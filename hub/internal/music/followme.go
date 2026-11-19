// Package music 内的 follow-me（跟随播放）：用户在 A 房间听音乐，
// 走到 B 房间喊一声唤醒词，音乐跟到 B 房间——同一首歌、同一位置，
// A 房间停播。
//
// 架构约束（决定了实现形状，先读再改）：
//   - 全屋只有一台 MPD、一条 snapfifo、一个 Snapcast 流（NAS音乐）。
//     MPD 是唯一的音频时间线：把它停掉重播，B 房间也会断；网易云
//     直播地址还可能过期、重取又要一次上游往返。所以“搬家”不在
//     MPD 层做停/重播，而在 Snapcast 路由层做：MPD 一直放，
//     只把 B 房间的音箱移进正在播 NAS音乐流的分组、把 A 房间的
//     音箱移出去。位置因此是精确连续的，不需要 seek；直播流
//     （无总时长）也不需要特殊处理，同一条流直接续上。
//   - MPD 仍然要查（Status + CurrentSong）：确认真在放、
//     拿到歌名/位置/是否直播，供决策与日志；查不到或没在放就不动。
//   - 默认拓扑只有一组、两台音箱都在里面时，“已在 B 房间”
//     不是“不搬”：那组同时也在 A 房间出声，本次搬家是把分组
//     收窄到只剩 B 房间。只有分组里已经只剩 B 房间时才什么都不做。
//   - 房间归属以注册表为准：Snapcast 客户端按 host.ip 对上 Hub
//     设备 IP，再由设备查房间。IP 对不上的客户端（未注册设备）
//     视为房间未知：搬家时不动它（fail-closed，不误踢别的播放器）。
//   - 失败一律 fail-closed：MPD/Snapcast 任一查不到、目标音箱
//     不在线，都只记日志、音乐留在原房间，不做半截搬家。
//     目标音箱离线时可向唤醒设备说一句（Speak 回调，未配则只记日志）。
//   - 真实无缝程度（搬家是否零卡顿、位置是否体感连续）只能在
//     真机 + 真 Snapcast/MPD 上验证，见 followme_test.go 末尾的说明。
package music

import (
	"context"
	"log"
	"sort"
	"strings"
	"sync"
)

// ==================== 决策（纯函数，可单测）====================

// DecideInput 是跟随决策的全部输入。
type DecideInput struct {
	Enabled      bool     // 跟随总开关（config hub.follow_me，默认开）
	Playing      bool     // MPD state == "play"
	Live         bool     // 直播流（无总时长）：只作记录，从不阻止搬家
	WakeRoom     string   // 唤醒设备所在房间，"" 表示未认领
	MusicRooms   []string // 当前听到音乐流的房间（去重后）
	TargetOnline bool     // 唤醒房间的音箱已连上 Snapcast
}

// Decision 是跟随决策的结果。
type Decision struct {
	Move   bool
	Reason string // 给日志/测试看的原因（中文）
}

// DecideFollowMe 判断一次唤醒要不要搬音乐。
// 顺序：开关 → 在放 → 认领了房间 → 目标在线 →
// 音乐已独占该房间（不搬）→ 其余一律搬（含换房间、
// 以及默认单分组同时响两个房间时的“收窄到唤醒房间”）。
func DecideFollowMe(in DecideInput) Decision {
	switch {
	case !in.Enabled:
		return Decision{Move: false, Reason: "follow-me 已关闭"}
	case !in.Playing:
		return Decision{Move: false, Reason: "没有在放音乐"}
	case in.WakeRoom == "":
		return Decision{Move: false, Reason: "唤醒设备未认领房间"}
	case !in.TargetOnline:
		return Decision{Move: false, Reason: "目标房间音箱不在线"}
	case len(in.MusicRooms) == 1 && in.MusicRooms[0] == in.WakeRoom:
		return Decision{Move: false, Reason: "音乐已在该房间播放"}
	default:
		return Decision{Move: true, Reason: "跟随到" + in.WakeRoom}
	}
}

// ==================== 搬家执行 ====================

// MPDReader 是跟随需要的 MPD 只读面：*MPDClient 满足它，
// 测试用假 MPD 也只要实现这两个方法。
type MPDReader interface {
	Status() (MPDStatus, error)
	CurrentSong() (MPDSong, error)
}

// SnapRouting 是跟随需要的 Snapcast 路由面：*SnapClient 满足它。
type SnapRouting interface {
	GetGroups() ([]SnapGroup, error)
	SetGroupClients(groupID string, clientIDs []string) error
	SetGroupStream(groupID, streamID string) error
}

// MoverDeps 是跟随搬家的依赖。
type MoverDeps struct {
	// MPD/Snap 是提供方函数（不要直接存客户端指针）：
	// Router 的看门狗会热换 MPD/Snap 客户端，每次 Move 都经
	// 提供方取当前值，才不会抓着断线重连前的旧连接。
	// 提供方返回 nil 接口表示未连接；不要直接 return 一个可能为
	// nil 的具体指针——nil 指针包进接口后是非 nil 接口
	// （typed-nil），Move 的 nil 判断失效、继而空指针 panic。
	MPD  func() MPDReader
	Snap func() SnapRouting
	// StreamID 是音乐流 ID（config hub.snapcast_stream，如 "NAS音乐"）；
	// 为空表示没配流，跟随不生效。
	StreamID string
	// RoomOfDevice 查 Hub 设备所在房间（未认领返回 ""）。
	RoomOfDevice func(deviceID string) string
	// DeviceOfSnapClient 把 Snapcast 客户端映射回 Hub 设备 ID
	// （main.go 按 host.ip 对注册表 IP）；找不到返回 ok=false。
	DeviceOfSnapClient func(snap SnapDevice) (deviceID string, ok bool)
	// Enabled 读总开关（nil 表示默认开启）；main.go 读
	// config.Hub.FollowMeEnabled，Web 改配置后立即生效。
	Enabled func() bool
	// Speak 可选：目标音箱离线时向唤醒设备说一句；
	// nil 表示只记日志（测试默认走这条）。
	Speak func(deviceID, text string)
}

// Mover 是跟随搬家执行器（一次只搬一家，Move 自带互斥）。
type Mover struct {
	deps MoverDeps
	mu   sync.Mutex
}

// NewMover 新建 Mover（deps 里缺 MPD/Snap 提供方时 Move 直接报未连接）。
func NewMover(d MoverDeps) *Mover {
	return &Mover{deps: d}
}

// Outcome 是一次跟随尝试的结果（供日志、Web 状态与测试断言）。
type Outcome struct {
	Moved     bool     // 音乐是否已搬到唤醒房间（经 GetGroups 核对后才为 true）
	Reason    string   // 中文原因（Moved 时为 "跟随到<房间>"）
	Song      MPDSong  // 搬家时的当前曲（没在放时为空）
	Position  float64  // 搬家时的已播位置（秒；直播流是已播秒数，无总时长）
	Duration  float64  // 总时长（秒；直播流为 0）
	Live      bool     // 是否直播流（Duration<=0）
	FromRooms []string // 搬家前听到音乐的房间
	ToRoom    string   // 唤醒房间
}

// Move 处理一次仲裁赢家的唤醒：该搬就搬，不该搬就只记日志。
// 失败只记日志、音乐留在原房间；调用方（main.go 仲裁回调）
// 应起 goroutine 调，不阻塞语音会话建立。
func (m *Mover) Move(_ context.Context, wakeDeviceID string) Outcome {
	if !m.enabled() {
		return Outcome{Reason: "follow-me 已关闭"}
	}
	if !m.mu.TryLock() {
		return Outcome{Reason: "已有跟随在进行"}
	}
	defer m.mu.Unlock()

	if m.deps.MPD == nil || m.deps.Snap == nil {
		return Outcome{Reason: "follow-me 未接 MPD/Snapcast"}
	}
	mpd := m.deps.MPD()
	if mpd == nil {
		return Outcome{Reason: "MPD 未连接"}
	}
	snap := m.deps.Snap()
	if snap == nil {
		return Outcome{Reason: "Snapcast 未连接"}
	}
	if strings.TrimSpace(m.deps.StreamID) == "" {
		return Outcome{Reason: "未配置音乐流"}
	}
	if m.deps.RoomOfDevice == nil || m.deps.DeviceOfSnapClient == nil {
		return Outcome{Reason: "未配置音箱映射"}
	}

	// (a) 查 MPD：状态 + 当前曲。没在放（pause/stop/空队列）
	// 或查不到，都不动——不要对着未知状态搬路由。
	st, err := mpd.Status()
	if err != nil {
		log.Printf("follow-me：读 MPD 状态失败，不搬家: %v", err)
		return Outcome{Reason: "读 MPD 状态失败"}
	}
	song, err := mpd.CurrentSong()
	if err != nil {
		// 曲名只是日志用：拿不到不拦搬家（状态已确认在放），
		// 但要记一条，免得事后日志里歌名对不上。
		log.Printf("follow-me：读当前曲失败（继续搬家）: %v", err)
		song = MPDSong{}
	}
	out := Outcome{
		Song: song, Position: st.Elapsed, Duration: st.Duration,
		Live: st.Duration <= 0,
	}
	if st.State != "play" {
		out.Reason = "没有在放音乐"
		return out
	}

	// 唤醒房间：没认领就无法归属，不搬。
	wakeRoom := m.deps.RoomOfDevice(wakeDeviceID)
	out.ToRoom = wakeRoom
	if wakeRoom == "" {
		out.Reason = "唤醒设备未认领房间"
		return out
	}

	groups, err := snap.GetGroups()
	if err != nil {
		log.Printf("follow-me：读 Snapcast 分组失败，不搬家: %v", err)
		out.Reason = "读 Snapcast 分组失败"
		return out
	}

	// 目标客户端：唤醒设备对应的 Snapcast 客户端（按 IP 映射），
	// 必须在分组里且 Connected，否则视为目标音箱离线。
	target, targetOK := m.findTargetClient(groups, wakeDeviceID)
	if !targetOK || !target.Connected {
		out.Reason = "目标房间音箱不在线"
		log.Printf("follow-me：%s（%s）的音箱不在线，音乐留在原房间", wakeRoom, wakeDeviceID)
		if m.deps.Speak != nil {
			m.deps.Speak(wakeDeviceID, "这间房的音箱还没连上，音乐还留在原房间。")
		}
		return out
	}

	// 当前听到音乐流的房间（只算房间已知的客户端）。
	musicGroup := findStreamGroup(groups, m.deps.StreamID)
	out.FromRooms = m.musicRooms(groups, musicGroup)
	decision := DecideFollowMe(DecideInput{
		Enabled: true, Playing: true, Live: out.Live,
		WakeRoom: wakeRoom, MusicRooms: out.FromRooms, TargetOnline: true,
	})
	if !decision.Move {
		out.Reason = decision.Reason
		return out
	}

	// (b)+(c) 路由层搬家：MPD 不停——停它会连 B 房间一起断。
	// 新的音乐分组 = 唤醒房间的全部音箱 + 原来就在音乐分组里
	// 且房间未知的客户端（不动它们）；其余已知在别的房间的
	// 音箱移出分组（= A 房间停播）。目标音箱显式加进去：
	// 即便它因快照时序暂时不在任何分组里，也要进音乐分组。
	keep := m.keepClients(groups, musicGroup, wakeRoom, target.ID)
	if musicGroup != nil {
		if err := snap.SetGroupClients(musicGroup.ID, keep); err != nil {
			log.Printf("follow-me：分组 %s 切客户端失败，不搬家: %v", musicGroup.ID, err)
			out.Reason = "切分组失败"
			return out
		}
	} else {
		// 没分组在播音乐流（MPD 在放但没人听）：把目标音箱
		// 所在分组收窄到唤醒房间、再切到音乐流。
		tg := findClientGroup(groups, target.ID)
		if tg == nil {
			out.Reason = "目标音箱不在任何分组"
			return out
		}
		if err := snap.SetGroupClients(tg.ID, keep); err != nil {
			log.Printf("follow-me：分组 %s 切客户端失败，不搬家: %v", tg.ID, err)
			out.Reason = "切分组失败"
			return out
		}
		if err := snap.SetGroupStream(tg.ID, m.deps.StreamID); err != nil {
			log.Printf("follow-me：分组 %s 切流 %s 失败: %v", tg.ID, m.deps.StreamID, err)
			out.Reason = "切流失败"
			return out
		}
	}

	// 核对：重读分组，目标音箱必须在播音乐流的分组里，
	// 且该分组里没有已知在别的房间的音箱；否则报未生效。
	if ok := m.verify(snap, wakeRoom, target.ID); !ok {
		out.Reason = "跟随未生效（分组验证失败）"
		return out
	}
	out.Moved = true
	out.Reason = decision.Reason
	log.Printf("follow-me：%q 从 %v 跟随到 %s（位置 %.1fs%s）",
		songTitle(song), out.FromRooms, wakeRoom, st.Elapsed, liveSuffix(out.Live))
	return out
}

// enabled 读总开关（nil 提供方 = 默认开启）。
func (m *Mover) enabled() bool {
	if m.deps.Enabled == nil {
		return true
	}
	return m.deps.Enabled()
}

// findTargetClient 在全部分组里找唤醒设备对应的 Snapcast 客户端。
func (m *Mover) findTargetClient(groups []SnapGroup, wakeDeviceID string) (SnapDevice, bool) {
	for _, g := range groups {
		for _, c := range g.Clients {
			if id, ok := m.deps.DeviceOfSnapClient(c); ok && id == wakeDeviceID {
				return c, true
			}
		}
	}
	return SnapDevice{}, false
}

// findStreamGroup 找正在播指定流的分组（没有返回 nil）。
func findStreamGroup(groups []SnapGroup, streamID string) *SnapGroup {
	for i := range groups {
		if groups[i].StreamID == streamID {
			return &groups[i]
		}
	}
	return nil
}

// findClientGroup 找包含指定客户端的分组（没有返回 nil）。
func findClientGroup(groups []SnapGroup, clientID string) *SnapGroup {
	for i := range groups {
		for _, c := range groups[i].Clients {
			if c.ID == clientID {
				return &groups[i]
			}
		}
	}
	return nil
}

// musicRooms 返回音乐分组里已知的房间（去重、排序；分组不存在时为空）。
func (m *Mover) musicRooms(groups []SnapGroup, musicGroup *SnapGroup) []string {
	if musicGroup == nil {
		return nil
	}
	seen := map[string]bool{}
	var rooms []string
	for _, c := range musicGroup.Clients {
		if id, ok := m.deps.DeviceOfSnapClient(c); ok {
			if room := m.deps.RoomOfDevice(id); room != "" && !seen[room] {
				seen[room] = true
				rooms = append(rooms, room)
			}
		}
	}
	sort.Strings(rooms)
	return rooms
}

// keepClients 算搬家后音乐分组应有的客户端列表：唤醒房间的全部
// 音箱（不论现在哪个分组）+ 原音乐分组里房间未知的客户端。
// 房间已知且在别的房间的音箱不在列表里（= 移出分组、停播）。
// targetID 是唤醒设备对应的 Snapcast 客户端，一定在列表里
// （即便它因快照时序暂时不在任何分组里）。
// 列表按客户端 ID 排序，保证同样的拓扑每次调出同样的参数。
func (m *Mover) keepClients(groups []SnapGroup, musicGroup *SnapGroup, wakeRoom, targetID string) []string {
	keep := map[string]bool{targetID: true}
	for _, g := range groups {
		for _, c := range g.Clients {
			id, ok := m.deps.DeviceOfSnapClient(c)
			if !ok {
				// 房间未知：只在它本来就在音乐分组里时保留。
				if musicGroup != nil && g.ID == musicGroup.ID {
					keep[c.ID] = true
				}
				continue
			}
			if m.deps.RoomOfDevice(id) == wakeRoom {
				keep[c.ID] = true
			}
		}
	}
	out := make([]string, 0, len(keep))
	for id := range keep {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// verify 重读分组核对搬家结果：目标音箱在播音乐流的分组里，
// 且该分组里没有已知在别的房间的音箱。核对读失败也算未通过。
func (m *Mover) verify(snap SnapRouting, wakeRoom, targetID string) bool {
	groups, err := snap.GetGroups()
	if err != nil {
		log.Printf("follow-me：搬家后验证读分组失败: %v", err)
		return false
	}
	g := findStreamGroup(groups, m.deps.StreamID)
	if g == nil {
		log.Printf("follow-me：搬家后验证失败：没有分组在播 %s", m.deps.StreamID)
		return false
	}
	found := false
	for _, c := range g.Clients {
		if c.ID == targetID {
			found = true
			continue
		}
		if id, ok := m.deps.DeviceOfSnapClient(c); ok {
			if room := m.deps.RoomOfDevice(id); room != "" && room != wakeRoom {
				log.Printf("follow-me：搬家后验证失败：分组 %s 里还有 %s 的音箱 %s", g.ID, room, c.ID)
				return false
			}
		}
	}
	if !found {
		log.Printf("follow-me：搬家后验证失败：目标音箱 %s 不在播 %s 的分组里", targetID, m.deps.StreamID)
		return false
	}
	return true
}

// songTitle 取歌名（日志用；Title 为空时退回 File）。
func songTitle(s MPDSong) string {
	if s.Title != "" {
		return s.Title
	}
	return s.File
}

// liveSuffix 是日志里的直播标注。
func liveSuffix(live bool) string {
	if live {
		return "，直播流"
	}
	return ""
}
