// Package opus 是 Opus 编解码的薄封装（16kHz 单声道，20ms 一帧）。
// 底层用 CGO 绑定系统 libopus（gopkg.in/hraban/opus.v2）。
// 需要 libopus-dev；NAS 部署时 Hub 镜像里同样要装 libopus。
package opus

import (
	"fmt"

	opus "gopkg.in/hraban/opus.v2"
)

const (
	// SampleRate 是语音采样率。
	SampleRate = 16000
	// Channels 是单声道。
	Channels = 1
	// FrameSize 是 20ms 一帧的采样点数。
	FrameSize = 320
	// Bitrate 是目标码率（约 16kbps）。
	Bitrate = 16000
)

// Encoder 是 Opus 编码器。
type Encoder struct {
	enc *opus.Encoder
	buf []byte
}

// NewEncoder 新建编码器。
func NewEncoder() (*Encoder, error) {
	enc, err := opus.NewEncoder(SampleRate, Channels, opus.AppVoIP)
	if err != nil {
		return nil, err
	}
	if err := enc.SetBitrate(Bitrate); err != nil {
		return nil, err
	}
	return &Encoder{enc: enc, buf: make([]byte, 4000)}, nil
}

// Encode 编一帧 320 点 PCM，返回 Opus 数据。
func (e *Encoder) Encode(pcm []int16) ([]byte, error) {
	if len(pcm) != FrameSize {
		return nil, fmt.Errorf("opus: 一帧必须是 %d 点，实际 %d 点", FrameSize, len(pcm))
	}
	n, err := e.enc.Encode(pcm, e.buf)
	if err != nil {
		return nil, err
	}
	out := make([]byte, n)
	copy(out, e.buf[:n])
	return out, nil
}

// Decoder 是 Opus 解码器。
type Decoder struct {
	dec *opus.Decoder
}

// NewDecoder 新建解码器。
func NewDecoder() (*Decoder, error) {
	dec, err := opus.NewDecoder(SampleRate, Channels)
	if err != nil {
		return nil, err
	}
	return &Decoder{dec: dec}, nil
}

// Decode 解一帧 Opus，返回 320 点 PCM。
func (d *Decoder) Decode(data []byte) ([]int16, error) {
	pcm := make([]int16, FrameSize)
	n, err := d.dec.Decode(data, pcm)
	if err != nil {
		return nil, err
	}
	return pcm[:n], nil
}
