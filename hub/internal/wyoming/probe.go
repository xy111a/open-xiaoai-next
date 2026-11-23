// ProbeSTT / ProbeTTS 是“假 HA”探针：按 HA Wyoming 集成与 Wyoming
// 服务端对话的真实顺序（describe → transcribe / synthesize），验证
// Whisper（STT :10300）与 Piper（TTS :10200）这两个容器是否答得
// 对。探针只做客户端该做的事：不改容器、不连真 HA、不写任何状态。
//
// 用法（在 NAS 上，Hub 与 Whisper/Piper 同一台机器）：
//
//	go run ./cmd/wyoming-probe --stt 127.0.0.1:10300 --tts 127.0.0.1:10200
//
// 两个探针都 OK 才算 HA 能接上；任一失败时先看 docs/HA_WYOMING.md
// 的排查表，不要直接在 HA 里反复加集成。
package wyoming

import (
	"fmt"
	"strings"
	"time"
)

// ProbeCheck 是一项检查的结果。
type ProbeCheck struct {
	// Name 是检查名，如 "dial" / "describe" / "transcribe"。
	Name string
	// OK 是否通过。
	OK bool
	// Detail 是给人看的说明（成功时写拿到的关键信息，失败时写原因）。
	Detail string
}

// ProbeResult 是一次探针的完整结果。
type ProbeResult struct {
	// Addr 是被探的地址，如 "127.0.0.1:10300"。
	Addr string
	// Kind 是 "stt" 或 "tts"。
	Kind string
	// Info 是 describe 拿到的 info 事件的 data（如 asr/tts 服务列表，
	// 失败时为 nil），供调用方/排查看模型与语言。
	Info map[string]any
	// Checks 是按执行顺序的各项检查。
	Checks []ProbeCheck
}

// OK 是全部检查都通过。
func (r ProbeResult) OK() bool {
	if len(r.Checks) == 0 {
		return false
	}
	for _, c := range r.Checks {
		if !c.OK {
			return false
		}
	}
	return true
}

// String 是给人看的探针报告（一行标题 + 每项一行），
// wyoming-probe 命令行直接打印它。
func (r ProbeResult) String() string {
	var b strings.Builder
	status := "FAIL"
	if r.OK() {
		status = "OK"
	}
	fmt.Fprintf(&b, "[%s] %s %s\n", status, r.Kind, r.Addr)
	for _, c := range r.Checks {
		mark := "ok  "
		if !c.OK {
			mark = "FAIL"
		}
		fmt.Fprintf(&b, "  %s %-11s %s\n", mark, c.Name, c.Detail)
	}
	return b.String()
}

func probeCheck(name string, ok bool, detail string) ProbeCheck {
	return ProbeCheck{Name: name, OK: ok, Detail: detail}
}

// describeInfoNames 从 info data 里取某类服务（如 "asr" / "tts"）
// 的名字列表，供 Detail 展示（rhasspy 的 info data 形如
// {"asr": [{"name": "whisper", "languages": ["zh"], ...}]}）。
func describeInfoNames(info map[string]any, key string) []string {
	raw, ok := info[key].([]any)
	if !ok {
		return nil
	}
	var names []string
	for _, v := range raw {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if name, ok := m["name"].(string); ok && name != "" {
			names = append(names, name)
		}
	}
	return names
}

// probeDial 是探针第一步：拨号 + describe，返回客户端与 info。
// 任一步失败即把失败记进 checks 并返回错误（调用方直接收尾）。
func probeDial(addr, kind, infoKey string, timeout time.Duration) (*Client, map[string]any, []ProbeCheck, error) {
	checks := []ProbeCheck{}
	c, err := Dial(addr, timeout)
	if err != nil {
		checks = append(checks, probeCheck("dial", false, fmt.Sprintf("连不上 %s：%v（容器没起？端口没绑 127.0.0.1？）", addr, err)))
		return nil, nil, checks, err
	}
	checks = append(checks, probeCheck("dial", true, "TCP 已连通"))
	info, err := c.Describe()
	if err != nil {
		checks = append(checks, probeCheck("describe", false, fmt.Sprintf("describe 失败：%v（服务端不是 Wyoming 协议？）", err)))
		_ = c.Close()
		return nil, nil, checks, err
	}
	names := describeInfoNames(info, infoKey)
	if len(names) == 0 {
		checks = append(checks, probeCheck("describe", false, fmt.Sprintf("info 里没有 %q 服务列表，HA 认不出它是 %s 服务：%v", infoKey, kind, summarizeInfo(info))))
		_ = c.Close()
		return nil, info, checks, fmt.Errorf("info 缺 %q", infoKey)
	}
	checks = append(checks, probeCheck("describe", true, fmt.Sprintf("info 正常，%s 服务：%s", infoKey, strings.Join(names, ", "))))
	return c, info, checks, nil
}

// summarizeInfo 把 info 头截成一小段可读摘要（排查看，不全文回显）。
func summarizeInfo(info map[string]any) string {
	s := fmt.Sprintf("%v", info)
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// ProbeSTT 按 HA 的用法验证 Whisper：dial → describe（须有 asr）→
// transcribe 一段 0.5 秒静音 PCM（须正常返回 transcript，不要求
// 识别出文字——静音本就没话；只要求协议走完、服务端不报错）。
// timeout<=0 时默认 10 秒。
func ProbeSTT(addr string, timeout time.Duration) ProbeResult {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	r := ProbeResult{Addr: addr, Kind: "stt"}
	c, info, checks, err := probeDial(addr, "stt", "asr", timeout)
	r.Checks = checks
	r.Info = info
	if err != nil {
		return r
	}
	// probeDial 在 err==nil 时必返回非 nil 的 c（所有返回 nil c 的
	// 路径都带非 nil err）；此处仍显式守卫，满足 lint 规则 2
	// （同一函数内 Transcribe 的接收者须有 ==nil/!=nil 检查）。
	if c == nil {
		r.Checks = append(r.Checks, probeCheck("dial", false, "探针客户端为空"))
		return r
	}
	defer c.Close()
	silence := make([]byte, 16000) // 0.5 秒 16kHz 单声道 PCM，全零
	text, err := c.Transcribe(silence, "zh")
	if err != nil {
		r.Checks = append(r.Checks, probeCheck("transcribe", false, fmt.Sprintf("transcribe 静音失败：%v", err)))
		return r
	}
	r.Checks = append(r.Checks, probeCheck("transcribe", true, fmt.Sprintf("transcribe 正常返回（静音识别结果 %q，空字符串也算通过）", text)))
	return r
}

// ProbeTTS 按 HA 的用法验证 Piper：dial → describe（须有 tts）→
// synthesize "你好"（须返回非空、偶数长度的 16kHz PCM；
// 服务端非 16kHz 时 Synthesize 内部重采样到 16kHz）。
// timeout<=0 时默认 10 秒。
func ProbeTTS(addr string, timeout time.Duration) ProbeResult {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	r := ProbeResult{Addr: addr, Kind: "tts"}
	c, info, checks, err := probeDial(addr, "tts", "tts", timeout)
	r.Checks = checks
	r.Info = info
	if err != nil {
		return r
	}
	// 同 ProbeSTT：probeDial 在 err==nil 时必返回非 nil 的 c，
	// 此处仍显式守卫，满足 lint 规则 2（Synthesize 的接收者须有
	// 同一函数内 ==nil/!=nil 检查）。
	if c == nil {
		r.Checks = append(r.Checks, probeCheck("dial", false, "探针客户端为空"))
		return r
	}
	defer c.Close()
	pcm, err := c.Synthesize("你好", "zh_CN-huayan-medium")
	if err != nil {
		r.Checks = append(r.Checks, probeCheck("synthesize", false, fmt.Sprintf("synthesize 失败：%v", err)))
		return r
	}
	if len(pcm) == 0 {
		r.Checks = append(r.Checks, probeCheck("synthesize", false, "synthesize 返回空 PCM"))
		return r
	}
	if len(pcm)%2 != 0 {
		r.Checks = append(r.Checks, probeCheck("synthesize", false, fmt.Sprintf("synthesize 返回 %d 字节 PCM，长度不是偶数（int16 流应对齐）", len(pcm))))
		return r
	}
	r.Checks = append(r.Checks, probeCheck("synthesize", true, fmt.Sprintf("synthesize 正常，返回 %d 字节 PCM（%.2f 秒 16kHz 音频）", len(pcm), float64(len(pcm)/2)/16000)))
	return r
}
