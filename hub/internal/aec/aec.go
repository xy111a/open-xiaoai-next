// Package aec 是回声消除（OwnDing 子带 AEC 思路的轻量移植）。
//
// 原理：Hub 知道自己正在给音箱放什么（TTS 音频），音箱麦克风采到的信号里
// 混进了这段播放声的回声。用 NLMS 自适应滤波器，以播放信号为参考，
// 从麦克风信号里减掉估计的回声，剩下的才是人声。
//
// 本实现是单频带 NLMS（OwnDing 是子带多滤波器组，效果更好但更复杂；
// 单频带先跑通，Phase 4 再视实测决定是否升级子带）。
// 在真机上验证前，本包只保证：合成信号测试里，回声能量显著下降，
// 且纯人声（无回声）通过时不被明显削弱。
package aec

import "math"

// NLMS 是归一化最小均方自适应滤波器。
type NLMS struct {
	taps []float64 // 滤波器系数
	ref  []float64 // 参考信号环形缓冲（播放信号）
	pos  int
	mu   float64 // 步长（0 < mu < 2，默认 0.5）
	eps  float64 // 归一化分母保护
}

// New 新建 NLMS。tapsLen 是滤波器长度（采样点数，16kHz 下 256 点 ≈ 16ms 回声尾长）。
func New(tapsLen int) *NLMS {
	if tapsLen <= 0 {
		tapsLen = 256
	}
	return &NLMS{
		taps: make([]float64, tapsLen),
		ref:  make([]float64, tapsLen),
		mu:   0.5,
		eps:  1e-6,
	}
}

// SetMu 设置步长。
func (n *NLMS) SetMu(mu float64) {
	if mu > 0 && mu < 2 {
		n.mu = mu
	}
}

// Process 处理一对采样：ref 是当前播放的采样（参考），mic 是麦克风采样。
// 返回消除回声后的人声估计。调用方必须保证 ref 与 mic 严格对齐（同帧同采样点）。
func (n *NLMS) Process(ref, mic float64) float64 {
	n.ref[n.pos] = ref
	// 估计回声：y = sum(w[i] * ref[pos-i])
	var y float64
	l := len(n.taps)
	for i := 0; i < l; i++ {
		idx := n.pos - i
		if idx < 0 {
			idx += l
		}
		y += n.taps[i] * n.ref[idx]
	}
	e := mic - y // 残差 = 人声估计
	// NLMS 更新：w += mu * e * ref / (||ref||^2 + eps)
	var norm float64
	for i := 0; i < l; i++ {
		idx := n.pos - i
		if idx < 0 {
			idx += l
		}
		norm += n.ref[idx] * n.ref[idx]
	}
	step := n.mu * e / (norm + n.eps)
	for i := 0; i < l; i++ {
		idx := n.pos - i
		if idx < 0 {
			idx += l
		}
		n.taps[i] += step * n.ref[idx]
	}
	n.pos = (n.pos + 1) % l
	return e
}

// ProcessFrame 批量处理一帧（ref 与 mic 等长）。
func (n *NLMS) ProcessFrame(ref, mic []float64) []float64 {
	out := make([]float64, len(mic))
	for i := range mic {
		r := 0.0
		if i < len(ref) {
			r = ref[i]
		}
		out[i] = n.Process(r, mic[i])
	}
	return out
}

// RMSEnergy 算信号能量（均方根），供测试断言回声抑制量。
func RMSEnergy(x []float64) float64 {
	if len(x) == 0 {
		return 0
	}
	var sum float64
	for _, v := range x {
		sum += v * v
	}
	return math.Sqrt(sum / float64(len(x)))
}
