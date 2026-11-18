//! open-xiaoai-client — 音箱端薄客户端（Phase 1）。
//!
//! 职责只有五件，多一件都不做：
//!   1. 录音：ALSA 采集 16kHz 单声道 PCM → AEC 消回声 → Opus 编码 → 发往 Hub
//!      （ALSA 已接线，见 audio.rs：无硬件时 capture 返回明确错误，录音任务
//!      按退避重开设备、不永久退出；真机实测仍 BLOCKED）
//!   2. 放音：Hub 下发的 Opus 解码 → ALSA 播放（解码 + 写 ALSA 均已接线；
//!      无硬件时 play_frame 返回明确错误，记日志丢帧，不崩溃）
//!   3. 唤醒检测：按 `kws_mode` 分流（见 `config.rs`，默认 `"server"`）。
//!      - `server`（默认）：不在音箱上初始化本地 KWS（不要求模型文件、
//!        不要求 .so、无后端也不退出）；录音任务持续把 Opus 音频流推给
//!        Hub，唤醒检测在 Hub/边车侧完成。会话一建立就发 `audio_start`，
//!        不等仲裁 `wake_win`。
//!      - `local`：本地 KWS（ONNX 模型监听唤醒词“你好小李”，命中后上报
//!        Hub；sherpa-onnx 后端见 `sherpa.rs`，feature `sherpa-kws`
//!        门控，默认不开；feature 开时无后端即非零退出（不静默跑 RMS），
//!        feature 不开时只报 RMS、不谎报唤醒；RMS 与帧结构已真实）。
//!        本地代码路径（`sherpa.rs`、`kws.rs`、feature 门控）保留不动，
//!        供未来 A 路（源码重编 .so）复用。`kws_mode` 非法值启动即报错。
//!   4. 上报事件：wake_word + RMS + 时间戳，供 Hub 做就近仲裁
//!   5. 执行 RPC：start_play / stop_play / start_recording / stop_recording /
//!      play_text / set_volume；收到 interrupt 立即停播
//!
//! 不做业务逻辑：不跑 VAD 决策、不跑 ASR、不跑 LLM、不管房间、不管音乐同步。
//! 音乐同步由 Snapcast 客户端（独立进程）负责，不走本程序。
//!
//! 参考实现（当 Spec 看，不直接抄）：OwnDing/open-xiaoai client-2026.10.09
//!   （连续对话 + 打断 + 子带 AEC 已在同款硬件上跑通）
//!   coderzc/open-xiaoai-bridge（服务端 Token 鉴权：OPEN_XIAOAI_TOKEN）

use anyhow::Result;
use std::sync::{
    atomic::{AtomicBool, AtomicUsize, Ordering},
    Arc,
};
use tokio::sync::mpsc;
use tokio::sync::mpsc::error::TrySendError;
use tracing::{info, warn};

mod aec;
mod audio;
mod config;
mod kws;
#[cfg(feature = "sherpa-kws")]
mod sherpa;
mod transport;

use aec::{Aec, AEC_REF_DELAY_FRAMES};
use audio::{Audio, FailureThrottle};
use kws::Kws;
use transport::{HubMsg, RawFrame, Transport};

/// 会话状态机：Hub 消息 → 状态变更 + 待执行动作。
/// 与网络 IO 分离，可单测；main.rs 的循环只负责执行动作。
#[derive(Debug, Default)]
pub struct Session {
    /// 仲裁赢了且正在推语音流；server 模式下会话一建立即为 true（持续推流）。
    pub streaming: bool,
    /// Hub 发 `start_recording` RPC 要求开始录音/推流（对讲发起方，P1-N2）；
    /// 与 `streaming` 是两个独立的推流原因，推流门是 `streaming || recording`。
    pub recording: bool,
    /// 正在放音（TTS/对讲）。
    pub playing: bool,
    /// `playing` 置 true 的时刻（P1-16：无帧跟进时按空闲超时复位，防 latch）。
    playing_since: Option<std::time::Instant>,
    /// 最近一帧成功播放的时刻（P1-16：播放中持续刷新，空闲超时据此判断）。
    last_frame_at: Option<std::time::Instant>,
    /// 打断后 200ms 内到达的在途音频帧直接丢弃（P1-16：刚打断又被在途帧
    /// 重启播放）；过期后恢复正常接收。
    interrupt_suppress_until: Option<std::time::Instant>,
    /// 音量 0–100。
    pub volume: u8,
    pub device_id: Option<String>,
    pub room: Option<String>,
    /// server KWS 模式：持续推流，不等仲裁；wake_win/wake_lose 不改变推流状态。
    pub server_mode: bool,
}

/// `playing` 空闲超时（P1-16）：置 true 后超过这么久既无新 `start_play` 也
/// 无成功播放的帧，即视为 latch/过期，由 run_session 的看门狗复位。
/// TTS/对讲帧是连续流（20ms 一帧），2s 空闲足以判定播完；Hub 正常路径
/// 发 `stop_play` 时会更早复位，超时只是兜底。
pub const PLAYBACK_IDLE_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(2);
/// 打断后丢弃在途音频帧的窗口（P2-N1：`stop_playback` 已丢弃 ALSA 缓冲，
/// 本窗口只拦住断线前已在途的帧，不重启播放）。
pub const INTERRUPT_SUPPRESS: std::time::Duration = std::time::Duration::from_millis(200);

/// 录音任务重开 ALSA 的退避（P2-40）：100ms 起步翻倍，上限 5s。
/// 启动瞬间驱动未就绪、偶发 XRUN 杀死采集时，任务按此退避重开设备，
/// 不再死一次就永久失聪。
pub const CAPTURE_BACKOFF_INITIAL_MS: u64 = 100;
pub const CAPTURE_BACKOFF_MAX_MS: u64 = 5_000;

/// 录音任务重开 ALSA 的退避：第 attempt 次重开前等待的时长，
/// 100ms 起步翻倍，上限 5s（无 jitter：单台设备，无同步脉冲问题）。
pub fn capture_backoff_delay(attempt: u32) -> std::time::Duration {
    let base_ms = CAPTURE_BACKOFF_INITIAL_MS
        .saturating_mul(1u64 << attempt.min(6))
        .min(CAPTURE_BACKOFF_MAX_MS);
    std::time::Duration::from_millis(base_ms)
}

/// Opus 编码连续失败多少次后重建编解码器（P2-N4）：50 帧 ≈ 1 秒持续失败。
pub const ENCODE_REBUILD_AFTER: u32 = 50;

/// Session 对一条 Hub 消息的反应：需要 main 循环代发/代做的动作。
#[derive(Debug, PartialEq)]
pub enum SessionAction {
    /// 发 audio_start，开始推语音流。
    SendAudioStart,
    /// 发 audio_end，结束语音流。
    SendAudioEnd,
    /// 立即停播（收到 interrupt）。
    StopPlayback,
    /// 真调 ALSA 混音器改音量（P1-N1）：apply_action 执行混音器调用后，
    /// 按真实结果回 rpc_result（成功才更新 `Session.volume` 并回 ok:true，
    /// 失败回 ok:false + 原因）。本动作本身不直接回 rpc_result。
    SetVolume { id: String, volume: u8 },
    /// 回应 rpc：(id, ok, error)。
    SendRpcResult {
        id: String,
        ok: bool,
        error: Option<String>,
    },
}

impl Session {
    pub fn new() -> Self {
        Self::new_with_volume(50)
    }

    /// server KWS 模式的会话：建立即推流（`streaming = true`），
    /// `run_session` 开头会补发一次 `audio_start`。
    pub fn new_server() -> Self {
        Self::new_server_with_volume(50)
    }

    /// 带初始音量的构造（P2-41：重连后音量不再跳回 50——main 循环把上
    /// 一会话最后一次成功设置的音量带进新会话；混音器硬件状态本身跨
    /// 会话保持，这里只保证上报值与之一致）。
    pub fn new_with_volume(volume: u8) -> Self {
        Self {
            streaming: false,
            recording: false,
            playing: false,
            playing_since: None,
            last_frame_at: None,
            interrupt_suppress_until: None,
            volume,
            device_id: None,
            room: None,
            server_mode: false,
        }
    }

    pub fn new_server_with_volume(volume: u8) -> Self {
        Self {
            streaming: true,
            recording: false,
            playing: false,
            playing_since: None,
            last_frame_at: None,
            interrupt_suppress_until: None,
            volume,
            device_id: None,
            room: None,
            server_mode: true,
        }
    }

    /// 是否正在向 Hub 推音频（仲裁会话或对讲录音任一原因）。
    /// run_session 的推流门：只看本函数，不再只看 `streaming`（P1-N2）。
    pub fn pushing_audio(&self) -> bool {
        self.streaming || self.recording
    }

    /// 置 `playing = true`（start_play / play_text / 首帧播出时调）：
    /// 只在 false→true 跃变时记 `playing_since`，空闲超时据此起算。
    fn mark_playing(&mut self) {
        if !self.playing {
            self.playing = true;
            self.playing_since = Some(std::time::Instant::now());
        }
    }

    /// 复位播放态（stop_play / interrupt / 空闲超时看门狗调）：`playing`
    /// 与计时一起清，`status` 上报随即恢复真实值（P1-16：latch 到此为止）。
    pub fn clear_playing(&mut self) {
        self.playing = false;
        self.playing_since = None;
        self.last_frame_at = None;
    }

    /// 一帧音频成功播出后调（P1-16）：视为播放开始（裸帧也能播，不再
    /// 因 `playing == false` 静默丢弃），并刷新空闲计时。
    pub fn note_played_frame(&mut self) {
        self.mark_playing();
        self.last_frame_at = Some(std::time::Instant::now());
    }

    /// `playing` 是否已空闲过期（P1-16）：置 true 后超过
    /// `PLAYBACK_IDLE_TIMEOUT` 既无新 `start_play` 也无成功播放的帧。
    /// 无计时锚点（理论上不应发生：所有置 true 路径都记 `playing_since`）
    /// 时返回 false，不误清。
    pub fn playback_idle_expired(&self) -> bool {
        if !self.playing {
            return false;
        }
        let anchor = match (self.playing_since, self.last_frame_at) {
            (Some(a), Some(b)) => Some(a.max(b)),
            (Some(a), None) => Some(a),
            (None, Some(b)) => Some(b),
            (None, None) => None,
        };
        match anchor {
            Some(t) => t.elapsed() > PLAYBACK_IDLE_TIMEOUT,
            None => false,
        }
    }

    /// 是否处在打断后的在途帧丢弃窗口内（P1-16）。
    pub fn interrupt_suppressed(&self) -> bool {
        self.interrupt_suppress_until
            .map(|t| std::time::Instant::now() < t)
            .unwrap_or(false)
    }

    /// 处理一条 Hub 消息，返回待执行的动作（可为空）。
    pub fn on_hub_msg(&mut self, msg: &HubMsg) -> Vec<SessionAction> {
        match msg {
            HubMsg::HelloOk { device_id, room } => {
                self.device_id = Some(device_id.clone());
                if room.is_some() {
                    self.room = room.clone();
                }
                vec![]
            }
            // 仲裁赢了：继续听，说话 → 推 audio_start + 音频帧。
            // server 模式已在持续推流：不再重复发 audio_start。
            // 若对讲录音已在推流（recording），audio_start 早已发过，不重复发。
            HubMsg::WakeWin => {
                let was_pushing = self.pushing_audio();
                self.streaming = true;
                if self.server_mode || was_pushing {
                    vec![]
                } else {
                    vec![SessionAction::SendAudioStart]
                }
            }
            // 仲裁输了：静默，不理这次唤醒；若正在推流则停。
            // server 模式：持续推流不停（一次 wake_lose 只是边车对某次
            // 唤醒的裁决，不停整条音频流）。
            // 对讲录音（recording）仍在推时也不发 audio_end——流还在。
            HubMsg::WakeLose => {
                if self.server_mode {
                    return vec![];
                }
                let was_pushing = self.pushing_audio();
                self.streaming = false;
                if was_pushing && !self.pushing_audio() {
                    vec![SessionAction::SendAudioEnd]
                } else {
                    vec![]
                }
            }
            // 打断：立即停播正在放的 TTS。
            // server 模式：只停播，不停持续推流（不断 audio 流）。
            // local 模式：停播 + 停推流（含对讲录音，P1-N2：打断即挂断）；
            // 若推流因此停下，补发 audio_end。打断后 200ms 内的在途帧丢弃
            // （P1-16），不重启播放。
            HubMsg::Interrupt => {
                let was_pushing = self.pushing_audio();
                self.clear_playing();
                self.interrupt_suppress_until =
                    Some(std::time::Instant::now() + INTERRUPT_SUPPRESS);
                // 打断即结束当前对讲录音（两模式一致）；server 模式的持续
                // 推流本身不受影响（streaming 仍为 true）。
                self.recording = false;
                if self.server_mode {
                    return vec![SessionAction::StopPlayback];
                }
                self.streaming = false;
                if was_pushing {
                    vec![SessionAction::StopPlayback, SessionAction::SendAudioEnd]
                } else {
                    vec![SessionAction::StopPlayback]
                }
            }
            HubMsg::Rpc { id, method, params } => match method.as_str() {
                // P1-N2：start_recording 是真正的录音启停，不再是空操作。
                // 置 recording 标志（推流门见 pushing_audio）；local 模式下
                // 若此前未在推流，补发 audio_start 打开 Hub 侧的流（server
                // 模式会话建立时已发过，不重复发）。
                "start_recording" => {
                    let mut actions = vec![];
                    if !self.server_mode && !self.pushing_audio() {
                        actions.push(SessionAction::SendAudioStart);
                    }
                    self.recording = true;
                    info!("rpc start_recording: streaming audio to hub (intercom)");
                    actions.push(SessionAction::SendRpcResult {
                        id: id.clone(),
                        ok: true,
                        error: None,
                    });
                    actions
                }
                "stop_recording" => {
                    let was_pushing = self.pushing_audio();
                    self.recording = false;
                    let mut actions = vec![];
                    if !self.server_mode && was_pushing && !self.pushing_audio() {
                        actions.push(SessionAction::SendAudioEnd);
                    }
                    info!("rpc stop_recording");
                    actions.push(SessionAction::SendRpcResult {
                        id: id.clone(),
                        ok: true,
                        error: None,
                    });
                    actions
                }
                // P1-N1：set_volume 不再只改内存数字。参数非法直接回错；
                // 合法值走 SetVolume 动作，由 apply_action 真调 ALSA 混音器
                // 后按真实结果回 rpc_result（成功才更新 Session.volume）。
                "set_volume" => match parse_volume_param(params) {
                    Ok(v) => vec![SessionAction::SetVolume {
                        id: id.clone(),
                        volume: v,
                    }],
                    Err(e) => vec![SessionAction::SendRpcResult {
                        id: id.clone(),
                        ok: false,
                        error: Some(e),
                    }],
                },
                _ => {
                    let (ok, error) = self.handle_rpc(method, params);
                    vec![SessionAction::SendRpcResult {
                        id: id.clone(),
                        ok,
                        error,
                    }]
                }
            },
            HubMsg::RoomOk { room } => {
                self.room = Some(room.clone());
                info!("room claimed: {room}");
                vec![]
            }
            HubMsg::Ota { version, .. } => {
                // Phase 4：一键 OTA 推送，本阶段只记日志，不执行。
                info!("ota available (Phase 4, ignored): version={version}");
                vec![]
            }
            HubMsg::Unknown(other) => {
                warn!("unknown hub message type: {other}");
                vec![]
            }
        }
    }

    /// 执行 Hub 的 rpc；返回 (ok, error)。Phase 1：状态变更真实执行，
    /// 实际放音/录音动作依赖 ALSA（未接线时记日志，不谎报失败）。
    /// 注意：`start_recording` / `stop_recording` / `set_volume` 不走本函数，
    /// 见 `on_hub_msg` 的 Rpc 分支（前两者要发 audio_start/audio_end，
    /// 后者要经 apply_action 真调混音器后按真实结果回 rpc_result）。
    fn handle_rpc(&mut self, method: &str, params: &serde_json::Value) -> (bool, Option<String>) {
        match method {
            "start_play" => {
                // P1-16：置 playing 的同时记 playing_since；无帧跟进时由
                // 空闲超时看门狗复位，不再永久 latch。
                self.mark_playing();
                info!("rpc start_play");
                (true, None)
            }
            "stop_play" => {
                self.clear_playing();
                info!("rpc stop_play");
                (true, None)
            }
            "play_text" => {
                let text = params
                    .get("text")
                    .and_then(|t| t.as_str())
                    .unwrap_or("")
                    .to_string();
                // TTS 合成在 Hub 侧；Client 只负责放下发的音频。
                // P1-16：置 playing 的同时记 playing_since（无音频跟随
                // 时由空闲超时复位，不再永久 latch 成“恰好能播”的假象）。
                self.mark_playing();
                info!("rpc play_text: {text}");
                (true, None)
            }
            other => {
                warn!("unknown rpc method: {other}");
                (false, Some(format!("unknown rpc method: {other}")))
            }
        }
    }
}

/// 解析 `set_volume` 的 volume 参数（P1-N1 + P2-18）：
/// 必须是 0–100 的整数；缺失、非数字、越界一律返回明确错误——不再静默
/// 重置为 50（旧行为：`.unwrap_or(50)`），也不再静默钳制。
/// 浮点数（如 80.5）同样拒绝：协议字段是整数，含糊的值不应猜。
fn parse_volume_param(params: &serde_json::Value) -> Result<u8, String> {
    let v = params
        .get("volume")
        .ok_or_else(|| "set_volume: missing required field \"volume\" (0-100)".to_string())?;
    let n = v
        .as_u64()
        .ok_or_else(|| format!("set_volume: volume must be an integer 0-100, got {v}"))?;
    if n > 100 {
        return Err(format!(
            "set_volume: volume {n} out of range 0-100 (not clamped silently)"
        ));
    }
    Ok(n as u8)
}

/// 按 `kws_mode` 初始化本地 KWS（启动路径唯一入口，测试直接调本函数）。
///
/// - `server`（默认）：返回 `Ok(None)`——不初始化本地 KWS，不要求模型
///   文件、不要求 `.so`，无后端也不退出；唤醒检测在 Hub/边车侧完成。
/// - `local`：保持 P1-40 前的行为不动——feature `sherpa-kws` 开时
///   sherpa 后端加载失败即经 `Kws::new` 返回 Err（调用方 `?` 传播，
///   进程非零退出，绝不静默跑 RMS）；feature 不开时返回纯 RMS 实例。
///   本地 KWS 代码路径（`sherpa.rs`、`kws.rs`、feature 门控）原样保留，
///   供未来 A 路（源码重编 ARM .so）复用。
fn init_kws(cfg: &config::Config) -> Result<Option<Kws>> {
    if cfg.is_server_kws_mode() {
        return Ok(None);
    }
    #[cfg(feature = "sherpa-kws")]
    {
        let keywords_file = format!("{}/keywords.txt", cfg.kws_model_dir.trim_end_matches('/'));
        match Kws::new_with_sherpa(
            &cfg.kws_model_dir,
            &keywords_file,
            &cfg.kws_lib,
            &cfg.wake_word,
        ) {
            Ok(k) => Ok(Some(k)),
            Err(e) => {
                warn!("sherpa KWS unavailable, RMS reporting only: {e}");
                // P1-40：feature 开时 Kws::new 直接 Err，进程非零退出。
                Ok(Some(Kws::new(&cfg.kws_model, &cfg.wake_word)?))
            }
        }
    }
    #[cfg(not(feature = "sherpa-kws"))]
    {
        Ok(Some(Kws::new(&cfg.kws_model, &cfg.wake_word)?))
    }
}

#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::fmt::init();

    let cfg = config::load()?;
    cfg.validate()?;
    info!("open-xiaoai-client starting, hub={}", cfg.hub_url);
    info!("wake word: {}", cfg.wake_word);
    info!("kws_mode={}", cfg.kws_mode);
    info!("device model: {} ({}MB RAM)", cfg.model, cfg.ram_mb);
    if !cfg.tls {
        warn!("tls=false: plain ws (local test only, never for production; requires allow_insecure_ws=true in client.toml)");
    }
    // P0-3：CHANGE_ME 不再是默认值（默认 Token 已改为空，见 config.rs），
    // 这里只拦显式配成 CHANGE_ME 的情况：connect_once 会在建连前就
    // 拒绝弱 Token（validate_token），这条 warn 是给操作员看的早提示，
    // 不是“默认值警告”——缺 Token 时 Client 不会拿公开值上线。
    if cfg.token == "CHANGE_ME" {
        warn!("token is still the default CHANGE_ME — hub will reject hello");
    }

    // KWS 初始化按 kws_mode 分流（见 init_kws）：
    //   server（默认）：Ok(None)——不初始化本地 KWS，不要求模型文件、
    //     不要求 .so，无后端也不退出；唤醒检测在 Hub/边车侧做。
    //   local：保持原有行为——feature `sherpa-kws` 开时，模型目录 +
    //     keywords.txt 齐全才尝试加载 sherpa 后端；失败（无模型/无 .so）
    //     时不回退纯 RMS——回退路径调 `Kws::new`，而 feature 开时
    //     `Kws::new` 直接返回 Err（P1-40），经 `?` 向上传播，进程以
    //     非零码退出并打印原因：没后端就绝不静默跑 RMS（那样 Client
    //     永远不会唤醒，Hub 也永远收不到 wake 事件，失败还看不出来）。
    //     feature 不开时直接走 `Kws::new`（纯 RMS，只算 RMS、不谎报
    //     唤醒），供宿主机测试与无唤醒场景使用。
    let kws = init_kws(&cfg)?;
    match &kws {
        None => info!("kws_mode=server: no local KWS backend — streaming Opus to hub for server-side wake detection"),
        Some(k) if !k.model_loaded() => {
            warn!("KWS model not loaded — wake detection disabled (BLOCKED: needs .onnx model file); RMS reporting only")
        }
        Some(_) => info!("kws_mode=local: local KWS backend loaded"),
    }

    // 录音任务：采集 PCM 帧 → AEC 消回声（播放参考见下）→（local 模式）
    // 喂 KWS（算 RMS、命中发 WakeEvent）、（server 模式）跳过本地 KWS →
    // 只在推流时编码成 Opus 帧发给主循环（P2-19：非推流时不编码）。
    // P2-40：ALSA 打开/采集失败不再直接退出——按退避重开设备（启动瞬间
    // 驱动未就绪、偶发 XRUN 都能恢复）；只有主循环退出（通道关闭）时
    // 任务才结束。P2-N5：audio_tx 满时丢当前帧并计数，永不阻塞采集。
    // P2-N4：编码失败节流记日志，连续失败 50 次后重建编解码器。
    // 回声消除（Opus 级回声接线）：播放参考由 run_session 的 play_frame
    // 解码产物经 ref 通道喂给录音任务的 Aec（见 aec.rs 模块头：为什么
    // 接在客户端、而不是 Hub 侧）。
    let (wake_tx, mut wake_rx) = mpsc::channel::<kws::WakeEvent>(8);
    let (audio_tx, mut audio_rx) = mpsc::channel::<Vec<u8>>(32);
    let (ref_tx, ref_rx) = mpsc::channel::<[i16; audio::FRAME_SAMPLES]>(16);
    // P2-19：推流标志由 run_session（会话状态机所在处）维护，录音任务
    // 只读。server 模式会话一建立即推流，初始值 true；local 模式 false。
    let pushing = Arc::new(AtomicBool::new(cfg.is_server_kws_mode()));
    let pushing_capture = pushing.clone();
    // C-1：AEC 参考延迟帧数（共享原子值）。初值是标称 4 帧；会话每次
    // 打开播放设备后按实际 ALSA 缓冲重新推导并写回，录音任务每帧取
    // 最新值——播放缓冲若被硬件取整为 3/5 周期，延迟不再静默错位。
    let aec_delay_frames = Arc::new(AtomicUsize::new(AEC_REF_DELAY_FRAMES));
    let aec_delay_frames_capture = aec_delay_frames.clone();
    let capture_handle = tokio::spawn(async move {
        let mut kws = kws;
        let mut aec = Aec::with_delay_frames(aec_delay_frames_capture.load(Ordering::Relaxed));
        let mut ref_rx = ref_rx;
        let mut encode_throttle =
            FailureThrottle::new(std::time::Duration::from_secs(1), ENCODE_REBUILD_AFTER);
        let mut dropped_frames: u64 = 0;
        let mut last_drop_log: Option<std::time::Instant> = None;
        let mut backoff_attempt: u32 = 0;
        loop {
            let mut audio = match Audio::open_capture_only() {
                Ok(a) => a,
                Err(e) => {
                    let delay = capture_backoff_delay(backoff_attempt);
                    warn!(
                        "audio open failed, retrying in {delay:?} (attempt {}): {e}",
                        backoff_attempt + 1
                    );
                    tokio::time::sleep(delay).await;
                    backoff_attempt = backoff_attempt.saturating_add(1);
                    continue;
                }
            };
            // 内层采集循环：任一采集错误都跳出重开设备（不是退出任务）。
            loop {
                // 非阻塞地收播放参考帧（run_session 的 play_frame 产物）。
                while let Ok(frame) = ref_rx.try_recv() {
                    aec.push_ref(frame);
                }
                // C-1：会话重开播放设备后可能推导出新的延迟帧数，变化时
                // 热更新（只改对齐点，不动滤波器系数；队列下一帧自调整）。
                let delay = aec_delay_frames_capture.load(Ordering::Relaxed);
                if delay != aec.delay_frames() {
                    info!(
                        "AEC reference delay frames changed: {} -> {} (derived from actual playback buffer)",
                        aec.delay_frames(),
                        delay
                    );
                    aec.set_delay_frames(delay);
                }
                match audio.capture_pcm() {
                    Ok(pcm) => {
                        // 采到一帧即证明设备恢复健康：退避计数清零。
                        backoff_attempt = 0;
                        // 回声消除先于 KWS 与编码：消回声后的 PCM 既喂
                        // 本地 KWS（防播放声误触发），也进 Opus 编码。
                        let pcm = aec.process_frame(&pcm);
                        // server 模式：kws 为 None，不做本地唤醒检测，只编码推流。
                        if let Some(k) = kws.as_mut() {
                            match k.feed(&pcm) {
                                Ok(Some(ev)) => {
                                    if wake_tx.send(ev).await.is_err() {
                                        return;
                                    }
                                }
                                Ok(None) => {}
                                Err(e) => warn!("kws feed failed: {e}"),
                            }
                        }
                        // P2-19：未推流（local 模式无仲裁会话/对讲录音）
                        // 时跳过 Opus 编码——编出来也是被主循环丢弃的帧。
                        if !pushing_capture.load(Ordering::Relaxed) {
                            continue;
                        }
                        match audio.encode_pcm(&pcm) {
                            Ok(opus) => {
                                encode_throttle.note_success();
                                match audio_tx.try_send(opus) {
                                    Ok(()) => {}
                                    Err(TrySendError::Full(_)) => {
                                        // P2-N5：通道满（Hub 消费慢/WS 抖动）
                                        // 时丢当前帧并计数，绝不反压堵采集
                                        // （否则 ALSA 采集缓冲 overrun →
                                        // XRUN → 录音任务死，见 P2-40）。
                                        dropped_frames += 1;
                                        let now = std::time::Instant::now();
                                        if last_drop_log
                                            .map(|t| {
                                                now.duration_since(t)
                                                    > std::time::Duration::from_secs(5)
                                            })
                                            .unwrap_or(true)
                                        {
                                            warn!("audio_tx full, dropping Opus frames ({dropped_frames} dropped so far) — hub is consuming slowly, capture never blocks");
                                            last_drop_log = Some(now);
                                            dropped_frames = 0;
                                        }
                                    }
                                    Err(TrySendError::Closed(_)) => return,
                                }
                            }
                            Err(e) => {
                                // P2-N4：编码失败不再静默吞掉——节流 warn
                                // （1 秒一条 + 累计），连续失败 50 次后
                                // 记 error 并重建编解码器。
                                if encode_throttle.note_failure() {
                                    warn!(
                                        "opus encode failed ({} failures total, {} consecutive): {e}",
                                        encode_throttle.total(),
                                        encode_throttle.consecutive()
                                    );
                                }
                                if encode_throttle.should_recover() {
                                    tracing::error!(
                                        "opus encode failed {} times in a row — rebuilding Opus codec",
                                        encode_throttle.consecutive()
                                    );
                                    match audio.rebuild_codec() {
                                        Ok(()) => {
                                            info!("opus codec rebuilt after consecutive failures");
                                        }
                                        Err(re) => {
                                            warn!("opus codec rebuild failed: {re}");
                                        }
                                    }
                                    encode_throttle.reset();
                                }
                            }
                        }
                    }
                    Err(e) => {
                        warn!("capture failed, reopening audio device: {e}");
                        break;
                    }
                }
            }
            let delay = capture_backoff_delay(backoff_attempt);
            tokio::time::sleep(delay).await;
            backoff_attempt = backoff_attempt.saturating_add(1);
        }
    });

    // 主循环：断线 → 指数退避重连（transport::connect_with_retry）。
    // 音量跨重连保留（P2-41：新会话带上一次成功设置的音量，不跳回 50；
    // 混音器硬件状态本身跨会话保持，这里只保证上报值与之一致）。
    let mut volume: u8 = 50;
    loop {
        // P2-19：重连等待期间先清推流标志——旧会话已断，录音任务不
        // 再为已死的会话编码；新会话建立后 run_session 入口会按新会话
        // 状态重新同步（server 模式 true，local 模式 false 起步）。
        pushing.store(false, Ordering::Relaxed);
        let mut session = if cfg.is_server_kws_mode() {
            Session::new_server_with_volume(volume)
        } else {
            Session::new_with_volume(volume)
        };
        // P1-43：连接期间也监听 Ctrl-C（以前 connect_with_retry 期间
        // SIGINT 无人处理，必须 kill 才能退出）；鉴权失败（hello_reject
        // 或弱 Token）重试 3 次后 connect_with_retry 返回 Err，主循环不
        // 再 1 秒一试，直接非零退出（配置错不应永重试）。
        let mut transport = tokio::select! {
            r = transport::connect_with_retry(&cfg) => match r {
                Ok(t) => t,
                Err(e) => {
                    if transport::is_auth_rejection(&e) {
                        tracing::error!("hub authentication failed repeatedly, giving up (check the token in client.toml): {e}");
                        capture_handle.abort();
                        return Err(e);
                    }
                    // connect_with_retry 原则上只在鉴权失败时返回 Err；
                    // 其他错误走到这里是防御性分支，退 5 秒再试。
                    warn!("connect_with_retry gave up: {e}");
                    tokio::time::sleep(std::time::Duration::from_secs(5)).await;
                    continue;
                }
            },
            _ = tokio::signal::ctrl_c() => {
                info!("shutting down (Ctrl-C during connect)");
                capture_handle.abort();
                return Ok(());
            }
        };
        session.device_id = transport.device_id.clone();
        session.room = transport.room.clone();
        info!("session started");

        let mut status_tick = tokio::time::interval(transport::STATUS_INTERVAL);
        // 播放任务用的 Opus 解码由主循环内的 audio 负责。
        // P0-N1：会话只开播放（`hw:0,2`），不碰采集 `hw:0,0`。
        let mut audio = match Audio::open_playback_only() {
            Ok(a) => Some(a),
            Err(e) => {
                warn!("audio unavailable in session: {e}");
                None
            }
        };

        // C-1：按播放设备的实际 ALSA 缓冲推导 AEC 参考延迟（每次重连
        // 都会重开播放设备、重新推导）。无播放设备时保持上次的值。
        if let Some(a) = audio.as_ref() {
            if let Some(frames) = a.playback_aec_delay_frames() {
                let prev = aec_delay_frames.swap(frames, Ordering::Relaxed);
                if prev != frames {
                    info!(
                        "AEC reference delay frames: {prev} -> {frames} (derived from actual playback buffer; nominal is {AEC_REF_DELAY_FRAMES})"
                    );
                }
            }
        }

        // 会话循环：Hub 消息 / KWS 唤醒 / 30s 状态上报 / Ctrl-C。
        let session_result = run_session(
            &mut transport,
            &mut session,
            &mut wake_rx,
            &mut audio_rx,
            &mut status_tick,
            &mut audio,
            &pushing,
            &ref_tx,
        )
        .await;

        // 会话结束：把最后一次成功设置的音量带进下一会话（P2-41）。
        volume = session.volume;

        match session_result {
            SessionEnd::Disconnected => {
                warn!("disconnected from hub — reconnecting");
            }
            SessionEnd::Shutdown => {
                info!("shutting down");
                capture_handle.abort();
                return Ok(());
            }
        }
    }
}

enum SessionEnd {
    Disconnected,
    Shutdown,
}

/// 一次会话的消息循环。返回 Disconnected（断线，重连）或 Shutdown（Ctrl-C）。
/// 录音任务退出后 wake/audio 通道关闭，用 alive 标志禁用对应分支，避免空转。
/// `pushing`：录音任务的推流标志（P2-19）——本函数在会话开始与每次 Hub
/// 消息处理后按 `session.pushing_audio()` 同步它，录音任务据此决定是否编码。
/// `ref_tx`：播放参考通道的发送端（Opus 级回声接线）——每次成功播出一帧
/// 音频后，把解出的 PCM 发给录音任务的 Aec 作参考；通道满时丢弃（尽力而为）。
// 8 个参数是会话循环的固有形态（transport/session/两个通道/节拍/音频/
// 推流标志/播放参考），拆 struct 只会多一层间接，不拆，clippy 例外有据可查。
#[allow(clippy::too_many_arguments)]
async fn run_session(
    transport: &mut Transport,
    session: &mut Session,
    wake_rx: &mut mpsc::Receiver<kws::WakeEvent>,
    audio_rx: &mut mpsc::Receiver<Vec<u8>>,
    status_tick: &mut tokio::time::Interval,
    audio: &mut Option<Audio>,
    pushing: &AtomicBool,
    ref_tx: &mpsc::Sender<[i16; audio::FRAME_SAMPLES]>,
) -> SessionEnd {
    let mut wake_alive = true;
    let mut audio_alive = true;
    // P1-16：播放看门狗——每 500ms 检查一次 playing 是否空闲过期（2s 无帧），
    // 过期即复位，防 `play_text`/`start_play` 后无帧跟进造成的永久 latch。
    let mut playback_tick = tokio::time::interval(std::time::Duration::from_millis(500));
    // 播放失败的节流日志：无播放设备/播放报错时不再每帧一条 warn 刷屏，
    // 改为最多 5 秒一条（含累计丢帧数），失败原因仍明确可见。
    let mut playback_err_count: u64 = 0;
    let mut last_playback_err_log: Option<std::time::Instant> = None;
    // server 模式：会话一建立就发 audio_start，开始持续推流（不等 wake_win）。
    if session.server_mode {
        session.streaming = true;
        if let Err(e) = transport.send_audio_start().await {
            warn!("send audio_start (server KWS mode) failed: {e}");
            return SessionEnd::Disconnected;
        }
    }
    // P2-19：会话开始即同步一次推流标志（server 模式 true，local 模式
    // 按当前会话状态；此后每次 Hub 消息处理后都再同步一次）。
    pushing.store(session.pushing_audio(), Ordering::Relaxed);
    loop {
        tokio::select! {
            // Hub 下行：文本消息 + 二进制音频帧。
            raw = transport.next_raw() => {
                match raw {
                    Ok(Some(RawFrame::Hub(msg))) => {
                        for action in session.on_hub_msg(&msg) {
                            if let Err(e) = apply_action(transport, action, session, audio).await {
                                warn!("failed to apply session action: {e}");
                                return SessionEnd::Disconnected;
                            }
                        }
                        pushing.store(session.pushing_audio(), Ordering::Relaxed);
                    }
                    Ok(Some(RawFrame::Audio(opus))) => {
                        // 对讲/广播下发的语音帧：解码后写 ALSA 放音。
                        // P1-16：收到帧即视为播放开始（裸帧也能播，不再因
                        // playing == false 静默丢弃）；打断后 200ms 内的
                        // 在途帧丢弃，不重启播放；无播放设备/播放失败时
                        // 返回明确错误（节流记日志 + 计数），不静默吞掉。
                        if session.interrupt_suppressed() {
                            continue;
                        }
                        match audio.as_mut() {
                            Some(a) => match a.play_frame(&opus) {
                                Ok(pcm) => {
                                    session.note_played_frame();
                                    // Opus 级回声接线：把刚播出的 PCM 作
                                    // 播放参考发给录音任务的 Aec（通道满
                                    // 时丢弃——参考是尽力而为的信号，
                                    // 丢一帧只影响一帧的消除效果）。
                                    if pcm.len() == audio::FRAME_SAMPLES {
                                        let mut frame = [0i16; audio::FRAME_SAMPLES];
                                        frame.copy_from_slice(&pcm);
                                        let _ = ref_tx.try_send(frame);
                                    }
                                    info!("played {}-sample frame via ALSA", pcm.len());
                                }
                                Err(e) => {
                                    playback_err_count += 1;
                                    let now = std::time::Instant::now();
                                    if last_playback_err_log
                                        .map(|t| now.duration_since(t) > std::time::Duration::from_secs(5))
                                        .unwrap_or(true)
                                    {
                                        warn!("playback frame failed ({playback_err_count} frames dropped so far): {e}");
                                        last_playback_err_log = Some(now);
                                        playback_err_count = 0;
                                    }
                                }
                            },
                            None => {
                                playback_err_count += 1;
                                let now = std::time::Instant::now();
                                if last_playback_err_log
                                    .map(|t| now.duration_since(t) > std::time::Duration::from_secs(5))
                                    .unwrap_or(true)
                                {
                                    warn!("audio unavailable, dropping playback frame ({playback_err_count} frames dropped so far)");
                                    last_playback_err_log = Some(now);
                                    playback_err_count = 0;
                                }
                            }
                        }
                    }
                    Ok(None) => return SessionEnd::Disconnected,
                    Err(e) => {
                        warn!("transport error: {e}");
                        return SessionEnd::Disconnected;
                    }
                }
            }
            // KWS 命中：上报 wake，等 Hub 仲裁（wake_win / wake_lose）。
            ev = wake_rx.recv(), if wake_alive => {
                match ev {
                    Some(ev) => {
                        if let Err(e) = transport.send_wake(&ev.wake_word, ev.rms_db).await {
                            warn!("send wake failed: {e}");
                            return SessionEnd::Disconnected;
                        }
                        info!("wake reported: rms={:.1}dB", ev.rms_db);
                    }
                    None => {
                        // 录音任务已退出（Phase 1：ALSA 未接线）；继续跑传输。
                        warn!("capture task ended; KWS idle for this session");
                        wake_alive = false;
                    }
                }
            }
            // 录音任务推来的 Opus 帧：仲裁赢了（streaming）或对讲录音
            // （recording）时往 Hub 推；server 模式下 streaming 恒为
            // true，即持续推流。
            frame = audio_rx.recv(), if audio_alive => {
                match frame {
                    Some(opus) => {
                        if session.pushing_audio() {
                            if let Err(e) = transport.send_audio_frame(&opus).await {
                                warn!("send audio frame failed: {e}");
                                return SessionEnd::Disconnected;
                            }
                        }
                    }
                    None => {
                        audio_alive = false;
                    }
                }
            }
            // 30s 状态上报 + 心跳（P1-44：每 30s 发 heartbeat，与 status
            // 同一节拍；任一发送失败即断线重连，与 status 同待遇）。
            _ = status_tick.tick() => {
                let version = env!("CARGO_PKG_VERSION");
                if let Err(e) = transport.send_status(session.volume, session.playing, version).await {
                    warn!("send status failed: {e}");
                    return SessionEnd::Disconnected;
                }
                if let Err(e) = transport.send_heartbeat().await {
                    warn!("send heartbeat failed: {e}");
                    return SessionEnd::Disconnected;
                }
            }
            // 播放看门狗（P1-16）：playing 空闲过期即复位。
            _ = playback_tick.tick() => {
                if session.playback_idle_expired() {
                    info!("playback idle timeout: clearing latched playing state");
                    session.clear_playing();
                }
            }
            // Ctrl-C：优雅退出；server 模式先尽力发 audio_end 收尾。
            _ = tokio::signal::ctrl_c() => {
                if session.server_mode {
                    let _ = transport.send_audio_end().await;
                }
                return SessionEnd::Shutdown;
            }
        }
    }
}

/// 执行 Session 动作（发消息给 Hub）。
/// `SetVolume` 需要 session（成功才更新 `Session.volume`）与 audio
/// （真调混音器），其余动作与旧签名一致。
async fn apply_action(
    transport: &mut Transport,
    action: SessionAction,
    session: &mut Session,
    audio: &mut Option<Audio>,
) -> Result<()> {
    match action {
        SessionAction::SendAudioStart => transport.send_audio_start().await,
        SessionAction::SendAudioEnd => transport.send_audio_end().await,
        SessionAction::StopPlayback => {
            // P2-N1：立即停播——调 ALSA `snd_pcm_drop` 丢弃播放缓冲里
            // 已写入的约 80ms 尾音，再 `prepare` 复位（下一帧可直接写）。
            // 无播放设备时只记日志（打断本身的状态复位已由 Interrupt
            // 处理完成）；停播失败时记 warn——尾音会放完，但状态机
            // 不因此断线（打断失败不应杀掉整个会话）。
            match audio.as_mut() {
                Some(a) => match a.stop_playback() {
                    Ok(()) => {
                        info!("interrupt: stopping playback immediately (ALSA buffer dropped)");
                    }
                    Err(e) => {
                        warn!("interrupt: stop_playback failed ({e}); buffered tail (~80ms) will play out");
                    }
                },
                None => {
                    info!("interrupt: stopping playback (no playback device in this session)");
                }
            }
            Ok(())
        }
        SessionAction::SetVolume { id, volume } => {
            // P1-N1：真调混音器，按真实结果回 rpc_result。成功才更新
            // Session.volume（status 上报随即反映真实音量）；失败回
            // ok:false + 原因，绝不谎报成功。
            match audio.as_mut() {
                Some(a) => match a.set_volume(volume) {
                    Ok(()) => {
                        session.volume = volume;
                        info!("rpc set_volume: {volume} (mixer applied)");
                        transport.send_rpc_result(&id, true, None).await
                    }
                    Err(e) => {
                        warn!("rpc set_volume {volume} failed: {e}");
                        transport
                            .send_rpc_result(&id, false, Some(&e.to_string()))
                            .await
                    }
                },
                None => {
                    warn!("rpc set_volume {volume} failed: no playback device in this session");
                    transport
                        .send_rpc_result(
                            &id,
                            false,
                            Some("audio unavailable: no playback device in this session"),
                        )
                        .await
                }
            }
        }
        SessionAction::SendRpcResult { id, ok, error } => {
            transport.send_rpc_result(&id, ok, error.as_deref()).await
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use transport::HubMsg;

    #[test]
    fn wake_win_starts_streaming_and_sends_audio_start() {
        let mut s = Session::new();
        let actions = s.on_hub_msg(&HubMsg::WakeWin);
        assert!(s.streaming);
        assert_eq!(actions, vec![SessionAction::SendAudioStart]);
    }

    #[test]
    fn wake_lose_keeps_silent() {
        let mut s = Session::new();
        s.streaming = true;
        let actions = s.on_hub_msg(&HubMsg::WakeLose);
        assert!(!s.streaming);
        assert_eq!(actions, vec![SessionAction::SendAudioEnd]);
        // 未推流时输了也只是静默，不发多余消息。
        let mut s2 = Session::new();
        assert!(s2.on_hub_msg(&HubMsg::WakeLose).is_empty());
        assert!(!s2.streaming);
    }

    #[test]
    fn interrupt_stops_playback_immediately() {
        let mut s = Session::new();
        s.playing = true;
        s.streaming = true;
        let actions = s.on_hub_msg(&HubMsg::Interrupt);
        assert!(!s.playing);
        assert!(!s.streaming);
        assert!(actions.contains(&SessionAction::StopPlayback));
        assert!(actions.contains(&SessionAction::SendAudioEnd));
    }

    #[test]
    fn rpc_play_text_and_set_volume() {
        let mut s = Session::new();
        let actions = s.on_hub_msg(&HubMsg::Rpc {
            id: "1".to_string(),
            method: "play_text".to_string(),
            params: serde_json::json!({"text": "你好"}),
        });
        assert!(s.playing);
        assert_eq!(
            actions,
            vec![SessionAction::SendRpcResult {
                id: "1".to_string(),
                ok: true,
                error: None
            }]
        );
        // P1-N1：set_volume 不再当场回 ok——合法值走 SetVolume 动作，
        // 由 apply_action 真调混音器后按真实结果回 rpc_result；
        // 在此之前 Session.volume 不变（不是只改内存数字）。
        let actions = s.on_hub_msg(&HubMsg::Rpc {
            id: "2".to_string(),
            method: "set_volume".to_string(),
            params: serde_json::json!({"volume": 80}),
        });
        assert_eq!(s.volume, 50, "volume must not change before the mixer call");
        assert_eq!(
            actions,
            vec![SessionAction::SetVolume {
                id: "2".to_string(),
                volume: 80
            }]
        );
    }

    #[test]
    fn rpc_set_volume_invalid_values_report_error_not_silent_50() {
        // P2-18：非法值不再静默重置为 50——缺失/非数字/越界一律回 ok:false。
        let mut s = Session::new();
        for (params, desc) in [
            (serde_json::json!({}), "missing volume"),
            (serde_json::json!({"volume": "loud"}), "non-numeric volume"),
            (serde_json::json!({"volume": 101}), "volume > 100"),
            (serde_json::json!({"volume": 80.5}), "float volume"),
        ] {
            let actions = s.on_hub_msg(&HubMsg::Rpc {
                id: "bad".to_string(),
                method: "set_volume".to_string(),
                params,
            });
            assert_eq!(s.volume, 50, "volume must stay 50 for {desc}");
            assert_eq!(actions.len(), 1, "one action for {desc}");
            match &actions[0] {
                SessionAction::SendRpcResult { ok, error, .. } => {
                    assert!(!ok, "must fail for {desc}");
                    assert!(
                        error.as_deref().unwrap().contains("set_volume"),
                        "error must name set_volume for {desc}: {error:?}"
                    );
                }
                other => panic!("unexpected action for {desc}: {other:?}"),
            }
        }
        // 边界值 0 与 100 合法。
        let actions = s.on_hub_msg(&HubMsg::Rpc {
            id: "ok".to_string(),
            method: "set_volume".to_string(),
            params: serde_json::json!({"volume": 0}),
        });
        assert_eq!(
            actions,
            vec![SessionAction::SetVolume {
                id: "ok".to_string(),
                volume: 0
            }]
        );
    }

    #[test]
    fn parse_volume_param_accepts_only_int_0_to_100() {
        assert_eq!(
            parse_volume_param(&serde_json::json!({"volume": 100})).unwrap(),
            100
        );
        assert!(parse_volume_param(&serde_json::json!({})).is_err());
        assert!(parse_volume_param(&serde_json::json!({"volume": -1})).is_err());
    }

    // --- P1-N2 / P1-15：start_recording 是真正的录音启停 ---
    #[test]
    fn start_recording_starts_streaming_and_sends_audio_start() {
        // local 模式：未在推流时 start_recording 置 recording + 发 audio_start。
        let mut s = Session::new();
        assert!(!s.pushing_audio());
        let actions = s.on_hub_msg(&HubMsg::Rpc {
            id: "r1".to_string(),
            method: "start_recording".to_string(),
            params: serde_json::json!({}),
        });
        assert!(s.recording);
        assert!(s.pushing_audio());
        assert_eq!(
            actions,
            vec![
                SessionAction::SendAudioStart,
                SessionAction::SendRpcResult {
                    id: "r1".to_string(),
                    ok: true,
                    error: None
                }
            ]
        );
        // stop_recording：清标志 + 发 audio_end + 回 ok。
        let actions = s.on_hub_msg(&HubMsg::Rpc {
            id: "r2".to_string(),
            method: "stop_recording".to_string(),
            params: serde_json::json!({}),
        });
        assert!(!s.recording);
        assert!(!s.pushing_audio());
        assert_eq!(
            actions,
            vec![
                SessionAction::SendAudioEnd,
                SessionAction::SendRpcResult {
                    id: "r2".to_string(),
                    ok: true,
                    error: None
                }
            ]
        );
    }

    #[test]
    fn start_recording_does_not_duplicate_audio_start_when_streaming() {
        // 已在推流（仲裁赢了）时 start_recording 不重复发 audio_start。
        let mut s = Session::new();
        s.on_hub_msg(&HubMsg::WakeWin);
        assert!(s.streaming);
        let actions = s.on_hub_msg(&HubMsg::Rpc {
            id: "r1".to_string(),
            method: "start_recording".to_string(),
            params: serde_json::json!({}),
        });
        assert!(s.recording);
        assert_eq!(
            actions,
            vec![SessionAction::SendRpcResult {
                id: "r1".to_string(),
                ok: true,
                error: None
            }]
        );
        // 此时 stop_recording 不发 audio_end——仲裁会话仍在推流。
        let actions = s.on_hub_msg(&HubMsg::Rpc {
            id: "r2".to_string(),
            method: "stop_recording".to_string(),
            params: serde_json::json!({}),
        });
        assert!(!s.recording);
        assert!(s.streaming);
        assert_eq!(
            actions,
            vec![SessionAction::SendRpcResult {
                id: "r2".to_string(),
                ok: true,
                error: None
            }]
        );
    }

    #[test]
    fn wake_lose_does_not_stop_intercom_recording() {
        // 仲裁输了只停仲裁会话，不停对讲录音（recording 仍在推流）。
        let mut s = Session::new();
        s.on_hub_msg(&HubMsg::Rpc {
            id: "r1".to_string(),
            method: "start_recording".to_string(),
            params: serde_json::json!({}),
        });
        let actions = s.on_hub_msg(&HubMsg::WakeLose);
        assert!(s.recording);
        assert!(s.pushing_audio());
        assert!(
            actions.is_empty(),
            "wake_lose must not end the intercom stream"
        );
    }

    #[test]
    fn interrupt_ends_recording_in_local_mode() {
        let mut s = Session::new();
        s.on_hub_msg(&HubMsg::Rpc {
            id: "r1".to_string(),
            method: "start_recording".to_string(),
            params: serde_json::json!({}),
        });
        assert!(s.recording);
        let actions = s.on_hub_msg(&HubMsg::Interrupt);
        assert!(!s.recording);
        assert!(!s.pushing_audio());
        assert!(actions.contains(&SessionAction::StopPlayback));
        assert!(actions.contains(&SessionAction::SendAudioEnd));
    }

    #[test]
    fn start_recording_in_server_mode_sends_no_audio_start() {
        // server 模式会话建立时已发过 audio_start，录音只置标志。
        let mut s = Session::new_server();
        assert!(s.pushing_audio());
        let actions = s.on_hub_msg(&HubMsg::Rpc {
            id: "r1".to_string(),
            method: "start_recording".to_string(),
            params: serde_json::json!({}),
        });
        assert!(s.recording);
        assert_eq!(
            actions,
            vec![SessionAction::SendRpcResult {
                id: "r1".to_string(),
                ok: true,
                error: None
            }]
        );
    }

    // --- P1-16：playing 空闲过期，不再永久 latch ---
    #[test]
    fn playing_does_not_latch_without_frames() {
        let mut s = Session::new();
        // play_text 置 playing 后立即检查：未过期（刚置 true）。
        s.on_hub_msg(&HubMsg::Rpc {
            id: "p1".to_string(),
            method: "play_text".to_string(),
            params: serde_json::json!({"text": "你好"}),
        });
        assert!(s.playing);
        assert!(!s.playback_idle_expired());
        // 2 秒无帧 → 过期；看门狗据此复位。
        std::thread::sleep(PLAYBACK_IDLE_TIMEOUT + std::time::Duration::from_millis(100));
        assert!(s.playback_idle_expired());
        s.clear_playing();
        assert!(!s.playing);
        assert!(!s.playback_idle_expired());
    }

    #[test]
    fn played_frame_refreshes_idle_timer() {
        let mut s = Session::new();
        s.on_hub_msg(&HubMsg::Rpc {
            id: "p1".to_string(),
            method: "play_text".to_string(),
            params: serde_json::json!({"text": "你好"}),
        });
        // 1 秒后播一帧 → 空闲计时从该帧起算，再过 1.5 秒仍未过期。
        std::thread::sleep(std::time::Duration::from_millis(1000));
        s.note_played_frame();
        assert!(s.playing);
        std::thread::sleep(std::time::Duration::from_millis(1500));
        assert!(
            !s.playback_idle_expired(),
            "a frame 1.5s ago must keep playing alive"
        );
    }

    #[test]
    fn stop_play_clears_playing_immediately() {
        let mut s = Session::new();
        s.on_hub_msg(&HubMsg::Rpc {
            id: "p1".to_string(),
            method: "start_play".to_string(),
            params: serde_json::json!({}),
        });
        assert!(s.playing);
        s.on_hub_msg(&HubMsg::Rpc {
            id: "p2".to_string(),
            method: "stop_play".to_string(),
            params: serde_json::json!({}),
        });
        assert!(!s.playing);
        assert!(!s.playback_idle_expired());
    }

    #[test]
    fn rpc_unknown_method_reports_error() {
        let mut s = Session::new();
        let actions = s.on_hub_msg(&HubMsg::Rpc {
            id: "9".to_string(),
            method: "run_shell".to_string(),
            params: serde_json::json!({}),
        });
        // run_shell 不在协议白名单里：必须拒绝（原项目裸奔 run_shell 是安全红线）。
        assert_eq!(actions.len(), 1);
        match &actions[0] {
            SessionAction::SendRpcResult { ok, error, .. } => {
                assert!(!ok);
                assert!(error.as_deref().unwrap().contains("unknown rpc method"));
            }
            other => panic!("unexpected action: {other:?}"),
        }
    }

    #[test]
    fn server_mode_session_streams_from_start_and_ignores_arbitration() {
        let mut s = Session::new_server();
        assert!(s.server_mode);
        assert!(s.streaming);
        // wake_win 不重复发 audio_start（已在持续推流）。
        assert!(s.on_hub_msg(&HubMsg::WakeWin).is_empty());
        assert!(s.streaming);
        // wake_lose 不停持续推流。
        assert!(s.on_hub_msg(&HubMsg::WakeLose).is_empty());
        assert!(s.streaming);
        // interrupt 只停播，不停流、不发 audio_end。
        s.playing = true;
        let actions = s.on_hub_msg(&HubMsg::Interrupt);
        assert!(!s.playing);
        assert!(s.streaming);
        assert_eq!(actions, vec![SessionAction::StopPlayback]);
    }

    #[test]
    fn local_mode_session_is_unchanged() {
        // Session::new() 仍是 local 语义：wake_win 发 audio_start、
        // wake_lose 停流、interrupt 停播又停流（与 server 模式对照）。
        let mut s = Session::new();
        assert!(!s.server_mode);
        assert!(!s.streaming);
        assert_eq!(
            s.on_hub_msg(&HubMsg::WakeWin),
            vec![SessionAction::SendAudioStart]
        );
        assert_eq!(
            s.on_hub_msg(&HubMsg::WakeLose),
            vec![SessionAction::SendAudioEnd]
        );
        assert!(!s.streaming);
    }

    /// 无本地后端的配置（模型目录/.so/模型文件全都不存在）。
    fn no_backend_config(kws_mode: &str) -> config::Config {
        config::Config {
            kws_mode: kws_mode.to_string(),
            kws_model: "/tmp/no-such-model.onnx".to_string(),
            kws_model_dir: "/tmp/no-such-kws-models".to_string(),
            kws_lib: String::new(),
            ..config::Config::default()
        }
    }

    #[test]
    fn init_kws_server_mode_needs_no_backend() {
        // server 模式：无模型目录、无 .so、无模型文件，也必须正常启动
        // （Ok(None)），两种 feature 下都如此——fail-loud 只属于 local 模式。
        let cfg = no_backend_config("server");
        let kws = init_kws(&cfg).expect("server mode must start without a local backend");
        assert!(kws.is_none());
    }

    /// local 模式 + sherpa feature 开 + 无后端：必须 fail-loud（P1-40 回归，
    /// 与 kws.rs 的 `kws_new_refuses_rms_only_when_sherpa_feature_on` 同行为，
    /// 这里覆盖的是 main.rs 的启动路径 init_kws）。
    #[test]
    #[cfg(feature = "sherpa-kws")]
    fn init_kws_local_mode_refuses_without_backend_when_sherpa_feature_on() {
        let cfg = no_backend_config("local");
        let err = init_kws(&cfg)
            .err()
            .expect("local mode + sherpa feature + no backend must fail loud");
        let msg = err.to_string();
        assert!(
            msg.contains("RMS-only") || msg.contains("P1-40"),
            "unexpected error: {msg}"
        );
    }

    /// local 模式 + sherpa feature 不开：保持老行为，返回纯 RMS 实例。
    #[test]
    #[cfg(not(feature = "sherpa-kws"))]
    fn init_kws_local_mode_is_rms_only_when_sherpa_feature_off() {
        let cfg = no_backend_config("local");
        let kws = init_kws(&cfg).expect("local mode without sherpa feature must return RMS-only");
        let kws = kws.expect("local mode must return a KWS instance");
        assert!(!kws.model_loaded());
    }

    /// server 模式端到端（fake Hub + 真实 run_session）：无本地后端、
    /// 无 wake_win 时，客户端仍正常启动、发 audio_start、持续把 Opus 帧
    /// 推给 Hub、不退出。音频是合成正弦波（非真人语音），只验证推流
    /// 不断，不验证唤醒检出（唤醒在 Hub/边车侧，不在本测试里）。
    #[tokio::test]
    async fn server_mode_streams_continuously_without_wake_win() {
        use futures_util::{SinkExt, StreamExt};
        use std::sync::{Arc, Mutex};
        use std::time::Duration;
        use tokio::net::TcpListener;
        use tokio_tungstenite::tungstenite::Message;

        const TEST_TOKEN: &str = "test-token-0123456789abcdef";
        const FRAMES_TO_SEND: usize = 10;

        // Fake Hub：回 hello_ok，记录收到的文本消息类型与二进制音频帧。
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let events: Arc<Mutex<Vec<String>>> = Arc::new(Mutex::new(Vec::new()));
        let events_hub = events.clone();
        let hub = tokio::spawn(async move {
            let (stream, _) = listener.accept().await.unwrap();
            let mut ws = tokio_tungstenite::accept_async(stream).await.unwrap();
            // hello → hello_ok（hello 本身不记入事件）。
            let first = ws.next().await.unwrap().unwrap();
            assert!(matches!(first, Message::Text(_)));
            ws.send(Message::Text(
                serde_json::json!({"type": "hello_ok", "device_id": "fake-hub-device", "room": "客厅"})
                    .to_string()
                    .into(),
            ))
            .await
            .unwrap();
            while let Some(msg) = ws.next().await {
                match msg {
                    Ok(Message::Text(t)) => {
                        let v: serde_json::Value =
                            serde_json::from_str(&t).unwrap_or(serde_json::Value::Null);
                        let kind = v
                            .get("type")
                            .and_then(|x| x.as_str())
                            .unwrap_or("?")
                            .to_string();
                        events_hub.lock().unwrap().push(format!("text:{kind}"));
                    }
                    Ok(Message::Binary(data)) => {
                        // 0x01 + Opus 包：记一帧（长度含 1 字节帧头）。
                        events_hub
                            .lock()
                            .unwrap()
                            .push(format!("audio:{}", data.len()));
                    }
                    Ok(Message::Ping(p)) => {
                        let _ = ws.send(Message::Pong(p)).await;
                    }
                    Ok(Message::Close(_)) | Err(_) => break,
                    _ => {}
                }
            }
        });

        // 客户端：server 模式，无本地后端（init_kws 直接 Ok(None)，不碰模型/.so）。
        let cfg = config::Config {
            hub_url: format!("ws://{addr}"),
            token: TEST_TOKEN.to_string(),
            tls: false,
            kws_mode: "server".to_string(),
            kws_model: "/tmp/no-such-model.onnx".to_string(),
            kws_model_dir: "/tmp/no-such-kws-models".to_string(),
            kws_lib: String::new(),
            ..config::Config::default()
        };
        assert!(init_kws(&cfg)
            .expect("server mode must start without a local backend")
            .is_none());

        let mut transport = transport::connect_once(&cfg)
            .await
            .expect("fake hub connect must pass");
        let mut session = Session::new_server();
        let (wake_tx, mut wake_rx) = mpsc::channel::<crate::kws::WakeEvent>(8);
        let (audio_tx, mut audio_rx) = mpsc::channel::<Vec<u8>>(32);
        let mut status_tick = tokio::time::interval(transport::STATUS_INTERVAL);
        let mut audio: Option<Audio> = None;

        // 真实 Opus 编码的合成正弦波帧（1000Hz，合成音频，非真人语音）。
        let mut codec = crate::audio::OpusCodec::new().unwrap();
        let mut frame = [0i16; crate::audio::FRAME_SAMPLES];
        for (i, s) in frame.iter_mut().enumerate() {
            let t = i as f32 / 16000.0;
            *s = (12000.0 * (2.0 * std::f32::consts::PI * 1000.0 * t).sin()) as i16;
        }
        let mut packets = Vec::new();
        for _ in 0..FRAMES_TO_SEND {
            packets.push(codec.encode_frame(&frame).unwrap());
        }

        // 另一个任务推帧；本任务跑 run_session（借用 transport/session，
        // 不能 spawn，只能在原地跑 + timeout 观察它是否一直活着）。
        let sender = tokio::spawn(async move {
            tokio::time::sleep(Duration::from_millis(100)).await;
            for pkt in packets {
                if audio_tx.send(pkt).await.is_err() {
                    break;
                }
            }
            // 保持 wake 通道 alive 到测试结束（不让 wake_rx 误判录音任务退出）。
            tokio::time::sleep(Duration::from_secs(30)).await;
            drop(wake_tx);
        });
        let pushing = std::sync::atomic::AtomicBool::new(false);
        let (ref_tx, _ref_rx) = mpsc::channel::<[i16; crate::audio::FRAME_SAMPLES]>(16);
        let result = tokio::time::timeout(
            Duration::from_secs(5),
            run_session(
                &mut transport,
                &mut session,
                &mut wake_rx,
                &mut audio_rx,
                &mut status_tick,
                &mut audio,
                &pushing,
                &ref_tx,
            ),
        )
        .await;
        // 超时 = 会话 5 秒内没退出、没断线：server 模式“不退出”得证。
        // （若 run_session 提前返回 Disconnected/Shutdown，timeout 会返回 Ok。）
        assert!(
            result.is_err(),
            "server-mode session must keep running without a local backend or wake_win, but it ended early"
        );

        let recorded = events.lock().unwrap().clone();
        sender.abort();
        hub.abort();
        let audio_start_pos = recorded
            .iter()
            .position(|e| e == "text:audio_start")
            .expect("hub must receive audio_start in server mode");
        let audio_frames: Vec<&String> = recorded
            .iter()
            .filter(|e| e.starts_with("audio:"))
            .collect();
        assert_eq!(
            audio_frames.len(),
            FRAMES_TO_SEND,
            "hub must receive all {FRAMES_TO_SEND} Opus frames without any wake_win; got: {recorded:?}"
        );
        // audio_start 必须在第一帧音频之前（先开流，后推帧）。
        let first_audio_pos = recorded
            .iter()
            .position(|e| e.starts_with("audio:"))
            .unwrap();
        assert!(
            audio_start_pos < first_audio_pos,
            "audio_start must precede the first audio frame; got: {recorded:?}"
        );
        // 每帧都是 0x01 帧头 + 非空 Opus 包（>1 字节）。
        for f in &audio_frames {
            let len: usize = f.trim_start_matches("audio:").parse().unwrap();
            assert!(
                len > 1,
                "audio frame must carry a non-empty Opus packet: {f}"
            );
        }
    }

    #[test]
    fn room_ok_records_room() {
        let mut s = Session::new();
        s.on_hub_msg(&HubMsg::RoomOk {
            room: "客厅".to_string(),
        });
        assert_eq!(s.room.as_deref(), Some("客厅"));
    }

    // --- 端到端（fake Hub + 真实 run_session）：P1-N1 / P1-16 / P1-N2 ---
    //
    // 以下测试用真实 run_session（不是只测 Session 状态机），验证胶水层：
    // apply_action 真调混音器并按真实结果回 rpc_result；裸音频帧能播；
    // local 模式 start_recording 后 Opus 帧真的推出去。音频是合成正弦波
    // （非真人语音），只验证推流/播放不断，不验证唤醒检出。

    /// e2e 测试用 Fake 设备：播放记帧数，set_volume 记值（经共享 Arc 读回）。
    struct TestAudioDevice {
        played_frames: std::sync::Arc<std::sync::Mutex<usize>>,
        last_volume: std::sync::Arc<std::sync::Mutex<Option<u8>>>,
        /// P2-N1：stop_playback 被调过几次（经共享 Arc 读回）。
        stop_calls: std::sync::Arc<std::sync::Mutex<usize>>,
    }

    impl crate::audio::AudioDevice for TestAudioDevice {
        fn capture(&mut self) -> anyhow::Result<[i16; crate::audio::FRAME_SAMPLES]> {
            Err(anyhow::anyhow!("test device has no capture"))
        }

        fn play(&mut self, pcm: &[i16]) -> anyhow::Result<()> {
            if pcm.len() == crate::audio::FRAME_SAMPLES {
                *self.played_frames.lock().unwrap() += 1;
            }
            Ok(())
        }

        fn describe(&self) -> String {
            "TestAudioDevice".to_string()
        }

        fn set_volume(&mut self, volume: u8) -> anyhow::Result<()> {
            *self.last_volume.lock().unwrap() = Some(volume);
            Ok(())
        }

        /// P2-N1：Fake 的 stop_playback 计数调用（模拟 ALSA drop 丢弃尾音）。
        fn stop_playback(&mut self) -> anyhow::Result<()> {
            *self.stop_calls.lock().unwrap() += 1;
            Ok(())
        }
    }

    /// 录一段合成正弦波 Opus 包（1000Hz，合成音频，非真人语音）。
    fn sine_opus_packets(n: usize) -> Vec<Vec<u8>> {
        let mut codec = crate::audio::OpusCodec::new().unwrap();
        let mut frame = [0i16; crate::audio::FRAME_SAMPLES];
        for (i, s) in frame.iter_mut().enumerate() {
            let t = i as f32 / 16000.0;
            *s = (12000.0 * (2.0 * std::f32::consts::PI * 1000.0 * t).sin()) as i16;
        }
        (0..n)
            .map(|_| codec.encode_frame(&frame).unwrap())
            .collect()
    }

    /// P1-N1 端到端：fake Hub 发 set_volume RPC → 客户端真调混音器（Fake
    /// 设备记值）→ 回 rpc_result ok:true，且 Session.volume 更新为 80。
    /// 无播放设备时回 ok:false（同一测试的第二段）。
    #[tokio::test]
    async fn set_volume_rpc_end_to_end_reports_real_result() {
        use futures_util::{SinkExt, StreamExt};
        use std::sync::{Arc, Mutex};
        use std::time::Duration;
        use tokio::net::TcpListener;
        use tokio_tungstenite::tungstenite::Message;

        const TEST_TOKEN: &str = "test-token-0123456789abcdef";

        // 跑一轮：with_audio=true 时接 Fake 设备，false 时 audio=None。
        // 返回（Hub 收到的 rpc_result 全文，Fake 设备记到的音量）。
        async fn one_round(with_audio: bool) -> (Vec<String>, Option<u8>) {
            let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
            let addr = listener.local_addr().unwrap();
            let results: Arc<Mutex<Vec<String>>> = Arc::new(Mutex::new(Vec::new()));
            let results_hub = results.clone();
            let hub = tokio::spawn(async move {
                let (stream, _) = listener.accept().await.unwrap();
                let mut ws = tokio_tungstenite::accept_async(stream).await.unwrap();
                let first = ws.next().await.unwrap().unwrap();
                assert!(matches!(first, Message::Text(_)));
                ws.send(Message::Text(
                    serde_json::json!({"type": "hello_ok", "device_id": "e2e-device", "room": "客厅"})
                        .to_string()
                        .into(),
                ))
                .await
                .unwrap();
                // 等客户端的 register 发完，再发 set_volume RPC。
                tokio::time::sleep(Duration::from_millis(200)).await;
                ws.send(Message::Text(
                    serde_json::json!({"type": "rpc", "id": "v1", "method": "set_volume", "params": {"volume": 80}})
                        .to_string()
                        .into(),
                ))
                .await
                .unwrap();
                let deadline = tokio::time::sleep(Duration::from_millis(1500));
                tokio::pin!(deadline);
                loop {
                    tokio::select! {
                        msg = ws.next() => {
                            match msg {
                                Some(Ok(Message::Text(t))) => {
                                    let v: serde_json::Value =
                                        serde_json::from_str(&t).unwrap_or(serde_json::Value::Null);
                                    if v.get("type").and_then(|x| x.as_str()) == Some("rpc_result") {
                                        results_hub.lock().unwrap().push(t.to_string());
                                    }
                                }
                                Some(Ok(Message::Ping(p))) => {
                                    let _ = ws.send(Message::Pong(p)).await;
                                }
                                Some(Ok(Message::Close(_))) | Some(Err(_)) | None => break,
                                _ => {}
                            }
                        }
                        _ = &mut deadline => break,
                    }
                }
                let _ = ws.close(None).await;
            });

            let cfg = config::Config {
                hub_url: format!("ws://{addr}"),
                token: TEST_TOKEN.to_string(),
                tls: false,
                allow_insecure_ws: false,
                kws_mode: "server".to_string(),
                kws_model: "/tmp/no-such-model.onnx".to_string(),
                kws_model_dir: "/tmp/no-such-kws-models".to_string(),
                kws_lib: String::new(),
                ..config::Config::default()
            };
            let mut transport = transport::connect_once(&cfg)
                .await
                .expect("fake hub connect must pass");
            let mut session = Session::new_server();
            let (wake_tx, mut wake_rx) = mpsc::channel::<crate::kws::WakeEvent>(8);
            let (audio_tx, mut audio_rx) = mpsc::channel::<Vec<u8>>(32);
            let mut status_tick = tokio::time::interval(transport::STATUS_INTERVAL);
            let played = Arc::new(Mutex::new(0usize));
            let last_volume = Arc::new(Mutex::new(None));
            let stop_calls = Arc::new(Mutex::new(0usize));
            let mut audio: Option<Audio> = if with_audio {
                Some(
                    Audio::with_device(Box::new(TestAudioDevice {
                        played_frames: played.clone(),
                        last_volume: last_volume.clone(),
                        stop_calls: stop_calls.clone(),
                    }))
                    .unwrap(),
                )
            } else {
                None
            };
            // 保持 wake/audio 通道 alive（不让 run_session 误判录音任务退出）。
            let keepalive = tokio::spawn(async move {
                tokio::time::sleep(Duration::from_secs(30)).await;
                drop(wake_tx);
                drop(audio_tx);
            });
            let pushing = std::sync::atomic::AtomicBool::new(false);
            let (ref_tx, _ref_rx) = mpsc::channel::<[i16; crate::audio::FRAME_SAMPLES]>(16);
            let result = tokio::time::timeout(
                Duration::from_secs(5),
                run_session(
                    &mut transport,
                    &mut session,
                    &mut wake_rx,
                    &mut audio_rx,
                    &mut status_tick,
                    &mut audio,
                    &pushing,
                    &ref_tx,
                ),
            )
            .await;
            // Hub 1.5 秒后关闭，会话应随之返回 Disconnected（不是超时挂起）。
            assert!(
                result.is_ok(),
                "session must end when the fake hub closes (got timeout instead)"
            );
            let volume_after = session.volume;
            keepalive.abort();
            hub.abort();
            let recorded = results.lock().unwrap().clone();
            let vol = *last_volume.lock().unwrap();
            // Session.volume 只在混音器成功时更新——断言它与设备值一致。
            if with_audio {
                assert_eq!(
                    volume_after, 80,
                    "session volume must update on mixer success"
                );
            } else {
                assert_eq!(
                    volume_after, 50,
                    "session volume must stay 50 on mixer failure"
                );
            }
            (recorded, vol)
        }

        // 有播放设备：ok:true，设备记到 80。
        let (results, vol) = one_round(true).await;
        assert_eq!(
            results.len(),
            1,
            "hub must receive exactly one rpc_result; got {results:?}"
        );
        let r: serde_json::Value = serde_json::from_str(&results[0]).unwrap();
        assert_eq!(r["id"], "v1");
        assert_eq!(r["ok"], true, "mixer success must be reported ok:true: {r}");
        assert_eq!(vol, Some(80), "mixer must actually receive volume 80");

        // 无播放设备：ok:false + 原因，不谎报成功。
        let (results, vol) = one_round(false).await;
        assert_eq!(
            results.len(),
            1,
            "hub must receive exactly one rpc_result; got {results:?}"
        );
        let r: serde_json::Value = serde_json::from_str(&results[0]).unwrap();
        assert_eq!(r["id"], "v1");
        assert_eq!(
            r["ok"], false,
            "missing playback device must be reported ok:false: {r}"
        );
        assert!(
            r["error"].as_str().unwrap().contains("audio unavailable"),
            "error must name the cause: {r}"
        );
        assert_eq!(vol, None);
    }

    /// P1-16 端到端：Hub 只发裸音频帧（不发 start_play）→ 客户端仍播出
    /// （Fake 设备记到帧）；发 start_play 后同样播出。两种情况都不静默丢弃。
    #[tokio::test]
    async fn bare_audio_frames_play_without_start_play() {
        use futures_util::{SinkExt, StreamExt};
        use std::sync::{Arc, Mutex};
        use std::time::Duration;
        use tokio::net::TcpListener;
        use tokio_tungstenite::tungstenite::Message;

        const TEST_TOKEN: &str = "test-token-0123456789abcdef";

        async fn one_round(send_start_play: bool) -> usize {
            let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
            let addr = listener.local_addr().unwrap();
            let packets = sine_opus_packets(3);
            let hub = tokio::spawn(async move {
                let (stream, _) = listener.accept().await.unwrap();
                let mut ws = tokio_tungstenite::accept_async(stream).await.unwrap();
                let first = ws.next().await.unwrap().unwrap();
                assert!(matches!(first, Message::Text(_)));
                ws.send(Message::Text(
                    serde_json::json!({"type": "hello_ok", "device_id": "e2e-device", "room": "客厅"})
                        .to_string()
                        .into(),
                ))
                .await
                .unwrap();
                tokio::time::sleep(Duration::from_millis(200)).await;
                if send_start_play {
                    ws.send(Message::Text(
                        serde_json::json!({"type": "rpc", "id": "p1", "method": "start_play", "params": {}})
                            .to_string()
                            .into(),
                    ))
                    .await
                    .unwrap();
                    tokio::time::sleep(Duration::from_millis(100)).await;
                }
                for pkt in packets {
                    let mut framed = vec![crate::audio::AUDIO_FRAME_TYPE];
                    framed.extend_from_slice(&pkt);
                    ws.send(Message::Binary(framed.into())).await.unwrap();
                    tokio::time::sleep(Duration::from_millis(30)).await;
                }
                // 等客户端播完，再关连接。
                tokio::time::sleep(Duration::from_millis(800)).await;
                let _ = ws.close(None).await;
            });

            let cfg = config::Config {
                hub_url: format!("ws://{addr}"),
                token: TEST_TOKEN.to_string(),
                tls: false,
                allow_insecure_ws: false,
                kws_mode: "server".to_string(),
                kws_model: "/tmp/no-such-model.onnx".to_string(),
                kws_model_dir: "/tmp/no-such-kws-models".to_string(),
                kws_lib: String::new(),
                ..config::Config::default()
            };
            let mut transport = transport::connect_once(&cfg)
                .await
                .expect("fake hub connect must pass");
            let mut session = Session::new_server();
            let (wake_tx, mut wake_rx) = mpsc::channel::<crate::kws::WakeEvent>(8);
            let (audio_tx, mut audio_rx) = mpsc::channel::<Vec<u8>>(32);
            let mut status_tick = tokio::time::interval(transport::STATUS_INTERVAL);
            let played = Arc::new(Mutex::new(0usize));
            let last_volume = Arc::new(Mutex::new(None));
            let stop_calls = Arc::new(Mutex::new(0usize));
            let mut audio: Option<Audio> = Some(
                Audio::with_device(Box::new(TestAudioDevice {
                    played_frames: played.clone(),
                    last_volume: last_volume.clone(),
                    stop_calls: stop_calls.clone(),
                }))
                .unwrap(),
            );
            let keepalive = tokio::spawn(async move {
                tokio::time::sleep(Duration::from_secs(30)).await;
                drop(wake_tx);
                drop(audio_tx);
            });
            let pushing = std::sync::atomic::AtomicBool::new(false);
            let (ref_tx, _ref_rx) = mpsc::channel::<[i16; crate::audio::FRAME_SAMPLES]>(16);
            let result = tokio::time::timeout(
                Duration::from_secs(5),
                run_session(
                    &mut transport,
                    &mut session,
                    &mut wake_rx,
                    &mut audio_rx,
                    &mut status_tick,
                    &mut audio,
                    &pushing,
                    &ref_tx,
                ),
            )
            .await;
            assert!(result.is_ok(), "session must end when the fake hub closes");
            keepalive.abort();
            hub.abort();
            let n = *played.lock().unwrap();
            // 收到帧即视为播放开始：playing 应为 true（裸帧不丢弃的另一面证据）。
            assert!(
                session.playing,
                "receiving audio frames must mark the session playing"
            );
            n
        }

        assert_eq!(
            one_round(false).await,
            3,
            "bare audio frames (no start_play) must still be played"
        );
        assert_eq!(
            one_round(true).await,
            3,
            "audio frames after start_play must be played"
        );
    }

    /// P1-N2 / P1-15 端到端（local 模式）：fake Hub 发 start_recording RPC
    /// → 客户端发 audio_start 并把随后推来的 Opus 帧发给 Hub（无 wake_win
    /// 也能推）；stop_recording 后不再推（再推一帧，Hub 应收不到）。
    #[tokio::test]
    async fn local_mode_start_recording_pushes_audio_frames() {
        use futures_util::{SinkExt, StreamExt};
        use std::sync::{Arc, Mutex};
        use std::time::Duration;
        use tokio::net::TcpListener;
        use tokio_tungstenite::tungstenite::Message;

        const TEST_TOKEN: &str = "test-token-0123456789abcdef";

        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let events: Arc<Mutex<Vec<String>>> = Arc::new(Mutex::new(Vec::new()));
        let events_hub = events.clone();
        let hub = tokio::spawn(async move {
            let (stream, _) = listener.accept().await.unwrap();
            let mut ws = tokio_tungstenite::accept_async(stream).await.unwrap();
            let first = ws.next().await.unwrap().unwrap();
            assert!(matches!(first, Message::Text(_)));
            ws.send(Message::Text(
                serde_json::json!({"type": "hello_ok", "device_id": "e2e-device", "room": "客厅"})
                    .to_string()
                    .into(),
            ))
            .await
            .unwrap();
            tokio::time::sleep(Duration::from_millis(200)).await;
            ws.send(Message::Text(
                serde_json::json!({"type": "rpc", "id": "r1", "method": "start_recording", "params": {}})
                    .to_string()
                    .into(),
            ))
            .await
            .unwrap();
            let deadline = tokio::time::sleep(Duration::from_secs(3));
            tokio::pin!(deadline);
            // stop_recording 只发一次（它的 rpc_result 回来时不再重发）。
            let mut stop_sent = false;
            loop {
                tokio::select! {
                    msg = ws.next() => {
                        match msg {
                            Some(Ok(Message::Text(t))) => {
                                let v: serde_json::Value =
                                    serde_json::from_str(&t).unwrap_or(serde_json::Value::Null);
                                let kind = v.get("type").and_then(|x| x.as_str()).unwrap_or("?").to_string();
                                events_hub.lock().unwrap().push(kind.clone());
                                if kind == "rpc_result" && !stop_sent {
                                    stop_sent = true;
                                    // 回完 start_recording 的 rpc_result 1 秒后再发
                                    // stop_recording：留足窗口让第一批音频帧推出去，
                                    // 之后的音频帧不应再被推。
                                    tokio::time::sleep(Duration::from_millis(1000)).await;
                                    let _ = ws.send(Message::Text(
                                        serde_json::json!({"type": "rpc", "id": "r2", "method": "stop_recording", "params": {}})
                                            .to_string()
                                            .into(),
                                    )).await;
                                }
                            }
                            Some(Ok(Message::Binary(_))) => {
                                events_hub.lock().unwrap().push("audio".to_string());
                            }
                            Some(Ok(Message::Ping(p))) => {
                                let _ = ws.send(Message::Pong(p)).await;
                            }
                            Some(Ok(Message::Close(_))) | Some(Err(_)) | None => break,
                            _ => {}
                        }
                    }
                    _ = &mut deadline => break,
                }
            }
            let _ = ws.close(None).await;
        });

        let cfg = config::Config {
            hub_url: format!("ws://{addr}"),
            token: TEST_TOKEN.to_string(),
            tls: false,
            allow_insecure_ws: false,
            kws_mode: "local".to_string(),
            kws_model: "/tmp/no-such-model.onnx".to_string(),
            kws_model_dir: "/tmp/no-such-kws-models".to_string(),
            kws_lib: String::new(),
            ..config::Config::default()
        };
        let mut transport = transport::connect_once(&cfg)
            .await
            .expect("fake hub connect must pass");
        // local 模式会话（streaming=false 起步：无 wake_win、无 recording 时不推流）。
        let mut session = Session::new();
        assert!(!session.pushing_audio());
        let (wake_tx, mut wake_rx) = mpsc::channel::<crate::kws::WakeEvent>(8);
        let (audio_tx, mut audio_rx) = mpsc::channel::<Vec<u8>>(32);
        let mut status_tick = tokio::time::interval(transport::STATUS_INTERVAL);
        let mut audio: Option<Audio> = None;

        let packets = sine_opus_packets(6);
        // 推帧任务：start_recording 到达后（约 300ms）推 3 帧，stop_recording
        // 后（约 1.5s）再推 3 帧——后 3 帧不应到达 Hub。
        let sender = tokio::spawn(async move {
            tokio::time::sleep(Duration::from_millis(400)).await;
            for pkt in packets.iter().take(3) {
                if audio_tx.send(pkt.clone()).await.is_err() {
                    return;
                }
            }
            tokio::time::sleep(Duration::from_millis(1200)).await;
            for pkt in packets.iter().skip(3) {
                if audio_tx.send(pkt.clone()).await.is_err() {
                    return;
                }
            }
            tokio::time::sleep(Duration::from_secs(30)).await;
            drop(wake_tx);
        });
        let pushing = std::sync::atomic::AtomicBool::new(false);
        let (ref_tx, _ref_rx) = mpsc::channel::<[i16; crate::audio::FRAME_SAMPLES]>(16);
        let result = tokio::time::timeout(
            Duration::from_secs(5),
            run_session(
                &mut transport,
                &mut session,
                &mut wake_rx,
                &mut audio_rx,
                &mut status_tick,
                &mut audio,
                &pushing,
                &ref_tx,
            ),
        )
        .await;
        assert!(result.is_ok(), "session must end when the fake hub closes");
        sender.abort();
        hub.abort();
        let recorded = events.lock().unwrap().clone();
        // start_recording 后：audio_start 在首帧音频之前，且恰好 3 帧到达
        // （stop_recording 后的 3 帧必须被拦下）。
        let start_pos = recorded
            .iter()
            .position(|e| e == "audio_start")
            .expect("hub must receive audio_start after start_recording; got: {recorded:?}");
        let audio_count = recorded.iter().filter(|e| *e == "audio").count();
        assert_eq!(
            audio_count, 3,
            "hub must receive exactly the 3 frames pushed while recording; got: {recorded:?}"
        );
        let first_audio_pos = recorded.iter().position(|e| e == "audio").unwrap();
        assert!(
            start_pos < first_audio_pos,
            "audio_start must precede the first audio frame; got: {recorded:?}"
        );
        assert!(
            recorded.iter().any(|e| e == "audio_end"),
            "hub must receive audio_end after stop_recording; got: {recorded:?}"
        );
    }
    #[test]
    fn capture_backoff_delay_grows_then_caps_at_5s() {
        // P2-40：100ms 起步翻倍、上限 5s。
        assert_eq!(
            capture_backoff_delay(0),
            std::time::Duration::from_millis(100)
        );
        assert_eq!(
            capture_backoff_delay(1),
            std::time::Duration::from_millis(200)
        );
        assert_eq!(
            capture_backoff_delay(3),
            std::time::Duration::from_millis(800)
        );
        assert_eq!(
            capture_backoff_delay(6),
            std::time::Duration::from_millis(5000)
        );
        assert_eq!(
            capture_backoff_delay(100),
            std::time::Duration::from_secs(5)
        );
    }

    /// P2-N1 端到端：fake Hub 发 2 帧音频（播出）→ 发 interrupt →
    /// 客户端必须调一次设备的 stop_playback（丢弃 ALSA 缓冲尾音），
    /// 且打断后 200ms 内的在途帧不再播出。真机上 drop/prepare 是否
    /// 真把 80ms 尾音清掉仍需真机确认（BLOCKED），这里验证的是胶水层：
    /// interrupt 确实一路调到了设备的 stop_playback，而不是只清标志。
    #[tokio::test]
    async fn interrupt_stops_playback_via_alsa_drop() {
        use futures_util::{SinkExt, StreamExt};
        use std::sync::{Arc, Mutex};
        use std::time::Duration;
        use tokio::net::TcpListener;
        use tokio_tungstenite::tungstenite::Message;

        const TEST_TOKEN: &str = "test-token-0123456789abcdef";

        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let packets = sine_opus_packets(3);
        let hub = tokio::spawn(async move {
            let (stream, _) = listener.accept().await.unwrap();
            let mut ws = tokio_tungstenite::accept_async(stream).await.unwrap();
            let first = ws.next().await.unwrap().unwrap();
            assert!(matches!(first, Message::Text(_)));
            ws.send(Message::Text(
                serde_json::json!({"type": "hello_ok", "device_id": "e2e-device", "room": "客厅"})
                    .to_string()
                    .into(),
            ))
            .await
            .unwrap();
            tokio::time::sleep(Duration::from_millis(200)).await;
            // 播 2 帧。
            for pkt in packets.iter().take(2) {
                let mut framed = vec![crate::audio::AUDIO_FRAME_TYPE];
                framed.extend_from_slice(pkt);
                ws.send(Message::Binary(framed.into())).await.unwrap();
                tokio::time::sleep(Duration::from_millis(30)).await;
            }
            tokio::time::sleep(Duration::from_millis(200)).await;
            // 打断。
            ws.send(Message::Text(
                serde_json::json!({"type": "interrupt"}).to_string().into(),
            ))
            .await
            .unwrap();
            tokio::time::sleep(Duration::from_millis(100)).await;
            // 打断后 100ms 内的在途帧（200ms 抑制窗内）：不应播出。
            let mut framed = vec![crate::audio::AUDIO_FRAME_TYPE];
            framed.extend_from_slice(&packets[2]);
            ws.send(Message::Binary(framed.into())).await.unwrap();
            tokio::time::sleep(Duration::from_millis(300)).await;
            let _ = ws.close(None).await;
        });

        let cfg = config::Config {
            hub_url: format!("ws://{addr}"),
            token: TEST_TOKEN.to_string(),
            tls: false,
            allow_insecure_ws: false,
            kws_mode: "server".to_string(),
            kws_model: "/tmp/no-such-model.onnx".to_string(),
            kws_model_dir: "/tmp/no-such-kws-models".to_string(),
            kws_lib: String::new(),
            ..config::Config::default()
        };
        let mut transport = transport::connect_once(&cfg)
            .await
            .expect("fake hub connect must pass");
        let mut session = Session::new_server();
        let (wake_tx, mut wake_rx) = mpsc::channel::<crate::kws::WakeEvent>(8);
        let (audio_tx, mut audio_rx) = mpsc::channel::<Vec<u8>>(32);
        let mut status_tick = tokio::time::interval(transport::STATUS_INTERVAL);
        let played = Arc::new(Mutex::new(0usize));
        let last_volume = Arc::new(Mutex::new(None));
        let stop_calls = Arc::new(Mutex::new(0usize));
        let mut audio: Option<Audio> = Some(
            Audio::with_device(Box::new(TestAudioDevice {
                played_frames: played.clone(),
                last_volume: last_volume.clone(),
                stop_calls: stop_calls.clone(),
            }))
            .unwrap(),
        );
        let keepalive = tokio::spawn(async move {
            tokio::time::sleep(Duration::from_secs(30)).await;
            drop(wake_tx);
            drop(audio_tx);
        });
        let pushing = std::sync::atomic::AtomicBool::new(false);
        let (ref_tx, _ref_rx) = mpsc::channel::<[i16; crate::audio::FRAME_SAMPLES]>(16);
        let result = tokio::time::timeout(
            Duration::from_secs(5),
            run_session(
                &mut transport,
                &mut session,
                &mut wake_rx,
                &mut audio_rx,
                &mut status_tick,
                &mut audio,
                &pushing,
                &ref_tx,
            ),
        )
        .await;
        assert!(result.is_ok(), "session must end when the fake hub closes");
        keepalive.abort();
        hub.abort();
        assert_eq!(
            *played.lock().unwrap(),
            2,
            "only the 2 pre-interrupt frames must be played; the in-flight frame after interrupt must be suppressed"
        );
        assert_eq!(
            *stop_calls.lock().unwrap(),
            1,
            "interrupt must call the audio device stop_playback exactly once (not just clear the playing flag)"
        );
        assert!(!session.playing, "interrupt must clear the playing flag");
    }
}
