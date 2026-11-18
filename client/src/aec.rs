//! 客户端回声消除（Opus 级回声接线的客户端落点，见 TEST_REPORT.md 未做缺口）。
//!
//! 为什么落在这里、而不是 Hub 侧（调查结论，2026-11-18）：
//!   - Hub 侧拿不到可对齐的参考信号。Hub 的对讲转发（`hub/cmd/hub/main.go`
//!     `OnAudioFrame`）是 Opus 帧原样转发，`EchoCancel`（PCM 级）全程零调用；
//!     Hub 发给设备的播放（`speakText` / `onReply`）是现编现发的 Opus 帧，
//!     Hub 从不保存发出的 PCM，也不掌握“哪一帧在何时被设备实际放出”——
//!     帧在网上的抖动 + 客户端 ALSA 播放缓冲（4 周期 ≈ 80ms）+ 声学传播
//!     延迟都无法在 Hub 侧对齐，参考信号与麦克风信号错位超过 NLMS 的
//!     16ms 抽头窗后滤波器无法收敛。
//!   - 客户端同时握有两路信号的同一时钟域：`play_frame` 解出的播放 PCM
//!     （放出什么自己知道）与 `capture_pcm` 采到的麦克风 PCM（同一 ALSA
//!     设备、同一 16kHz 时钟）。参考信号只需经“播放参考延迟队列”对齐
//!     ALSA 缓冲延迟，再进 NLMS，残留回声随麦克风 PCM 一起进 Opus 编码
//!     发往 Hub——Hub 侧的对讲转发与语音管线一行不用改。
//!   - 因此接线点是客户端录音任务：播放 PCM（`run_session` 的 `play_frame`
//!     解码产物，经 `playback_ref_tx` 通道）→ 本模块 → 消回声后的
//!     麦克风 PCM → KWS 喂帧 + Opus 编码。Hub 的 `EchoCancel` 保持原样
//!     （半双工不需要、全双工的 Hub 侧参考仍不可得，不再强行接）。
//!
//! 算法：单频带 NLMS，与 `hub/internal/aec` 同口径（256 抽头 ≈ 16ms 回声
//! 尾长，mu 0.5），另加 P2-22 的零能量守卫——参考能量低于阈值时跳过
//! 自适应更新并钳制步长，静默播放时滤波器系数绝不发散；参考全零的一帧
//! 直接短路返回原麦克风 PCM（不跑 256 抽头循环，省 CPU）。
//!
//! 未验证（BLOCKED，需真机）：
//!   - `AEC_REF_DELAY_FRAMES = 4`（80ms）只是 ALSA 缓冲 4 周期的理论值，
//!     真实声学延迟（缓冲占用 + 扬声器-麦克风传播）需真机校准；延迟错
//!     超过 ±16ms 时抑制率会明显下降。
//!   - 真实回声抑制率（dB）与打断场景下的残留：合成信号测试只能证明
//!     逻辑收敛，不能当作真机回声消除率。

use std::collections::VecDeque;

use crate::audio::FRAME_SAMPLES;

/// NLMS 抽头数：256 点 @16kHz ≈ 16ms 回声尾长（与 Hub 侧 `aec.New(256)` 一致）。
pub const AEC_TAPS: usize = 256;
/// 播放参考延迟：4 帧 × 20ms = 80ms，等于 ALSA 播放缓冲（4 周期）。
/// 含义：现在采到的麦克风帧里混的回声，对应的是 4 帧前解码播放出去的 PCM。
/// 真实延迟需真机校准（见模块头注释）；NLMS 抽头窗覆盖该延迟前后各 16ms。
pub const AEC_REF_DELAY_FRAMES: usize = 4;
/// 参考能量下限（P2-22）：256 点参考窗的能量范数低于此值时跳过自适应
/// 更新——静默/零能量参考下步长 `mu*e/(norm+eps)` 不再爆炸。
/// 参考采样是 ±1.0 归一化浮点；满幅正弦一帧的 norm 约 128，1 LSB 级
/// 噪声的 norm 约 1e-6，阈值取 1e-9 只拦住“基本无声”的参考。
pub const AEC_MIN_REF_NORM: f64 = 1e-9;
/// NLMS 步长钳制（P2-22 第二道防线）：单次系数更新量不超过该值，
/// 参考能量极小但非零时滤波器也不会一步跳变。
pub const AEC_MAX_STEP: f64 = 1.0;
/// NLMS 步长（0 < mu < 2，默认 0.5，与 Hub 侧一致）。
pub const AEC_MU: f64 = 0.5;

/// 客户端回声消除器：NLMS + 播放参考延迟队列。
/// 只被录音任务独占（串行调用），不实现 Sync。
pub struct Aec {
    taps: [f64; AEC_TAPS],
    ref_ring: [f64; AEC_TAPS],
    pos: usize,
    mu: f64,
    /// 待对齐的播放参考帧队列（每帧 320 点）；队首是最早播放的帧。
    ref_delay: VecDeque<[i16; FRAME_SAMPLES]>,
}

impl Aec {
    pub fn new() -> Self {
        Self {
            taps: [0.0; AEC_TAPS],
            ref_ring: [0.0; AEC_TAPS],
            pos: 0,
            mu: AEC_MU,
            ref_delay: VecDeque::new(),
        }
    }

    /// 喂入一帧刚解码播放的 PCM（`run_session` 的 `play_frame` 产物）。
    /// 帧进延迟队列，不立即参与消除——对齐的是 `AEC_REF_DELAY_FRAMES`
    /// 帧前播放的信号（见模块头注释）。
    pub fn push_ref(&mut self, frame: [i16; FRAME_SAMPLES]) {
        self.ref_delay.push_back(frame);
    }

    /// 对一帧麦克风 PCM 做回声消除，返回消回声后的 PCM。
    /// 无播放参考（队列未攒够延迟帧）或参考全零时原样返回输入，
    /// 不改滤波器状态（P2-22：零能量参考不更新系数）。
    pub fn process_frame(&mut self, mic: &[i16; FRAME_SAMPLES]) -> [i16; FRAME_SAMPLES] {
        // 延迟对齐：队列长度超过延迟帧数时弹出最旧一帧作本帧的参考；
        // 否则参考为全零（短路返回原 PCM，不跑 NLMS 循环）。
        let reference = if self.ref_delay.len() > AEC_REF_DELAY_FRAMES {
            self.ref_delay.pop_front().unwrap()
        } else {
            return *mic;
        };
        if reference.iter().all(|s| *s == 0) {
            return *mic;
        }
        let mut out = [0i16; FRAME_SAMPLES];
        for i in 0..FRAME_SAMPLES {
            let r = reference[i] as f64 / 32768.0;
            let m = mic[i] as f64 / 32768.0;
            let e = self.process_sample(r, m);
            out[i] = (e * 32768.0).clamp(i16::MIN as f64, i16::MAX as f64) as i16;
        }
        // 延迟队列只保留最近的延迟帧数 + 1 帧（再多就是播放端卡住后
        // 的陈旧参考，丢掉，免得用几秒前的播放信号去“消除”现在的回声）。
        while self.ref_delay.len() > AEC_REF_DELAY_FRAMES + 1 {
            self.ref_delay.pop_front();
        }
        out
    }

    /// 单采样 NLMS：参考进环形缓冲 → 估计回声 y → 残差 e = mic - y；
    /// 参考窗能量低于阈值时只输出残差、不更新系数（P2-22），
    /// 步长钳制在 `±AEC_MAX_STEP` 内。
    fn process_sample(&mut self, reference: f64, mic: f64) -> f64 {
        self.ref_ring[self.pos] = reference;
        let mut y = 0.0;
        for i in 0..AEC_TAPS {
            let idx = (self.pos + AEC_TAPS - i) % AEC_TAPS;
            y += self.taps[i] * self.ref_ring[idx];
        }
        let e = mic - y;
        let mut norm = 0.0;
        for i in 0..AEC_TAPS {
            let idx = (self.pos + AEC_TAPS - i) % AEC_TAPS;
            norm += self.ref_ring[idx] * self.ref_ring[idx];
        }
        if norm >= AEC_MIN_REF_NORM {
            let step = (self.mu * e / (norm + 1e-6)).clamp(-AEC_MAX_STEP, AEC_MAX_STEP);
            for i in 0..AEC_TAPS {
                let idx = (self.pos + AEC_TAPS - i) % AEC_TAPS;
                self.taps[i] += step * self.ref_ring[idx];
            }
        }
        self.pos = (self.pos + 1) % AEC_TAPS;
        e
    }

    /// 滤波器系数最大绝对值（测试用：零能量参考下必须保持 0）。
    #[allow(dead_code)]
    pub fn taps_max_abs(&self) -> f64 {
        self.taps.iter().fold(0.0f64, |a, t| a.max(t.abs()))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::f32::consts::PI;

    fn sine_frame_i16(freq: f32, amp: i16) -> [i16; FRAME_SAMPLES] {
        let mut frame = [0i16; FRAME_SAMPLES];
        for (i, s) in frame.iter_mut().enumerate() {
            let t = i as f32 / 16000.0;
            *s = (amp as f32 * (2.0 * PI * freq * t).sin()) as i16;
        }
        frame
    }

    fn rms(pcm: &[i16]) -> f64 {
        let sum_sq: f64 = pcm.iter().map(|s| (*s as f64) * (*s as f64)).sum();
        (sum_sq / pcm.len() as f64).sqrt()
    }

    #[test]
    fn echo_suppression_synthetic_known_echo_plus_ref() {
        // 合成信号：参考 = 300Hz 正弦（幅值 8000）；麦克风 = 回声
        // （参考延迟 4 帧 + 40 点、衰减 0.5）+ 微弱语音（1000Hz，幅值 200）。
        // 延迟 4 帧 + 40 点 = 1320 点，正好是 AEC 延迟队列（4 帧）+
        // NLMS 抽头窗（256 点）能覆盖的范围。先适应 0.5s，再测 0.5s 残余。
        let mut aec = Aec::new();
        const TOTAL_FRAMES: usize = 50; // 1s @ 20ms/帧
        let mut ref_signal = Vec::new();
        for _ in 0..TOTAL_FRAMES {
            ref_signal.push(sine_frame_i16(300.0, 8000));
            // 参考帧的相位连续：sine_frame_i16 每帧都从 0 相位起，
            // 300Hz × 20ms = 6 个整周期，帧间相位恰好连续，可直接拼接。
        }
        // 麦克风信号：逐采样生成（回声延迟 1320 点）。
        let total_samples = TOTAL_FRAMES * FRAME_SAMPLES;
        let mut mic_signal = vec![0i16; total_samples];
        for i in 0..total_samples {
            let voice = 200.0 * (2.0 * PI * 1000.0 * i as f32 / 16000.0).sin();
            let echo = if i >= 1320 {
                let r = ref_signal[(i - 1320) / FRAME_SAMPLES][(i - 1320) % FRAME_SAMPLES] as f64;
                0.5 * r
            } else {
                0.0
            };
            mic_signal[i] = (voice as f64 + echo).clamp(i16::MIN as f64, i16::MAX as f64) as i16;
        }
        // 前 25 帧适应，后 25 帧测残余。
        let mut out_second_half: Vec<i16> = Vec::new();
        let mut in_second_half: Vec<i16> = Vec::new();
        for f in 0..TOTAL_FRAMES {
            aec.push_ref(ref_signal[f]);
            let mic_frame: [i16; FRAME_SAMPLES] = mic_signal
                [f * FRAME_SAMPLES..(f + 1) * FRAME_SAMPLES]
                .try_into()
                .unwrap();
            let out = aec.process_frame(&mic_frame);
            if f >= TOTAL_FRAMES / 2 {
                out_second_half.extend_from_slice(&out);
                in_second_half.extend_from_slice(&mic_frame);
            }
        }
        let in_rms = rms(&in_second_half);
        let out_rms = rms(&out_second_half);
        // 输入以回声为主（≈2828）；残余应 < 输入的 50%（与 Hub 侧
        // aec.TestEchoSuppressionSynthetic 同判定口径）。
        assert!(
            out_rms < 0.5 * in_rms,
            "echo suppression insufficient: in RMS {in_rms:.1} -> residual RMS {out_rms:.1} ({:.1}%)",
            100.0 * out_rms / in_rms
        );
        // 残余里应主要剩语音（200 幅值正弦 ≈ 141 RMS），而不是回声。
        assert!(
            out_rms < 800.0,
            "residual RMS {out_rms:.1} is far above the voice-only level (~141); echo not cancelled"
        );
    }

    #[test]
    fn zero_reference_passes_mic_through_unchanged_and_keeps_taps_zero() {
        // P2-22：从不播放（无参考帧）时输出必须与输入逐采样相同，
        // 滤波器系数保持全零。
        let mut aec = Aec::new();
        for _ in 0..10 {
            let mic = sine_frame_i16(1000.0, 12000);
            let out = aec.process_frame(&mic);
            assert_eq!(out, mic, "no reference: AEC must be a passthrough");
        }
        assert_eq!(aec.taps_max_abs(), 0.0);
        // 喂全零参考帧同样不改变输出与系数（零能量参考不更新）。
        for _ in 0..10 {
            aec.push_ref([0i16; FRAME_SAMPLES]);
            let mic = sine_frame_i16(1000.0, 12000);
            let out = aec.process_frame(&mic);
            assert_eq!(out, mic, "zero reference: AEC must be a passthrough");
        }
        assert_eq!(aec.taps_max_abs(), 0.0);
    }

    #[test]
    fn tiny_reference_does_not_explode() {
        // P2-22：参考只有 1 LSB 幅度时，输出必须有界（RMS 不超过输入量级）、
        // 系数有界，不能出现系数爆炸把输出顶到满幅。
        let mut aec = Aec::new();
        for _ in 0..20 {
            aec.push_ref([1i16; FRAME_SAMPLES]);
            let mic = sine_frame_i16(440.0, 10000);
            let out = aec.process_frame(&mic);
            assert!(
                rms(&out) < 20000.0,
                "tiny reference must not blow up the output RMS: {}",
                rms(&out)
            );
            assert!(
                aec.taps_max_abs() < 1.0,
                "tiny reference must not blow up taps: {}",
                aec.taps_max_abs()
            );
        }
    }

    #[test]
    fn pure_voice_without_echo_is_preserved() {
        // 无回声时（参考是静音、麦克风是纯语音），AEC 不应削弱语音：
        // 适应后输出 RMS 与输入 RMS 误差 <5%。
        let mut aec = Aec::new();
        let mut last_out = [0i16; FRAME_SAMPLES];
        for _ in 0..25 {
            aec.push_ref([0i16; FRAME_SAMPLES]);
            let mic = sine_frame_i16(1000.0, 12000);
            last_out = aec.process_frame(&mic);
        }
        let mic = sine_frame_i16(1000.0, 12000);
        let in_rms = rms(&mic);
        let out_rms = rms(&last_out);
        let err = (out_rms - in_rms).abs() / in_rms;
        assert!(
            err < 0.05,
            "pure voice must pass through: in RMS {in_rms:.1}, out RMS {out_rms:.1}"
        );
    }
}
