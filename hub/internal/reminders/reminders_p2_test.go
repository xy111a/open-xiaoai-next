package reminders

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// P2-13：重复 Start 不双倍触发；Stop 后可再次 Start。
func TestDoubleStartNoDoubleFireP2(t *testing.T) {
	dir := t.TempDir()
	var n atomic.Int32
	s := New(filepath.Join(dir, "r.json"), func(r Reminder) { n.Add(1) })
	if _, err := s.Add(Reminder{Kind: KindOnce, Text: "喝水", Next: time.Now().Add(100 * time.Millisecond)}); err != nil {
		t.Fatal(err)
	}
	s.Start()
	s.Start() // 重复 Start 应是 no-op
	s.Start()
	time.Sleep(1500 * time.Millisecond)
	s.Stop()
	if got := n.Load(); got != 1 {
		t.Fatalf("一次性提醒应只触发 1 次，实际 %d 次（重复 Start 导致双倍触发）", got)
	}
}

func TestStopThenStartAgainP2(t *testing.T) {
	dir := t.TempDir()
	fired := make(chan struct{}, 2)
	s := New(filepath.Join(dir, "r.json"), func(r Reminder) { fired <- struct{}{} })
	s.Start()
	s.Stop()
	// Stop 后再次 Start 应能正常调度（旧代码 stopCh 已关闭，新 goroutine
	// 一启动就退出，调度器再也起不来）。
	s.Start()
	defer s.Stop()
	if _, err := s.Add(Reminder{Kind: KindOnce, Text: "喝水", Next: time.Now().Add(100 * time.Millisecond)}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop 后再次 Start，提醒未触发")
	}
}

// P2-44：无变化时 tick 不写盘（mtime 不变）。
func TestTickDoesNotRewriteWhenUnchangedP2(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "r.json")
	s := New(path, nil)
	if _, err := s.Add(Reminder{Kind: KindDaily, Text: "吃药", At: "08:00", Next: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Start()
	defer s.Stop()
	time.Sleep(2200 * time.Millisecond)
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("无变化时 tick 不应重写文件：mtime %v → %v", before.ModTime(), after.ModTime())
	}
}

// P2-44：损坏的提醒文件被备份、不再静默丢全部，Load 返回错误。
func TestCorruptFileBackedUpP2(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "r.json")
	if err := os.WriteFile(path, []byte("{不是 json"), 0600); err != nil {
		t.Fatal(err)
	}
	s := New(path, nil)
	if err := s.Load(); err == nil {
		t.Fatal("损坏文件 Load 应返回错误")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("损坏文件应被改名移走，原路径应不存在，实际 %v", err)
	}
	matches, _ := filepath.Glob(path + ".corrupt.*")
	if len(matches) != 1 {
		t.Fatalf("应留 1 个 .corrupt.* 备份，实际 %v", matches)
	}
	if len(s.List()) != 0 {
		t.Fatal("损坏文件加载后提醒应为空")
	}
}

// P2-14：落盘失败时 Cancel/CancelByText 返回 error，不谎报成功。
func TestCancelSaveErrorSurfacedP2(t *testing.T) {
	dir := t.TempDir()
	// path 的父目录是个文件：任何写盘都失败（与权限无关，确定性复现）。
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	s := New(filepath.Join(blocker, "r.json"), nil)
	// 直接往内存里放一条（Add 会因落盘失败返回错误，但条目已在内存里）。
	if _, err := s.Add(Reminder{Kind: KindOnce, Text: "喝水", Next: time.Now().Add(time.Hour)}); err == nil {
		t.Fatal("写盘必失败时 Add 应返回错误")
	}
	if ok, err := s.Cancel(s.List()[0].ID); err == nil || !ok {
		t.Fatalf("Cancel 落盘失败应返回 error，实际 ok=%v err=%v", ok, err)
	}
	if n, err := s.CancelByText("喝水"); err != nil || n != 0 {
		// 注意：上一条 Cancel 已删掉内存里的条目，这里 n 应为 0 且无错误。
		t.Fatalf("二次取消应为 0 条无错误，实际 n=%d err=%v", n, err)
	}
}
