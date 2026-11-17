//! 传输：与 Hub 的 WebSocket 连接（Phase 1）。
//!
//! 按 docs/PROTOCOL.md（v1 冻结）：
//!   - wss 连接 Hub :4399，`hello` 带 token 鉴权；鉴权失败（hello_reject）即关闭并报错；
//!   - Hub 自签证书：用 client.toml 的 `hub_ca_fingerprint`（SHA-256）校验；
//!     未配置指纹时跳过校验并打警告（仅测试环境可接受）；
//!   - 断线后指数退避重连：首 100ms，上限 1s，30s 内恢复；
//!   - `tls: false` 时用纯 ws（仅本地测试）。

use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use anyhow::{anyhow, bail, Context, Result};
use futures_util::{SinkExt, StreamExt};
use rustls::client::danger::{HandshakeSignatureValid, ServerCertVerified, ServerCertVerifier};
use rustls::pki_types::pem::PemObject;
use rustls::pki_types::{CertificateDer, PrivateKeyDer, ServerName, UnixTime};
use rustls::{ClientConfig, DigitallySignedStruct, Error as RustlsError, SignatureScheme};
use serde::Serialize;
use sha2::{Digest, Sha256};
use tokio::net::TcpStream;
use tokio_tungstenite::tungstenite::Message;
use tokio_tungstenite::{
    connect_async, connect_async_tls_with_config, Connector, MaybeTlsStream, WebSocketStream,
};
use tracing::{info, warn};

use crate::audio::{unwrap_audio_frame, wrap_audio_frame};
use crate::config::Config;

/// hello 握手等待上限。
pub const HELLO_TIMEOUT: Duration = Duration::from_secs(5);
/// 定期状态上报间隔（协议：每 30s）。
pub const STATUS_INTERVAL: Duration = Duration::from_secs(30);
/// 重连退避：首次 100ms。
pub const BACKOFF_INITIAL_MS: u64 = 100;
/// 重连退避上限：1s。
pub const BACKOFF_MAX_MS: u64 = 1000;

/// 指数退避：第 attempt 次重试（从 0 计）前等待的时长，100ms 起步翻倍，上限 1s。
pub fn backoff_delay(attempt: u32) -> Duration {
    let ms = BACKOFF_INITIAL_MS.saturating_mul(1u64 << attempt.min(10));
    Duration::from_millis(ms.min(BACKOFF_MAX_MS))
}

/// 一段 DER 证书的 SHA-256 指纹（小写 hex，不带冒号）。
pub fn sha256_fingerprint_hex(der: &[u8]) -> String {
    let mut hasher = Sha256::new();
    hasher.update(der);
    hex_encode(&hasher.finalize())
}

fn hex_encode(bytes: &[u8]) -> String {
    let mut s = String::with_capacity(bytes.len() * 2);
    for b in bytes {
        s.push_str(&format!("{b:02x}"));
    }
    s
}

/// DER 证书指纹是否与期望值（已归一化的小写 hex）匹配。
pub fn fingerprint_matches(der: &[u8], expected_normalized: &str) -> bool {
    sha256_fingerprint_hex(der) == expected_normalized
}

/// 只校验指纹的 rustls 证书验证器：Hub 用自签证书，链式校验走不通，
/// 改为校验叶子证书的 SHA-256 指纹（与 client.toml 的 hub_ca_fingerprint 比对）。
/// TLS 签名本身仍真实验证：用 webpki（ring 后端）按证书公钥验签名，
/// 只跳过信任链，不跳过签名。
#[derive(Debug)]
struct FingerprintVerifier {
    expected: String,
}

impl FingerprintVerifier {
    fn new(expected: String) -> Self {
        Self { expected }
    }

    /// 按证书公钥验证一条 TLS 签名（rustls 回调 verify_tls12/13_signature 的公共逻辑）。
    fn verify_signature(
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, RustlsError> {
        use rustls::CertificateError;
        let alg: &dyn rustls::pki_types::SignatureVerificationAlgorithm = match dss.scheme {
            SignatureScheme::ECDSA_NISTP256_SHA256 => webpki::ring::ECDSA_P256_SHA256,
            SignatureScheme::ECDSA_NISTP384_SHA384 => webpki::ring::ECDSA_P384_SHA384,
            SignatureScheme::ED25519 => webpki::ring::ED25519,
            SignatureScheme::RSA_PKCS1_SHA256 => webpki::ring::RSA_PKCS1_2048_8192_SHA256,
            SignatureScheme::RSA_PKCS1_SHA384 => webpki::ring::RSA_PKCS1_2048_8192_SHA384,
            SignatureScheme::RSA_PKCS1_SHA512 => webpki::ring::RSA_PKCS1_2048_8192_SHA512,
            SignatureScheme::RSA_PSS_SHA256 => webpki::ring::RSA_PSS_2048_8192_SHA256_LEGACY_KEY,
            SignatureScheme::RSA_PSS_SHA384 => webpki::ring::RSA_PSS_2048_8192_SHA384_LEGACY_KEY,
            SignatureScheme::RSA_PSS_SHA512 => webpki::ring::RSA_PSS_2048_8192_SHA512_LEGACY_KEY,
            _ => {
                return Err(RustlsError::General(format!(
                    "unsupported signature scheme: {:?}",
                    dss.scheme
                )));
            }
        };
        let end_entity = webpki::EndEntityCert::try_from(cert)
            .map_err(|_| RustlsError::InvalidCertificate(CertificateError::BadEncoding))?;
        end_entity
            .verify_signature(alg, message, dss.signature())
            .map_err(|_| RustlsError::InvalidCertificate(CertificateError::BadSignature))?;
        Ok(HandshakeSignatureValid::assertion())
    }
}

impl ServerCertVerifier for FingerprintVerifier {
    fn verify_server_cert(
        &self,
        end_entity: &CertificateDer<'_>,
        _intermediates: &[CertificateDer<'_>],
        _server_name: &ServerName<'_>,
        _ocsp_response: &[u8],
        _now: UnixTime,
    ) -> Result<ServerCertVerified, RustlsError> {
        if fingerprint_matches(end_entity.as_ref(), &self.expected) {
            Ok(ServerCertVerified::assertion())
        } else {
            Err(RustlsError::InvalidCertificate(
                rustls::CertificateError::UnknownIssuer,
            ))
        }
    }

    fn verify_tls12_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, RustlsError> {
        Self::verify_signature(message, cert, dss)
    }

    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, RustlsError> {
        Self::verify_signature(message, cert, dss)
    }

    fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
        vec![
            SignatureScheme::ECDSA_NISTP256_SHA256,
            SignatureScheme::ECDSA_NISTP384_SHA384,
            SignatureScheme::ED25519,
            SignatureScheme::RSA_PKCS1_SHA256,
            SignatureScheme::RSA_PKCS1_SHA384,
            SignatureScheme::RSA_PKCS1_SHA512,
            SignatureScheme::RSA_PSS_SHA256,
            SignatureScheme::RSA_PSS_SHA384,
            SignatureScheme::RSA_PSS_SHA512,
        ]
    }
}

fn crypto_provider() -> Arc<rustls::crypto::CryptoProvider> {
    Arc::new(rustls::crypto::ring::default_provider())
}

// ---------------------------------------------------------------------------
// 弱 Token（P0-3，与 hub/internal/transport.ValidateToken 同规则）
// ---------------------------------------------------------------------------

/// 弱 Token：长度 <16 字节，或等于公开/常见弱值（大小写不敏感）。
pub fn is_weak_token(token: &str) -> bool {
    if token.len() < 16 {
        return true;
    }
    matches!(
        token.to_lowercase().trim(),
        "change_me"
            | "changeme"
            | "secret"
            | "password"
            | "123456"
            | "12345678"
            | "123456789"
            | "1234567890"
            | "admin"
            | "letmein"
            | "qwerty"
            | "open-xiaoai"
            | "open_xiaoai"
            | "xiaoai"
            | "test"
            | "token"
            | "0000000000000000"
            | "aaaaaaaaaaaaaaaa"
    )
}

/// 弱 Token 时返回 Err（connect_once 在建连前调，拒绝用弱 Token 上线）。
pub fn validate_token(token: &str) -> Result<()> {
    if is_weak_token(token) {
        bail!(
            "hub token is weak (must be >= 16 chars and not a well-known value); generate one: openssl rand -hex 16"
        );
    }
    Ok(())
}

// ---------------------------------------------------------------------------
// mTLS 客户端身份（P0-3）：CA 签发的客户端证书 + 私钥
// ---------------------------------------------------------------------------

/// 客户端身份文件位置（按优先级）：
///   1. 环境变量 OPEN_XIAOAI_CLIENT_CERT / OPEN_XIAOAI_CLIENT_KEY（宿主机调试）；
///   2. /etc/open-xiaoai/tls/client.crt + client.key（固件补丁 --cert-dir 装入）；
///   3. /data/open-xiaoai/tls/client.crt + client.key（OTA/手动下发）。
/// 两文件必须同时存在才返回 Some；否则返回 None（无 mTLS 身份，
/// 要求客户端证书的 Hub 会拒绝连接，见 build_tls_config 的警告）。
pub fn client_identity_paths() -> Option<(PathBuf, PathBuf)> {
    if let (Ok(cert), Ok(key)) = (
        std::env::var("OPEN_XIAOAI_CLIENT_CERT"),
        std::env::var("OPEN_XIAOAI_CLIENT_KEY"),
    ) {
        let (c, k) = (PathBuf::from(cert), PathBuf::from(key));
        if c.is_file() && k.is_file() {
            return Some((c, k));
        }
    }
    for dir in ["/etc/open-xiaoai/tls", "/data/open-xiaoai/tls"] {
        let cert = Path::new(dir).join("client.crt");
        let key = Path::new(dir).join("client.key");
        if cert.is_file() && key.is_file() {
            return Some((cert, key));
        }
    }
    None
}

/// 从 PEM 文件读客户端身份（证书 + 私钥）。
/// 私钥接受 PKCS#8（openssl genpkey / gen-certs.sh）与 PKCS#1
///（Go 的 tlscert 生成）两种格式。
pub fn load_client_identity(
    cert_path: &Path,
    key_path: &Path,
) -> Result<(Vec<CertificateDer<'static>>, PrivateKeyDer<'static>)> {
    let cert_pem = std::fs::read(cert_path)
        .with_context(|| format!("read client cert {}", cert_path.display()))?;
    let key_pem = std::fs::read(key_path)
        .with_context(|| format!("read client key {}", key_path.display()))?;
    let cert = CertificateDer::from_pem_slice(&cert_pem)
        .map_err(|e| anyhow!("parse client cert {}: {e:?}", cert_path.display()))?;
    let key = PrivateKeyDer::from_pem_slice(&key_pem)
        .map_err(|e| anyhow!("parse client key {}: {e:?}", key_path.display()))?;
    Ok((vec![cert], key))
}

/// 按配置构造 rustls ClientConfig：
///   - 有合法指纹 → 指纹验证器（自签证书可用）；
///   - 无指纹 → 系统根证书验证 + 警告（测试环境；生产 Hub 自签证书会验不过，
///     部署时必须配指纹）；
///   - 指纹格式非法 → 直接报错，拒绝用错指纹连接。
/// mTLS：找到客户端证书时带证书连接（Hub 开 RequireAndVerifyClientCert
/// 才会放行）；找不到时不带证书并打警告（仅 Token 鉴权，见 P0-3）。
pub fn build_tls_config(cfg: &Config) -> Result<Arc<ClientConfig>> {
    build_tls_config_with_identity(cfg, client_identity_paths())
}

/// build_tls_config 的可测试版本：identity 由调用方显式给
///（None = 无客户端证书；Some((cert, key)) = 用该身份连接）。
pub fn build_tls_config_with_identity(
    cfg: &Config,
    identity: Option<(PathBuf, PathBuf)>,
) -> Result<Arc<ClientConfig>> {
    if cfg.has_malformed_fingerprint() {
        bail!(
            "hub_ca_fingerprint malformed (want 64 hex chars, colons/spaces allowed): {:?}",
            cfg.hub_ca_fingerprint
        );
    }
    // 客户端身份：有文件才读，读失败直接报错（不静默降级为无证书）。
    let client_identity = match &identity {
        Some((cert_path, key_path)) => Some(load_client_identity(cert_path, key_path)?),
        None => None,
    };
    let builder = ClientConfig::builder_with_provider(crypto_provider())
        .with_safe_default_protocol_versions()
        .map_err(|e| anyhow!("tls protocol versions: {e}"))?;
    // 带/不带客户端证书的两条收尾（rustls：with_client_auth_cert 返回
    // Result，with_no_client_auth 直接返回 ClientConfig）。
    macro_rules! finish {
        ($b:expr) => {
            match client_identity {
                Some((certs, key)) => $b
                    .with_client_auth_cert(certs, key)
                    .map_err(|e| anyhow!("client auth cert: {e}"))?,
                None => {
                    warn!("tls: no mTLS client certificate found (looked in $OPEN_XIAOAI_CLIENT_CERT/KEY, /etc/open-xiaoai/tls, /data/open-xiaoai/tls) — a hub with RequireAndVerifyClientCert will reject this client");
                    $b.with_no_client_auth()
                }
            }
        };
    }
    let config = match cfg.normalized_fingerprint() {
        Some(fp) => {
            info!("tls: verifying hub cert by SHA-256 fingerprint");
            finish!(builder
                .dangerous()
                .with_custom_certificate_verifier(Arc::new(FingerprintVerifier::new(fp))))
        }
        None => {
            warn!(
                "tls: no hub_ca_fingerprint configured — skipping fingerprint check (test only!)"
            );
            let mut roots = rustls::RootCertStore::empty();
            let native = rustls_native_certs::load_native_certs();
            if !native.errors.is_empty() {
                warn!("some native certs failed to load: {:?}", native.errors);
            }
            for cert in native.certs {
                roots
                    .add(cert)
                    .map_err(|e| anyhow!("add native cert: {e}"))?;
            }
            finish!(builder.with_root_certificates(roots))
        }
    };
    Ok(Arc::new(config))
}

// ---------------------------------------------------------------------------
// 协议消息
// ---------------------------------------------------------------------------

/// Hub → Client 的消息（PROTOCOL.md v1）。
#[derive(Debug, Clone, PartialEq)]
pub enum HubMsg {
    HelloOk {
        device_id: String,
        room: Option<String>,
    },
    WakeWin,
    WakeLose,
    Rpc {
        id: String,
        method: String,
        params: serde_json::Value,
    },
    Interrupt,
    Ota {
        version: String,
        url: String,
        sha256: String,
    },
    RoomOk {
        room: String,
    },
    /// 未知类型：保留原文供日志，不中断连接。
    Unknown(String),
}

/// Client → Hub 的 hello（连接后第一条消息）。
#[derive(Debug, Serialize)]
struct HelloMsg<'a> {
    #[serde(rename = "type")]
    kind: &'a str,
    model: &'a str,
    ram_mb: u32,
    version: &'a str,
    token: &'a str,
    #[serde(skip_serializing_if = "Option::is_none")]
    room: Option<&'a str>,
}

fn msg_type(v: &serde_json::Value) -> &str {
    v.get("type").and_then(|t| t.as_str()).unwrap_or("")
}

/// 解析一条 Hub 文本帧。返回 Err = 无法解析的 JSON（调用方记日志后忽略）。
pub fn parse_hub_text(text: &str) -> Result<HubMsg> {
    let v: serde_json::Value = serde_json::from_str(text).context("hub message is not JSON")?;
    Ok(match msg_type(&v) {
        "hello_ok" => HubMsg::HelloOk {
            device_id: v
                .get("device_id")
                .and_then(|d| d.as_str())
                .unwrap_or("")
                .to_string(),
            room: v
                .get("room")
                .and_then(|r| r.as_str())
                .map(|s| s.to_string()),
        },
        "wake_win" => HubMsg::WakeWin,
        "wake_lose" => HubMsg::WakeLose,
        "rpc" => HubMsg::Rpc {
            id: v
                .get("id")
                .and_then(|i| i.as_str())
                .unwrap_or("")
                .to_string(),
            method: v
                .get("method")
                .and_then(|m| m.as_str())
                .unwrap_or("")
                .to_string(),
            params: v.get("params").cloned().unwrap_or(serde_json::Value::Null),
        },
        "interrupt" => HubMsg::Interrupt,
        "ota" => HubMsg::Ota {
            version: v
                .get("version")
                .and_then(|x| x.as_str())
                .unwrap_or("")
                .to_string(),
            url: v
                .get("url")
                .and_then(|x| x.as_str())
                .unwrap_or("")
                .to_string(),
            sha256: v
                .get("sha256")
                .and_then(|x| x.as_str())
                .unwrap_or("")
                .to_string(),
        },
        "room_ok" => HubMsg::RoomOk {
            room: v
                .get("room")
                .and_then(|r| r.as_str())
                .unwrap_or("")
                .to_string(),
        },
        other => HubMsg::Unknown(other.to_string()),
    })
}

fn json_text(kind: &str, body: serde_json::Value) -> String {
    let mut obj = serde_json::Map::new();
    obj.insert(
        "type".to_string(),
        serde_json::Value::String(kind.to_string()),
    );
    if let serde_json::Value::Object(map) = body {
        obj.extend(map);
    }
    serde_json::Value::Object(obj).to_string()
}

// ---------------------------------------------------------------------------
// Transport
// ---------------------------------------------------------------------------

pub struct Transport {
    ws: WebSocketStream<MaybeTlsStream<TcpStream>>,
    pub device_id: Option<String>,
    pub room: Option<String>,
}

impl Transport {
    /// 发送一条 JSON 文本消息。
    pub async fn send_json(&mut self, text: &str) -> Result<()> {
        self.ws
            .send(Message::Text(text.to_string().into()))
            .await
            .map_err(|e| anyhow!("ws send failed: {e}"))?;
        Ok(())
    }

    /// 发送一帧 Opus 音频（二进制帧，首字节 0x01）。
    pub async fn send_audio_frame(&mut self, opus: &[u8]) -> Result<()> {
        self.ws
            .send(Message::Binary(wrap_audio_frame(opus).into()))
            .await
            .map_err(|e| anyhow!("ws send audio frame failed: {e}"))?;
        Ok(())
    }

    /// 上报唤醒事件（KWS 命中后）。
    pub async fn send_wake(&mut self, wake_word: &str, rms_db: f32) -> Result<()> {
        let ts_ms = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_millis() as u64)
            .unwrap_or(0);
        let text = json_text(
            "wake",
            serde_json::json!({ "wake_word": wake_word, "rms_db": rms_db, "ts_ms": ts_ms }),
        );
        self.send_json(&text).await
    }

    /// 开始推语音流（仲裁赢了之后）。
    pub async fn send_audio_start(&mut self) -> Result<()> {
        self.send_json(&json_text("audio_start", serde_json::json!({})))
            .await
    }

    /// 一句话说完（VAD 静音后）。
    pub async fn send_audio_end(&mut self) -> Result<()> {
        self.send_json(&json_text("audio_end", serde_json::json!({})))
            .await
    }

    /// 定期状态上报（每 30s）。
    pub async fn send_status(&mut self, volume: u8, playing: bool, version: &str) -> Result<()> {
        let text = json_text(
            "status",
            serde_json::json!({ "volume": volume, "playing": playing, "version": version }),
        );
        self.send_json(&text).await
    }

    /// 语音认领房间。
    #[allow(dead_code)]
    pub async fn send_room_claim(&mut self, room: &str) -> Result<()> {
        let text = json_text("room_claim", serde_json::json!({ "room": room }));
        self.send_json(&text).await
    }

    /// 回应 Hub 的 rpc。
    pub async fn send_rpc_result(&mut self, id: &str, ok: bool, error: Option<&str>) -> Result<()> {
        let mut body = serde_json::json!({ "id": id, "ok": ok });
        if let Some(e) = error {
            body["error"] = serde_json::Value::String(e.to_string());
        }
        self.send_json(&json_text("rpc_result", body)).await
    }

    /// 收下一条 Hub 消息。Ok(None) = Hub 正常关闭；Err = 断线/协议错误。
    /// 二进制音频帧不走本函数：调用方用 `next_raw`。
    /// （主循环用 next_raw；本函数供测试与后续只关心文本消息的调用方用。）
    #[allow(dead_code)]
    pub async fn next_msg(&mut self) -> Result<Option<HubMsg>> {
        loop {
            match self.ws.next().await {
                Some(Ok(Message::Text(text))) => {
                    let msg = parse_hub_text(&text)?;
                    if let HubMsg::HelloOk { .. } = msg {
                        // 握手后不应再出现 hello_ok，记日志后继续等。
                        warn!("unexpected second hello_ok, ignoring");
                        continue;
                    }
                    return Ok(Some(msg));
                }
                Some(Ok(Message::Binary(data))) => {
                    if unwrap_audio_frame(&data).is_some() {
                        // Hub 下发的音频帧（对讲/广播）：调用方应直接处理。
                        // 这里以 Unknown 标记并继续，避免丢弃后无声。
                        warn!(
                            "received audio binary frame ({} bytes), not consumed by next_msg",
                            data.len()
                        );
                        continue;
                    }
                    warn!("received unknown binary frame, ignoring");
                    continue;
                }
                Some(Ok(Message::Ping(payload))) => {
                    self.ws
                        .send(Message::Pong(payload))
                        .await
                        .map_err(|e| anyhow!("pong failed: {e}"))?;
                    continue;
                }
                Some(Ok(Message::Pong(_))) => continue,
                Some(Ok(Message::Close(_))) => return Ok(None),
                Some(Ok(Message::Frame(_))) => continue,
                Some(Err(e)) => return Err(anyhow!("ws recv failed: {e}")),
                None => return Ok(None),
            }
        }
    }

    /// 收下一条原始帧：文本返回解析后的 HubMsg，二进制音频帧返回 Opus 数据。
    /// 供 main.rs 的对讲/播放路径用。
    pub async fn next_raw(&mut self) -> Result<Option<RawFrame>> {
        loop {
            match self.ws.next().await {
                Some(Ok(Message::Text(text))) => {
                    return Ok(Some(RawFrame::Hub(parse_hub_text(&text)?)));
                }
                Some(Ok(Message::Binary(data))) => {
                    if let Some(opus) = unwrap_audio_frame(&data) {
                        return Ok(Some(RawFrame::Audio(opus.to_vec())));
                    }
                    warn!("received unknown binary frame, ignoring");
                    continue;
                }
                Some(Ok(Message::Ping(payload))) => {
                    self.ws.send(Message::Pong(payload)).await.ok();
                    continue;
                }
                Some(Ok(Message::Pong(_))) => continue,
                Some(Ok(Message::Close(_))) => return Ok(None),
                Some(Ok(Message::Frame(_))) => continue,
                Some(Err(e)) => return Err(anyhow!("ws recv failed: {e}")),
                None => return Ok(None),
            }
        }
    }

    /// 优雅关闭。
    #[allow(dead_code)]
    pub async fn close(mut self) {
        let _ = self.ws.close(None).await;
    }
}

/// next_raw 返回的帧。
#[derive(Debug)]
pub enum RawFrame {
    Hub(HubMsg),
    Audio(Vec<u8>),
}

/// 单次连接 + hello 握手（不重试）。鉴权失败（hello_reject）立即关闭并报错。
/// 弱 Token（<16 位或常见值）在建连前直接拒绝，不发 hello。
pub async fn connect_once(cfg: &Config) -> Result<Transport> {
    validate_token(&cfg.token)?;
    let url = if cfg.tls {
        if let Some(rest) = cfg.hub_url.strip_prefix("ws://") {
            warn!("tls=true but hub_url is ws:// — upgrading to wss://");
            format!("wss://{rest}")
        } else {
            cfg.hub_url.clone()
        }
    } else {
        cfg.hub_url.clone()
    };
    info!("connecting to hub: {url} (tls={})", cfg.tls);

    let (mut ws, _resp) = if cfg.tls {
        let tls_config = build_tls_config(cfg)?;
        connect_async_tls_with_config(
            url.as_str(),
            None,
            false,
            Some(Connector::Rustls(tls_config)),
        )
        .await
        .map_err(|e| anyhow!("wss connect failed: {e}"))?
    } else {
        connect_async(url.as_str())
            .await
            .map_err(|e| anyhow!("ws connect failed: {e}"))?
    };

    // hello：连接后第一条消息，带 token 鉴权。
    let hello = HelloMsg {
        kind: "hello",
        model: &cfg.model,
        ram_mb: cfg.ram_mb,
        version: env!("CARGO_PKG_VERSION"),
        token: &cfg.token,
        room: cfg.room.as_deref(),
    };
    let hello_json = serde_json::to_string(&hello)?;
    ws.send(Message::Text(hello_json.into()))
        .await
        .map_err(|e| anyhow!("send hello failed: {e}"))?;

    // 等握手回复（5s 超时）：hello_ok 放行，hello_reject 关闭并报错。
    let first = tokio::time::timeout(HELLO_TIMEOUT, ws.next())
        .await
        .map_err(|_| anyhow!("hello handshake timed out after {HELLO_TIMEOUT:?}"))?
        .ok_or_else(|| anyhow!("hub closed connection during hello handshake"))?
        .map_err(|e| anyhow!("hello handshake recv failed: {e}"))?;
    let text = match first {
        Message::Text(t) => t,
        Message::Close(_) => bail!("hub closed connection during hello handshake"),
        other => bail!("unexpected first message from hub: {other:?}"),
    };
    let v: serde_json::Value = serde_json::from_str(&text).context("hello reply is not JSON")?;
    match msg_type(&v) {
        "hello_ok" => {
            let device_id = v
                .get("device_id")
                .and_then(|d| d.as_str())
                .unwrap_or("")
                .to_string();
            let room = v
                .get("room")
                .and_then(|r| r.as_str())
                .map(|s| s.to_string());
            info!("authenticated with hub, device_id={device_id}");
            Ok(Transport {
                ws,
                device_id: Some(device_id),
                room,
            })
        }
        "hello_reject" => {
            let reason = v
                .get("reason")
                .and_then(|r| r.as_str())
                .unwrap_or("unknown reason");
            let _ = ws.close(None).await;
            bail!("hub rejected hello: {reason}")
        }
        other => {
            let _ = ws.close(None).await;
            bail!("unexpected hello reply type: {other}")
        }
    }
}

/// 带指数退避重连的连接：一直重试直到握手成功（退避 100ms 起步，上限 1s）。
/// 用于断线恢复；调用方应在 30s 内看到恢复（测试断言）。
pub async fn connect_with_retry(cfg: &Config) -> Result<Transport> {
    let mut attempt: u32 = 0;
    loop {
        match connect_once(cfg).await {
            Ok(t) => return Ok(t),
            Err(e) => {
                let delay = backoff_delay(attempt);
                warn!(
                    "connect attempt {} failed: {e} — retry in {delay:?}",
                    attempt + 1
                );
                tokio::time::sleep(delay).await;
                attempt = attempt.saturating_add(1);
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::net::SocketAddr;
    use tokio::net::TcpListener;

    /// 测试用强 Token（≥16 位，与 hub 的 ValidateToken 同规则）。
    const TEST_TOKEN: &str = "test-token-0123456789abcdef";

    fn txt(s: String) -> Message {
        Message::Text(s.into())
    }

    fn test_config(url: &str, token: &str, tls: bool) -> Config {
        Config {
            hub_url: url.to_string(),
            token: token.to_string(),
            wake_word: "你好小李".to_string(),
            kws_mode: "server".to_string(),
            kws_model: "/tmp/no-such-model.onnx".to_string(),
            kws_model_dir: "/tmp/no-such-kws-models".to_string(),
            kws_lib: String::new(),
            model: "lx06".to_string(),
            ram_mb: 256,
            tls,
            hub_ca_fingerprint: None,
            room: None,
        }
    }

    /// 测试 Hub：真实的 tokio-tungstenite 服务；正确 token 回 hello_ok，
    /// 错误 token 回 hello_reject 并关闭。accept 循环，支持多次连接（重连测试用）。
    /// `shutdown` 被置为 true 时，服务端主动关闭所有已建立连接（模拟 Hub 停机；
    /// 只 abort 监听任务关不掉已建立的连接，测试会假通过/假失败）。
    async fn run_test_hub_ws(
        listener: TcpListener,
        expected_token: &str,
        shutdown: tokio::sync::watch::Receiver<bool>,
    ) {
        loop {
            let (stream, _) = match listener.accept().await {
                Ok(x) => x,
                Err(_) => return,
            };
            let expected = expected_token.to_string();
            let mut shutdown = shutdown.clone();
            tokio::spawn(async move {
                let mut ws = match tokio_tungstenite::accept_async(stream).await {
                    Ok(ws) => ws,
                    Err(_) => return,
                };
                // 读 hello
                let first = match ws.next().await {
                    Some(Ok(Message::Text(t))) => t,
                    _ => return,
                };
                let v: serde_json::Value = match serde_json::from_str(&first) {
                    Ok(v) => v,
                    Err(_) => return,
                };
                let token_ok = v.get("token").and_then(|t| t.as_str()) == Some(expected.as_str())
                    && v.get("type").and_then(|t| t.as_str()) == Some("hello");
                if token_ok {
                    let _ = ws
                        .send(txt(
                            serde_json::json!({"type": "hello_ok", "device_id": "test-device-1", "room": "客厅"}).to_string(),
                        ))
                        .await;
                    // 保持连接：回显收到的文本帧，直到对端关闭或服务端停机。
                    loop {
                        tokio::select! {
                            msg = ws.next() => {
                                match msg {
                                    Some(Ok(Message::Text(t))) => {
                                        let _ = ws.send(Message::Text(t)).await;
                                    }
                                    Some(Ok(Message::Close(_))) | Some(Err(_)) | None => break,
                                    _ => {}
                                }
                            }
                            _ = shutdown.changed() => {
                                let _ = ws.close(None).await;
                                break;
                            }
                        }
                    }
                } else {
                    let _ = ws
                        .send(txt(
                            serde_json::json!({"type": "hello_reject", "reason": "bad token"})
                                .to_string(),
                        ))
                        .await;
                    let _ = ws.close(None).await;
                }
            });
        }
    }

    /// 自签证书（rcgen）+ tokio-rustls 的 wss 测试 Hub。
    async fn run_test_hub_wss(
        listener: TcpListener,
        expected_token: &str,
        server_config: Arc<rustls::ServerConfig>,
    ) {
        let acceptor = tokio_rustls::TlsAcceptor::from(server_config);
        loop {
            let (stream, _) = match listener.accept().await {
                Ok(x) => x,
                Err(_) => return,
            };
            let acceptor = acceptor.clone();
            let expected = expected_token.to_string();
            tokio::spawn(async move {
                let tls_stream = match acceptor.accept(stream).await {
                    Ok(s) => s,
                    Err(_) => return,
                };
                let mut ws = match tokio_tungstenite::accept_async(tls_stream).await {
                    Ok(ws) => ws,
                    Err(_) => return,
                };
                let first = match ws.next().await {
                    Some(Ok(Message::Text(t))) => t,
                    _ => return,
                };
                let v: serde_json::Value = match serde_json::from_str(&first) {
                    Ok(v) => v,
                    Err(_) => return,
                };
                let token_ok = v.get("token").and_then(|t| t.as_str()) == Some(expected.as_str());
                if token_ok {
                    let _ = ws
                        .send(txt(
                            serde_json::json!({"type": "hello_ok", "device_id": "test-device-wss"})
                                .to_string(),
                        ))
                        .await;
                    while let Some(msg) = ws.next().await {
                        match msg {
                            Ok(Message::Close(_)) | Err(_) => break,
                            _ => {}
                        }
                    }
                } else {
                    let _ = ws
                        .send(txt(
                            serde_json::json!({"type": "hello_reject", "reason": "bad token"})
                                .to_string(),
                        ))
                        .await;
                    let _ = ws.close(None).await;
                }
            });
        }
    }

    fn make_self_signed() -> (Arc<rustls::ServerConfig>, String) {
        use rustls::pki_types::PrivateKeyDer;
        let certified = rcgen::generate_simple_self_signed(vec!["localhost".to_string()]).unwrap();
        let cert_der = certified.cert.der().to_vec();
        let fingerprint = sha256_fingerprint_hex(&cert_der);
        let key_der = certified.signing_key.serialize_der();
        let server_config = rustls::ServerConfig::builder_with_provider(Arc::new(
            rustls::crypto::ring::default_provider(),
        ))
        .with_safe_default_protocol_versions()
        .unwrap()
        .with_no_client_auth()
        .with_single_cert(
            vec![CertificateDer::from(cert_der)],
            PrivateKeyDer::Pkcs8(rustls::pki_types::PrivatePkcs8KeyDer::from(key_der)),
        )
        .unwrap();
        (Arc::new(server_config), fingerprint)
    }

    #[test]
    fn backoff_grows_then_caps_at_1s() {
        assert_eq!(backoff_delay(0), Duration::from_millis(100));
        assert_eq!(backoff_delay(1), Duration::from_millis(200));
        assert_eq!(backoff_delay(2), Duration::from_millis(400));
        assert_eq!(backoff_delay(3), Duration::from_millis(800));
        assert_eq!(backoff_delay(4), Duration::from_millis(1000));
        assert_eq!(backoff_delay(100), Duration::from_millis(1000));
    }

    #[test]
    fn parse_hub_messages() {
        assert_eq!(
            parse_hub_text(r#"{"type":"hello_ok","device_id":"d1","room":"客厅"}"#).unwrap(),
            HubMsg::HelloOk {
                device_id: "d1".to_string(),
                room: Some("客厅".to_string())
            }
        );
        assert_eq!(
            parse_hub_text(r#"{"type":"wake_win"}"#).unwrap(),
            HubMsg::WakeWin
        );
        assert_eq!(
            parse_hub_text(r#"{"type":"wake_lose"}"#).unwrap(),
            HubMsg::WakeLose
        );
        assert_eq!(
            parse_hub_text(r#"{"type":"interrupt"}"#).unwrap(),
            HubMsg::Interrupt
        );
        assert_eq!(
            parse_hub_text(
                r#"{"type":"rpc","id":"7","method":"play_text","params":{"text":"hi"}}"#
            )
            .unwrap(),
            HubMsg::Rpc {
                id: "7".to_string(),
                method: "play_text".to_string(),
                params: serde_json::json!({"text": "hi"}),
            }
        );
        assert_eq!(
            parse_hub_text(r#"{"type":"room_ok","room":"卧室"}"#).unwrap(),
            HubMsg::RoomOk {
                room: "卧室".to_string()
            }
        );
        assert!(parse_hub_text("not json").is_err());
        assert_eq!(
            parse_hub_text(r#"{"type":"something_new"}"#).unwrap(),
            HubMsg::Unknown("something_new".to_string())
        );
    }

    #[test]
    fn fingerprint_roundtrip() {
        let der = b"fake-cert-der-bytes";
        let fp = sha256_fingerprint_hex(der);
        assert_eq!(fp.len(), 64);
        assert!(fingerprint_matches(der, &fp));
        assert!(!fingerprint_matches(der, &"00".repeat(32)));
        assert!(!fingerprint_matches(b"other", &fp));
    }

    #[tokio::test]
    async fn ws_auth_good_token_passes_bad_token_rejected() {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr: SocketAddr = listener.local_addr().unwrap();
        let (shutdown_tx, shutdown_rx) = tokio::sync::watch::channel(false);
        let hub = tokio::spawn(run_test_hub_ws(listener, TEST_TOKEN, shutdown_rx));
        let _ = shutdown_tx;

        // 正确 token → hello_ok，device_id 透传
        let cfg = test_config(&format!("ws://{addr}"), TEST_TOKEN, false);
        let mut t = connect_once(&cfg).await.expect("good token must pass");
        assert_eq!(t.device_id.as_deref(), Some("test-device-1"));
        assert_eq!(t.room.as_deref(), Some("客厅"));
        // 发一条 wake，能回显回来（测试 Hub 会原样回显文本帧）
        t.send_wake("你好小李", -12.5).await.unwrap();
        let msg = t.next_msg().await.unwrap().unwrap();
        match msg {
            HubMsg::Unknown(other) => assert_eq!(other, "wake"),
            other => panic!("expected echo of wake as Unknown, got {other:?}"),
        }
        t.close().await;

        // 错误 token → hello_reject，connect_once 报错
        let bad = test_config(
            &format!("ws://{addr}"),
            "wrong-token-0123456789abcdef",
            false,
        );
        let err = connect_once(&bad)
            .await
            .err()
            .expect("bad token must be rejected");
        assert!(
            err.to_string().contains("bad token"),
            "unexpected error: {err}"
        );

        hub.abort();
    }

    #[tokio::test]
    async fn wss_auth_with_fingerprint() {
        let (server_config, fingerprint) = make_self_signed();
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr: SocketAddr = listener.local_addr().unwrap();
        let hub = tokio::spawn(run_test_hub_wss(listener, TEST_TOKEN, server_config));

        // 正确指纹 + 正确 token → 通过
        let mut cfg = test_config(
            &format!("wss://localhost:{}", addr.port()),
            TEST_TOKEN,
            true,
        );
        cfg.hub_ca_fingerprint = Some(fingerprint.clone());
        let t = connect_once(&cfg)
            .await
            .expect("wss + fingerprint + good token must pass");
        assert_eq!(t.device_id.as_deref(), Some("test-device-wss"));
        t.close().await;

        // 正确指纹 + 错误 token → 被拒
        let mut bad = test_config(
            &format!("wss://localhost:{}", addr.port()),
            "wrong-token-0123456789abcdef",
            true,
        );
        bad.hub_ca_fingerprint = Some(fingerprint.clone());
        let err = connect_once(&bad)
            .await
            .err()
            .expect("bad token must be rejected");
        assert!(
            err.to_string().contains("bad token"),
            "unexpected error: {err}"
        );

        // 错误指纹 → TLS 握手失败
        let mut wrong_fp = test_config(
            &format!("wss://localhost:{}", addr.port()),
            TEST_TOKEN,
            true,
        );
        wrong_fp.hub_ca_fingerprint = Some("00".repeat(32));
        assert!(connect_once(&wrong_fp).await.is_err());

        // 指纹格式非法 → 配置期直接报错
        let mut malformed = test_config(
            &format!("wss://localhost:{}", addr.port()),
            TEST_TOKEN,
            true,
        );
        malformed.hub_ca_fingerprint = Some("xyz".to_string());
        let err = connect_once(&malformed)
            .await
            .err()
            .expect("malformed fingerprint must fail");
        assert!(
            err.to_string().contains("malformed"),
            "unexpected error: {err}"
        );

        hub.abort();
    }

    #[tokio::test]
    async fn reconnect_after_server_restart_within_30s() {
        // 1. 起服务，连上
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr: SocketAddr = listener.local_addr().unwrap();
        let port = addr.port();
        let (shutdown_tx, shutdown_rx) = tokio::sync::watch::channel(false);
        let hub1 = tokio::spawn(run_test_hub_ws(listener, TEST_TOKEN, shutdown_rx));
        let cfg = test_config(&format!("ws://127.0.0.1:{port}"), TEST_TOKEN, false);
        let mut t = connect_once(&cfg).await.expect("initial connect must pass");

        // 2. 停掉服务：先让服务端主动关闭已建立连接，再 abort 监听任务
        let _ = shutdown_tx.send(true);
        hub1.abort();
        // 客户端应在短时间内感知断线（next_msg 返回 Err 或 None）
        let gone = tokio::time::timeout(Duration::from_secs(5), t.next_msg()).await;
        assert!(gone.is_ok(), "client did not notice disconnect within 5s");
        match gone.unwrap() {
            Ok(None) => {} // 正常关闭
            Err(_) => {}   // 连接重置
            Ok(Some(m)) => panic!("expected disconnect, got message {m:?}"),
        }
        drop(t);

        // 3. 同端口重启服务，Client 重连必须在 30s 内成功
        let listener2 = TcpListener::bind(format!("127.0.0.1:{port}"))
            .await
            .unwrap();
        let (_shutdown_tx2, shutdown_rx2) = tokio::sync::watch::channel(false);
        let hub2 = tokio::spawn(run_test_hub_ws(listener2, TEST_TOKEN, shutdown_rx2));
        let start = std::time::Instant::now();
        let t2 = tokio::time::timeout(Duration::from_secs(30), connect_with_retry(&cfg))
            .await
            .expect("reconnect timed out after 30s")
            .expect("reconnect must succeed");
        let elapsed = start.elapsed();
        assert!(
            elapsed < Duration::from_secs(30),
            "reconnect took {elapsed:?}"
        );
        assert_eq!(t2.device_id.as_deref(), Some("test-device-1"));
        t2.close().await;
        hub2.abort();
    }

    #[tokio::test]
    async fn retry_waits_for_delayed_server() {
        // 服务晚 1s 才起：connect_with_retry 应等到它出现（30s 内）。
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr: SocketAddr = listener.local_addr().unwrap();
        let port = addr.port();
        drop(listener);
        let cfg = test_config(&format!("ws://127.0.0.1:{port}"), TEST_TOKEN, false);
        let start = std::time::Instant::now();
        let join = tokio::spawn(async move {
            tokio::time::sleep(Duration::from_millis(1000)).await;
            let l2 = TcpListener::bind(format!("127.0.0.1:{port}"))
                .await
                .unwrap();
            let (_tx, rx) = tokio::sync::watch::channel(false);
            run_test_hub_ws(l2, TEST_TOKEN, rx).await;
        });
        let t = tokio::time::timeout(Duration::from_secs(30), connect_with_retry(&cfg))
            .await
            .expect("delayed connect timed out")
            .expect("delayed connect must succeed");
        assert!(start.elapsed() >= Duration::from_millis(900));
        assert!(start.elapsed() < Duration::from_secs(30));
        t.close().await;
        join.abort();
    }

    // --- P0-3：弱 Token ---

    #[test]
    fn weak_token_rules() {
        assert!(is_weak_token(""));
        assert!(is_weak_token("short"));
        assert!(is_weak_token("0123456789abcde")); // 15 位
        assert!(is_weak_token("CHANGE_ME"));
        assert!(is_weak_token("change_me"));
        assert!(is_weak_token("secret"));
        assert!(is_weak_token("password"));
        assert!(is_weak_token("open-xiaoai"));
        assert!(!is_weak_token("0123456789abcdef")); // 16 位
        assert!(!is_weak_token(TEST_TOKEN));
        assert!(validate_token(TEST_TOKEN).is_ok());
        let err = validate_token("CHANGE_ME")
            .err()
            .expect("weak token must fail");
        assert!(err.to_string().contains("openssl rand -hex 16"));
    }

    #[tokio::test]
    async fn connect_once_rejects_weak_token_without_dialing() {
        // 无需起服务：弱 Token 在建连前就被拒绝（错误与网络无关）。
        let cfg = test_config("ws://127.0.0.1:1", "CHANGE_ME", false);
        let err = connect_once(&cfg)
            .await
            .err()
            .expect("weak token must fail");
        assert!(err.to_string().contains("weak"), "unexpected error: {err}");
        let cfg2 = test_config("ws://127.0.0.1:1", "short", false);
        assert!(connect_once(&cfg2).await.is_err());
    }

    // --- P0-3：mTLS ---

    /// rcgen 生成一套测试 CA + CA 签发的服务端/客户端证书。
    /// 返回 (CA 的 DER，服务端证书 DER，服务端私钥 PKCS#8 DER，
    /// 客户端证书 PEM，客户端私钥 PEM)。
    fn make_test_pki() -> (Vec<u8>, Vec<u8>, Vec<u8>, String, String) {
        use rcgen::{
            BasicConstraints, CertificateParams, CertifiedIssuer, ExtendedKeyUsagePurpose, IsCa,
            KeyPair, KeyUsagePurpose,
        };
        let mut ca_params = CertificateParams::new(vec![]).unwrap();
        ca_params.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
        ca_params.key_usages = vec![
            KeyUsagePurpose::KeyCertSign,
            KeyUsagePurpose::DigitalSignature,
        ];
        ca_params
            .distinguished_name
            .push(rcgen::DnType::CommonName, "test-ca");
        let ca_key = KeyPair::generate().unwrap();
        let ca_issuer = CertifiedIssuer::self_signed(ca_params, ca_key).unwrap();
        let ca_der = ca_issuer.der().to_vec();

        let mut server_params = CertificateParams::new(vec!["localhost".to_string()]).unwrap();
        server_params.extended_key_usages = vec![ExtendedKeyUsagePurpose::ServerAuth];
        let server_key = KeyPair::generate().unwrap();
        let server_cert = server_params.signed_by(&server_key, &ca_issuer).unwrap();
        let server_der = server_cert.der().to_vec();
        let server_key_der = server_key.serialize_der();

        let mut client_params = CertificateParams::new(vec![]).unwrap();
        client_params.extended_key_usages = vec![ExtendedKeyUsagePurpose::ClientAuth];
        let client_key = KeyPair::generate().unwrap();
        let client_cert = client_params.signed_by(&client_key, &ca_issuer).unwrap();
        (
            ca_der,
            server_der,
            server_key_der,
            client_cert.pem(),
            client_key.serialize_pem(),
        )
    }

    fn make_mtls_server_config(
        ca_der: &[u8],
        server_der: Vec<u8>,
        server_key_der: Vec<u8>,
    ) -> Arc<rustls::ServerConfig> {
        use rustls::pki_types::PrivateKeyDer;
        let mut roots = rustls::RootCertStore::empty();
        roots.add(CertificateDer::from(ca_der.to_vec())).unwrap();
        let verifier = rustls::server::WebPkiClientVerifier::builder_with_provider(
            Arc::new(roots),
            crypto_provider(),
        )
        .build()
        .unwrap();
        let cfg = rustls::ServerConfig::builder_with_provider(crypto_provider())
            .with_safe_default_protocol_versions()
            .unwrap()
            .with_client_cert_verifier(verifier)
            .with_single_cert(
                vec![CertificateDer::from(server_der)],
                PrivateKeyDer::Pkcs8(rustls::pki_types::PrivatePkcs8KeyDer::from(server_key_der)),
            )
            .unwrap();
        Arc::new(cfg)
    }

    #[test]
    fn load_client_identity_roundtrip() {
        let (_ca, _srv, _key, client_pem, client_key_pem) = make_test_pki();
        let dir =
            std::env::temp_dir().join(format!("open-xiaoai-mtls-test-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let cert_path = dir.join("client.crt");
        let key_path = dir.join("client.key");
        std::fs::write(&cert_path, &client_pem).unwrap();
        std::fs::write(&key_path, &client_key_pem).unwrap();
        let (certs, key) = load_client_identity(&cert_path, &key_path).unwrap();
        assert_eq!(certs.len(), 1);
        assert!(matches!(key, PrivateKeyDer::Pkcs8(_)));
        // 坏 PEM 应报错，不静默降级。
        std::fs::write(&key_path, "not a pem").unwrap();
        assert!(load_client_identity(&cert_path, &key_path).is_err());
        std::fs::remove_dir_all(&dir).unwrap();
    }

    #[tokio::test]
    async fn wss_mtls_with_and_without_client_cert() {
        let (ca_der, server_der, server_key_der, client_pem, client_key_pem) = make_test_pki();
        let server_fp = sha256_fingerprint_hex(&server_der);
        let server_config = make_mtls_server_config(&ca_der, server_der, server_key_der);
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr: SocketAddr = listener.local_addr().unwrap();
        let hub = tokio::spawn(run_test_hub_wss(listener, TEST_TOKEN, server_config));

        // 客户端身份写临时文件，经环境变量传给 connect_once。
        let dir = std::env::temp_dir().join(format!("open-xiaoai-mtls-e2e-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let cert_path = dir.join("client.crt");
        let key_path = dir.join("client.key");
        std::fs::write(&cert_path, &client_pem).unwrap();
        std::fs::write(&key_path, &client_key_pem).unwrap();
        std::env::set_var("OPEN_XIAOAI_CLIENT_CERT", &cert_path);
        std::env::set_var("OPEN_XIAOAI_CLIENT_KEY", &key_path);

        // 有客户端证书 + 正确指纹 + 正确 Token → hello_ok。
        let mut cfg = test_config(
            &format!("wss://localhost:{}", addr.port()),
            TEST_TOKEN,
            true,
        );
        cfg.hub_ca_fingerprint = Some(server_fp.clone());
        let t = connect_once(&cfg)
            .await
            .expect("mTLS + fingerprint + good token must pass");
        assert_eq!(t.device_id.as_deref(), Some("test-device-wss"));
        t.close().await;

        // 撤掉客户端证书 → TLS 握手失败（Hub 要求客户端证书）。
        std::env::remove_var("OPEN_XIAOAI_CLIENT_CERT");
        std::env::remove_var("OPEN_XIAOAI_CLIENT_KEY");
        let err = connect_once(&cfg)
            .await
            .err()
            .expect("no client cert must fail mTLS");
        assert!(
            err.to_string().contains("wss connect failed"),
            "unexpected error: {err}"
        );

        std::fs::remove_dir_all(&dir).unwrap();
        hub.abort();
    }
}
