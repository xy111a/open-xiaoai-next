package wyoming

import (
	"strings"
	"testing"
	"time"
)

// TestProbeSTTAsHAWould：假 HA 的视角——对假 Whisper 跑 ProbeSTT，
// describe 拿到 asr、transcribe 走完即通过（与真实 HA 的调用顺序一致）。
func TestProbeSTTAsHAWould(t *testing.T) {
	f := startFake(t)
	defer f.ln.Close()

	r := ProbeSTT(f.addr(), 5*time.Second)
	if !r.OK() {
		t.Fatalf("ProbeSTT 应通过，实际:\n%s", r.String())
	}
	if r.Kind != "stt" {
		t.Fatalf("Kind 应为 stt，实际 %q", r.Kind)
	}
	if len(r.Checks) != 3 {
		t.Fatalf("应有 dial/describe/transcribe 共 3 项检查，实际 %d", len(r.Checks))
	}
	if got := describeInfoNames(r.Info, "asr"); len(got) == 0 || got[0] != "whisper" {
		t.Fatalf("info.asr 应含 whisper，实际 %v", got)
	}
}

// TestProbeTTSAsHAWould：假 HA 的视角——对假 Piper 跑 ProbeTTS，
// describe 拿到 tts、synthesize 返回非空 PCM 即通过。
func TestProbeTTSAsHAWould(t *testing.T) {
	f := startFake(t)
	defer f.ln.Close()

	r := ProbeTTS(f.addr(), 5*time.Second)
	if !r.OK() {
		t.Fatalf("ProbeTTS 应通过，实际:\n%s", r.String())
	}
	if len(r.Checks) != 3 {
		t.Fatalf("应有 dial/describe/synthesize 共 3 项检查，实际 %d", len(r.Checks))
	}
	if got := describeInfoNames(r.Info, "tts"); len(got) == 0 || got[0] != "piper" {
		t.Fatalf("info.tts 应含 piper，实际 %v", got)
	}
}

// TestProbeUnreachable：容器没起时探针必须明确 FAIL 在 dial，
// 而不是挂起或报通过（NAS 上排错先看这一项）。
func TestProbeUnreachable(t *testing.T) {
	// 127.0.0.1:1 几乎不可能有监听（且拨号立即拒绝，不会等超时）。
	for _, r := range []ProbeResult{ProbeSTT("127.0.0.1:1", time.Second), ProbeTTS("127.0.0.1:1", time.Second)} {
		if r.OK() {
			t.Fatalf("连不上的地址探针应 FAIL，实际:\n%s", r.String())
		}
		if len(r.Checks) != 1 || r.Checks[0].Name != "dial" || r.Checks[0].OK {
			t.Fatalf("失败应停在 dial 一项，实际 %+v", r.Checks)
		}
	}
}

// TestProbeResultString：报告文本须含状态行与每项检查名，
// 命令行输出靠它，改格式时这里先拦。
func TestProbeResultString(t *testing.T) {
	r := ProbeResult{Addr: "127.0.0.1:10300", Kind: "stt", Checks: []ProbeCheck{
		{Name: "dial", OK: true, Detail: "TCP 已连通"},
		{Name: "describe", OK: false, Detail: "info 缺 asr"},
	}}
	if r.OK() {
		t.Fatal("有一项 FAIL 时整体应为 FAIL")
	}
	s := r.String()
	for _, want := range []string{"[FAIL]", "stt", "127.0.0.1:10300", "dial", "describe"} {
		if !strings.Contains(s, want) {
			t.Fatalf("报告应含 %q，实际:\n%s", want, s)
		}
	}
}
