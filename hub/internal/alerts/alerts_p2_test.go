package alerts

import (
	"testing"
	"time"
)

// P2-26：安全判定不再只用子串匹配——
// sensor 域的热水器/烟雾探测器电池不再误判为水浸/烟雾。
func TestKindForRequiresBinarySensorDomainP2(t *testing.T) {
	shouldMatch := []struct {
		entity string
		kind   Kind
	}{
		{"binary_sensor.kitchen_leak", KindLeak},
		{"binary_sensor.living_room_smoke", KindSmoke},
		{"binary_sensor.bathroom_water", KindWater},
		{"binary_sensor.basement_water_sensor", KindWater},
	}
	for _, c := range shouldMatch {
		k, ok := KindFor(c.entity)
		if !ok || k != c.kind {
			t.Fatalf("%s 应判为 %q，实际 kind=%q ok=%v", c.entity, c.kind, k, ok)
		}
	}
	shouldNot := []string{
		"sensor.water_heater_status",        // 热水器状态，不是水浸
		"sensor.smoke_detector_battery",     // 烟雾探测器电池，不是烟雾报警
		"binary_sensor.water_heater_status", // 即使 binary_sensor，heater 也排除
		"binary_sensor.smoke_detector_battery",
		"light.living_room",
		"binary_sensor.front_door",
		"sensor.kitchen_leak", // 非 binary_sensor 域
	}
	for _, e := range shouldNot {
		if k, ok := KindFor(e); ok {
			t.Fatalf("%s 不应判为安全事件，实际 kind=%q", e, k)
		}
	}
}

// P2-26：热水器状态变 on 不再播报“水浸”。
func TestShouldAnnounceIgnoresWaterHeaterP2(t *testing.T) {
	day := time.Date(2026, 11, 17, 14, 0, 0, 0, time.Local)
	for _, e := range []string{"sensor.water_heater_status", "binary_sensor.water_heater_status"} {
		if text, ok := ShouldAnnounce(nestedEvent(e, "on"), "23:00-07:00", day); ok {
			t.Fatalf("%s 变 on 不应播报，实际 %q", e, text)
		}
	}
}
