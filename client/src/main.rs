//! open-xiaoai-client — 音箱端薄客户端（Phase 1）。
//!
//! 职责只有五件，多一件都不做：
//!   1. 录音：ALSA 采集 16kHz 单声道 PCM → Opus 编码 → 发往 Hub
//!      （ALSA 已接线，见 audio.rs：无硬件时 capture 返回明确错误，录音任务
//!      记日志退出、主循环继续跑传输；真机实测仍 BLOCKED）
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
use tokio::sync::mpsc;
use tracing::{info, warn};

mod audio;
mod config;
mod kws;
#[cfg(feature = "sherpa-kws")]
mod sherpa;
mod transport;

use audio::Audio;
use kws::Kws;
use transport::{HubMsg, RawFrame, Transport};

/// 会话状态机：Hub 消息 → 状态变更 + 待执行动作。
/// 与网络 IO 分离，可单测；main.rs 的循环只负责执行动作。
#[derive(Debug, Default)]
pub struct Session {
    /// 仲裁赢了且正在推语音流；server 模式下会话一建立即为 true（持续推流）。
    pub streaming: bool,
    /// 正在放音（TTS/对讲）。
    pub playing: bool,
    /// 音量 0–100。
    pub volume: u8,
    pub device_id: Option<String>,
    pub room: Option<String>,
    /// server KWS 模式：持续推流，不等仲裁；wake_win/wake_lose 不改变推流状态。
    pub server_mode: bool,
}

/// Session 对一条 Hub 消息的反应：需要 main 循环代发/代做的动作。
#[derive(Debug, PartialEq)]
pub enum SessionAction {
    /// 发 audio_start，开始推语音流。
    SendAudioStart,
    /// 发 audio_end，结束语音流。
    SendAudioEnd,
    /// 立即停播（收到 interrupt）。
    StopPlayback,
    /// 回应 rpc：(id, ok, error)。
    SendRpcResult {
        id: String,
        ok: bool,
        error: Option<String>,
    },
}

impl Session {
    pub fn new() -> Self {
        Self {
            streaming: false,
            playing: false,
            volume: 50,
            device_id: None,
            room: None,
            server_mode: false,
        }
    }

    /// server KWS 模式的会话：建立即推流（`streaming = true`），
    /// `run_session` 开头会补发一次 `audio_start`。
    pub fn new_server() -> Self {
        Self {
            streaming: true,
            playing: false,
            volume: 50,
            device_id: None,
            room: None,
            server_mode: true,
        }
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
            HubMsg::WakeWin => {
                self.streaming = true;
                if self.server_mode {
                    vec![]
                } else {
                    vec![SessionAction::SendAudioStart]
                }
            }
            // 仲裁输了：静默，不理这次唤醒；若正在推流则停。
            // server 模式：持续推流不停（一次 wake_lose 只是边车对某次
            // 唤醒的裁决，不停整条音频流）。
            HubMsg::WakeLose => {
                if self.server_mode {
                    return vec![];
                }
                let mut actions = vec![];
                if self.streaming {
                    self.streaming = false;
                    actions.push(SessionAction::SendAudioEnd);
                } else {
                    self.streaming = false;
                }
                actions
            }
            // 打断：立即停播正在放的 TTS。
            // server 模式：只停播，不停持续推流（不断 audio 流）。
            HubMsg::Interrupt => {
                self.playing = false;
                if self.server_mode {
                    return vec![SessionAction::StopPlayback];
                }
                if self.streaming {
                    self.streaming = false;
                    vec![SessionAction::StopPlayback, SessionAction::SendAudioEnd]
                } else {
                    vec![SessionAction::StopPlayback]
                }
            }
            HubMsg::Rpc { id, method, params } => {
                let (ok, error) = self.handle_rpc(method, params);
                vec![SessionAction::SendRpcResult {
                    id: id.clone(),
                    ok,
                    error,
                }]
            }
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
    fn handle_rpc(&mut self, method: &str, params: &serde_json::Value) -> (bool, Option<String>) {
        match method {
            "start_play" => {
                self.playing = true;
                info!("rpc start_play");
                (true, None)
            }
            "stop_play" => {
                self.playing = false;
                info!("rpc stop_play");
                (true, None)
            }
            "start_recording" => {
                info!("rpc start_recording (intercom: hub will forward audio frames)");
                (true, None)
            }
            "stop_recording" => {
                info!("rpc stop_recording");
                (true, None)
            }
            "play_text" => {
                let text = params
                    .get("text")
                    .and_then(|t| t.as_str())
                    .unwrap_or("")
                    .to_string();
                // TTS 合成在 Hub 侧；Client 只负责放下发的音频。
                // Phase 1：记日志 + 置 playing，待 ALSA 接线后真正放音。
                self.playing = true;
                info!("rpc play_text: {text}");
                (true, None)
            }
            "set_volume" => {
                let v = params
                    .get("volume")
                    .and_then(|x| x.as_u64())
                    .unwrap_or(50)
                    .clamp(0, 100) as u8;
                self.volume = v;
                info!("rpc set_volume: {v}");
                (true, None)
            }
            other => {
                warn!("unknown rpc method: {other}");
                (false, Some(format!("unknown rpc method: {other}")))
            }
        }
    }
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
        warn!("tls=false: plain ws (local test only, never for production)");
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

    // 录音任务：采集 PCM 帧 →（local 模式）喂 KWS（算 RMS、命中发 WakeEvent）、
    // （server 模式）跳过本地 KWS → 编码成 Opus 帧发给主循环（local 模式只在
    // 仲裁赢了之后才往 Hub 推；server 模式持续推）。
    // 无硬件时 Audio::capture_pcm 返回明确错误，任务记一条日志后退出，
    // 主循环继续跑传输（不崩溃、不造假音频）。
    let (wake_tx, mut wake_rx) = mpsc::channel::<kws::WakeEvent>(8);
    let (audio_tx, mut audio_rx) = mpsc::channel::<Vec<u8>>(32);
    let capture_handle = tokio::spawn(async move {
        let mut audio = match Audio::open() {
            Ok(a) => a,
            Err(e) => {
                warn!("audio open failed, capture task exiting: {e}");
                return;
            }
        };
        let mut kws = kws;
        loop {
            match audio.capture_pcm() {
                Ok(pcm) => {
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
                    if let Ok(opus) = audio.encode_pcm(&pcm) {
                        if audio_tx.send(opus).await.is_err() {
                            return;
                        }
                    }
                }
                Err(e) => {
                    warn!("capture unavailable, capture task exiting: {e}");
                    return;
                }
            }
        }
    });

    // 主循环：断线 → 指数退避重连（transport::connect_with_retry）。
    loop {
        let mut session = if cfg.is_server_kws_mode() {
            Session::new_server()
        } else {
            Session::new()
        };
        let mut transport = match transport::connect_with_retry(&cfg).await {
            Ok(t) => t,
            Err(e) => {
                // connect_with_retry 原则上不返回 Err（无限重试）；防御性分支。
                warn!("connect_with_retry gave up: {e}");
                tokio::time::sleep(std::time::Duration::from_secs(1)).await;
                continue;
            }
        };
        session.device_id = transport.device_id.clone();
        session.room = transport.room.clone();
        info!("session started");

        let mut status_tick = tokio::time::interval(transport::STATUS_INTERVAL);
        // 播放任务用的 Opus 解码由主循环内的 audio 负责。
        let mut audio = match Audio::open() {
            Ok(a) => Some(a),
            Err(e) => {
                warn!("audio unavailable in session: {e}");
                None
            }
        };

        // 会话循环：Hub 消息 / KWS 唤醒 / 30s 状态上报 / Ctrl-C。
        let session_result = run_session(
            &mut transport,
            &mut session,
            &mut wake_rx,
            &mut audio_rx,
            &mut status_tick,
            &mut audio,
        )
        .await;

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
async fn run_session(
    transport: &mut Transport,
    session: &mut Session,
    wake_rx: &mut mpsc::Receiver<kws::WakeEvent>,
    audio_rx: &mut mpsc::Receiver<Vec<u8>>,
    status_tick: &mut tokio::time::Interval,
    audio: &mut Option<Audio>,
) -> SessionEnd {
    let mut wake_alive = true;
    let mut audio_alive = true;
    // server 模式：会话一建立就发 audio_start，开始持续推流（不等 wake_win）。
    if session.server_mode {
        session.streaming = true;
        if let Err(e) = transport.send_audio_start().await {
            warn!("send audio_start (server KWS mode) failed: {e}");
            return SessionEnd::Disconnected;
        }
    }
    loop {
        tokio::select! {
            // Hub 下行：文本消息 + 二进制音频帧。
            raw = transport.next_raw() => {
                match raw {
                    Ok(Some(RawFrame::Hub(msg))) => {
                        for action in session.on_hub_msg(&msg) {
                            if let Err(e) = apply_action(transport, action).await {
                                warn!("failed to apply session action: {e}");
                                return SessionEnd::Disconnected;
                            }
                        }
                    }
                    Ok(Some(RawFrame::Audio(opus))) => {
                        // 对讲/广播下发的语音帧：解码后写 ALSA 放音。
                        if session.playing {
                            match audio.as_mut() {
                                Some(a) => match a.play_frame(&opus) {
                                    Ok(pcm) => {
                                        info!("played {}-sample frame via ALSA", pcm.len());
                                    }
                                    Err(e) => warn!("playback frame failed: {e}"),
                                },
                                None => warn!("audio unavailable, dropping playback frame"),
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
            // 录音任务推来的 Opus 帧：只有仲裁赢了（streaming）才往 Hub 推；
            // server 模式下 streaming 恒为 true，即持续推流。
            frame = audio_rx.recv(), if audio_alive => {
                match frame {
                    Some(opus) => {
                        if session.streaming {
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
            // 30s 状态上报。
            _ = status_tick.tick() => {
                let version = env!("CARGO_PKG_VERSION");
                if let Err(e) = transport.send_status(session.volume, session.playing, version).await {
                    warn!("send status failed: {e}");
                    return SessionEnd::Disconnected;
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
async fn apply_action(transport: &mut Transport, action: SessionAction) -> Result<()> {
    match action {
        SessionAction::SendAudioStart => transport.send_audio_start().await,
        SessionAction::SendAudioEnd => transport.send_audio_end().await,
        SessionAction::StopPlayback => {
            info!("interrupt: stopping playback immediately");
            Ok(())
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
        let actions = s.on_hub_msg(&HubMsg::Rpc {
            id: "2".to_string(),
            method: "set_volume".to_string(),
            params: serde_json::json!({"volume": 80}),
        });
        assert_eq!(s.volume, 80);
        assert_eq!(
            actions,
            vec![SessionAction::SendRpcResult {
                id: "2".to_string(),
                ok: true,
                error: None
            }]
        );
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
        let result = tokio::time::timeout(
            Duration::from_secs(5),
            run_session(
                &mut transport,
                &mut session,
                &mut wake_rx,
                &mut audio_rx,
                &mut status_tick,
                &mut audio,
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
}
