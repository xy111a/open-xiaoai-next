//! 配置：只读 `/data/open-xiaoai/client.toml`；文件不存在即报错退出，
//! 不静默回退默认配置。`hub_url` 必填（无默认值，缺了启动即报错）；
//! 其余字段缺失走默认值，旧 client.toml 兼容。
//! 唤醒词、Hub 地址、Token 都在这里改，不用重刷机；改完由 Hub OTA 推送。
//!
//! Phase 1 新增：`tls`（默认 true；false = 纯 ws，仅本地测试，需显式
//! `allow_insecure_ws = true`）、`hub_ca_fingerprint`（Hub 自签证书的
//! SHA-256 指纹；`tls=true` 时必填，缺失即拒绝启动，见 P1-10）、
//! `room`（已认领的房间名，hello 里带给 Hub，可选）。

use anyhow::Result;
use serde::Deserialize;

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
    /// Hub 地址，例如 wss://192.168.1.10:4399。必填，无默认值
    /// （C-15 / P2-42，2026-11-18 bai 定）：缺失或空串时 `validate()`
    /// 拒绝启动；写死的内网 IP 默认值已删，换网络/NAS 换 IP 时不再
    /// 静默连错地方。
    #[serde(default)]
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
    /// `tls=false` 的显式 opt-in（P1-10）：纯 ws 只允许本地测试，生产
    /// 必须 `tls=true` + 指纹。默认 false；`tls=false` 且本字段为 false
    /// 时 `validate()` 拒绝启动，不再只 warn 照常启动。
    #[serde(default)]
    pub allow_insecure_ws: bool,
    /// Hub 自签证书的 SHA-256 指纹（可带冒号/大小写混用）；`tls=true` 时
    /// 必填（`validate()` 拒绝无指纹启动，见 P1-10）；`tls=false` 时不用
    /// （走纯 ws，不校验证书）。
    #[serde(default)]
    pub hub_ca_fingerprint: Option<String>,
    /// 已认领的房间名（如“客厅”），hello 里带给 Hub；未认领为 None
    #[serde(default)]
    pub room: Option<String>,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            hub_url: String::new(),
            token: default_token(),
            wake_word: default_wake_word(),
            kws_mode: default_kws_mode(),
            kws_model: default_kws_model(),
            kws_model_dir: default_kws_model_dir(),
            kws_lib: default_kws_lib(),
            model: default_model(),
            ram_mb: default_ram_mb(),
            tls: true,
            allow_insecure_ws: false,
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

    /// 校验配置（启动即失败，绝不静默带病上线）。
    ///
    /// - `kws_mode`：只接受 `"server"` / `"local"`（旧行为）。
    /// - P1-10（收紧）：`tls=true` 时 `hub_ca_fingerprint` 必填——无指纹
    ///   只 warn 走系统根证书的时代结束了（自签 Hub 证书不在系统根里，
    ///   不配指纹要么连不上、要么被引导去关 TLS）。指纹获取：Hub 启动时
    ///   打到日志，`firmware-patch/scripts/gen-certs.sh` 也写进
    ///   `fingerprints.txt`；刷机前抄进 client.toml。格式非法同样报错。
    /// - P1-10（收紧）：`tls=false` 只允许显式本地测试（`allow_insecure_ws
    ///   = true`），否则拒绝启动；且 `tls=false` 时 `hub_url` 必须是
    ///   `ws://`——`wss://` 会悄悄走系统根证书 TLS、不走指纹校验，与
    ///   “tls=false = 纯 ws”自述矛盾（P2-N3，挡着 P1-10 的路顺带修掉）。
    /// - `hub_url` 必填（C-15 / P2-42，2026-11-18 bai 定）：缺失或空串
    ///   即拒绝启动（与空 Token 的 fail-closed 一个风格）；非空时必须带
    ///   `ws://` / `wss://` scheme；`model` 只支持 `lx06` / `oh2p`；
    ///   `ram_mb` 必须 >0（P2-42 的一部分：以前 `model`/`ram_mb`/
    ///   `hub_url` 无任何校验）。
    ///
    /// 注意：`FingerprintVerifier` 同时校验有效期与 SAN 是 P2-1 的范围，
    /// 不在本项（见 AUDIT_ROUND2.md §6）。
    pub fn validate(&self) -> Result<()> {
        if !(self.is_server_kws_mode() || self.is_local_kws_mode()) {
            anyhow::bail!(
                "invalid kws_mode {:?}: expected \"server\" (default, wake detection on the NAS sidecar) or \"local\" (on-device sherpa KWS)",
                self.kws_mode
            )
        }
        if self.hub_url.trim().is_empty() {
            anyhow::bail!(
                "hub_url is required in client.toml (no default): set it to the Hub WebSocket address, e.g. wss://192.168.1.10:4399"
            )
        }
        if !(self.hub_url.starts_with("ws://") || self.hub_url.starts_with("wss://")) {
            anyhow::bail!(
                "invalid hub_url {:?}: expected ws:// or wss:// (e.g. wss://192.168.1.10:4399)",
                self.hub_url
            )
        }
        if self.tls {
            match self.normalized_fingerprint() {
                Some(_) => {}
                None => {
                    if self
                        .hub_ca_fingerprint
                        .as_ref()
                        .map(|s| !s.trim().is_empty())
                        .unwrap_or(false)
                    {
                        anyhow::bail!(
                            "hub_ca_fingerprint malformed (want 64 hex chars, colons/spaces allowed): {:?}",
                            self.hub_ca_fingerprint
                        );
                    }
                    anyhow::bail!(
                        "hub_ca_fingerprint is required when tls=true: the hub uses a self-signed certificate, copy its SHA-256 fingerprint from the hub startup log (or fingerprints.txt from gen-certs.sh) into client.toml"
                    );
                }
            }
        } else {
            if !self.allow_insecure_ws {
                anyhow::bail!(
                    "tls=false requires allow_insecure_ws=true in client.toml (plain ws is local-test only; production must use tls=true with hub_ca_fingerprint)"
                );
            }
            if !self.hub_url.starts_with("ws://") {
                anyhow::bail!(
                    "tls=false requires hub_url to start with ws:// (got {:?}); wss:// with tls=false would silently use system-root TLS instead of fingerprint verification",
                    self.hub_url
                );
            }
        }
        if self.model != "lx06" && self.model != "oh2p" {
            anyhow::bail!(
                "invalid model {:?}: expected \"lx06\" or \"oh2p\"",
                self.model
            )
        }
        if self.ram_mb == 0 {
            anyhow::bail!(
                "invalid ram_mb 0: expected the speaker RAM size in MB (256 for LX06/OH2P)"
            );
        }
        Ok(())
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
    // P2-42：配置文件缺失或不可读时明确报错退出，不再静默回退默认配置——
    // 旧默认配置的 hub_url 是写死的 192.168.1.10、token 为空，静默回退只会
    // 让一次部署失误（忘写 client.toml、路径写错、权限不足）变成“无限
    // 重连一个不存在的 Hub + 一条容易误读的指纹缺失报错”。client.toml
    // 是部署时手写的（见 client/README.md 部署清单），缺失就是部署未完成。
    // 注意：文件存在但字段缺失时其余字段仍走默认值（旧 client.toml 兼容），
    // 唯独 hub_url 无默认值——缺失即空串，随后 `validate()` 拒绝启动。
    let text = std::fs::read_to_string(path).map_err(|e| {
        let hint = if e.kind() == std::io::ErrorKind::NotFound {
            format!(
                "config file not found at {path} — write client.toml there first (see client/README.md deployment checklist: hub_url, token, hub_ca_fingerprint)"
            )
        } else {
            format!("cannot read config file at {path} (check permissions/path): {e}")
        };
        anyhow::anyhow!("{hint}")
    })?;
    load_from_str(&text)
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
        // C-15 / P2-42：hub_url 无默认值（空串），缺了启动即报错。
        assert_eq!(cfg.hub_url, "");
        assert_eq!(cfg.wake_word, "你好小李");
        assert_eq!(cfg.model, "lx06");
        assert_eq!(cfg.ram_mb, 256);
        assert!(cfg.tls);
        assert!(!cfg.allow_insecure_ws);
        assert_eq!(cfg.hub_ca_fingerprint, None);
        assert_eq!(cfg.room, None);
        assert_eq!(cfg.kws_model_dir, "/data/open-xiaoai/kws/models");
        assert_eq!(cfg.kws_lib, "");
        // 服务端唤醒是默认模式（KWS 在 NAS 边车，音箱不要求本地后端）。
        assert_eq!(cfg.kws_mode, "server");
        assert!(cfg.is_server_kws_mode());
        assert!(!cfg.is_local_kws_mode());
        // 空 hub_url → validate 先报 hub_url 必填（在指纹校验之前）。
        let err = cfg
            .validate()
            .expect_err("default config without hub_url must fail validation");
        assert!(
            err.to_string().contains("hub_url is required"),
            "unexpected error: {err}"
        );
        // 补上 hub_url 后，P1-10：无指纹 + tls=true 仍拒绝启动
        // （以前只 warn 走系统根证书）。配上合法指纹后才通过。
        let mut with_url = cfg.clone();
        with_url.hub_url = "wss://192.168.1.10:4399".to_string();
        let err = with_url
            .validate()
            .expect_err("config without fingerprint must fail validation");
        assert!(
            err.to_string().contains("hub_ca_fingerprint is required"),
            "unexpected error: {err}"
        );
        let mut with_fp = with_url.clone();
        with_fp.hub_ca_fingerprint = Some("aa".repeat(32));
        assert!(with_fp.validate().is_ok());
    }

    #[test]
    fn missing_or_empty_hub_url_fails_startup_with_clear_error() {
        // C-15 / P2-42：hub_url 必填——TOML 里缺字段与显式空串都拒绝启动，
        // 报错点名 hub_url（不只报 scheme 非法）。
        let fp_line = "hub_ca_fingerprint = \"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\"";
        let err = load_from_str(fp_line).expect_err("missing hub_url must fail");
        assert!(
            err.to_string().contains("hub_url is required"),
            "unexpected error: {err}"
        );
        let err = load_from_str(&format!("hub_url = \"\"\n{fp_line}\n"))
            .expect_err("empty hub_url must fail");
        assert!(
            err.to_string().contains("hub_url is required"),
            "unexpected error: {err}"
        );
        // 只有空白字符同样拒绝。
        let err = load_from_str(&format!("hub_url = \"   \"\n{fp_line}\n"))
            .expect_err("blank hub_url must fail");
        assert!(
            err.to_string().contains("hub_url is required"),
            "unexpected error: {err}"
        );
        // 构造出来的非法 Config 直接调 validate 也必须报错（不只走解析路径）。
        let cfg = Config {
            hub_url: String::new(),
            ..Config::default()
        };
        let err = cfg.validate().expect_err("empty hub_url must fail");
        assert!(
            err.to_string().contains("hub_url is required"),
            "unexpected error: {err}"
        );
    }

    #[test]
    fn kws_mode_local_parses_and_server_explicit_parses() {
        // P1-10：load_from_str 走 validate，tls=true 时须带合法指纹。
        // C-15：hub_url 必填，TOML 里必须显式写。
        let fp_line = format!("hub_ca_fingerprint = \"{}\"", "aa".repeat(32));
        let hub_line = "hub_url = \"wss://192.168.1.10:4399\"";
        let local =
            load_from_str(&format!("kws_mode = \"local\"\n{hub_line}\n{fp_line}\n")).unwrap();
        assert_eq!(local.kws_mode, "local");
        assert!(local.is_local_kws_mode());
        assert!(!local.is_server_kws_mode());
        let server =
            load_from_str(&format!("kws_mode = \"server\"\n{hub_line}\n{fp_line}\n")).unwrap();
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
            allow_insecure_ws = true
            hub_ca_fingerprint = "AA:bb:CC:dd:ee:ff:00:11:22:33:44:55:66:77:88:99:AA:bb:CC:dd:ee:ff:00:11:22:33:44:55:66:77:88:99"
            room = "客厅"
            "#,
        )
        .unwrap();
        assert_eq!(cfg.hub_url, "ws://127.0.0.1:4399");
        assert_eq!(cfg.token, "secret");
        assert_eq!(cfg.model, "oh2p");
        assert!(!cfg.tls);
        assert!(cfg.allow_insecure_ws);
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
        // P1-10：tls=true 默认须带指纹，这里补一个合法指纹（64 位 hex）。
        let cfg = load_from_str(
            "hub_url = \"wss://10.0.0.2:4399\"\ntoken = \"x\"\nhub_ca_fingerprint = \"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\"\n",
        )
        .unwrap();
        assert_eq!(cfg.hub_url, "wss://10.0.0.2:4399");
        assert_eq!(cfg.wake_word, "你好小李");
        assert!(cfg.tls);
        assert!(cfg.has_valid_fingerprint());
    }

    #[test]
    fn malformed_fingerprint_detected() {
        let mut cfg = Config {
            hub_ca_fingerprint: Some("not-a-fingerprint".to_string()),
            ..Config::default()
        };
        assert!(cfg.normalized_fingerprint().is_none());
        assert!(cfg.has_malformed_fingerprint());
        cfg.hub_ca_fingerprint = Some("aabb".to_string());
        assert!(cfg.has_malformed_fingerprint());
    }

    #[test]
    fn missing_file_is_clear_error_not_silent_default() {
        // P2-42：配置文件缺失必须明确报错（写清路径与补救方法），
        // 不能静默回退默认配置（默认 hub_url 是写死的 192.168.1.10）。
        let err = load_from_path("/tmp/definitely-not-exist-open-xiaoai-client.toml")
            .expect_err("missing config file must fail loud");
        let msg = err.to_string();
        assert!(msg.contains("not found"), "unexpected error: {msg}");
        assert!(
            msg.contains("/tmp/definitely-not-exist-open-xiaoai-client.toml"),
            "error must name the missing path: {msg}"
        );
        assert!(msg.contains("client.toml"), "unexpected error: {msg}");
    }

    // --- P1-10 收紧 + P2-N3 / model / ram_mb 校验 ---
    #[test]
    fn validate_requires_fingerprint_when_tls() {
        // C-15：hub_url 必填，构造测试配置时必须显式给。
        let mut cfg = Config {
            hub_url: "wss://192.168.1.10:4399".to_string(),
            hub_ca_fingerprint: Some("aa".repeat(32)),
            ..Config::default()
        };
        assert!(cfg.validate().is_ok());
        // 指纹格式非法 → 报错（不是 warn 后继续）。
        cfg.hub_ca_fingerprint = Some("xyz".to_string());
        let err = cfg.validate().expect_err("malformed fingerprint must fail");
        assert!(
            err.to_string().contains("malformed"),
            "unexpected error: {err}"
        );
        // 指纹缺失 → 报错（P1-10：不再只 warn 走系统根证书）。
        cfg.hub_ca_fingerprint = None;
        let err = cfg.validate().expect_err("missing fingerprint must fail");
        assert!(
            err.to_string().contains("hub_ca_fingerprint is required"),
            "unexpected error: {err}"
        );
    }

    #[test]
    fn validate_tls_false_needs_explicit_opt_in() {
        let mut cfg = Config {
            tls: false,
            hub_url: "ws://127.0.0.1:4399".to_string(),
            ..Config::default()
        };
        // 无 opt-in → 拒绝启动（P1-10：不再只 warn 照常启动）。
        let err = cfg
            .validate()
            .expect_err("tls=false must fail without opt-in");
        assert!(
            err.to_string().contains("allow_insecure_ws"),
            "unexpected error: {err}"
        );
        // 显式 opt-in 后通过。
        cfg.allow_insecure_ws = true;
        assert!(cfg.validate().is_ok());
        // P2-N3：tls=false + wss:// → 拒绝（会悄悄走系统根证书 TLS）。
        cfg.hub_url = "wss://127.0.0.1:4399".to_string();
        let err = cfg.validate().expect_err("tls=false with wss:// must fail");
        assert!(err.to_string().contains("ws://"), "unexpected error: {err}");
    }

    #[test]
    fn validate_rejects_bad_hub_url_model_ram() {
        let mut cfg = Config {
            hub_ca_fingerprint: Some("aa".repeat(32)),
            hub_url: "192.168.1.10:4399".to_string(),
            ..Config::default()
        };
        assert!(cfg.validate().is_err(), "hub_url without scheme must fail");
        cfg.hub_url = "wss://192.168.1.10:4399".to_string();
        cfg.model = "lx05".to_string();
        assert!(cfg.validate().is_err(), "unknown model must fail");
        cfg.model = "oh2p".to_string();
        cfg.ram_mb = 0;
        assert!(cfg.validate().is_err(), "ram_mb 0 must fail");
        cfg.ram_mb = 256;
        assert!(cfg.validate().is_ok());
    }
}
