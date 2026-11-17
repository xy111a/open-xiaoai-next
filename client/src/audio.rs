//! 音频：Opus 编解码 + 帧封装（Phase 1）。
//!
//! 参数（按 docs/PROTOCOL.md 冻结值，别改）：
//!   16kHz 单声道 16-bit PCM，20ms 一帧（320 采样点），VOIP 应用，约 16kbps。
//!   音乐不同路：Snapcast 客户端独立进程走 48kHz，不经过本模块，避免 ALSA 抢占。
//!
//! 编解码器：`opus-rs`（libopus 1.6 的纯 Rust 移植，无 C 依赖、无系统 libopus，
//! ARMv7 musl 交叉编译不卡住）。来源见 Cargo.toml 注释。
//!
//! ALSA 接线（任务 E，2026-11-17）：
//!   - 硬件隔离：`AudioDevice` trait（`capture` / `play`）；真实设备是
//!     `AlsaAudioDevice`（`alsa` crate，feature `alsa` 门控，默认开），
//!     测试用 `FakeAudioDevice`，无硬件/未编译 ALSA 时用 `NoAudioDevice`。
//!   - 设备名（按 REA_ANALYSIS.md asound.conf）：采集 `hw:0,0`，播放 `hw:0,2`；
//!     格式 S16LE / 16kHz / 单声道 / 20ms 周期（320 采样点），缓冲 4 周期。
//!   - 无硬件时：`Audio::open()` 仍成功（构造器不因缺硬件失败），`capture_pcm` /
//!     `play_frame` 返回明确错误（带设备名与原因），调用方（main.rs 录音任务）
//!     记日志后退出，主循环继续跑传输、不崩溃、不造假音频（冒烟行为保持）。
//!   - XRUN（EPIPE）：`prepare()` 后各重试一次，仍失败才返回错误。
//!   - 真机实测仍 BLOCKED（本 VM 无 /dev/snd、无音箱硬件）：见 README。
//!   - 注意：`capture_pcm` 是阻塞读（约 20ms），录音任务是独立 tokio 任务，
//!     不阻塞主循环；不要在主循环里直接调它。

use anyhow::{anyhow, Result};
use opus_rs::{Application, OpusDecoder, OpusEncoder};

/// 采样率：16kHz（协议冻结值）。
pub const SAMPLE_RATE: u32 = 16_000;
/// 每帧采样点数：20ms @ 16kHz。
pub const FRAME_SAMPLES: usize = 320;
/// 每帧时长：20ms（协议常量，供调用方/后续相位用）。
#[allow(dead_code)]
pub const FRAME_DURATION_MS: u32 = 20;
/// 目标码率：约 16kbps（协议冻结值）。opus-rs 默认 64kbps，必须显式改。
pub const TARGET_BITRATE_BPS: i32 = 16_000;
/// 二进制帧类型字节：0x01 = Opus 语音帧（协议冻结值）。
pub const AUDIO_FRAME_TYPE: u8 = 0x01;
/// Opus 单帧最大包长（RFC 6716 上限 1276，留余量）。
const MAX_PACKET_BYTES: usize = 1276;

/// 流式编解码器：一对 Encoder/Decoder 长期持有（每 20ms 一帧复用，
/// 不要每帧新建——编码器内部有状态，新建会丢连续性并浪费 ~250KB 状态分配）。
pub struct OpusCodec {
    encoder: OpusEncoder,
    decoder: OpusDecoder,
}

impl OpusCodec {
    pub fn new() -> Result<Self> {
        let mut encoder = OpusEncoder::new(SAMPLE_RATE as i32, 1, Application::Voip)
            .map_err(|e| anyhow!("opus encoder init failed: {e}"))?;
        // opus-rs 默认码率 64kbps；协议要求约 16kbps，显式改（pub 字段）。
        encoder.bitrate_bps = TARGET_BITRATE_BPS;
        let decoder = OpusDecoder::new(SAMPLE_RATE as i32, 1)
            .map_err(|e| anyhow!("opus decoder init failed: {e}"))?;
        Ok(Self { encoder, decoder })
    }

    /// 编码一帧 20ms PCM（320 采样点）→ Opus 包。
    pub fn encode_frame(&mut self, pcm: &[i16; FRAME_SAMPLES]) -> Result<Vec<u8>> {
        let mut out = vec![0u8; MAX_PACKET_BYTES];
        let n = self
            .encoder
            .encode_i16(pcm, FRAME_SAMPLES, &mut out)
            .map_err(|e| anyhow!("opus encode failed: {e}"))?;
        out.truncate(n);
        Ok(out)
    }

    /// 编码任意长度 PCM 切片；长度必须正好是 320（错了就报错，不静默截断）。
    #[allow(dead_code)]
    pub fn encode_frame_slice(&mut self, pcm: &[i16]) -> Result<Vec<u8>> {
        let frame: &[i16; FRAME_SAMPLES] = pcm.try_into().map_err(|_| {
            anyhow!(
                "pcm frame must be exactly {FRAME_SAMPLES} samples, got {}",
                pcm.len()
            )
        })?;
        self.encode_frame(frame)
    }

    /// 解码一帧 Opus 包 → 320 采样点 PCM（opus-rs 解码输出 f32，转回 i16）。
    pub fn decode_frame(&mut self, opus: &[u8]) -> Result<Vec<i16>> {
        if opus.is_empty() {
            return Err(anyhow!("empty opus packet"));
        }
        let mut f32buf = vec![0f32; FRAME_SAMPLES];
        let n = self
            .decoder
            .decode(opus, FRAME_SAMPLES, &mut f32buf)
            .map_err(|e| anyhow!("opus decode failed: {e}"))?;
        Ok(f32buf[..n].iter().map(|s| f32_to_i16(*s)).collect())
    }
}

/// f32（±1.0）→ i16，钳制防溢出（opus-rs 解码输出即此约定）。
pub fn f32_to_i16(s: f32) -> i16 {
    (s * 32768.0).clamp(i16::MIN as f32, i16::MAX as f32) as i16
}

use std::cell::RefCell;

thread_local! {
    /// 任务书指定的自由函数形态：`encode_pcm_frame(&[i16;320]) -> Vec<u8>`。
    /// 用线程本地编解码器复用，避免每帧重建编码器状态。
    static CODEC: RefCell<OpusCodec> = RefCell::new(
        OpusCodec::new().expect("opus codec init must succeed"),
    );
}

/// 编码一帧 20ms PCM → Opus 包（任务书签名；编解码失败时 panic，
/// 调用方应在流式路径用 `OpusCodec` 并处理 Result）。
#[allow(dead_code)]
pub fn encode_pcm_frame(pcm: &[i16; FRAME_SAMPLES]) -> Vec<u8> {
    CODEC.with(|c| {
        c.borrow_mut()
            .encode_frame(pcm)
            .expect("opus encode must succeed for a valid 320-sample frame")
    })
}

/// 解码一帧 Opus 包 → PCM（任务书签名；失败时 panic，流式路径用 `OpusCodec`）。
#[allow(dead_code)]
pub fn decode_opus_frame(opus: &[u8]) -> Vec<i16> {
    CODEC.with(|c| {
        c.borrow_mut()
            .decode_frame(opus)
            .expect("opus decode must succeed for a packet produced by encode_pcm_frame")
    })
}

/// 给 Opus 包加协议帧头：第 1 字节 0x01，其后是 Opus 数据。
pub fn wrap_audio_frame(opus: &[u8]) -> Vec<u8> {
    let mut buf = Vec::with_capacity(opus.len() + 1);
    buf.push(AUDIO_FRAME_TYPE);
    buf.extend_from_slice(opus);
    buf
}

/// 去协议帧头：首字节必须是 0x01，否则返回 None（不是语音帧）。
pub fn unwrap_audio_frame(data: &[u8]) -> Option<&[u8]> {
    match data {
        [AUDIO_FRAME_TYPE, rest @ ..] => Some(rest),
        _ => None,
    }
}

/// 采集设备名（REA_ANALYSIS.md asound.conf；LX06/OH2P 录音口）。
pub const CAPTURE_DEVICE_DEFAULT: &str = "hw:0,0";
/// 播放设备名（REA_ANALYSIS.md asound.conf；LX06/OH2P 放音口）。
pub const PLAYBACK_DEVICE_DEFAULT: &str = "hw:0,2";
/// ALSA 缓冲：4 个 20ms 周期（80ms），兼顾延迟与抗抖动。
const ALSA_BUFFER_PERIODS: u32 = 4;

/// 音频硬件抽象：采集一帧 / 播放一段 PCM。真实 ALSA、Fake、NoDevice
/// 都实现同一 trait，`Audio` 只依赖它，不直接碰硬件。
/// 注意：`capture` 是阻塞调用（约 20ms 等一帧），调用方应在独立任务里跑。
pub trait AudioDevice: Send {
    /// 采集一帧 20ms PCM（320 采样点 @16kHz）。无硬件/失败时返回明确错误。
    fn capture(&mut self) -> Result<[i16; FRAME_SAMPLES]>;
    /// 播放一段 PCM（任意长度，通常 320 采样点）。无硬件/失败时返回明确错误。
    fn play(&mut self, pcm: &[i16]) -> Result<()>;
    /// 一行状态描述（设备名 + 打开结果），供日志/诊断用。
    fn describe(&self) -> String;
}

/// 无可用音频硬件（或编译时未开 `alsa` feature）：采集/播放一律返回
/// 明确错误，绝不返回静默/假数据。
/// 注：开 `alsa` feature 的生产构建里它不被构造（只在 feature 关时用），
/// 但它是无硬件路径的正式实现，不是死代码；测试也用它。
#[allow(dead_code)]
pub struct NoAudioDevice {
    reason: String,
}

#[allow(dead_code)]
impl NoAudioDevice {
    pub fn new(reason: impl Into<String>) -> Self {
        Self {
            reason: reason.into(),
        }
    }
}

impl AudioDevice for NoAudioDevice {
    fn capture(&mut self) -> Result<[i16; FRAME_SAMPLES]> {
        Err(anyhow!("ALSA capture unavailable: {}", self.reason))
    }

    fn play(&mut self, _pcm: &[i16]) -> Result<()> {
        Err(anyhow!("ALSA playback unavailable: {}", self.reason))
    }

    fn describe(&self) -> String {
        format!("NoAudioDevice ({})", self.reason)
    }
}

/// 真实 ALSA 设备（feature `alsa` 门控）。
///
/// 两路独立打开、互不拖累：采集打不开不影响播放，反之亦然；打开失败的那路
/// 记下原因，`capture`/`play` 用到时才返回带设备名的明确错误。
/// 构造器本身永不因缺硬件失败——`Audio::open()` 在无硬件机器上也成功，
/// 保持 Phase 1 冒烟行为（录音任务记日志退出，主循环继续跑传输）。
#[cfg(feature = "alsa")]
pub struct AlsaAudioDevice {
    capture: Option<alsa::pcm::PCM>,
    capture_err: Option<String>,
    playback: Option<alsa::pcm::PCM>,
    playback_err: Option<String>,
    capture_dev: String,
    playback_dev: String,
}

#[cfg(feature = "alsa")]
impl AlsaAudioDevice {
    /// 打开采集 + 播放设备（各路失败只记录，不整体失败）。
    pub fn open_devices(capture_dev: &str, playback_dev: &str) -> Self {
        let (capture, capture_err) = match open_pcm(capture_dev, alsa::Direction::Capture) {
            Ok(p) => (Some(p), None),
            Err(e) => {
                tracing::warn!("ALSA capture device {capture_dev} open failed: {e}");
                (None, Some(e))
            }
        };
        let (playback, playback_err) = match open_pcm(playback_dev, alsa::Direction::Playback) {
            Ok(p) => (Some(p), None),
            Err(e) => {
                tracing::warn!("ALSA playback device {playback_dev} open failed: {e}");
                (None, Some(e))
            }
        };
        Self {
            capture,
            capture_err,
            playback,
            playback_err,
            capture_dev: capture_dev.to_string(),
            playback_dev: playback_dev.to_string(),
        }
    }
}

/// 打开一路 PCM 并按协议设好硬件参数（S16LE / 16kHz / 单声道 / 20ms 周期）。
#[cfg(feature = "alsa")]
fn open_pcm(device: &str, dir: alsa::Direction) -> std::result::Result<alsa::pcm::PCM, String> {
    let pcm = alsa::pcm::PCM::new(device, dir, false)
        .map_err(|e| format!("snd_pcm_open {device} failed: {e} (errno {})", e.errno()))?;
    // HwParams 借用 pcm，设完即 drop（HwParams 的 Drop 会释放参数结构），
    // 否则 pcm 无法移出返回。
    {
        let hwp = alsa::pcm::HwParams::any(&pcm)
            .map_err(|e| format!("HwParams::any {device} failed: {e} (errno {})", e.errno()))?;
        hwp.set_access(alsa::pcm::Access::RWInterleaved)
            .map_err(|e| format!("set_access {device} failed: {e}"))?;
        hwp.set_format(alsa::pcm::Format::S16LE)
            .map_err(|e| format!("set_format S16LE {device} failed: {e}"))?;
        let rate = hwp
            .set_rate_near(SAMPLE_RATE, alsa::ValueOr::Nearest)
            .map_err(|e| format!("set_rate_near {device} failed: {e}"))?;
        if rate != SAMPLE_RATE {
            return Err(format!(
                "device {device} cannot do {SAMPLE_RATE}Hz, nearest is {rate}Hz"
            ));
        }
        hwp.set_channels(1)
            .map_err(|e| format!("set_channels 1 {device} failed: {e}"))?;
        hwp.set_period_size_near(FRAME_SAMPLES as alsa::pcm::Frames, alsa::ValueOr::Nearest)
            .map_err(|e| format!("set_period_size_near {device} failed: {e}"))?;
        // 注意：alsa::pcm::Frames 在 64-bit 是 i64、32-bit ARM 是 i32，
        // 两边都先转 Frames 再乘，避免 i64*i32 类型错。
        hwp.set_buffer_size_near(
            (ALSA_BUFFER_PERIODS as alsa::pcm::Frames) * (FRAME_SAMPLES as alsa::pcm::Frames),
        )
        .map_err(|e| format!("set_buffer_size_near {device} failed: {e}"))?;
        pcm.hw_params(&hwp)
            .map_err(|e| format!("hw_params {device} failed: {e} (errno {})", e.errno()))?;
    }
    Ok(pcm)
}

#[cfg(feature = "alsa")]
impl AudioDevice for AlsaAudioDevice {
    fn capture(&mut self) -> Result<[i16; FRAME_SAMPLES]> {
        let pcm = self.capture.as_ref().ok_or_else(|| {
            anyhow!(
                "ALSA capture unavailable: device {} open failed: {}",
                self.capture_dev,
                self.capture_err.as_deref().unwrap_or("unknown error")
            )
        })?;
        let mut buf = [0i16; FRAME_SAMPLES];
        let mut filled = 0usize;
        let mut xruns = 0u32;
        // 循环读满一帧：readi 可能返回不足 320 的短读。
        while filled < FRAME_SAMPLES {
            match pcm.io_i16().and_then(|io| io.readi(&mut buf[filled..])) {
                Ok(0) => {
                    return Err(anyhow!(
                        "ALSA capture read on {} returned 0 frames (device closed?)",
                        self.capture_dev
                    ));
                }
                Ok(n) => filled += n,
                Err(e) if e.errno() == libc::EPIPE => {
                    // XRUN（overrun）：prepare 后重试一次，最多两次。
                    xruns += 1;
                    if xruns > 2 {
                        return Err(anyhow!(
                            "ALSA capture xrun on {} persists after prepare, giving up: {e}",
                            self.capture_dev
                        ));
                    }
                    tracing::warn!("ALSA capture xrun on {}, prepare+retry", self.capture_dev);
                    pcm.prepare().map_err(|pe| {
                        anyhow!("ALSA capture prepare on {} failed: {pe}", self.capture_dev)
                    })?;
                }
                Err(e) => {
                    return Err(anyhow!(
                        "ALSA capture read on {} failed: {e} (errno {})",
                        self.capture_dev,
                        e.errno()
                    ));
                }
            }
        }
        Ok(buf)
    }

    fn play(&mut self, pcm_samples: &[i16]) -> Result<()> {
        let pcm = self.playback.as_ref().ok_or_else(|| {
            anyhow!(
                "ALSA playback unavailable: device {} open failed: {}",
                self.playback_dev,
                self.playback_err.as_deref().unwrap_or("unknown error")
            )
        })?;
        if pcm_samples.is_empty() {
            return Ok(());
        }
        let mut written = 0usize;
        let mut xruns = 0u32;
        while written < pcm_samples.len() {
            match pcm
                .io_i16()
                .and_then(|io| io.writei(&pcm_samples[written..]))
            {
                Ok(0) => {
                    return Err(anyhow!(
                        "ALSA playback write on {} returned 0 frames (device closed?)",
                        self.playback_dev
                    ));
                }
                Ok(n) => written += n,
                Err(e) if e.errno() == libc::EPIPE => {
                    xruns += 1;
                    if xruns > 2 {
                        return Err(anyhow!(
                            "ALSA playback xrun on {} persists after prepare, giving up: {e}",
                            self.playback_dev
                        ));
                    }
                    tracing::warn!("ALSA playback xrun on {}, prepare+retry", self.playback_dev);
                    pcm.prepare().map_err(|pe| {
                        anyhow!(
                            "ALSA playback prepare on {} failed: {pe}",
                            self.playback_dev
                        )
                    })?;
                }
                Err(e) => {
                    return Err(anyhow!(
                        "ALSA playback write on {} failed: {e} (errno {})",
                        self.playback_dev,
                        e.errno()
                    ));
                }
            }
        }
        Ok(())
    }

    fn describe(&self) -> String {
        format!(
            "AlsaAudioDevice capture={} [{}] playback={} [{}]",
            self.capture_dev,
            if self.capture.is_some() {
                "open".to_string()
            } else {
                format!("FAILED: {}", self.capture_err.as_deref().unwrap_or("?"))
            },
            self.playback_dev,
            if self.playback.is_some() {
                "open".to_string()
            } else {
                format!("FAILED: {}", self.playback_err.as_deref().unwrap_or("?"))
            },
        )
    }
}

/// 音频设备：Opus 编解码器 + 一个 `AudioDevice`（真实 ALSA / Fake / 无设备）。
/// 构造器 `open()` 永远不因缺硬件失败；缺硬件时 `capture_pcm`/`play_frame`
/// 返回明确错误（见 NoAudioDevice）。
pub struct Audio {
    codec: OpusCodec,
    device: Box<dyn AudioDevice>,
}

impl Audio {
    /// 打开默认设备（采集 `hw:0,0` / 播放 `hw:0,2`）。无硬件时仍成功，
    /// 只是 capture/play 会返回明确错误；Opus 初始化失败时才返回 Err。
    pub fn open() -> Result<Self> {
        Self::open_devices(CAPTURE_DEVICE_DEFAULT, PLAYBACK_DEVICE_DEFAULT)
    }

    /// 打开指定设备名（供配置/测试覆盖默认设备名用）。两路各开各的，
    /// 失败只记录、不整体失败。
    pub fn open_devices(capture_dev: &str, playback_dev: &str) -> Result<Self> {
        #[cfg(feature = "alsa")]
        let device: Box<dyn AudioDevice> =
            Box::new(AlsaAudioDevice::open_devices(capture_dev, playback_dev));
        #[cfg(not(feature = "alsa"))]
        let device: Box<dyn AudioDevice> = Box::new(NoAudioDevice::new(format!(
            "binary built without the `alsa` feature (ARM cross build without musl libasound); requested capture={capture_dev} playback={playback_dev}"
        )));
        let audio = Self {
            codec: OpusCodec::new()?,
            device,
        };
        tracing::info!("audio device: {}", audio.device.describe());
        Ok(audio)
    }

    /// 注入任意 `AudioDevice` 实现（测试用 Fake；生产不用）。
    #[allow(dead_code)]
    pub fn with_device(device: Box<dyn AudioDevice>) -> Result<Self> {
        Ok(Self {
            codec: OpusCodec::new()?,
            device,
        })
    }

    /// 采集一帧 20ms PCM（320 采样点 @16kHz）。
    /// 有硬件时返回真实 PCM；无硬件时返回明确错误（绝不返回静默/假数据）。
    pub fn capture_pcm(&mut self) -> Result<[i16; FRAME_SAMPLES]> {
        self.device.capture()
    }

    /// 采集一帧 20ms PCM，Opus 编码后返回。
    #[allow(dead_code)]
    pub fn capture_frame(&mut self) -> Result<Vec<u8>> {
        let pcm = self.capture_pcm()?;
        self.codec.encode_frame(&pcm)
    }

    /// 解码一帧 Opus → 写 ALSA 播放 → 返回解出的 PCM。
    /// 有硬件时真实输出；无硬件时返回明确错误（解码本身成功也不谎报已播放）。
    pub fn play_frame(&mut self, opus: &[u8]) -> Result<Vec<i16>> {
        let pcm = self.codec.decode_frame(opus)?;
        self.device.play(&pcm)?;
        Ok(pcm)
    }

    /// 直接编码已采集到的 PCM 帧（供 KWS/捕获任务用）。
    pub fn encode_pcm(&mut self, pcm: &[i16; FRAME_SAMPLES]) -> Result<Vec<u8>> {
        self.codec.encode_frame(pcm)
    }

    /// 直接解码（供播放任务用）。
    #[allow(dead_code)]
    pub fn decode_opus(&mut self, opus: &[u8]) -> Result<Vec<i16>> {
        self.codec.decode_frame(opus)
    }
}

/// 一段 PCM 的 RMS（线性，满刻度 1.0）；空输入返回 0.0。
pub fn rms_linear(pcm: &[i16]) -> f32 {
    if pcm.is_empty() {
        return 0.0;
    }
    let sum_sq: f64 = pcm.iter().map(|s| (*s as f64) * (*s as f64)).sum();
    ((sum_sq / pcm.len() as f64).sqrt() / 32768.0) as f32
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::f32::consts::PI;

    /// 16kHz 正弦波一帧：频率 freq，幅度 amp（i16 峰值）。
    fn sine_frame(freq: f32, amp: i16) -> [i16; FRAME_SAMPLES] {
        let mut frame = [0i16; FRAME_SAMPLES];
        for (i, s) in frame.iter_mut().enumerate() {
            let t = i as f32 / SAMPLE_RATE as f32;
            *s = (amp as f32 * (2.0 * PI * freq * t).sin()) as i16;
        }
        frame
    }

    fn rms_of(pcm: &[i16]) -> f32 {
        rms_linear(pcm)
    }

    #[test]
    fn opus_roundtrip_sine_rms_within_10_percent() {
        let mut codec = OpusCodec::new().unwrap();
        // 前 3 帧让编码器进入稳态（Opus 有 lookahead，首帧失真大，不计入断言）。
        let mut decoded_last = Vec::new();
        let mut encoded_total_bytes = 0usize;
        let mut frames = 0usize;
        for _ in 0..50 {
            let frame = sine_frame(1000.0, 12000);
            let packet = codec.encode_frame(&frame).unwrap();
            assert!(!packet.is_empty(), "encoder must produce a packet");
            assert!(packet.len() <= MAX_PACKET_BYTES);
            encoded_total_bytes += packet.len();
            frames += 1;
            decoded_last = codec.decode_frame(&packet).unwrap();
            assert_eq!(
                decoded_last.len(),
                FRAME_SAMPLES,
                "decoded frame must be 320 samples"
            );
        }
        // 解码后 RMS 误差 <10%（Opus 是有损编码；16kbps 下正弦波应远优于此）。
        let orig_rms = rms_of(&sine_frame(1000.0, 12000));
        let dec_rms = rms_of(&decoded_last);
        let err = (dec_rms - orig_rms).abs() / orig_rms;
        assert!(
            err < 0.10,
            "decoded RMS {dec_rms:.4} vs original {orig_rms:.4}, error {:.1}% exceeds 10%",
            err * 100.0
        );
        // 平均码率应在约 16kbps 附近（20ms/帧 → 40 字节/帧 = 16kbps；
        // VBR 下正弦波可能更低，允许 4~40kbps，即 10~100 字节/帧）。
        let avg_bytes = encoded_total_bytes as f64 / frames as f64;
        assert!(
            (10.0..=100.0).contains(&avg_bytes),
            "avg packet {avg_bytes:.1} bytes/frame is outside 10..100 (4..40kbps)"
        );
    }

    #[test]
    fn free_function_roundtrip() {
        let frame = sine_frame(440.0, 10000);
        // 多喂几帧进线程本地编解码器以过稳态，再测最后一帧。
        let mut decoded = Vec::new();
        for _ in 0..10 {
            let packet = encode_pcm_frame(&frame);
            decoded = decode_opus_frame(&packet);
        }
        assert_eq!(decoded.len(), FRAME_SAMPLES);
        let err = (rms_of(&decoded) - rms_of(&frame)).abs() / rms_of(&frame);
        assert!(
            err < 0.10,
            "free-function roundtrip RMS error {:.1}%",
            err * 100.0
        );
    }

    #[test]
    fn silence_frame_roundtrips() {
        let mut codec = OpusCodec::new().unwrap();
        let silence = [0i16; FRAME_SAMPLES];
        let packet = codec.encode_frame(&silence).unwrap();
        let decoded = codec.decode_frame(&packet).unwrap();
        assert_eq!(decoded.len(), FRAME_SAMPLES);
        assert!(
            rms_of(&decoded) < 0.01,
            "decoded silence RMS {} too high",
            rms_of(&decoded)
        );
    }

    #[test]
    fn encode_rejects_wrong_length() {
        let mut codec = OpusCodec::new().unwrap();
        assert!(codec.encode_frame_slice(&[0i16; 100]).is_err());
        assert!(codec.encode_frame_slice(&[0i16; 321]).is_err());
        assert!(codec.encode_frame_slice(&[0i16; 320]).is_ok());
    }

    #[test]
    fn decode_rejects_empty_packet() {
        let mut codec = OpusCodec::new().unwrap();
        assert!(codec.decode_frame(&[]).is_err());
    }

    #[test]
    fn audio_frame_wrapping() {
        let opus = vec![0xAA, 0xBB, 0xCC];
        let wrapped = wrap_audio_frame(&opus);
        assert_eq!(wrapped[0], 0x01);
        assert_eq!(&wrapped[1..], &opus[..]);
        assert_eq!(unwrap_audio_frame(&wrapped), Some(opus.as_slice()));
        assert_eq!(unwrap_audio_frame(&[0x02, 0xAA]), None);
        assert_eq!(unwrap_audio_frame(&[]), None);
        assert_eq!(unwrap_audio_frame(&[0x01]), Some([].as_slice()));
    }

    #[test]
    fn capture_frame_is_honest_blocked_error() {
        // 无硬件（NoAudioDevice）：必须返回明确错误，不能返回假音频。
        // 用显式 NoAudioDevice 而不用 Audio::open()，使测试在任何机器上都确定
        // （有声卡的机器上 Audio::open() 会真的打开设备）。
        let mut audio = Audio::with_device(Box::new(NoAudioDevice::new(
            "test: no hardware (NoAudioDevice)",
        )))
        .unwrap();
        let err = audio.capture_frame().unwrap_err();
        assert!(err.to_string().contains("ALSA"), "unexpected error: {err}");
        // play_frame 同样必须明确报错，不能谎报已播放。
        let mut codec = OpusCodec::new().unwrap();
        let packet = codec.encode_frame(&sine_frame(440.0, 10000)).unwrap();
        let err = audio.play_frame(&packet).unwrap_err();
        assert!(err.to_string().contains("ALSA"), "unexpected error: {err}");
    }

    #[test]
    fn audio_open_never_fails_for_missing_hardware() {
        // 构造器不因缺硬件失败：无硬件时 open() 成功，只是 capture/play 报错。
        // （本 VM 无 /dev/snd；有声卡的机器上同样成功，只是设备真的打开了。）
        let mut audio = Audio::open().unwrap();
        match audio.capture_pcm() {
            Ok(pcm) => assert_eq!(pcm.len(), FRAME_SAMPLES),
            Err(e) => assert!(e.to_string().contains("ALSA"), "unexpected error: {e}"),
        }
    }

    /// 测试用 Fake 设备：按队列吐采集帧，记录播放帧；与 AudioDevice 同 trait。
    struct FakeAudioDevice {
        capture_queue: std::collections::VecDeque<[i16; FRAME_SAMPLES]>,
        played: Vec<i16>,
    }

    impl FakeAudioDevice {
        fn with_frames(frames: Vec<[i16; FRAME_SAMPLES]>) -> Self {
            Self {
                capture_queue: frames.into(),
                played: Vec::new(),
            }
        }
    }

    impl AudioDevice for FakeAudioDevice {
        fn capture(&mut self) -> Result<[i16; FRAME_SAMPLES]> {
            self.capture_queue
                .pop_front()
                .ok_or_else(|| anyhow!("fake capture queue empty"))
        }

        fn play(&mut self, pcm: &[i16]) -> Result<()> {
            self.played.extend_from_slice(pcm);
            Ok(())
        }

        fn describe(&self) -> String {
            format!(
                "FakeAudioDevice ({} frames queued)",
                self.capture_queue.len()
            )
        }
    }

    #[test]
    fn fake_device_capture_frame_encodes_and_play_frame_writes() {
        // Fake 设备走与真实 ALSA 同一 trait：capture_frame 编码真实 PCM，
        // play_frame 解码后真实“写出”（记到 Fake 的 played 里）。
        // Opus 编解码器首帧有 lookahead 失真：先交错编/解 10 帧暖机
        // （与 opus_roundtrip 测试同一做法），再测最后一帧的 RMS。
        let frame = sine_frame(1000.0, 12000);
        let frames = vec![frame; 11];
        let mut audio = Audio::with_device(Box::new(FakeAudioDevice::with_frames(frames))).unwrap();
        for _ in 0..10 {
            let packet = audio.capture_frame().unwrap();
            assert!(!packet.is_empty());
            let played = audio.play_frame(&packet).unwrap();
            assert_eq!(played.len(), FRAME_SAMPLES);
        }
        let packet = audio.capture_frame().unwrap();
        let played = audio.play_frame(&packet).unwrap();
        assert_eq!(played.len(), FRAME_SAMPLES);
        let err = (rms_of(&played) - rms_of(&frame)).abs() / rms_of(&frame);
        assert!(err < 0.10, "fake playback RMS error {:.1}%", err * 100.0);
    }

    #[test]
    fn fake_device_capture_empty_queue_is_clear_error() {
        let mut audio = Audio::with_device(Box::new(FakeAudioDevice::with_frames(vec![]))).unwrap();
        let err = audio.capture_frame().unwrap_err();
        assert!(
            err.to_string().contains("queue empty"),
            "unexpected error: {err}"
        );
    }

    #[test]
    #[cfg(feature = "alsa")]
    fn alsa_open_bogus_device_reports_clear_error() {
        // 真实 ALSA 代码路径：不存在的设备名必须返回带设备名的明确错误，
        // 且两路互不拖累（describe 里各路状态分别可见）。
        let mut dev = AlsaAudioDevice::open_devices("hw:99,99", "hw:99,98");
        assert!(dev.capture.is_none() && dev.playback.is_none());
        let desc = dev.describe();
        assert!(desc.contains("hw:99,99"), "describe: {desc}");
        assert!(desc.contains("hw:99,98"), "describe: {desc}");
        let err = dev.capture().unwrap_err();
        assert!(
            err.to_string().contains("hw:99,99"),
            "unexpected error: {err}"
        );
        let err = dev.play(&[0i16; FRAME_SAMPLES]).unwrap_err();
        assert!(
            err.to_string().contains("hw:99,98"),
            "unexpected error: {err}"
        );
    }

    #[test]
    #[cfg(feature = "alsa")]
    fn alsa_open_without_hardware_falls_back_to_clear_error() {
        // 本 VM 无 /dev/snd：默认设备打不开，Audio::open() 仍成功，
        // capture 返回带设备名 hw:0,0 的明确错误（冒烟行为：不崩溃）。
        let mut audio = Audio::open().unwrap();
        let err = audio.capture_pcm().unwrap_err();
        let msg = err.to_string();
        assert!(msg.contains("ALSA"), "unexpected error: {msg}");
        assert!(msg.contains("hw:0,0"), "error must name the device: {msg}");
    }
}
