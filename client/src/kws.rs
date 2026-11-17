//! 本地 KWS：唤醒词检测接口 + 真实 RMS 计算（Phase 1）。
//!
//! 硬件红线（已验证）：模型 ~5MB + 运行时 ~20MB，128MB 机型跑不起来，会被杀掉（Killed）。
//! 本项目两台都是 256MB，可以跑。换唤醒词 = 换模型文件 + 改 client.toml，
//! 不用重新刷机，走 Hub OTA 推送。
//!
//! 上报给 Hub 的事件：{ wake_word, rms_db, timestamp_ms }。
//! RMS 是就近仲裁的唯一依据（见 hub/arbitration）：响的回，静的静默。
//!
//! BLOCKED（Phase 1 未做）：ONNX 模型推理。已全仓检索 references：
//!   - `ownding-open-xiaoai/`、`open-xiaoai/`、`open-xiaoai-bridge/` 三处均无 .onnx 文件
//!     （find -iname '*.onnx' 结果为空）；参考实现用的是 Sherpa-ONNX（Python）
//!     与 kws.log 文件监听，不是本项目可直接用的 Rust 模型。
//!
//! 任务 F（2026-11-17）已验证 sherpa-onnx WenetSpeech kws-zipformer 模型
//! 能检出“你好小李”（合成语音冒烟，见 `kws/README.md`），并以 feature
//! `sherpa-kws` 接入（`src/sherpa.rs`，动态加载 C API，默认不开）。
//!
//! P1-40（2026-11-17）：feature 开时，`Kws::new`（纯 RMS 回退）直接返回
//! Err——Client 宁可非零退出也不静默跑 RMS（`main.rs` 的 `?` 会把错误
//! 向上传，进程以非零码退出）。想显式要一个纯 RMS 实例（测试用），
//! 调 `Kws::new_rms_only`。feature 不开时 `Kws::new` 仍是纯 RMS（老行为，
//! 宿主机测试与无唤醒场景用）。
//!
//! 过渡（TRANSITIONAL）：ARM .so 尚不能在音箱上加载前（原因见
//! `kws/vendor-arm/SOURCE.md`：官方 .so 要求 GLIBC_2.29 / GLIBCXX_3.4.29，
//! 音箱只有 2.25 / 3.4.22；且 musl 静态 Client 不能 dlopen），可跑参考
//! kws 二进制 sidecar + 本文件的 `KwsLogMonitor` 尾随
//! `/tmp/open-xiaoai/kws.log` 取唤醒事件。日志里没有 RMS，
//! `KwsLogMonitor` 不编造 RMS——调用方必须用自己的采集帧提供
//! （接线进 `main.rs` 仍 TODO，见 `kws/README.md` §ARM 部署）。
//!
//! 作废（2026-11-17）：服务端 KWS 已落地（唤醒移出音箱、放 NAS 端边车），
//! 本旁路就此作废，不再接线进 `main.rs`；`KwsLogMonitor` 代码保留
//! （不删除），仅不再作为唤醒路径。详见 `kws/README.md` §过渡路线。

use anyhow::{anyhow, Result};
use serde::Serialize;

use crate::audio::{rms_linear, FRAME_SAMPLES};

#[cfg(feature = "sherpa-kws")]
use crate::sherpa::SherpaSpotter;

/// 静音下限（dBFS）：RMS 为 0 时返回该值而不是 -inf。
pub const SILENCE_FLOOR_DB: f32 = -120.0;

/// 一帧 PCM 的 RMS（dBFS，满刻度 0dB）。空输入返回静音下限。
/// 例：恒定 16384（半满刻度）→ 20*log10(0.5) ≈ -6.02 dB。
pub fn rms_db(pcm: &[i16]) -> f32 {
    let lin = rms_linear(pcm);
    if lin <= 0.0 {
        SILENCE_FLOOR_DB
    } else {
        (20.0 * lin.log10()).max(SILENCE_FLOOR_DB)
    }
}

/// 上报给 Hub 的唤醒事件（与 PROTOCOL.md 的 `wake` 消息字段一致）。
#[derive(Debug, Clone, Serialize)]
pub struct WakeEvent {
    pub wake_word: String,
    pub rms_db: f32,
    pub ts_ms: u64,
}

impl WakeEvent {
    #[allow(dead_code)]
    pub fn new(wake_word: &str, rms_db: f32) -> Self {
        Self {
            wake_word: wake_word.to_string(),
            rms_db,
            ts_ms: std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .map(|d| d.as_millis() as u64)
                .unwrap_or(0),
        }
    }

    /// 用调用方给定的时间戳建事件（过渡的 `KwsLogMonitor` 用：日志行自带
    /// 唤醒时刻，`rms_db` 仍由调用方自己的采集帧提供，禁止编造）。
    #[allow(dead_code)]
    pub fn new_at(wake_word: &str, rms_db: f32, ts_ms: u64) -> Self {
        Self {
            wake_word: wake_word.to_string(),
            rms_db,
            ts_ms,
        }
    }
}

#[allow(dead_code)]
pub struct Kws {
    pub wake_word: String,
    pub model_path: String,
    /// 模型是否已加载（无 feature/无模型时为 false；sherpa 后端加载成功为 true）。
    model_loaded: bool,
    /// 未满一帧的 PCM 残余（feed 可能收到任意长度输入，按 20ms 帧切分处理）。
    pending: Vec<i16>,
    /// 已处理的完整 20ms 帧数。
    pub frames_seen: u64,
    /// 最近一帧的 RMS（dBFS）。
    last_rms_db: f32,
    /// Sherpa-ONNX 后端（feature `sherpa-kws` 且模型加载成功时 Some）。
    #[cfg(feature = "sherpa-kws")]
    sherpa: Option<SherpaSpotter>,
}

impl Kws {
    /// 纯 RMS 构造函数。
    ///
    /// P1-40：feature `sherpa-kws` 开时，本函数直接返回 Err——生产路径
    /// （`main.rs`）在 sherpa 后端加载失败后调到这里，Err 经 `?` 向上传，
    /// 进程以非零码退出并打印原因，绝不静默跑 RMS（那样 Client 永远
    /// 不会唤醒，Hub 也永远收不到 wake 事件，失败还看不出来）。
    /// feature 不开时行为不变：返回纯 RMS 实例（`feed` 恒返回 None，
    /// 只算 RMS、不谎报唤醒），供宿主机测试与无唤醒场景使用。
    /// 测试里想显式要纯 RMS 实例，调 `Kws::new_rms_only`。
    #[cfg(feature = "sherpa-kws")]
    pub fn new(model_path: &str, wake_word: &str) -> Result<Self> {
        Err(anyhow!(
            "KWS backend `sherpa-kws` is enabled but no wake backend could be loaded (P1-40): \
             refusing to run RMS-only — the client would never wake. \
             Check kws_model_dir / kws_lib and the ARM .so (see kws/vendor-arm/SOURCE.md); \
             model_path={model_path} wake_word={wake_word}"
        ))
    }

    /// 纯 RMS 构造函数（feature 不开时的 `Kws::new`）。
    #[cfg(not(feature = "sherpa-kws"))]
    pub fn new(model_path: &str, wake_word: &str) -> Result<Self> {
        Self::new_rms_only(model_path, wake_word)
    }

    /// 显式纯 RMS 实例：不加载任何后端，`feed` 只算 RMS、恒返回 None。
    /// 只供测试与明确不要唤醒的场景使用；生产路径（feature 开时）禁止
    /// 经由本函数静默降级——`Kws::new` 会直接 Err。
    #[allow(dead_code)] // 测试用；生产路径禁止经由本函数静默降级
    pub fn new_rms_only(model_path: &str, wake_word: &str) -> Result<Self> {
        let exists = std::path::Path::new(model_path).exists();
        if !exists {
            tracing::warn!(
                "KWS model file not found: {model_path} — wake detection disabled, RMS reporting only (BLOCKED: needs .onnx model file)"
            );
        }
        // 即使文件存在，没有 sherpa 后端也不谎称已加载。
        if exists {
            tracing::warn!(
                "KWS model file present but no KWS backend linked — wake detection still disabled (enable feature `sherpa-kws` with a model dir, see kws/README.md)"
            );
        }
        Ok(Self {
            wake_word: wake_word.to_string(),
            model_path: model_path.to_string(),
            model_loaded: false,
            pending: Vec::new(),
            frames_seen: 0,
            last_rms_db: SILENCE_FLOOR_DB,
            #[cfg(feature = "sherpa-kws")]
            sherpa: None,
        })
    }

    /// 用 sherpa-onnx 后端创建（feature `sherpa-kws` 才有）。
    /// `model_dir` 含 encoder/decoder/joiner .onnx + tokens.txt；
    /// `keywords_file` 是 text2token 生成的 keywords.txt；
    /// `lib_path` 为 .so 路径，空串 = 按默认名搜索。
    /// 模型/.so 缺失或加载失败时返回 Err；feature `sherpa-kws` 开时调用方
    /// （main.rs）随后调 `Kws::new` 会再返回 Err，进程非零退出，
    /// 绝不静默回退纯 RMS。
    #[cfg(feature = "sherpa-kws")]
    pub fn new_with_sherpa(
        model_dir: &str,
        keywords_file: &str,
        lib_path: &str,
        wake_word: &str,
    ) -> Result<Self> {
        let spotter = SherpaSpotter::open(model_dir, keywords_file, lib_path, 1)?;
        Ok(Self {
            wake_word: wake_word.to_string(),
            model_path: model_dir.to_string(),
            model_loaded: true,
            pending: Vec::new(),
            frames_seen: 0,
            last_rms_db: SILENCE_FLOOR_DB,
            sherpa: Some(spotter),
        })
    }

    pub fn model_loaded(&self) -> bool {
        self.model_loaded
    }

    #[allow(dead_code)]
    pub fn last_rms_db(&self) -> f32 {
        self.last_rms_db
    }

    /// 对一段 16kHz PCM 做帧处理：按 320 采样点切帧，逐帧算 RMS。
    /// 有 sherpa 后端时每帧还过模型推理，命中返回 Some(WakeEvent)；
    /// 无模型时恒返回 None（但 RMS 与帧计数真实更新，供仲裁/调试用）。
    /// 注意：推理失败（例如模型内部错误）只记 warn，不中断喂帧、不谎报。
    pub fn feed(&mut self, pcm: &[i16]) -> Result<Option<WakeEvent>> {
        self.pending.extend_from_slice(pcm);
        while self.pending.len() >= FRAME_SAMPLES {
            let frame: Vec<i16> = self.pending.drain(..FRAME_SAMPLES).collect();
            self.last_rms_db = rms_db(&frame);
            self.frames_seen += 1;
            #[cfg(feature = "sherpa-kws")]
            if let Some(spotter) = self.sherpa.as_mut() {
                let frame_arr: &[i16; FRAME_SAMPLES] =
                    frame.as_slice().try_into().expect("frame is 320 samples");
                match spotter.feed_frame(frame_arr) {
                    Ok(Some(keyword)) => {
                        return Ok(Some(WakeEvent {
                            wake_word: keyword,
                            rms_db: self.last_rms_db,
                            ts_ms: std::time::SystemTime::now()
                                .duration_since(std::time::UNIX_EPOCH)
                                .map(|d| d.as_millis() as u64)
                                .unwrap_or(0),
                        }));
                    }
                    Ok(None) => {}
                    Err(e) => tracing::warn!("sherpa feed failed: {e}"),
                }
            }
            #[cfg(not(feature = "sherpa-kws"))]
            if self.model_loaded {
                // 预留分支：模型加载后才可能命中；现阶段不可达。
            }
        }
        Ok(None)
    }
}

// ---------------------------------------------------------------------------
// 过渡后端（TRANSITIONAL，P1-40）：参考 kws 二进制 sidecar 的日志监听。
//
// 背景：ARM .so 尚不能在音箱上加载（glibc/libstdc++ 版本 + musl 静态
// Client 不能 dlopen，见 `kws/vendor-arm/SOURCE.md`），Rust 直连暂时走不通。
// 过渡做法是跑参考实现 `kws.tar.gz` 里的 `kws` 二进制作 sidecar：它独占
// 麦克风跑唤醒，把唤醒事件以行写入 `/tmp/open-xiaoai/kws.log`，本组件
// 尾随该日志取事件。行格式与参考实现
// `references/open-xiaoai/packages/client-rust/src/services/monitor/kws.rs`
// 一致：`<timestamp_ms>@<关键词>`，`__STARTED__` 表示 sidecar 启动完成。
//
// 诚实红线：
//   - 日志里没有 RMS，`poll` 返回的事件不带 RMS；调用方必须用自己的
//     采集帧提供 RMS（`WakeEvent::new_at`），禁止用 -120dB 或 0dB 冒充。
//   - 同一时间戳的行只报一次（与参考实现一致的去重语义）。
//   - 日志文件被截断/轮转（大小小于已读偏移）时从头重读，不丢新事件。
//   - 本组件只做“读日志”；sidecar 的拉起/保活、与 Client 的 ALSA 争用、
//     以及接线进 `main.rs`（sidecar 占麦克风时 Client 自己的采集任务
//     会退出）仍 TODO，见 `kws/README.md` §ARM 部署。
// ---------------------------------------------------------------------------

/// `KwsLogMonitor::poll` 产出的事件。
// TODO 接线进 main.rs 前暂无生产调用方（见本文件头注释）；allow 掉
// dead_code 警告，免得构建噪音盖住真正的警告。
#[allow(dead_code)]
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum KwsLogEvent {
    /// sidecar 已启动（日志行 `<ts>@__STARTED__`），不是唤醒。
    Started,
    /// 检出唤醒词（日志行 `<ts>@<关键词>`）。
    Keyword { keyword: String, ts_ms: u64 },
}

/// 解析一行 kws 日志：`<timestamp_ms>@<关键词>`。
/// 行格式不对（无 `@`、时间戳不是数字、关键词为空）返回 None；
/// `__STARTED__` 返回 `KwsLogEvent::Started`。
#[allow(dead_code)] // TODO 接线进 main.rs 前暂无生产调用方
pub fn parse_kws_log_line(line: &str) -> Option<KwsLogEvent> {
    let line = line.trim_end_matches(['\r', '\n']);
    if line.is_empty() {
        return None;
    }
    let (ts, keyword) = line.split_once('@')?;
    let ts_ms: u64 = ts.parse().ok()?;
    if keyword.is_empty() {
        return None;
    }
    if keyword == "__STARTED__" {
        Some(KwsLogEvent::Started)
    } else {
        Some(KwsLogEvent::Keyword {
            keyword: keyword.to_string(),
            ts_ms,
        })
    }
}

/// 尾随 kws sidecar 日志文件的监听器（轮询式：调用方周期调 `poll`）。
#[allow(dead_code)] // TODO 接线进 main.rs 前暂无生产调用方
pub struct KwsLogMonitor {
    path: std::path::PathBuf,
    /// 已读字节偏移（下次 poll 从这里继续读）。
    offset: u64,
    /// 上次返回的事件时间戳（同时间戳的行只报一次）。
    last_ts_ms: u64,
    /// 未满一行的残余（poll 只处理以 \n 结尾的完整行）。
    pending: String,
}

#[allow(dead_code)] // TODO 接线进 main.rs 前暂无生产调用方
impl KwsLogMonitor {
    /// 缺省日志路径与参考实现一致：`/tmp/open-xiaoai/kws.log`。
    #[allow(dead_code)] // TODO 接线进 main.rs 前暂无生产调用方
    pub const DEFAULT_LOG_PATH: &'static str = "/tmp/open-xiaoai/kws.log";

    pub fn new(path: &str) -> Self {
        Self {
            path: std::path::PathBuf::from(path),
            offset: 0,
            last_ts_ms: 0,
            pending: String::new(),
        }
    }

    /// 读自上次 poll 后新追加的完整行，返回去重后的事件。
    /// 日志文件不存在时返回空（sidecar 还没起来），不报错；
    /// 文件被截断/轮转时从头重读。
    pub fn poll(&mut self) -> Result<Vec<KwsLogEvent>> {
        use std::io::{Read, Seek, SeekFrom};
        let mut file = match std::fs::File::open(&self.path) {
            Ok(f) => f,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(vec![]),
            Err(e) => return Err(anyhow!("cannot open kws log {}: {e}", self.path.display())),
        };
        let len = file.metadata()?.len();
        if len < self.offset {
            // 截断/轮转：从头重读，去重状态也重置（新文件的时间戳会重新走）。
            self.offset = 0;
            self.last_ts_ms = 0;
            self.pending.clear();
        }
        file.seek(SeekFrom::Start(self.offset))?;
        let mut buf = Vec::new();
        file.read_to_end(&mut buf)?;
        self.offset += buf.len() as u64;
        self.pending.push_str(&String::from_utf8_lossy(&buf));

        let mut out = vec![];
        while let Some(pos) = self.pending.find('\n') {
            let line: String = self.pending.drain(..=pos).collect();
            if let Some(ev) = parse_kws_log_line(&line) {
                match &ev {
                    KwsLogEvent::Started => out.push(ev),
                    KwsLogEvent::Keyword { ts_ms, .. } => {
                        if *ts_ms != self.last_ts_ms {
                            self.last_ts_ms = *ts_ms;
                            out.push(ev);
                        }
                    }
                }
            }
        }
        Ok(out)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::f32::consts::PI;

    #[test]
    fn rms_of_constant_half_scale() {
        // 恒定 16384 = 半满刻度 → 20*log10(0.5) = -6.0206 dB。
        let pcm = vec![16384i16; 320];
        let db = rms_db(&pcm);
        assert!((db - (-6.0206)).abs() < 0.05, "got {db}");
    }

    #[test]
    fn rms_of_sine_wave() {
        // 幅度 16384 的正弦波：RMS = 16384/√2 → 20*log10(0.35355) = -9.0309 dB。
        let mut pcm = vec![0i16; 320];
        for (i, s) in pcm.iter_mut().enumerate() {
            let t = i as f32 / 16000.0;
            *s = (16384.0 * (2.0 * PI * 1000.0 * t).sin()) as i16;
        }
        let db = rms_db(&pcm);
        assert!((db - (-9.0309)).abs() < 0.1, "got {db}");
    }

    #[test]
    fn rms_of_silence_is_floor_not_inf() {
        assert_eq!(rms_db(&[0i16; 320]), SILENCE_FLOOR_DB);
        assert_eq!(rms_db(&[]), SILENCE_FLOOR_DB);
        assert!(rms_db(&[0i16; 320]).is_finite());
    }

    #[test]
    fn rms_of_full_scale_is_zero_db() {
        let pcm = vec![32767i16; 320];
        let db = rms_db(&pcm);
        assert!(db > -0.01 && db <= 0.0, "got {db}");
    }

    #[test]
    fn feed_accumulates_frames_and_tracks_rms() {
        let mut kws = Kws::new_rms_only("/tmp/no-such-model.onnx", "你好小李").unwrap();
        assert!(!kws.model_loaded());
        // 喂 100 个采样点（不满一帧）→ 不计帧；再喂 540 个 → 共 640 = 2 帧。
        assert!(kws.feed(&[16384i16; 100]).unwrap().is_none());
        assert_eq!(kws.frames_seen, 0);
        assert!(kws.feed(&[16384i16; 540]).unwrap().is_none());
        assert_eq!(kws.frames_seen, 2);
        // 最近一帧是恒定 16384 → -6.02 dB。
        assert!(
            (kws.last_rms_db() - (-6.0206)).abs() < 0.05,
            "got {}",
            kws.last_rms_db()
        );
    }

    #[test]
    fn feed_never_false_wakes_without_model() {
        // 没有模型时，即使喂大音量语音也不谎报唤醒。
        let mut kws = Kws::new_rms_only("/tmp/no-such-model.onnx", "你好小李").unwrap();
        for _ in 0..100 {
            let loud = vec![20000i16; 320];
            assert!(kws.feed(&loud).unwrap().is_none());
        }
        assert_eq!(kws.frames_seen, 100);
    }

    #[test]
    fn wake_event_serializes_to_protocol_shape() {
        let ev = WakeEvent::new("你好小李", -12.5);
        let v = serde_json::to_value(&ev).unwrap();
        assert_eq!(v["wake_word"], "你好小李");
        assert_eq!(v["rms_db"], -12.5);
        assert!(v["ts_ms"].as_u64().unwrap() > 0);
    }

    #[test]
    fn wake_event_new_at_keeps_given_timestamp() {
        let ev = WakeEvent::new_at("你好小李", -12.5, 1_700_000_000_123);
        assert_eq!(ev.ts_ms, 1_700_000_000_123);
        assert_eq!(ev.wake_word, "你好小李");
    }

    /// P1-40：feature `sherpa-kws` 开时，`Kws::new`（纯 RMS 回退）必须
    /// 拒绝——生产路径靠这个 Err 非零退出，而不是静默跑 RMS。
    #[test]
    #[cfg(feature = "sherpa-kws")]
    fn kws_new_refuses_rms_only_when_sherpa_feature_on() {
        let err = match Kws::new("/tmp/no-such-model.onnx", "你好小李") {
            Ok(_) => panic!("Kws::new must refuse RMS-only when sherpa-kws is on"),
            Err(e) => e,
        };
        let msg = err.to_string();
        assert!(msg.contains("RMS-only"), "unexpected error: {msg}");
        assert!(msg.contains("P1-40"), "unexpected error: {msg}");
    }

    /// feature 不开时 `Kws::new` 仍是纯 RMS（老行为，不破坏宿主机测试）。
    #[test]
    #[cfg(not(feature = "sherpa-kws"))]
    fn kws_new_is_rms_only_when_sherpa_feature_off() {
        let kws = Kws::new("/tmp/no-such-model.onnx", "你好小李").unwrap();
        assert!(!kws.model_loaded());
    }

    #[test]
    fn parse_kws_log_line_keyword_and_started() {
        assert_eq!(
            parse_kws_log_line("1700000000123@你好小李"),
            Some(KwsLogEvent::Keyword {
                keyword: "你好小李".to_string(),
                ts_ms: 1_700_000_000_123
            })
        );
        assert_eq!(
            parse_kws_log_line("1700000000000@__STARTED__"),
            Some(KwsLogEvent::Started)
        );
        // 换行/回车尾巴不影响。
        assert_eq!(
            parse_kws_log_line("1700000000123@你好小李\r\n"),
            Some(KwsLogEvent::Keyword {
                keyword: "你好小李".to_string(),
                ts_ms: 1_700_000_000_123
            })
        );
    }

    #[test]
    fn parse_kws_log_line_rejects_malformed() {
        assert_eq!(parse_kws_log_line(""), None);
        assert_eq!(parse_kws_log_line("no-at-sign"), None);
        assert_eq!(parse_kws_log_line("not-a-number@你好小李"), None);
        assert_eq!(parse_kws_log_line("1700000000123@"), None);
        assert_eq!(parse_kws_log_line("@你好小李"), None);
    }

    #[test]
    fn kws_log_monitor_polls_appends_and_dedups() {
        let dir =
            std::env::temp_dir().join(format!("open-xiaoai-kws-log-test-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("kws.log");
        let path_str = path.to_str().unwrap().to_string();

        // 日志不存在 → 空，不报错。
        let mut mon = KwsLogMonitor::new(&path_str);
        assert!(mon.poll().unwrap().is_empty());

        // 追加 Started + 一次唤醒 + 同时间戳重复行（去重，只报一次）。
        std::fs::write(
            &path,
            "1700000000000@__STARTED__\n1700000000123@你好小李\n1700000000123@你好小李\n",
        )
        .unwrap();
        let evs = mon.poll().unwrap();
        assert_eq!(evs.len(), 2);
        assert_eq!(evs[0], KwsLogEvent::Started);
        assert_eq!(
            evs[1],
            KwsLogEvent::Keyword {
                keyword: "你好小李".to_string(),
                ts_ms: 1_700_000_000_123
            }
        );
        // 无新内容 → 空。
        assert!(mon.poll().unwrap().is_empty());

        // 不完整行（无 \n）先不报；补上换行后才报。
        std::fs::write(&path, "1700000000456@你好小李").unwrap();
        // 注意：上面 write 截断了文件（变小）→ 触发轮转重读；已报过的
        // 时间戳在新文件里不存在，不会重复报。
        assert!(mon.poll().unwrap().is_empty());
        {
            use std::io::Write;
            let mut f = std::fs::OpenOptions::new()
                .append(true)
                .open(&path)
                .unwrap();
            f.write_all(b"\n").unwrap();
        }
        let evs = mon.poll().unwrap();
        assert_eq!(evs.len(), 1);
        assert_eq!(
            evs[0],
            KwsLogEvent::Keyword {
                keyword: "你好小李".to_string(),
                ts_ms: 1_700_000_000_456
            }
        );

        let _ = std::fs::remove_dir_all(&dir);
    }
}
