package aec

import (
	"math"
	"testing"
)

// 合成回声：mic = 0.5 * ref(延迟 40 点) + 微弱语音。
// NLMS 自适应后，纯回声段的残余 RMS 应 < 输入的 50%（真跑滤波器，不是 mock）。
func TestEchoSuppressionSynthetic(t *testing.T) {
	n := New(256)
	const total = 16000
	ref := make([]float64, total)
	mic := make([]float64, total)
	for i := range ref {
		ref[i] = 8000 * math.Sin(2*math.Pi*300*float64(i)/16000)
	}
	// 回声：延迟 40 采样、衰减 0.5；再加一点不相关的“语音”噪声。
	for i := 40; i < total; i++ {
		mic[i] = 0.5*ref[i-40] + 200*math.Sin(2*math.Pi*1000*float64(i)/16000)
	}
	// 先让滤波器适应 0.5 秒。
	for i := 0; i < 8000; i++ {
		n.Process(ref[i], mic[i])
	}
	// 再测 0.5 秒的残余。
	var inSq, outSq float64
	for i := 8000; i < total; i++ {
		out := n.Process(ref[i], mic[i])
		inSq += mic[i] * mic[i]
		outSq += out * out
	}
	inRMS := math.Sqrt(inSq / 8000)
	outRMS := math.Sqrt(outSq / 8000)
	t.Logf("回声段输入 RMS %.1f → 残余 RMS %.1f（%.1f%%）", inRMS, outRMS, 100*outRMS/inRMS)
	if outRMS > 0.5*inRMS {
		t.Fatalf("回声抑制不足：残余 %.1f%% > 50%%", 100*outRMS/inRMS)
	}
}

func TestRMSEnergy(t *testing.T) {
	if got := RMSEnergy([]float64{3, 4}); math.Abs(got-3.5355339) > 1e-6 {
		t.Fatalf("RMSEnergy([3,4]) 应为 3.5355，实际 %f", got)
	}
	if got := RMSEnergy(nil); got != 0 {
		t.Fatalf("空输入 RMS 应为 0，实际 %f", got)
	}
}

func TestProcessFrameLength(t *testing.T) {
	n := New(64)
	ref := make([]float64, 320)
	mic := make([]float64, 320)
	for i := range ref {
		ref[i] = 1000 * math.Sin(float64(i))
		mic[i] = ref[i] * 0.5
	}
	out := n.ProcessFrame(ref, mic)
	if len(out) != 320 {
		t.Fatalf("ProcessFrame 应返回 320 点，实际 %d", len(out))
	}
}
