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

// P2-22：零能量参考信号时滤波器系数必须保持为零、输出等于输入，
// 步长不再爆炸；极小参考下系数有界、输出有限。
func TestZeroReferenceDoesNotExplode(t *testing.T) {
	n := New(256)
	// 全零参考 + 大麦克风信号：输出必须逐采样等于输入，系数全零。
	for i := 0; i < 1000; i++ {
		out := n.Process(0, 8000*math.Sin(2*math.Pi*300*float64(i)/16000))
		want := 8000 * math.Sin(2*math.Pi*300*float64(i)/16000)
		if math.Abs(out-want) > 1e-9 {
			t.Fatalf("zero reference: output %v must equal mic %v at sample %d", out, want, i)
		}
	}
	for i, w := range n.taps {
		if w != 0 {
			t.Fatalf("zero reference: tap %d must stay 0, got %v", i, w)
		}
	}
	// 极小参考（1e-4 量级）+ 大麦克风信号：输出有限、系数有界。
	n2 := New(256)
	for i := 0; i < 1000; i++ {
		out := n2.Process(1e-4, 8000)
		if math.IsNaN(out) || math.IsInf(out, 0) {
			t.Fatalf("tiny reference: output must stay finite, got %v at sample %d", out, i)
		}
	}
	for i, w := range n2.taps {
		if math.IsNaN(w) || math.IsInf(w, 0) || math.Abs(w) > 1.0 {
			t.Fatalf("tiny reference: tap %d must stay bounded, got %v", i, w)
		}
	}
}
