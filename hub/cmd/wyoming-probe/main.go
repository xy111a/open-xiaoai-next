// Command wyoming-probe 是“假 HA”探针的命令行入口：按 HA Wyoming
// 集成的真实调用顺序（describe → transcribe / synthesize）验证
// NAS 上的 Whisper（STT）与 Piper（TTS）容器是否答得对。
//
// 在 NAS 上跑（Hub 与两个容器同一台机器，端口只绑了 127.0.0.1）：
//
//	go run ./cmd/wyoming-probe --stt 127.0.0.1:10300 --tts 127.0.0.1:10200
//
// 两个探针都打印 [OK] 时再去 HA 里加 Wyoming 集成（步骤见
// docs/HA_WYOMING.md）；任一 [FAIL] 时按打印的检查名排查，
// 不要在 HA 里反复添加。退出码：都通过为 0，否则为 1。
// 本命令不连真 HA、不改容器、不写任何状态。
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/huajun/open-xiaoai-next/hub/internal/wyoming"
)

func main() {
	sttAddr := flag.String("stt", "127.0.0.1:10300", "Whisper Wyoming 地址（host:port）")
	ttsAddr := flag.String("tts", "127.0.0.1:10200", "Piper Wyoming 地址（host:port）")
	timeout := flag.Duration("timeout", 10*time.Second, "单次探针超时")
	flag.Parse()

	stt := wyoming.ProbeSTT(*sttAddr, *timeout)
	tts := wyoming.ProbeTTS(*ttsAddr, *timeout)
	fmt.Print(stt.String())
	fmt.Print(tts.String())

	if stt.OK() && tts.OK() {
		fmt.Println("两个 Wyoming 服务都正常，可以去 HA 里加 Wyoming 集成了（见 docs/HA_WYOMING.md）。")
		return
	}
	fmt.Println("有探针失败：先按上面 FAIL 的检查名排查（容器状态 / 端口绑定），再重跑本命令。")
	os.Exit(1)
}
