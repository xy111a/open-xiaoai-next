package observability

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// 用已知分段时长走一遍 Begin→RecordStage→Finish，
// 校验 stages 里每段毫秒数与 total_ms 的关系。
func TestRecorderKnownStageDurations(t *testing.T) {
	r := NewRecorder(20)
	inter := r.Begin("lx06-1", "voice")
	if inter == nil {
		t.Fatal("Begin 不应返回 nil")
	}
	// KWS 由 NoteWake 在 Begin 前记下，应被消费进同一条记录。
	r2 := NewRecorder(20)
	r2.NoteWake("lx06-1", 120)
	inter2 := r2.Begin("lx06-1", "voice")
	if got := inter2.Stages[StageKWS]; got != 120 {
		t.Fatalf("kws_ms 应为 120，实际 %d", got)
	}

	r.RecordStage(inter, StageSTT, 900*time.Millisecond)
	r.RecordStage(inter, StageLLM, 800*time.Millisecond)
	r.RecordStage(inter, StageTTS, 600*time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	r.Finish(inter, nil)

	recent := r.Recent()
	if len(recent) != 1 {
		t.Fatalf("应有 1 条记录，实际 %d", len(recent))
	}
	got := recent[0]
	if got.Stages[StageSTT] != 900 || got.Stages[StageLLM] != 800 || got.Stages[StageTTS] != 600 {
		t.Fatalf("分段毫秒不对: %v", got.Stages)
	}
	if !got.OK || got.Error != "" {
		t.Fatalf("成功交互应 ok=true 且无 error，实际 ok=%v error=%q", got.OK, got.Error)
	}
	// total 是墙钟耗时：至少覆盖三段记录之间的 sleep，不会小于 5ms，
	// 也不应离谱（测试内三段只是数字写入，不真耗时）。
	if got.TotalMs < 5 || got.TotalMs > 5000 {
		t.Fatalf("total_ms 应在 [5,5000]，实际 %d", got.TotalMs)
	}
	if got.Kind != "voice" || got.DeviceID != "lx06-1" {
		t.Fatalf("kind/device 不对: %+v", got)
	}
	if got.FinishedAt.Before(got.StartedAt) {
		t.Fatal("finished_at 不应早于 started_at")
	}
}

// 失败的交互记 ok=false 与错误摘要；错误摘要截断、不含换行。
func TestRecorderFinishError(t *testing.T) {
	r := NewRecorder(20)
	inter := r.Begin("oh2p-1", "voice")
	r.Finish(inter, errors.New("STT 不可用（Whisper 未连接）: dial 失败\n第二行不应出现"))
	got := r.Recent()[0]
	if got.OK {
		t.Fatal("失败交互 ok 应为 false")
	}
	if got.Error != "STT 不可用（Whisper 未连接）: dial 失败" {
		t.Fatalf("error 应只留首行，实际 %q", got.Error)
	}
	long := string(make([]byte, 200))
	inter2 := r.Begin("oh2p-1", "voice")
	r.Finish(inter2, errors.New(long))
	if len(r.Recent()[0].Error) > 120 {
		t.Fatalf("error 应截断到 120 字，实际 %d 字", len(r.Recent()[0].Error))
	}
}

// 环形缓冲：记 25 条只留最近 20 条，倒序（最新在前）。
func TestRecorderRingBuffer(t *testing.T) {
	r := NewRecorder(20)
	for i := 0; i < 25; i++ {
		inter := r.Begin("lx06-1", "voice")
		r.Finish(inter, nil)
	}
	recent := r.Recent()
	if len(recent) != 20 {
		t.Fatalf("环形缓冲应留 20 条，实际 %d", len(recent))
	}
	// 最新一条的 ID 最大（纳秒递增），最旧一条不是第 1 条。
	if recent[0].ID == recent[19].ID {
		t.Fatal("倒序列表首尾 ID 不应相同")
	}
	for i := 1; i < len(recent); i++ {
		if !recent[i-1].StartedAt.After(recent[i].StartedAt) && !recent[i-1].StartedAt.Equal(recent[i].StartedAt) {
			t.Fatalf("应按开始时间倒序，第 %d 条晚于第 %d 条", i-1, i)
		}
	}
	// Latest 与 Recent()[0] 一致，且是拷贝：改它不影响内部。
	latest := r.Latest()
	if latest == nil || latest.ID != recent[0].ID {
		t.Fatal("Latest 应与 Recent()[0] 一致")
	}
	latest.Stages[StageSTT] = 99999
	if r.Recent()[0].Stages[StageSTT] == 99999 {
		t.Fatal("Recent 应返回深拷贝，外部改动不应污染内部")
	}
}

// NoteWake 只被消费一次；过期（>120s）的唤醒延迟不被消费。
func TestRecorderNoteWakeConsumedOnceAndExpires(t *testing.T) {
	r := NewRecorder(20)
	r.NoteWake("lx06-1", 150)
	first := r.Begin("lx06-1", "voice")
	if first.Stages[StageKWS] != 150 {
		t.Fatalf("第一次 Begin 应消费 kws_ms=150，实际 %v", first.Stages)
	}
	r.Finish(first, nil)
	second := r.Begin("lx06-1", "voice")
	if _, ok := second.Stages[StageKWS]; ok {
		t.Fatal("NoteWake 只应被消费一次，第二次 Begin 不应再有 kws_ms")
	}
	r.Finish(second, nil)

	// 过期：把 pending 的时间拨回 121 秒前，Begin 不应再消费。
	r.NoteWake("lx06-1", 200)
	r.mu.Lock()
	pw := r.pending["lx06-1"]
	pw.at = pw.at.Add(-121 * time.Second)
	r.pending["lx06-1"] = pw
	r.mu.Unlock()
	third := r.Begin("lx06-1", "voice")
	if _, ok := third.Stages[StageKWS]; ok {
		t.Fatal("过期 121s 的唤醒延迟不应被消费")
	}
	r.Finish(third, nil)
}

// RecordStageForDevice：无进行中交互时静默丢弃；有则写进同一条。
func TestRecorderRecordStageForDevice(t *testing.T) {
	r := NewRecorder(20)
	// 无进行中交互：不应 panic、不应凭空造记录。
	r.RecordStageForDevice("lx06-1", StageLLM, 800*time.Millisecond)
	if len(r.Recent()) != 0 {
		t.Fatal("无进行中交互时 RecordStageForDevice 不应产生记录")
	}
	inter := r.Begin("lx06-1", "voice")
	r.RecordStageForDevice("lx06-1", StageLLM, 800*time.Millisecond)
	r.RecordStageForDevice("oh2p-1", StageLLM, 800*time.Millisecond) // 别的设备，不影响
	if inter.Stages[StageLLM] != 800 {
		t.Fatalf("llm_ms 应写入进行中交互，实际 %v", inter.Stages)
	}
	r.Finish(inter, nil)
}

// RecordMusic：问答进行中写进同一条；不在问答里则单独立一条 music 记录。
func TestRecorderRecordMusic(t *testing.T) {
	r := NewRecorder(20)
	r.RecordMusic("lx06-1", 1500*time.Millisecond)
	recent := r.Recent()
	if len(recent) != 1 || recent[0].Kind != "music" || recent[0].Stages[StageMusic] != 1500 {
		t.Fatalf("独立点歌应单独立一条 music 记录，实际 %+v", recent)
	}
	inter := r.Begin("lx06-1", "voice")
	r.RecordMusic("lx06-1", 1200*time.Millisecond)
	if inter.Stages[StageMusic] != 1200 {
		t.Fatalf("问答中的点歌应写进语音交互的 music_ms，实际 %v", inter.Stages)
	}
	r.Finish(inter, nil)
	if len(r.Recent()) != 2 {
		t.Fatalf("问答中的点歌不应另立记录，应仍为 2 条，实际 %d", len(r.Recent()))
	}
}

// 同设备 Begin 两次：旧的按未完成归档，不永久占 active。
func TestRecorderBeginSupersedesActive(t *testing.T) {
	r := NewRecorder(20)
	first := r.Begin("lx06-1", "voice")
	second := r.Begin("lx06-1", "voice")
	if first == second {
		t.Fatal("两次 Begin 应返回不同交互")
	}
	r.Finish(second, nil)
	recent := r.Recent()
	if len(recent) != 2 {
		t.Fatalf("被顶掉的旧交互也应归档，共 2 条，实际 %d", len(recent))
	}
	if recent[1].OK {
		t.Fatal("被顶掉的旧交互 ok 应为 false")
	}
}

// nil Recorder 上所有方法都不 panic（调用方可选注入）。
func TestNilRecorderIsNoop(t *testing.T) {
	var r *Recorder
	r.NoteWake("lx06-1", 100)
	if r.Begin("lx06-1", "voice") != nil {
		t.Fatal("nil Recorder 的 Begin 应返回 nil")
	}
	r.RecordStage(nil, StageSTT, time.Second)
	r.RecordStageForDevice("lx06-1", StageLLM, time.Second)
	r.RecordMusic("lx06-1", time.Second)
	r.Finish(nil, nil)
	if len(r.Recent()) != 0 || r.Latest() != nil {
		t.Fatal("nil Recorder 的 Recent 应为空、Latest 应为 nil")
	}
}

// 并发 Begin/Finish/Record：-race 下跑，不应 panic、不应丢锁。
func TestRecorderConcurrent(t *testing.T) {
	r := NewRecorder(20)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				dev := "lx06-1"
				if i%2 == 1 {
					dev = "oh2p-1"
				}
				inter := r.Begin(dev, "voice")
				r.RecordStageForDevice(dev, StageSTT, time.Duration(j)*time.Millisecond)
				r.RecordMusic(dev, time.Duration(j)*time.Millisecond)
				r.Finish(inter, nil)
			}
		}(i)
	}
	wg.Wait()
	if len(r.Recent()) != 20 {
		t.Fatalf("并发写完环形缓冲应留 20 条，实际 %d", len(r.Recent()))
	}
}
