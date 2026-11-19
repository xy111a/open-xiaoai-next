// 留言式对讲（“叫一下客厅，吃饭了”）：一句话传话，不开实时通话。
//
// 与已有的实时对讲（FR-04，router.intercomCall + intercom.Manager）
// 的区别：实时对讲是“呼叫卧室”接通后双向传原声；这里是 Hub 解析出
// 目标房间 + 一句话，经 Piper TTS 后用现成的播报路径（speakText →
// playFrames：start_play → Opus 帧 → stop_play）播到目标房间的音箱。
// 客户端不需要新消息类型——start_play/裸 Opus 帧是提醒/回答已在用的协议。
//
// 支持的说法（触发词 + 房间 + 消息）：
//   - 叫一下客厅，吃饭了 / 叫一下客厅：吃饭了 / 叫一下客厅 吃饭了
//   - 广播到客厅：开会了 / 广播到客厅，开会了
//   - 告诉卧室，该睡觉了 / 告诉卧室该睡觉了（无分隔时按已知房间切分）
// 房间与消息之间用 ， 、 ： : , 或空格分隔；无分隔时只有已知房间
// （KnownRooms 里的最长前缀）才能切分，否则不识别，避免把
// “叫一下小明吃饭了”里的人名当成房间。
//
// 歧义处理：
//   - “告诉”+ 未知房间不认领，放行给 Agent——“告诉我，天气怎么样”
//     里的“我”不是房间；“叫一下/广播到”无歧义，未知房间时认领并
//     在发起方音箱上明说（“不知道哪个是书房”）。
//   - 叫一下/广播到但没说出消息（“叫一下客厅”）也认领，提示正确
//     说法；否则会掉进实时对讲分支回一句“找不到一下客厅的音箱”。
//   - 目标房间就是发起方所在房间：不向自己播报，回“直接喊一声就行了”。
//   - 目标房间已知但无在线设备：报“不在线”；未知房间：报“不知道哪个是”。
//
// 播报文本：发起方房间已知时前缀“<发起房间>说：”（目标方知道谁传的话），
// 否则播消息原文；目标房间全部在线设备都播（与提醒的 P2-27 同款）。
package router

import (
	"context"
	"slices"
	"strings"

	"github.com/huajun/open-xiaoai-next/hub/internal/registry"
)

// intercomMessageSeparators 是房间与消息之间的分隔符：
// 中文逗号/顿号/冒号、英文逗号/冒号、空格。STT 文本里逗号最常见。
const intercomMessageSeparators = "，、：:, "

// intercomMessageTriggers 是留言式对讲的触发词（按出现顺序匹配）。
var intercomMessageTriggers = []string{"叫一下", "广播到", "告诉"}

// intercomMessageTrigger 剥掉触发词：返回（触发词，剩余文本，是否命中）。
// 未命中时触发词为空串。
func intercomMessageTrigger(text string) (string, string, bool) {
	for _, trig := range intercomMessageTriggers {
		if rest, ok := strings.CutPrefix(text, trig); ok {
			return trig, strings.TrimSpace(rest), true
		}
	}
	return "", "", false
}

// ParseIntercomMessage 解析留言式对讲命令，返回（房间，消息，触发词，是否识别）。
// knownRooms 是 Hub 已知的房间名（注册表里出现过的房间），只用于
// 无分隔符时的切分（取最长前缀匹配）；传 nil/空时无分隔形式不识别。
func ParseIntercomMessage(text string, knownRooms []string) (room, message, trigger string, ok bool) {
	trig, rest, hit := intercomMessageTrigger(strings.TrimSpace(text))
	if !hit || rest == "" {
		return "", "", "", false
	}
	// 先找分隔符：房间是第一个分隔符之前，消息是之后。
	for i, r := range rest {
		if strings.ContainsRune(intercomMessageSeparators, r) {
			room = normalizeIntercomRoom(rest[:i])
			message = strings.TrimSpace(rest[i+len(string(r)):])
			if room == "" || message == "" {
				return "", "", "", false
			}
			if err := registry.ValidateRoomName(room); err != nil {
				return "", "", "", false
			}
			return room, message, trig, true
		}
	}
	// 无分隔符：只有已知房间的最长前缀能切分才识别。
	best := ""
	for _, kr := range knownRooms {
		if kr != "" && strings.HasPrefix(rest, kr) && len(rest) > len(kr) && len(kr) > len(best) {
			best = kr
		}
	}
	if best == "" {
		return "", "", "", false
	}
	room = best
	message = strings.TrimSpace(rest[len(best):])
	if message == "" {
		return "", "", "", false
	}
	return room, message, trig, true
}

// normalizeIntercomRoom 规范化房间名：去空白、去“音箱/房间”后缀
// （与实时对讲 intercomCall 同款，如“客厅音箱”→“客厅”）。
func normalizeIntercomRoom(room string) string {
	room = strings.TrimSpace(room)
	room = strings.TrimSuffix(room, "音箱")
	room = strings.TrimSuffix(room, "房间")
	return strings.TrimSpace(room)
}

// knownRooms 取已知的房间名（Deps.KnownRooms 未配时为空）。
func (r *Router) knownRooms() []string {
	r.depsMu.RLock()
	defer r.depsMu.RUnlock()
	if r.deps.KnownRooms == nil {
		return nil
	}
	return r.deps.KnownRooms()
}

// devicesByRoom 取某房间全部在线设备的 ID（Deps.DevicesByRoom 未配时为空）。
func (r *Router) devicesByRoom(room string) []string {
	r.depsMu.RLock()
	defer r.depsMu.RUnlock()
	if r.deps.DevicesByRoom == nil {
		return nil
	}
	return r.deps.DevicesByRoom(room)
}

// intercomMessage 处理留言式对讲（见本文件头注释）：解析 → 按房间
// 找目标设备 → Speak（TTS 播报）到目标房间，回复只报给发起方。
// 不识别时返回 done=false，落到后续路由（实时对讲/Agent）。
func (r *Router) intercomMessage(_ context.Context, deviceID, text string) (string, bool) {
	rooms := r.knownRooms()
	room, msg, trigger, ok := ParseIntercomMessage(text, rooms)
	if !ok {
		// 叫一下/广播到是无歧义的传话触发词：解析失败也认领，
		// 提示正确说法；否则会掉进实时对讲分支回一句
		// “找不到一下客厅的音箱”，用户摸不着头脑。
		if trig, _, hit := intercomMessageTrigger(strings.TrimSpace(text)); hit &&
			(trig == "叫一下" || trig == "广播到") {
			return "想传话到哪个房间、说什么？比如“叫一下客厅，吃饭了”。", true
		}
		return "", false
	}
	// “告诉”+ 未知房间：很可能是“告诉我……”类闲聊，放行给 Agent。
	if trigger == "告诉" && !slices.Contains(rooms, room) {
		return "", false
	}
	originRoom := ""
	if r.deps.RoomOfDevice != nil {
		originRoom = r.deps.RoomOfDevice(deviceID)
	}
	// 同房间：向自己传话没意义，直接喊一声就行。
	if originRoom != "" && originRoom == room {
		return "你就在" + room + "，直接喊一声就行了。", true
	}
	targets := r.devicesByRoom(room)
	if len(targets) == 0 {
		if slices.Contains(rooms, room) {
			return room + "的音箱现在不在线，稍后再试。", true
		}
		return "不知道哪个是" + room + "，先让那边的音箱说“我在" + room + "”认领一下房间。", true
	}
	if r.deps.Speak == nil {
		return "对讲消息没接好（播报未配置），先检查 Hub 的 TTS。", true
	}
	// 播报文本：发起方房间已知时前缀“<房间>说：”，目标方知道谁传的话。
	spoken := msg
	if originRoom != "" {
		spoken = originRoom + "说：" + msg
	}
	for _, to := range targets {
		r.deps.Speak(to, spoken)
	}
	return "已经告诉" + room + "了。", true
}
