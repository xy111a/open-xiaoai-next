//! 配置：只读 `/data/open-xiaoai/client.toml`，不存在则用默认值。
//! 唤醒词、Hub 地址、Token 都在这里改，不用重刷机；改完由 Hub OTA 推送。
//!
//! Phase 1 新增：`tls`（默认 true；false = 纯 ws，仅本地测试）、
//! `hub_ca_fingerprint`（Hub 自签证书的 SHA-256 指纹；有则校验，无则跳过并打警告）、
//! `room`（已认领的房间名，hello 里带给 Hub，可选）。

use anyhow::Result;
use serde::Deserialize;

fn default_hub_url() -> String {
    "wss://192.168.1.10:4399".to_string()
}
fn default_token() -> String {
    // P0-3：默认 Token 为空（fail closed）。空值/弱值在建连前就被
    // transport.rs 的 connect_once（validate_token）拒绝，Client
    // 不会拿公开默认值上线；配置文件里必须显式填一个 ≥16 位的强 Token。
    String::new()
}
fn default_wake_word() -> String {
    "你好小李".to_string()
}
fn default_kws_model() -> String {
    "/data/open-xiaoai/kws/nihaoxiaoli.onnx".to_string()
}
fn default_kws_model_dir() -> String {
    "/data/open-xiaoai/kws/models".to_string()
}
fn default_kws_lib() -> String {
    String::new()
}
fn default_kws_mode() -> String {
    // 服务端唤醒模式是默认：KWS 在 NAS 端边车做，音箱只持续推 Opus 流，
    // 不要求本地模型/.so。本地模式（"local"）才在音箱上跑 sherpa KWS。
    "server".to_string()
}
fn default_model() -> String {
    "lx06".to_string()
}
fn default_ram_mb() -> u32 {
    256
}
fn default_tls() -> bool {
    true
}

#[derive(Debug, Clone, Deserialize)]
pub struct Config {
    /// Hub 地址，例如 wss://192.168.1.10:4399
    #[serde(default = "default_hub_url")]
    pub hub_url: String,
    /// 与 Hub 约定的 Token（与服务端 OPEN_XIAOAI_TOKEN 一致，否则拒绝连接）
    #[serde(default = "default_token")]
    pub token: String,
    /// 唤醒词，例如 "你好小李"；换词 = 换 KWS 模型文件 + 改这一行
    #[serde(default = "default_wake_word")]
    pub wake_word: String,
    /// 唤醒检测放在哪：`"server"`（默认）= NAS 端边车检测，音箱持续推
    /// Opus 流，不初始化本地 KWS；`"local"` = 音箱本地 sherpa KWS，
    /// 保持 P1-40 的 fail-loud 行为。其他值启动时报错（见 `validate`）。
    #[serde(default = "default_kws_mode")]
    pub kws_mode: String,
    /// KWS 模型文件路径（/data 分区持久化）
    #[serde(default = "default_kws_model")]
    pub kws_model: String,
    /// Sherpa-ONNX KWS 模型目录（feature `sherpa-kws` 用）：含 encoder/
    /// decoder/joiner .onnx + tokens.txt + keywords.txt；见 kws/README.md。
    /// 为空或目录不存在时不启用 sherpa 后端（回退纯 RMS，不报错崩溃）。
    /// 注：只在 feature 开时被 main.rs 读；默认构建下 serde 反序列化会读它，
    /// 但 release 二进制仍报 dead_code，故 allow。
    #[allow(dead_code)]
    #[serde(default = "default_kws_model_dir")]
    pub kws_model_dir: String,
    /// libsherpa-onnx-c-api.so 路径（feature `sherpa-kws` 用）；空串 =
    /// 按默认名在系统库路径里找。见 kws/README.md。
    #[allow(dead_code)]
    #[serde(default = "default_kws_lib")]
    pub kws_lib: String,
    /// 设备型号：lx06 / oh2p（只支持这两款）
    #[serde(default = "default_model")]
    pub model: String,
    /// 内存 MB：256（LX06 DTS / OH2P 都是 256；128MB 机型跑不了 KWS）
    #[serde(default = "default_ram_mb")]
    pub ram_mb: u32,
    /// true = wss（生产必须），false = 纯 ws（仅本地测试）
    #[serde(default = "default_tls")]
    pub tls: bool,
    /// Hub 自签证书的 SHA-256 指纹（可带冒号/大小写混用）；有则校验，
    /// 无则跳过校验并打警告（仅测试环境可接受）。
    #[serde(default)]
    pub hub_ca_fingerprint: Option<String>,
    /// 已认领的房间名（如“客厅”），hello 里带给 Hub；未认领为 None
    #[serde(default)]
    pub room: Option<String>,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            hub_url: default_hub_url(),
            token: default_token(),
            wake_word: default_wake_word(),
            kws_mode: default_kws_mode(),
            kws_model: default_kws_model(),
            kws_model_dir: default_kws_model_dir(),
            kws_lib: default_kws_lib(),
            model: default_model(),
            ram_mb: default_ram_mb(),
            tls: true,
            hub_ca_fingerprint: None,
            room: None,
        }
    }
}

impl Config {
    /// 指纹归一化：去冒号/空白，转小写。合法指纹 = 64 个 hex 字符。
    /// 返回 None 表示未配置或格式非法（调用方应打警告/报错）。
    pub fn normalized_fingerprint(&self) -> Option<String> {
        let raw = self.hub_ca_fingerprint.as_ref()?;
        let norm: String = raw
            .chars()
            .filter(|c| !c.is_whitespace() && *c != ':')
            .collect::<String>()
            .to_lowercase();
        if norm.len() == 64 && norm.chars().all(|c| c.is_ascii_hexdigit()) {
            Some(norm)
        } else {
            None
        }
    }

    /// 是否配置了合法指纹。
    #[allow(dead_code)]
    pub fn has_valid_fingerprint(&self) -> bool {
        self.normalized_fingerprint().is_some()
    }

    /// 是否配置了指纹字段但格式非法（应报错，拒绝用错指纹连接）。
    pub fn has_malformed_fingerprint(&self) -> bool {
        match &self.hub_ca_fingerprint {
            Some(s) if !s.trim().is_empty() => self.normalized_fingerprint().is_none(),
            _ => false,
        }
    }

    /// 服务端唤醒模式：KWS 在 NAS 端边车做，音箱只持续推流。
    pub fn is_server_kws_mode(&self) -> bool {
        self.kws_mode == "server"
    }

    /// 本地唤醒模式：音箱本地跑 sherpa KWS（P1-40 fail-loud 行为适用）。
    pub fn is_local_kws_mode(&self) -> bool {
        self.kws_mode == "local"
    }

    /// 校验 `kws_mode`：只接受 `"server"` / `"local"`，其他值报错，
    /// 启动即失败（`load_from_str` 调本函数；非法值绝不静默当 server 用）。
    pub fn validate(&self) -> Result<()> {
        if self.is_server_kws_mode() || self.is_local_kws_mode() {
            Ok(())
        } else {
            anyhow::bail!(
                "invalid kws_mode {:?}: expected \"server\" (default, wake detection on the NAS sidecar) or \"local\" (on-device sherpa KWS)",
                self.kws_mode
            )
        }
    }
}

/// 默认配置路径（音箱上）；可用环境变量 OPEN_XIAOAI_CLIENT_TOML 覆盖（宿主机调试用）。
pub fn default_path() -> String {
    std::env::var("OPEN_XIAOAI_CLIENT_TOML")
        .unwrap_or_else(|_| "/data/open-xiaoai/client.toml".to_string())
}

pub fn load() -> Result<Config> {
    load_from_path(&default_path())
}

pub fn load_from_path(path: &str) -> Result<Config> {
    match std::fs::read_to_string(path) {
        Ok(text) => load_from_str(&text),
        Err(_) => Ok(Config::default()),
    }
}

pub fn load_from_str(text: &str) -> Result<Config> {
    let cfg: Config = toml::from_str(text)?;
    cfg.validate()?;
    Ok(cfg)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn default_config() {
        let cfg = Config::default();
        assert_eq!(cfg.hub_url, "wss://192.168.1.10:4399");
        assert_eq!(cfg.wake_word, "你好小李");
        assert_eq!(cfg.model, "lx06");
        assert_eq!(cfg.ram_mb, 256);
        assert!(cfg.tls);
        assert_eq!(cfg.hub_ca_fingerprint, None);
        assert_eq!(cfg.room, None);
        assert_eq!(cfg.kws_model_dir, "/data/open-xiaoai/kws/models");
        assert_eq!(cfg.kws_lib, "");
        // 服务端唤醒是默认模式（KWS 在 NAS 边车，音箱不要求本地后端）。
        assert_eq!(cfg.kws_mode, "server");
        assert!(cfg.is_server_kws_mode());
        assert!(!cfg.is_local_kws_mode());
        assert!(cfg.validate().is_ok());
    }

    #[test]
    fn kws_mode_local_parses_and_server_explicit_parses() {
        let local = load_from_str("kws_mode = \"local\"\n").unwrap();
        assert_eq!(local.kws_mode, "local");
        assert!(local.is_local_kws_mode());
        assert!(!local.is_server_kws_mode());
        let server = load_from_str("kws_mode = \"server\"\n").unwrap();
        assert!(server.is_server_kws_mode());
    }

    #[test]
    fn kws_mode_invalid_value_fails_startup_with_clear_error() {
        let err = load_from_str("kws_mode = \"auto\"\n").expect_err("invalid kws_mode must fail");
        let msg = err.to_string();
        assert!(msg.contains("invalid kws_mode"), "unexpected error: {msg}");
        assert!(msg.contains("\"server\""), "unexpected error: {msg}");
        assert!(msg.contains("\"local\""), "unexpected error: {msg}");
        // 构造出来的非法 Config 直接调 validate 也必须报错（不只走解析路径）。
        for bad in ["AUTO", ""] {
            let cfg = Config {
                kws_mode: bad.to_string(),
                ..Config::default()
            };
            assert!(
                cfg.validate().is_err(),
                "kws_mode={bad:?} must fail validation"
            );
        }
    }

    #[test]
    fn parse_custom_toml() {
        let cfg = load_from_str(
            r#"
            hub_url = "ws://127.0.0.1:4399"
            token = "secret"
            wake_word = "你好小李"
            kws_model = "/tmp/kws.onnx"
            model = "oh2p"
            ram_mb = 256
            tls = false
            hub_ca_fingerprint = "AA:bb:CC:dd:ee:ff:00:11:22:33:44:55:66:77:88:99:AA:bb:CC:dd:ee:ff:00:11:22:33:44:55:66:77:88:99"
            room = "客厅"
            "#,
        )
        .unwrap();
        assert_eq!(cfg.hub_url, "ws://127.0.0.1:4399");
        assert_eq!(cfg.token, "secret");
        assert_eq!(cfg.model, "oh2p");
        assert!(!cfg.tls);
        assert_eq!(cfg.room.as_deref(), Some("客厅"));
        // 冒号分隔 + 大小写混用应归一化为 64 位小写 hex
        let fp = cfg.normalized_fingerprint().unwrap();
        assert_eq!(fp.len(), 64);
        assert_eq!(&fp[..6], "aabbcc");
        assert!(cfg.has_valid_fingerprint());
        assert!(!cfg.has_malformed_fingerprint());
    }

    #[test]
    fn parse_partial_toml_uses_defaults() {
        // 只写两个字段，其余全部走默认值（旧 client.toml 不会被新字段拖垮）。
        let cfg = load_from_str("hub_url = \"wss://10.0.0.2:4399\"\ntoken = \"x\"\n").unwrap();
        assert_eq!(cfg.hub_url, "wss://10.0.0.2:4399");
        assert_eq!(cfg.wake_word, "你好小李");
        assert!(cfg.tls);
        assert_eq!(cfg.hub_ca_fingerprint, None);
    }

    #[test]
    fn malformed_fingerprint_detected() {
        let mut cfg = Config::default();
        cfg.hub_ca_fingerprint = Some("not-a-fingerprint".to_string());
        assert!(cfg.normalized_fingerprint().is_none());
        assert!(cfg.has_malformed_fingerprint());
        cfg.hub_ca_fingerprint = Some("aabb".to_string());
        assert!(cfg.has_malformed_fingerprint());
    }

    #[test]
    fn missing_file_falls_back_to_default() {
        let cfg = load_from_path("/tmp/definitely-not-exist-open-xiaoai-client.toml").unwrap();
        assert_eq!(cfg.hub_url, Config::default().hub_url);
        assert!(cfg.tls);
    }
}
