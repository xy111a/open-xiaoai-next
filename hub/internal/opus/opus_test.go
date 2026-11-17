package opus

import (
	"math"
	"testing"
)

// 正弦波往返：440Hz、16kHz、2 秒，编解码后对齐延迟再算 RMS 误差 < 10%。
// Opus 编解码器自带约 30 采样前瞻延迟，直接逐点比会把相位差算成误差，
// 所以先互相关找最佳延迟（±80 采样）再对齐比较——这是测编解码保真度的标准做法。
func TestRoundTripSine(t *testing.T) {
	enc, err := NewEncoder()
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	dec, err := NewDecoder()
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	const total = 32000 // 2 秒
	pcm := make([]int16, total)
	for i := range pcm {
		pcm[i] = int16(10000 * math.Sin(2*math.Pi*440*float64(i)/16000))
	}
	decoded := make([]int16, 0, total)
	for f := 0; f < total/FrameSize; f++ {
		frame := pcm[f*FrameSize : (f+1)*FrameSize]
		pkt, err := enc.Encode(frame)
		if err != nil {
			t.Fatalf("Encode 第 %d 帧: %v", f, err)
		}
		if len(pkt) == 0 || len(pkt) >= FrameSize*2 {
			t.Fatalf("第 %d 帧编码包大小异常: %d 字节", f, len(pkt))
		}
		out, err := dec.Decode(pkt)
		if err != nil {
			t.Fatalf("Decode 第 %d 帧: %v", f, err)
		}
		if len(out) != FrameSize {
			t.Fatalf("解码应为 %d 点，实际 %d 点", FrameSize, len(out))
		}
		decoded = append(decoded, out...)
	}
	// 互相关找延迟（解码输出相对原始滞后多少采样）。
	bestLag, bestCorr := 0, -1e18
	for lag := -80; lag <= 80; lag++ {
		var s float64
		for i := 1000; i < 30000; i++ {
			j := i + lag
			if j < 0 || j >= len(decoded) {
				continue
			}
			s += float64(pcm[i]) * float64(decoded[j])
		}
		if s > bestCorr {
			bestCorr, bestLag = s, lag
		}
	}
	t.Logf("编解码延迟: %d 采样", bestLag)
	if bestLag < -80 || bestLag > 80 {
		t.Fatalf("延迟 %d 采样超出 ±80 合理范围", bestLag)
	}
	var errSq, origSq float64
	n := 0
	for i := 1000; i < 30000; i++ {
		j := i + bestLag
		if j < 0 || j >= len(decoded) {
			continue
		}
		d := float64(decoded[j] - pcm[i])
		errSq += d * d
		origSq += float64(pcm[i]) * float64(pcm[i])
		n++
	}
	relErr := math.Sqrt(errSq / origSq)
	t.Logf("对齐后往返相对 RMS 误差: %.4f（%d 点）", relErr, n)
	if relErr > 0.10 {
		t.Fatalf("往返误差 %.4f 超过 10%% 阈值", relErr)
	}
}

func TestEncodeWrongFrameSize(t *testing.T) {
	enc, err := NewEncoder()
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if _, err := enc.Encode(make([]int16, 100)); err == nil {
		t.Fatal("非 320 点的帧应返回错误")
	}
}
