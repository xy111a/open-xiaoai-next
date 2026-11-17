package intercom

import (
	"math"
	"testing"
	"time"
)

func TestHalfDuplexBusy(t *testing.T) {
	m := New(nil)
	if !m.Start("lx06-1", "oh2p-1") {
		t.Fatal("首次 Start 应成功")
	}
	// 半双工：已有会话时，另一方发起应被拒绝。
	if m.Start("oh2p-1", "lx06-1") {
		t.Fatal("已有会话时反向 Start 应被拒绝")
	}
	if m.Active() != 1 {
		t.Fatalf("Active 应为 1，实际 %d", m.Active())
	}
	if !m.End("lx06-1") {
		t.Fatal("End 发起方应成功")
	}
	if m.Active() != 0 {
		t.Fatalf("End 后 Active 应为 0，实际 %d", m.Active())
	}
	if m.End("lx06-1") {
		t.Fatal("重复 End 应返回 false")
	}
}

func TestForwardRelay(t *testing.T) {
	var relayed []string
	m := New(func(to string, opus []byte) error {
		relayed = append(relayed, to)
		return nil
	})
	if !m.Start("lx06-1", "oh2p-1") {
		t.Fatal("Start 失败")
	}
	to, toAll, err := m.Forward("lx06-1", []byte{0x01, 0x02})
	if err != nil || toAll || to != "oh2p-1" {
		t.Fatalf("Forward 应中继到 oh2p-1（toAll=false），实际 to=%q toAll=%v err=%v", to, toAll, err)
	}
	if len(relayed) != 1 || relayed[0] != "oh2p-1" {
		t.Fatalf("Relay 回调应收到 oh2p-1，实际 %v", relayed)
	}
	// 非会话方发音频：不中继、不报错（静默丢弃，调用方按“不在对讲中”走语音管线）。
	if to, toAll, err := m.Forward("陌生设备", []byte{0x01}); err != nil || to != "" || toAll {
		t.Fatalf("非会话方 Forward 应返回 (\"\", false, nil)，实际 to=%q toAll=%v err=%v", to, toAll, err)
	}
}

func TestBroadcast(t *testing.T) {
	m := New(nil)
	if !m.StartBroadcast("lx06-1") {
		t.Fatal("StartBroadcast 应成功")
	}
	if m.Active() != 1 {
		t.Fatalf("广播会话 Active 应为 1，实际 %d", m.Active())
	}
}

func TestSweepIdleTimeout(t *testing.T) {
	m := New(nil)
	if !m.Start("lx06-1", "oh2p-1") {
		t.Fatal("Start 失败")
	}
	// 1 秒内扫荡：会话刚建立，不应超时。
	if got := m.Sweep(time.Now().Add(time.Second)); len(got) != 0 {
		t.Fatalf("1s 内不应扫出超时，实际 %v", got)
	}
	// 10 秒后扫荡（空闲超时 5s）：应被清掉。
	if got := m.Sweep(time.Now().Add(10 * time.Second)); len(got) != 1 {
		t.Fatalf("10s 后应扫出 1 个超时会话，实际 %v", got)
	}
	if m.Active() != 0 {
		t.Fatalf("超时清掉后 Active 应为 0，实际 %d", m.Active())
	}
}

// ---- 全双工（Phase 4，任务 B）----

func TestFullDuplexSwitchPreservesSession(t *testing.T) {
	m := New(nil)
	if !m.Start("lx06-1", "oh2p-1") {
		t.Fatal("Start 失败")
	}
	mode, ok := m.ModeOf("lx06-1")
	if !ok || mode != HalfDuplex {
		t.Fatalf("新建会话默认应为半双工，实际 mode=%v ok=%v", mode, ok)
	}
	// 切到全双工：会话不丢。
	if !m.SetFullDuplex("lx06-1") {
		t.Fatal("SetFullDuplex 应成功")
	}
	mode, ok = m.ModeOf("lx06-1")
	if !ok || mode != FullDuplex {
		t.Fatalf("切换后应为全双工，实际 mode=%v ok=%v", mode, ok)
	}
	if m.Active() != 1 {
		t.Fatalf("切换后会话数应仍为 1，实际 %d", m.Active())
	}
	// 切回半双工，再切回全双工：往返切换不丢会话。
	if !m.SetHalfDuplex("lx06-1") {
		t.Fatal("SetHalfDuplex 应成功")
	}
	if mode, _ := m.ModeOf("lx06-1"); mode != HalfDuplex {
		t.Fatalf("切回后应为半双工，实际 %v", mode)
	}
	if !m.SetFullDuplex("lx06-1") {
		t.Fatal("再次 SetFullDuplex 应成功")
	}
	if m.Active() != 1 {
		t.Fatalf("往返切换后会话数应仍为 1，实际 %d", m.Active())
	}
	// 不存在的会话 / 广播会话不能切全双工。
	if m.SetFullDuplex("不存在的设备") {
		t.Fatal("不存在的会话 SetFullDuplex 应返回 false")
	}
	m2 := New(nil)
	if !m2.StartBroadcast("lx06-1") {
		t.Fatal("StartBroadcast 失败")
	}
	if m2.SetFullDuplex("lx06-1") {
		t.Fatal("广播会话 SetFullDuplex 应返回 false（广播是单向的）")
	}
	if mode, _ := m2.ModeOf("lx06-1"); mode != HalfDuplex {
		t.Fatalf("广播会话应保持半双工，实际 %v", mode)
	}
}

func TestFullDuplexBidirectionalForward(t *testing.T) {
	var relayed []string
	m := New(func(to string, opus []byte) error {
		relayed = append(relayed, to)
		return nil
	})
	if !m.Start("lx06-1", "oh2p-1") {
		t.Fatal("Start 失败")
	}
	// 半双工：目标方发来的帧不中继。
	if to, _, err := m.Forward("oh2p-1", []byte{0x01}); err != nil || to != "" {
		t.Fatalf("半双工下目标方 Forward 应返回 (\"\", false, nil)，实际 to=%q err=%v", to, err)
	}
	if len(relayed) != 0 {
		t.Fatalf("半双工下目标方音频不应被中继，实际 %v", relayed)
	}
	// 切全双工：双向都中继。
	if !m.SetFullDuplex("lx06-1") {
		t.Fatal("SetFullDuplex 失败")
	}
	if to, _, err := m.Forward("lx06-1", []byte{0x01}); err != nil || to != "oh2p-1" {
		t.Fatalf("全双工发起方应中继到 oh2p-1，实际 to=%q err=%v", to, err)
	}
	if to, _, err := m.Forward("oh2p-1", []byte{0x02}); err != nil || to != "lx06-1" {
		t.Fatalf("全双工目标方应中继回 lx06-1，实际 to=%q err=%v", to, err)
	}
	if len(relayed) != 2 || relayed[0] != "oh2p-1" || relayed[1] != "lx06-1" {
		t.Fatalf("双向中继顺序应为 [oh2p-1 lx06-1]，实际 %v", relayed)
	}
	// 切回半双工：目标方再次静默。
	if !m.SetHalfDuplex("lx06-1") {
		t.Fatal("SetHalfDuplex 失败")
	}
	if to, _, err := m.Forward("oh2p-1", []byte{0x03}); err != nil || to != "" {
		t.Fatalf("切回半双工后目标方应不再中继，实际 to=%q err=%v", to, err)
	}
	// 发起方仍可中继，会话本身没丢。
	if to, _, err := m.Forward("lx06-1", []byte{0x04}); err != nil || to != "oh2p-1" {
		t.Fatalf("切回半双工后发起方仍应中继，实际 to=%q err=%v", to, err)
	}
}

// TestEchoCancelSynthetic：与 aec.TestEchoSuppressionSynthetic 同一判定方式
// （合成回声：mic = 0.5 * ref(延迟 40 点) + 微弱语音；残余 RMS 必须 < 输入的 50%），
// 但走 intercom 的回声路径（会话全双工 + EchoCancel），且按 20ms/帧（320 点）分帧喂入。
func TestEchoCancelSynthetic(t *testing.T) {
	m := New(nil)
	if !m.Start("lx06-1", "oh2p-1") {
		t.Fatal("Start 失败")
	}
	const total = 16000
	ref := make([]float64, total)
	mic := make([]float64, total)
	for i := range ref {
		ref[i] = 8000 * math.Sin(2*math.Pi*300*float64(i)/16000)
	}
	for i := 40; i < total; i++ {
		mic[i] = 0.5*ref[i-40] + 200*math.Sin(2*math.Pi*1000*float64(i)/16000)
	}
	// 半双工下 EchoCancel 不做消除：原样返回。
	out := m.EchoCancel("lx06-1", ref[:320], mic[:320])
	for i, v := range out {
		if v != mic[i] {
			t.Fatalf("半双工下 EchoCancel 应原样返回 mic，第 %d 点不同", i)
		}
	}
	// 切全双工，先让 AEC 适应 0.5 秒（分帧喂）。
	if !m.SetFullDuplex("lx06-1") {
		t.Fatal("SetFullDuplex 失败")
	}
	for i := 0; i < 8000; i += 320 {
		m.EchoCancel("lx06-1", ref[i:i+320], mic[i:i+320])
	}
	// 再测 0.5 秒的残余。
	var inSq, outSq float64
	for i := 8000; i < total; i += 320 {
		res := m.EchoCancel("lx06-1", ref[i:i+320], mic[i:i+320])
		for j, v := range res {
			inSq += mic[i+j] * mic[i+j]
			outSq += v * v
		}
	}
	inRMS := math.Sqrt(inSq / 8000)
	outRMS := math.Sqrt(outSq / 8000)
	t.Logf("全双工回声路径：输入 RMS %.1f → 残余 RMS %.1f（%.1f%%）", inRMS, outRMS, 100*outRMS/inRMS)
	if outRMS > 0.5*inRMS {
		t.Fatalf("全双工回声抑制不足：残余 %.1f%% > 50%%", 100*outRMS/inRMS)
	}
	// 切回半双工后 EchoCancel 恢复原样返回（AEC 已丢弃）。
	if !m.SetHalfDuplex("lx06-1") {
		t.Fatal("SetHalfDuplex 失败")
	}
	out = m.EchoCancel("lx06-1", ref[:320], mic[:320])
	for i, v := range out {
		if v != mic[i] {
			t.Fatalf("切回半双工后 EchoCancel 应原样返回 mic，第 %d 点不同", i)
		}
	}
}

// TestFullDuplexIdleSweepKeepsSessionWhileTargetTalks：
// 全双工下目标方持续发音频也算会话活跃，不应被空闲扫荡清掉。
func TestFullDuplexIdleSweepKeepsSessionWhileTargetTalks(t *testing.T) {
	m := New(func(string, []byte) error { return nil })
	if !m.Start("lx06-1", "oh2p-1") {
		t.Fatal("Start 失败")
	}
	if !m.SetFullDuplex("lx06-1") {
		t.Fatal("SetFullDuplex 失败")
	}
	// 只有目标方在说话：4 秒后扫荡，不应清掉（LastFrame 被目标方帧刷新）。
	if _, _, err := m.Forward("oh2p-1", []byte{0x01}); err != nil {
		t.Fatal(err)
	}
	if got := m.Sweep(time.Now().Add(4 * time.Second)); len(got) != 0 {
		t.Fatalf("目标方 4s 内有帧，不应扫出超时，实际 %v", got)
	}
	if m.Active() != 1 {
		t.Fatalf("扫荡后会话应仍在，实际 Active=%d", m.Active())
	}
}
