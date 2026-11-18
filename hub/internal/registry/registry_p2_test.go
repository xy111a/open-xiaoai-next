package registry

import "testing"

// P2-3：房间名校验——长度、控制字符、非法字符。
func TestValidateRoomNameP2(t *testing.T) {
	for _, ok := range []string{"客厅", "主卧", "living room", "Room-1", "书房·东"} {
		if err := ValidateRoomName(ok); err != nil {
			t.Fatalf("房间名 %q 应合法，实际 %v", ok, err)
		}
	}
	for _, bad := range []string{
		"", "   ",
		"客厅\n伪造日志行", "客厅\t制表符",
		"这是一个超过十六个字符的房间名字啊啊啊",
		"客厅<script>", "客厅/卧室",
	} {
		if err := ValidateRoomName(bad); err == nil {
			t.Fatalf("房间名 %q 应被拒绝", bad)
		}
	}
}

// P2-3：超长自由文本字段丢弃。
func TestSanitizeFieldP2(t *testing.T) {
	if got := SanitizeField("1.0.0"); got != "1.0.0" {
		t.Fatalf("正常 Version 不应被丢，实际 %q", got)
	}
	long := ""
	for i := 0; i < 100; i++ {
		long += "x"
	}
	if got := SanitizeField(long); got != "" {
		t.Fatalf("超长字段应丢弃为空，实际 %q", got)
	}
}

// P2-51：FillRoomIfEmpty 只在无房间时填充；ClearRoom 能清空房间。
func TestFillRoomIfEmptyAndClearRoomP2(t *testing.T) {
	r := New()
	r.Upsert(Device{ID: "lx06-1-1-1-1", Model: "lx06", IP: "1.1.1.1"})
	if !r.FillRoomIfEmpty("lx06-1-1-1-1", "客厅") {
		t.Fatal("无房间时应能填充")
	}
	if r.FillRoomIfEmpty("lx06-1-1-1-1", "卧室") {
		t.Fatal("已有房间时 hello 上报不得覆盖认领")
	}
	d, _ := r.Get("lx06-1-1-1-1")
	if d.Room != "客厅" {
		t.Fatalf("房间应仍为客厅，实际 %q", d.Room)
	}
	if !r.ClearRoom("lx06-1-1-1-1") {
		t.Fatal("ClearRoom 应成功")
	}
	d, _ = r.Get("lx06-1-1-1-1")
	if d.Room != "" {
		t.Fatalf("房间应已清空，实际 %q", d.Room)
	}
	// SetRoom 拒绝非法房间名。
	if r.SetRoom("lx06-1-1-1-1", "客厅\n伪造") {
		t.Fatal("SetRoom 应拒绝含换行符的房间名")
	}
}

// P2-27：ByRoom 确定性（同房间多设备固定取 ID 最小者）；
// ByRoomAll 返回全部在线设备。
func TestByRoomDeterministicP2(t *testing.T) {
	r := New()
	for _, id := range []string{"oh2p-1-1-1-2", "lx06-1-1-1-1", "lx06-1-1-1-3"} {
		r.Upsert(Device{ID: id, Model: "lx06", IP: "1.1.1.1", Room: "客厅"})
		r.SetOnline(id, true)
	}
	for i := 0; i < 20; i++ {
		d := r.ByRoom("客厅")
		if d == nil || d.ID != "lx06-1-1-1-1" {
			t.Fatalf("ByRoom 应稳定返回 ID 最小的设备，实际 %+v", d)
		}
	}
	all := r.ByRoomAll("客厅")
	if len(all) != 3 {
		t.Fatalf("ByRoomAll 应返回 3 台，实际 %d", len(all))
	}
	if all[0].ID != "lx06-1-1-1-1" || all[1].ID != "lx06-1-1-1-3" || all[2].ID != "oh2p-1-1-1-2" {
		t.Fatalf("ByRoomAll 应按 ID 排序，实际 %v", all)
	}
}
