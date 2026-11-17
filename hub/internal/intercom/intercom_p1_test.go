package intercom

// P1-15 回归测试：EndFor 双向结束会话。

import "testing"

func TestEndForBothDirections(t *testing.T) {
	m := New(nil)
	if !m.Start("lx06-1", "oh2p-1") {
		t.Fatal("Start 应成功")
	}
	// 目标方断线：EndFor(oh2p-1) 应结束这场会话（只调 End(from) 会漏）。
	if !m.EndFor("oh2p-1") {
		t.Fatal("EndFor(目标方) 应结束会话")
	}
	if m.Active() != 0 {
		t.Fatalf("目标方断线后应无进行中会话，实际 %d", m.Active())
	}
	// 无会话时返回 false。
	if m.EndFor("lx06-1") {
		t.Fatal("无会话时 EndFor 应返回 false")
	}
	// 发起方断线：同样结束。
	if !m.Start("lx06-1", "oh2p-1") {
		t.Fatal("Start 应成功")
	}
	if !m.EndFor("lx06-1") {
		t.Fatal("EndFor(发起方) 应结束会话")
	}
	if m.Active() != 0 {
		t.Fatalf("发起方断线后应无进行中会话，实际 %d", m.Active())
	}
}

func TestEndForBroadcastOnlyInitiator(t *testing.T) {
	m := New(nil)
	if !m.StartBroadcast("lx06-1") {
		t.Fatal("StartBroadcast 应成功")
	}
	// 广播的某个收听方断线：不应结束整场广播。
	if m.EndFor("oh2p-1") {
		t.Fatal("收听方断线不应结束广播")
	}
	if m.Active() != 1 {
		t.Fatalf("广播应仍在，实际 %d 场", m.Active())
	}
	// 发起方断线：广播结束。
	if !m.EndFor("lx06-1") {
		t.Fatal("EndFor(发起方) 应结束广播")
	}
	if m.Active() != 0 {
		t.Fatalf("发起方断线后广播应结束，实际 %d 场", m.Active())
	}
}
