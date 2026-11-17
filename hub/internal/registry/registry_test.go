package registry

import "testing"

func TestUpsertPreservesRoom(t *testing.T) {
	r := New()
	r.Upsert(Device{ID: "lx06-1", Model: "lx06", IP: "192.168.1.2", Room: "客厅"})
	// 同一设备重新发现（IP 变了、Room 未上报）时，已认领的房间不能丢。
	r.Upsert(Device{ID: "lx06-1", Model: "lx06", IP: "192.168.1.5"})
	d, ok := r.Get("lx06-1")
	if !ok {
		t.Fatal("Upsert 后 Get 不到")
	}
	if d.Room != "客厅" {
		t.Fatalf("Upsert 丢了已认领房间，实际 %q", d.Room)
	}
	if d.IP != "192.168.1.5" {
		t.Fatalf("Upsert 应更新 IP，实际 %q", d.IP)
	}
}

// 缺字段的 Upsert 不能抹掉已有字段、不能改在线状态（回归：OnStatus 只带 Version 时曾抹掉 Model/IP）。
func TestUpsertMergeDoesNotWipe(t *testing.T) {
	r := New()
	r.Upsert(Device{ID: "lx06-1", Model: "lx06", IP: "192.168.1.2", Room: "客厅", Version: "1.94.13"})
	r.SetOnline("lx06-1", true)
	// 只带 Version 的 Upsert（模拟 OnStatus）。
	r.Upsert(Device{ID: "lx06-1", Version: "1.94.14"})
	d, _ := r.Get("lx06-1")
	if d.Model != "lx06" || d.IP != "192.168.1.2" || d.Room != "客厅" {
		t.Fatalf("缺字段 Upsert 抹掉了已有字段: %+v", d)
	}
	if d.Version != "1.94.14" {
		t.Fatalf("Version 应更新到 1.94.14，实际 %q", d.Version)
	}
	if !d.Online {
		t.Fatal("Upsert 不应改在线状态")
	}
	// 不带 Online 的 Upsert（模拟配置播种/自动发现）不能把在线设备标离线。
	r.Upsert(Device{ID: "lx06-1", Model: "lx06", IP: "192.168.1.2"})
	d, _ = r.Get("lx06-1")
	if !d.Online {
		t.Fatal("不带 Online 的 Upsert 不应把在线设备标离线")
	}
	if d.LastSeen.IsZero() {
		t.Fatal("Upsert 后 LastSeen 不应为零值")
	}
}

func TestSetOnlineAndRoom(t *testing.T) {
	r := New()
	r.Upsert(Device{ID: "lx06-1", Model: "lx06", IP: "192.168.1.2"})
	r.Upsert(Device{ID: "oh2p-1", Model: "oh2p", IP: "192.168.1.3"})
	r.SetOnline("lx06-1", true)
	if got := r.Online(); len(got) != 1 || got[0].ID != "lx06-1" {
		t.Fatalf("Online 应只有 lx06-1，实际 %+v", got)
	}
	if !r.SetRoom("oh2p-1", "卧室") {
		t.Fatal("SetRoom 应返回 true")
	}
	// ByRoom 只返回在线设备（离线设备接不到语音），所以先上线再查。
	r.SetOnline("oh2p-1", true)
	if got := r.ByRoom("卧室"); got == nil || got.ID != "oh2p-1" {
		t.Fatalf("ByRoom(卧室) 应找到 oh2p-1，实际 %+v", got)
	}
	if r.SetRoom("不存在", "书房") {
		t.Fatal("SetRoom 不存在的设备应返回 false")
	}
	if got := r.ByRoom("书房"); got != nil {
		t.Fatalf("ByRoom(书房) 应为 nil，实际 %+v", got)
	}
	if len(r.All()) != 2 {
		t.Fatalf("All 应有 2 台，实际 %d", len(r.All()))
	}
	r.SetOnline("lx06-1", false)
	if got := r.Online(); len(got) != 1 || got[0].ID != "oh2p-1" {
		t.Fatalf("lx06-1 下线后 Online 应只剩 oh2p-1，实际 %+v", got)
	}
	r.SetOnline("oh2p-1", false)
	if len(r.Online()) != 0 {
		t.Fatal("两台都下线后 Online 应为空")
	}
}
