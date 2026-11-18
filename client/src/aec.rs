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
//!   - 客户端同时握有两路信号：`play_frame` 解出的播放 PCM（放出什么
//!     自己知道）与 `capture_pcm` 采到的麦克风 PCM（同一声卡、同一 16kHz
//!     采样率，但采集 `hw:0,0` 与播放 `hw:0,2` 是两个 PCM 设备，延迟按
//!     帧计数对齐而非墙钟——采样率稍有漂移即表现为延迟抖动，见 C-1）。
//!     参考信号只需经“播放参考延迟队列”对齐 ALSA 缓冲延迟，再进 NLMS，
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
//!   - 真实声学延迟（缓冲占用 + 扬声器-麦克风传播 + 设备内部延迟）需
//!     真机校准：启动时已按实际播放缓冲推导延迟帧数（`ref_delay_frames_for_buffer`，
//!     回读见 audio.rs `open_pcm`），缓冲被硬件取整为 3/5 周期时不再静默
//!     错位；但缓冲占用只是延迟的一部分，扬声器到麦克风的声学传播、
//!     驱动内部排队等仍未知，刷机验证时须实测回声延迟。延迟错出 NLMS
//!     单侧窗（标称延迟之后 0~16ms）时抑制率会明显下降。
//!   - 真实回声抑制率（dB）与打断场景下的残留：合成信号测试只能证明
//!     逻辑收敛，不能当作真机回声消除率。

use std::collections::VecDeque;

use crate::audio::FRAME_SAMPLES;

/// NLMS 抽头数：256 点 @16kHz ≈ 16ms 回声尾长（与 Hub 侧 `aec.New(256)` 一致）。
pub const AEC_TAPS: usize = 256;
/// 播放参考延迟（默认值）：4 帧 × 20ms = 80ms，等于请求的 ALSA 播放缓冲（4 周期）。
/// 含义：现在采到的麦克风帧里混的回声，对应的是 4 帧前解码播放出去的 PCM。
/// 启动时按实际播放缓冲推导（`ref_delay_frames_for_buffer`，回读见 audio.rs
/// `open_pcm`）；真实声学延迟仍需真机校准（见模块头注释）。NLMS 抽头窗是
/// 单侧的：只覆盖标称延迟之后 0~16ms——真实延迟比标称短、或长出 16ms 以上
/// 时回声都落窗外（不是“前后各 16ms”）。
pub const AEC_REF_DELAY_FRAMES: usize = 4;

/// 从回读的实际播放缓冲大小推导参考延迟帧数（C-1）：播放缓冲被写满后才
/// 依次放出，延迟 ≈ 整缓冲放空的时间 = buffer_frames / 320（20ms/帧），
/// 四舍五入，至少 1 帧。只依赖缓冲总帧数，不依赖周期数——周期被取整时
/// 只要缓冲总帧数不变，延迟就不变。
pub fn ref_delay_frames_for_buffer(buffer_frames: i64) -> usize {
    let frames = (buffer_frames.max(0) + (FRAME_SAMPLES as i64) / 2) / FRAME_SAMPLES as i64;
    (frames.max(1)) as usize
}
/// 参考能量下限（P2-22）：256 点参考窗的能量范数低于此值时跳过自适应
/// 更新——静默/零能量参考下步长 `mu*e/(norm+eps)` 不再爆炸。
/// 参考采样是 ±1.0 归一化浮点；满幅正弦一帧的 norm 约 128，1 LSB 级
/// 噪声的 norm 约 1e-6。注意（C-4）：阈值 1e-9 的口径是“范数和”
/// 而非能量——单采样 1 LSB 的平方范数 `(1/32768)² ≈ 9.3e-10 < 1e-9`，
/// 两个 1 LSB 采样即 `1.9e-9 > 阈值`，所以这道守卫只拦得住“全窗至多
/// 一个 1 LSB 采样”的毛刺；且全零帧早被 `process_frame` 的短路拦下。
/// 真正防发散的是第二道防线——步长钳制 `±AEC_MAX_STEP`（见
/// `process_sample`）。阈值不改（改能量口径的阈值会动行为，需另立项
/// 配测试）；这里只把注释写实，不许再把 1e-9 读成“静默守卫”。
pub const AEC_MIN_REF_NORM: f64 = 1e-9;
/// NLMS 步长钳制（P2-22 第二道防线）：单次系数更新量不超过该值，
/// 参考能量极小但非零时滤波器也不会一步跳变。
pub const AEC_MAX_STEP: f64 = 1.0;
/// NLMS 步长（0 < mu < 2，默认 0.5，与 Hub 侧一致）。
pub const AEC_MU: f64 = 0.5;

/// 客户端回声消除器：NLMS + 播放参考延迟队列。
/// 只被录音任务独占（串行调用），不实现 Sync。
/// 延迟帧数是实例状态（不是常量）：启动时按实际播放缓冲推导
/// （`ref_delay_frames_for_buffer`），会话重开播放设备后可经
/// `set_delay_frames` 热更新——队列按新延迟自调整（见 `process_frame`）。
pub struct Aec {
    taps: [f64; AEC_TAPS],
    ref_ring: [f64; AEC_TAPS],
    pos: usize,
    mu: f64,
    /// 当前参考延迟帧数（默认 `AEC_REF_DELAY_FRAMES`）。
    delay_frames: usize,
    /// 待对齐的播放参考帧队列（每帧 320 点）；队首是最早播放的帧。
    ref_delay: VecDeque<[i16; FRAME_SAMPLES]>,
}

impl Aec {
    /// 默认延迟（`AEC_REF_DELAY_FRAMES`）构造。生产路径现用
    /// `with_delay_frames`（按实际缓冲推导）；本函数留给测试与
    /// 不关心实际缓冲的调用方。
    #[allow(dead_code)]
    pub fn new() -> Self {
        Self::with_delay_frames(AEC_REF_DELAY_FRAMES)
    }

    /// 以指定延迟帧数构造（测试与“按实际缓冲推导”的启动路径用）。
    pub fn with_delay_frames(delay_frames: usize) -> Self {
        Self {
            taps: [0.0; AEC_TAPS],
            ref_ring: [0.0; AEC_TAPS],
            pos: 0,
            mu: AEC_MU,
            delay_frames: delay_frames.max(1),
            ref_delay: VecDeque::new(),
        }
    }

    /// 当前参考延迟帧数。
    pub fn delay_frames(&self) -> usize {
        self.delay_frames
    }

    /// 热更新参考延迟帧数（会话重开播放设备、缓冲变化时调用）：
    /// 只改对齐点，不动滤波器系数；队列在下一帧 `process_frame` 里
    /// 按新延迟自调整（多出的旧帧被弹出、不够时短路直通一帧）。
    pub fn set_delay_frames(&mut self, delay_frames: usize) {
        self.delay_frames = delay_frames.max(1);
    }

    /// 喂入一帧刚解码播放的 PCM（`run_session` 的 `play_frame` 产物）。
    /// 帧进延迟队列，不立即参与消除——对齐的是 `delay_frames`
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
        let reference = if self.ref_delay.len() > self.delay_frames {
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
        // 延迟队列只保留最近的延迟帧数（再多就是播放端卡住后
        // 的陈旧参考，丢掉，免得用几秒前的播放信号去“消除”现在的回声）。
        // 注意这里是 `> delay_frames` 而不是 `+ 1`：延迟热下调
        // （`set_delay_frames`）后队列必须立即缩到新长度，否则队首
        // 永远是旧延迟的帧、新延迟永不生效（C-1 回归测试覆盖）。
        while self.ref_delay.len() > self.delay_frames {
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
    fn pure_voice_with_nonzero_reference_is_preserved() {
        // C-3：旧版 `pure_voice_without_echo_is_preserved` 全程喂零参考，
        // 走 `process_frame` 的短路分支，根本没进 NLMS——“NLMS 不削弱
        // 语音”从未被测到。本版改喂非零参考（宽带噪声，幅值 8000），
        // 麦克风是纯语音（1000Hz 正弦，幅值 12000，无回声）。
        // 断言口径（总 RMS 与音调幅度都不能用）：
        //   - 总 RMS：NLMS 稳态失调给输出叠加与参考相关的噪声，实测
        //     总 RMS 10941 vs 输入 8485（+29%，与 mu=0.5 的失调理论
        //     量级一致），不是语音被削弱；
        //   - 音调幅度 |12000+Y|：Y 是输出里 1000Hz 处随机相位的噪声
        //     相量，|·| 有 Jensen 上偏（实测 10 帧均值 13815，+15%），
        //     量的是噪声不是语音。
        // 所以改量有符号正交投影：输入是 12000·sin，输出的 sin 分量
        // 须保留在 ±10% 内、cos 分量须 <10%（Y 的投影零均值，50 帧
        // 平均后起伏约 ±5%）。另加总 RMS 双边界：不得坍缩（<0.8×输入
        // 即语音被抵消）也不得爆炸（>1.5×输入即系数发散）。
        // 不要用纯音参考（如 300Hz 正弦）：实测 NLMS 时变抽头会把
        // 1000Hz 语音与 300Hz 参考互调出 1000Hz 分量、反向抵消约 36%
        // 语音（out RMS 5393 vs in RMS 8485），那是纯音合成信号的
        // 假象，不是真实播放信号（音乐/语音均为宽带）的行为。
        fn tone_projection(pcm: &[i16]) -> (f64, f64) {
            let mut s = 0.0;
            let mut c = 0.0;
            for (i, v) in pcm.iter().enumerate() {
                let phase = 2.0 * PI * 1000.0 * i as f32 / 16000.0;
                s += *v as f64 * phase.sin() as f64;
                c += *v as f64 * phase.cos() as f64;
            }
            let n = pcm.len() as f64;
            (2.0 * s / n, 2.0 * c / n)
        }
        let mut aec = Aec::new();
        let ref_signal = noise_signal(100 * FRAME_SAMPLES, 8000);
        let mut tail_out: Vec<i16> = Vec::new();
        for f in 0..100 {
            let ref_frame: [i16; FRAME_SAMPLES] = ref_signal
                [f * FRAME_SAMPLES..(f + 1) * FRAME_SAMPLES]
                .try_into()
                .unwrap();
            aec.push_ref(ref_frame);
            let mic = sine_frame_i16(1000.0, 12000);
            let out = aec.process_frame(&mic);
            if f >= 50 {
                tail_out.extend_from_slice(&out);
            }
        }
        let mic = sine_frame_i16(1000.0, 12000);
        let in_rms = rms(&mic);
        let out_rms = rms(&tail_out);
        // 语音分量：尾段 50 帧的有符号投影均值。输入 (12000, 0)。
        let mut sin_sum = 0.0;
        let mut cos_sum = 0.0;
        for chunk in tail_out.chunks(FRAME_SAMPLES) {
            let (s, c) = tone_projection(chunk);
            sin_sum += s;
            cos_sum += c;
        }
        let (out_sin, out_cos) = (sin_sum / 50.0, cos_sum / 50.0);
        assert!(
            (out_sin - 12000.0).abs() < 0.10 * 12000.0,
            "pure voice sin-component must be preserved with nonzero reference: in 12000.0, out {out_sin:.1}"
        );
        assert!(
            out_cos.abs() < 0.10 * 12000.0,
            "pure voice must not acquire a quadrature component: cos {out_cos:.1}"
        );
        assert!(
            out_rms > 0.8 * in_rms,
            "output RMS collapsed ({out_rms:.1} vs in {in_rms:.1}): voice is being cancelled"
        );
        assert!(
            out_rms < 1.5 * in_rms,
            "output RMS exploded ({out_rms:.1} vs in {in_rms:.1}): taps diverging on uncorrelated reference"
        );
    }

    #[test]
    fn echo_delay_off_by_one_frame_degrades_predictably() {
        // C-3：回声延迟比标称（4 帧 + 40 点 = 1320 点）差 ±1 帧时，
        // 回声落在 NLMS 单侧抽头窗（标称延迟之后 0~256 点）之外，
        // 抑制必须明显衰减——且衰减方向对称（早到/晚到都落窗外）。
        // 对照：对齐时（1320 点）抑制达标（<50% 残余）。
        let (in_aligned, out_aligned) = echo_suppression_noise(4, 1320);
        assert!(
            out_aligned < 0.5 * in_aligned,
            "aligned echo must be suppressed: in RMS {in_aligned:.1} -> residual RMS {out_aligned:.1}"
        );
        // 早 1 帧（1000 点）：回声比对齐点早 320 点，超出抽头窗。
        let (in_early, out_early) = echo_suppression_noise(4, 1000);
        assert!(
            out_early >= 0.5 * in_early,
            "echo 1 frame early must NOT be suppressed: in RMS {in_early:.1} -> residual RMS {out_early:.1} ({:.1}%)",
            100.0 * out_early / in_early
        );
        // 晚 1 帧（1640 点）：回声比对齐点晚 320 点，同样超出抽头窗。
        let (in_late, out_late) = echo_suppression_noise(4, 1640);
        assert!(
            out_late >= 0.5 * in_late,
            "echo 1 frame late must NOT be suppressed: in RMS {in_late:.1} -> residual RMS {out_late:.1} ({:.1}%)",
            100.0 * out_late / in_late
        );
        // 衰减可预期：失配残余须明显大于对齐残余（不只是“没达标”，
        // 而是“差一个量级”的对照，防止判据被整体抬高后蒙混过关）。
        assert!(
            out_early > 2.0 * out_aligned,
            "early-mismatch residual ({out_early:.1}) must be well above aligned residual ({out_aligned:.1})"
        );
        assert!(
            out_late > 2.0 * out_aligned,
            "late-mismatch residual ({out_late:.1}) must be well above aligned residual ({out_aligned:.1})"
        );
    }

    #[test]
    fn zero_reference_mid_stream_then_recovers() {
        // C-3：中途喂零参考帧后恢复——覆盖“播放暂停几帧又继续”的
        // 自愈路径。零参考期间 `process_frame` 短路直通（系数冻结、
        // 回声原样通过，是预期行为）；恢复非零参考后，已收敛的系数
        // 应接着用，尾段抑制须回到 <50% 残余。注意延迟队列的滞后：
        // 零参考要 5 帧才顶掉队首（队列 4 帧 + 当前帧），恢复同理，
        // 所以零参考段与恢复段各给足 10/25 帧，尾段只取恢复段后 10 帧。
        const ECHO_DELAY: usize = 1320; // 4 帧 + 40 点，对齐默认延迟
        const ADAPT_FRAMES: usize = 25;
        const ZERO_FRAMES: usize = 10;
        const RECOVER_FRAMES: usize = 25;
        const TOTAL_FRAMES: usize = ADAPT_FRAMES + ZERO_FRAMES + RECOVER_FRAMES;
        let total_samples = TOTAL_FRAMES * FRAME_SAMPLES;
        let ref_signal = noise_signal(total_samples, 8000);
        let mut mic_signal = vec![0i16; total_samples];
        for i in 0..total_samples {
            let voice = 200.0 * (2.0 * PI * 1000.0 * i as f32 / 16000.0).sin();
            let echo = if i >= ECHO_DELAY {
                0.5 * ref_signal[i - ECHO_DELAY] as f64
            } else {
                0.0
            };
            mic_signal[i] = (voice as f64 + echo).clamp(i16::MIN as f64, i16::MAX as f64) as i16;
        }
        let mut aec = Aec::new();
        let mut out_tail: Vec<i16> = Vec::new();
        let mut in_tail: Vec<i16> = Vec::new();
        for f in 0..TOTAL_FRAMES {
            // 适应段与恢复段喂真实参考，中间段喂全零参考。
            let ref_frame: [i16; FRAME_SAMPLES] =
                if (ADAPT_FRAMES..ADAPT_FRAMES + ZERO_FRAMES).contains(&f) {
                    [0i16; FRAME_SAMPLES]
                } else {
                    ref_signal[f * FRAME_SAMPLES..(f + 1) * FRAME_SAMPLES]
                        .try_into()
                        .unwrap()
                };
            aec.push_ref(ref_frame);
            let mic_frame: [i16; FRAME_SAMPLES] = mic_signal
                [f * FRAME_SAMPLES..(f + 1) * FRAME_SAMPLES]
                .try_into()
                .unwrap();
            let out = aec.process_frame(&mic_frame);
            if f >= TOTAL_FRAMES - 10 {
                out_tail.extend_from_slice(&out);
                in_tail.extend_from_slice(&mic_frame);
            }
        }
        let in_rms = rms(&in_tail);
        let out_rms = rms(&out_tail);
        assert!(
            out_rms < 0.5 * in_rms,
            "after zero-reference gap, echo must be suppressed again: in RMS {in_rms:.1} -> residual RMS {out_rms:.1}"
        );
        assert!(
            out_rms < 800.0,
            "residual RMS {out_rms:.1} is far above the voice-only level (~141); echo not cancelled after recovery"
        );
    }

    // --- C-1：按实际播放缓冲推导延迟帧数 + 延迟错位即失效 ---

    #[test]
    fn ref_delay_frames_for_buffer_values() {
        // 4 周期 × 320 = 1280 → 标称 4 帧；取整为 3/5 周期时据实推导。
        assert_eq!(ref_delay_frames_for_buffer(1280), 4);
        assert_eq!(ref_delay_frames_for_buffer(960), 3);
        assert_eq!(ref_delay_frames_for_buffer(1600), 5);
        assert_eq!(ref_delay_frames_for_buffer(640), 2);
        // 边界：0/负值钳制到 1 帧（总比用 0 帧错位强）；非整周期四舍五入。
        assert_eq!(ref_delay_frames_for_buffer(0), 1);
        assert_eq!(ref_delay_frames_for_buffer(-100), 1);
        assert_eq!(ref_delay_frames_for_buffer(1279), 4);
        assert_eq!(ref_delay_frames_for_buffer(1440), 5);
    }

    #[test]
    fn delay_frames_setter_clamps_to_at_least_one() {
        let aec = Aec::with_delay_frames(0);
        assert_eq!(aec.delay_frames(), 1);
        let mut aec = Aec::new();
        assert_eq!(aec.delay_frames(), AEC_REF_DELAY_FRAMES);
        aec.set_delay_frames(0);
        assert_eq!(aec.delay_frames(), 1);
        aec.set_delay_frames(5);
        assert_eq!(aec.delay_frames(), 5);
    }

    /// 确定性伪随机噪声流（LCG）：回声测试用非周期参考——纯正弦的
    /// 周期恰好整除帧长，延迟错位时 NLMS 仍能靠相位拟合“蒙对”，测不出
    /// C-1 的失配；白噪声只有对齐进抽头窗才能抵消。
    fn noise_signal(total_samples: usize, amp: i16) -> Vec<i16> {
        let mut state: u32 = 0x1234_5678;
        // 不用 with_capacity：semgrep 规则 3 要求 Vec 容量须为字面量/常量/.len()；
        // 测试 helper 的长度来自调用方，Vec::new + push 即可，无性能要求。
        let mut out = Vec::new();
        for _ in 0..total_samples {
            // LCG（glibc 常数），取高 15 位再缩放到 ±amp。
            state = state.wrapping_mul(1103515245).wrapping_add(12345);
            let v = ((state >> 16) & 0x7fff) as i32; // 0..32767
            out.push(((v - 16384) * i32::from(amp) / 16384) as i16);
        }
        out
    }

    /// 白噪声参考的回声抑制：参考为连续噪声流，麦克风 = 回声（参考延迟
    /// echo_delay_samples、衰减 0.5）+ 微弱语音（1000Hz，幅值 200）。
    /// 返回后半段（已适应）的（输入 RMS，输出 RMS）。
    fn echo_suppression_noise(aec_delay_frames: usize, echo_delay_samples: usize) -> (f64, f64) {
        const TOTAL_FRAMES: usize = 50; // 1s @ 20ms/帧
        let total_samples = TOTAL_FRAMES * FRAME_SAMPLES;
        let ref_signal = noise_signal(total_samples, 8000);
        let mut mic_signal = vec![0i16; total_samples];
        for i in 0..total_samples {
            let voice = 200.0 * (2.0 * PI * 1000.0 * i as f32 / 16000.0).sin();
            let echo = if i >= echo_delay_samples {
                0.5 * ref_signal[i - echo_delay_samples] as f64
            } else {
                0.0
            };
            mic_signal[i] = (voice as f64 + echo).clamp(i16::MIN as f64, i16::MAX as f64) as i16;
        }
        let mut aec = Aec::with_delay_frames(aec_delay_frames);
        let mut out_second_half: Vec<i16> = Vec::new();
        let mut in_second_half: Vec<i16> = Vec::new();
        for f in 0..TOTAL_FRAMES {
            let ref_frame: [i16; FRAME_SAMPLES] = ref_signal
                [f * FRAME_SAMPLES..(f + 1) * FRAME_SAMPLES]
                .try_into()
                .unwrap();
            aec.push_ref(ref_frame);
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
        (rms(&in_second_half), rms(&out_second_half))
    }

    #[test]
    fn custom_delay_suppresses_echo_at_that_delay() {
        // 回声延迟 3 帧 + 40 点 = 1000 点，落在 3 帧延迟 + 256 抽头窗内
        // （960~1216 点）；配 3 帧延迟应与默认 4 帧同等抑制。
        let (in_rms, out_rms) = echo_suppression_noise(3, 1000);
        assert!(
            out_rms < 0.5 * in_rms,
            "delay 3 must suppress echo at 1000 samples: in RMS {in_rms:.1} -> residual RMS {out_rms:.1}"
        );
        assert!(
            out_rms < 800.0,
            "residual RMS {out_rms:.1} is far above the voice-only level (~141); echo not cancelled"
        );
    }

    #[test]
    fn delay_mismatch_leaves_echo_unsuppressed() {
        // C-1 回归：回声在 3 帧 + 40 点（1000 点），Aec 仍用默认 4 帧——
        // 回声比参考对齐点早 280 点，超出 NLMS 单侧窗（256 点），抑制必须
        // 明显失效（50% 抑制判据不达标）。这正是旧代码在缓冲被硬件取整
        // 为 3 周期时静默发生的情形。
        let (in_rms, out_rms) = echo_suppression_noise(4, 1000);
        assert!(
            out_rms >= 0.5 * in_rms,
            "mismatched delay must NOT suppress echo: in RMS {in_rms:.1} -> residual RMS {out_rms:.1} ({:.1}%)",
            100.0 * out_rms / in_rms
        );
    }

    #[test]
    fn set_delay_frames_mid_stream_retunes_to_new_delay() {
        // 前 25 帧延迟错位（4 帧 vs 回声 3 帧 + 40 点），不抑制；第 25 帧
        // 起改成 3 帧，再适应 25 帧后应恢复抑制——覆盖 main.rs 会话重开
        // 播放设备后热更新延迟的路径。
        const TOTAL_FRAMES: usize = 75; // 1.5s @ 20ms/帧
        const ECHO_DELAY: usize = 1000; // 3 帧 + 40 点
        let total_samples = TOTAL_FRAMES * FRAME_SAMPLES;
        let ref_signal = noise_signal(total_samples, 8000);
        let mut mic_signal = vec![0i16; total_samples];
        for i in 0..total_samples {
            let voice = 200.0 * (2.0 * PI * 1000.0 * i as f32 / 16000.0).sin();
            let echo = if i >= ECHO_DELAY {
                0.5 * ref_signal[i - ECHO_DELAY] as f64
            } else {
                0.0
            };
            mic_signal[i] = (voice as f64 + echo).clamp(i16::MIN as f64, i16::MAX as f64) as i16;
        }
        let mut aec = Aec::with_delay_frames(4);
        let mut out_tail: Vec<i16> = Vec::new();
        let mut in_tail: Vec<i16> = Vec::new();
        for f in 0..TOTAL_FRAMES {
            if f == 25 {
                aec.set_delay_frames(3);
            }
            let ref_frame: [i16; FRAME_SAMPLES] = ref_signal
                [f * FRAME_SAMPLES..(f + 1) * FRAME_SAMPLES]
                .try_into()
                .unwrap();
            aec.push_ref(ref_frame);
            let mic_frame: [i16; FRAME_SAMPLES] = mic_signal
                [f * FRAME_SAMPLES..(f + 1) * FRAME_SAMPLES]
                .try_into()
                .unwrap();
            let out = aec.process_frame(&mic_frame);
            if f >= 50 {
                out_tail.extend_from_slice(&out);
                in_tail.extend_from_slice(&mic_frame);
            }
        }
        let in_rms = rms(&in_tail);
        let out_rms = rms(&out_tail);
        assert!(
            out_rms < 0.5 * in_rms,
            "after retune to delay 3, echo must be suppressed: in RMS {in_rms:.1} -> residual RMS {out_rms:.1}"
        );
    }
}
