// Package alerts 是摄像头/安全事件主动播报（FR-10）：
// HA 的 state_changed 事件里，漏水/烟雾/水浸类 binary_sensor 变 on 时，
// 生成一句播报文本（含量事件类型与位置），勿扰时段内跳过。
//
// 播报判定是纯函数 ShouldAnnounce，可直接单测；
// Watcher 把它接到 ha.Client 的订阅上（main.go 用它）。
//
// 注意真 HA 事件信封：entity_id 与 new_state 在 event.data 下面
// （{"event": {"data": {"entity_id": ..., "new_state": {"state": "on"}}}}），
// 不是扁平的 ev["entity_id"]；ShouldAnnounce 两种形状都认，
// 扁平形状只作兼容（旧 main.go 接线与早期测试用）。
package alerts

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/huajun/open-xiaoai-next/hub/internal/ha"
)

// Kind 是安全事件类型。
type Kind string

const (
	// KindLeak 漏水（leak）。
	KindLeak Kind = "leak"
	// KindSmoke 烟雾（smoke）。
	KindSmoke Kind = "smoke"
	// KindWater 水浸（water）。
	KindWater Kind = "water"
)

// Chinese 返回事件类型的中文名（播报文本用）。
func (k Kind) Chinese() string {
	switch k {
	case KindLeak:
		return "漏水"
	case KindSmoke:
		return "烟雾"
	case KindWater:
		return "水浸"
	}
	return ""
}

// KindFor 按 entity_id 判断是不是安全类实体（P2-26）：
// 必须同时满足两点才算：
//  1. domain 是 binary_sensor（sensor.water_heater_status 这类
//     热水器状态实体不再误判为水浸）；
//  2. 对象名按 "_" 分词后含 leak/smoke/water 词元（子串匹配会把
//     water_heater 当水浸；词元匹配 + 下面的排除表兜底）。
//
// 含 heater/battery/temperature/humidity 等非报警词元的一律不算。
func KindFor(entityID string) (Kind, bool) {
	parts := strings.SplitN(entityID, ".", 2)
	if len(parts) != 2 || parts[0] != "binary_sensor" {
		return "", false
	}
	tokens := strings.Split(parts[1], "_")
	tokenSet := map[string]bool{}
	for _, tok := range tokens {
		tokenSet[tok] = true
	}
	for _, excl := range []string{"heater", "battery", "temperature", "humidity", "tamper"} {
		if tokenSet[excl] {
			return "", false
		}
	}
	switch {
	case tokenSet["leak"]:
		return KindLeak, true
	case tokenSet["smoke"]:
		return KindSmoke, true
	case tokenSet["water"]:
		return KindWater, true
	}
	return "", false
}

// areaNames 是 entity_id 里的房间词 → 中文位置。
var areaNames = map[string]string{
	"kitchen":     "厨房",
	"bathroom":    "浴室",
	"bedroom":     "卧室",
	"living_room": "客厅",
	"livingroom":  "客厅",
	"basement":    "地下室",
	"laundry":     "洗衣房",
	"garage":      "车库",
	"hallway":     "走廊",
	"toilet":      "卫生间",
	"balcony":     "阳台",
	"study":       "书房",
	"dining_room": "餐厅",
	"kids_room":   "儿童房",
	"guest_room":  "客房",
	"utility":     "设备间",
	"attic":       "阁楼",
	"entrance":    "玄关",
	"storeroom":   "储藏室",
}

// kindTokens 是 entity_id 里表示“类型/传感器本身”的词，定位时去掉。
var kindTokens = map[string]bool{
	"leak": true, "smoke": true, "water": true,
	"sensor": true, "detector": true, "alarm": true, "binary": true,
}

// LocationOf 从 entity_id 里取位置，例如 binary_sensor.kitchen_leak → “厨房”。
// 房间词不在表里时返回原词本身（仍能指认位置）；完全取不到时返回“家里”。
func LocationOf(entityID string) string {
	rest := entityID
	if i := strings.Index(rest, "."); i >= 0 {
		rest = rest[i+1:]
	}
	// 先试整段匹配（含下划线的房间名，如 living_room）。
	if name, ok := areaNames[rest]; ok {
		return name
	}
	parts := strings.Split(rest, "_")
	var area []string
	for _, p := range parts {
		if kindTokens[p] {
			continue
		}
		area = append(area, p)
	}
	if len(area) == 0 {
		return "家里"
	}
	key := strings.Join(area, "_")
	if name, ok := areaNames[key]; ok {
		return name
	}
	return key
}

// AnnouncementText 生成播报文本，例如：
// binary_sensor.kitchen_leak → “注意，厨房漏水报警了，请检查一下。”
// 非安全类实体返回 ok=false。
func AnnouncementText(entityID string) (string, bool) {
	kind, ok := KindFor(entityID)
	if !ok {
		return "", false
	}
	return "注意，" + LocationOf(entityID) + kind.Chinese() + "报警了，请检查一下。", true
}

// InDoNotDisturb 判断是否在勿扰时段（格式 "23:00-07:00"，可跨午夜）。
// P2-25：空串表示未配勿扰，返回 false；非空但格式非法时返回 true
// （fail-closed：按“在勿扰”保守处理并记 warn，深夜不再误播报）。
// config.Validate 已在启动时校验格式，这里的保守分支只兜底
// 未经 Validate 的直接调用。
func InDoNotDisturb(spec string, now time.Time) bool {
	if strings.TrimSpace(spec) == "" {
		return false
	}
	parts := strings.Split(spec, "-")
	if len(parts) != 2 {
		log.Printf("alerts: 勿扰时段格式非法 %q，按在勿扰处理", spec)
		return true
	}
	start, err1 := time.Parse("15:04", strings.TrimSpace(parts[0]))
	end, err2 := time.Parse("15:04", strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil {
		log.Printf("alerts: 勿扰时段格式非法 %q，按在勿扰处理", spec)
		return true
	}
	minutes := now.Hour()*60 + now.Minute()
	s := start.Hour()*60 + start.Minute()
	e := end.Hour()*60 + end.Minute()
	if s <= e {
		return minutes >= s && minutes < e
	}
	return minutes >= s || minutes < e
}

// ShouldAnnounce 对一条 state_changed 事件做播报判定：
// 安全类实体变 on、且不在勿扰时段 → 返回播报文本；否则 ok=false。
// 事件形状两种都认：真 HA 信封 ev["data"]["entity_id"] / ev["data"]["new_state"]，
// 以及扁平的 ev["entity_id"] / ev["new_state"]。
func ShouldAnnounce(ev map[string]any, dndSpec string, now time.Time) (string, bool) {
	data, _ := ev["data"].(map[string]any)
	entityID, _ := ev["entity_id"].(string)
	if entityID == "" && data != nil {
		entityID, _ = data["entity_id"].(string)
	}
	if entityID == "" {
		return "", false
	}
	var newState map[string]any
	if data != nil {
		newState, _ = data["new_state"].(map[string]any)
	}
	if newState == nil {
		newState, _ = ev["new_state"].(map[string]any)
	}
	if newState == nil || newState["state"] != "on" {
		return "", false
	}
	text, ok := AnnouncementText(entityID)
	if !ok {
		return "", false
	}
	if InDoNotDisturb(dndSpec, now) {
		return "", false
	}
	return text, true
}

// Watcher 把 ShouldAnnounce 接到 HA 的 state_changed 订阅上，
// 判定要播报时调 speak（main.go 里 speak 是“向所有在线设备 TTS 播报”）。
type Watcher struct {
	client *ha.Client
	dnd    string
	speak  func(text string)

	// mu 保护 client/started：看门狗 Rebind 与事件读循环并发访问。
	mu      sync.Mutex
	started bool

	// nowMu 保护时钟：读循环 goroutine 读 Now，调用方可能随后换时钟，
	// 直接读写字段会数据竞争，所以经 SetNow/now 加锁访问。
	nowMu sync.RWMutex
	nowFn func() time.Time
}

// New 新建 Watcher（默认时钟 time.Now）。
func New(client *ha.Client, dndSpec string, speak func(string)) *Watcher {
	return &Watcher{client: client, dnd: dndSpec, speak: speak, nowFn: time.Now}
}

// SetNow 换时钟（测试注入固定时间用）。
func (w *Watcher) SetNow(fn func() time.Time) {
	w.nowMu.Lock()
	defer w.nowMu.Unlock()
	w.nowFn = fn
}

func (w *Watcher) now() time.Time {
	w.nowMu.RLock()
	defer w.nowMu.RUnlock()
	if w.nowFn != nil {
		return w.nowFn()
	}
	return time.Now()
}

// handler 是 state_changed 事件处理闭包（Start 与 Rebind 共用）。
func (w *Watcher) handler() func(ev map[string]any) {
	return func(ev map[string]any) {
		if text, ok := ShouldAnnounce(ev, w.dnd, w.now()); ok && w.speak != nil {
			w.speak(text)
		}
	}
}

// Start 订阅 state_changed，开始监听。
func (w *Watcher) Start() error {
	w.mu.Lock()
	w.started = true
	c := w.client
	w.mu.Unlock()
	if c == nil {
		return fmt.Errorf("alerts: HA 未连接，无法订阅")
	}
	return c.SubscribeEvents("state_changed", w.handler())
}

// Rebind 在 HA 断线重连后换客户端并重新订阅（P1-27：看门狗调）。
// 旧客户端的订阅随其连接关闭而失效，必须在新客户端上重订，
// 否则 HA 重连后摄像头监视就静默失效了。未 Start 过时只换客户端。
func (w *Watcher) Rebind(c *ha.Client) error {
	if c == nil {
		return fmt.Errorf("alerts: Rebind 的 HA 客户端为 nil")
	}
	w.mu.Lock()
	w.client = c
	started := w.started
	w.mu.Unlock()
	if !started {
		return nil
	}
	return c.SubscribeEvents("state_changed", w.handler())
}
