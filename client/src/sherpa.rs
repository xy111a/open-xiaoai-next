//! Sherpa-ONNX KWS 后端（feature `sherpa-kws` 门控，默认不开）。
//!
//! 用 sherpa-onnx 的 C API（`libsherpa-onnx-c-api.so`）跑 WenetSpeech
//! kws-zipformer 模型，检自定义唤醒词（默认“你好小李”）。
//! 模型、实验结果与下载方法见 `kws/README.md`；本模块只负责把模型接进
//! `Kws::feed`，不负责下载模型。
//!
//! 设计要点：
//!   - 动态加载：用 `libloading` 在运行时 `dlopen` .so，不做链接期依赖。
//!     因此 ARMv7 交叉编译不受影响（feature 默认不开；即使开了也只是
//!     多一个 dlopen 调用，链接不找 .so）。运行时刻 .so 不存在 → `open()`
//!     返回明确错误；P1-40 起 `Kws::new` 不再静默回退纯 RMS，
//!     而是直接 Err、Client 非零退出，绝不谎报唤醒、也不静默失能。
//!   - 结构体布局：`#[repr(C)]` 逐字段对照 sherpa-onnx 2.0.1 的
//!     `sherpa-onnx/c-api/c-api.h`；64-bit 下尺寸已用 C 探针锚定
//!     （见本文件测试 `c_api_struct_sizes_match_c_header`：
//!     OnlineModelConfig=136，KeywordSpotterConfig=184，KeywordResult=56）。
//!     32-bit ARM 上指针 4 字节，尺寸不同是正常的（repr(C) 自动算）。
//!   - 线程：sherpa C API 的调用全由 `&mut self` 串行化；`SherpaSpotter`
//!     只被录音任务独占，因此 `unsafe impl Send`（不实现 Sync）。
//!   - 音箱端现状（P1-40，2026-11-17 实测，P1-N3 纠正后）：ARM 版
//!     `libsherpa-onnx-c-api.so` 已取到（官方 v2.0.1
//!     linux-arm-gnueabihf 预编译，见 `kws/vendor-arm/SOURCE.md`），但
//!     **仍不能在音箱上加载**：官方 v2.0.1 的两个 ARM `.so`
//!     （`libsherpa-onnx-c-api.so` 与 `libonnxruntime.so`）都要求
//!     GLIBC 最高 2.34 + GLIBCXX 最高 3.4.29（`readelf -V` 实测），
//!     音箱只有 glibc 2.25 / libstdc++ 3.4.22。**“ORT 只要求
//!     GLIBC_2.4”的旧说法已被证伪**（见 AUDIT_ROUND2.md P1-N3）。
//!     早先的第二层障碍（musl 静态 Client 不能 dlopen）已随构建目标
//!     改为 gnueabihf（动态链接）而消失；剩下 glibc 版本问题，须从
//!     源码重编 ORT + sherpa，或 KWS 继续放 NAS 边车——**当前唯一能
//!     工作的唤醒路径是 server 模式**（Hub → `kws-sidecar`，见
//!     `TEST_REPORT.md`“服务端唤醒”一节）。参考 kws 二进制 sidecar +
//!     `KwsLogMonitor` 的过渡路线已于 2026-11-17 作废，不再接线。
//!     本模块的 FFI 定义本身与上述无关：32-bit ARM 上指针 4 字节，
//!     结构体尺寸见测试 `c_api_struct_sizes_match_c_header` 的 32-bit 分支。

#![cfg(feature = "sherpa-kws")]

use std::ffi::{c_char, c_float, c_int, CStr, CString};
use std::path::Path;

use anyhow::{anyhow, Result};

use crate::audio::FRAME_SAMPLES;

/// 构建断言用的标记：`build-armv7.sh` 用 `strings` 在产物里找它，
/// 证明该 ARM 二进制确实开了 `sherpa-kws` feature（dlopen 是运行时行为，
/// 链接期看不出来；没有这个标记，feature 没开也能编出一个能跑的二进制，
/// P1-40 就是这么漏网的）。`#[used]` 保证 LTO/strip 后仍保留。
#[used]
pub static SHERPA_KWS_MARKER: &[u8] = b"open-xiaoai-kws-backend=sherpa-onnx-2.0.1\0";

// ---------------------------------------------------------------------------
// C API 结构体（sherpa-onnx 2.0.1 c-api.h，字段顺序与类型必须与头文件一致）
// ---------------------------------------------------------------------------

/// SherpaOnnxFeatureConfig：{ sample_rate: i32, feature_dim: i32 }。
#[repr(C)]
#[derive(Debug, Clone, Copy)]
struct FeatureConfig {
    sample_rate: c_int,
    feature_dim: c_int,
}

/// SherpaOnnxOnlineTransducerModelConfig：{ encoder, decoder, joiner }。
#[repr(C)]
#[derive(Debug, Clone, Copy)]
struct TransducerModelConfig {
    encoder: *const c_char,
    decoder: *const c_char,
    joiner: *const c_char,
}

/// SherpaOnnxOnlineParaformerModelConfig：{ encoder, decoder }。
#[repr(C)]
#[derive(Debug, Clone, Copy)]
struct ParaformerModelConfig {
    encoder: *const c_char,
    decoder: *const c_char,
}

/// SherpaOnnxOnlineZipformer2CtcModelConfig：{ model }。
#[repr(C)]
#[derive(Debug, Clone, Copy)]
struct Zipformer2CtcModelConfig {
    model: *const c_char,
}

/// SherpaOnnxOnlineNemoCtcModelConfig：{ model }。
#[repr(C)]
#[derive(Debug, Clone, Copy)]
struct NemoCtcModelConfig {
    model: *const c_char,
}

/// SherpaOnnxOnlineToneCtcModelConfig：{ model }。
#[repr(C)]
#[derive(Debug, Clone, Copy)]
struct ToneCtcModelConfig {
    model: *const c_char,
}

/// SherpaOnnxOnlineModelConfig（64-bit 下 136 字节，见测试）。
#[repr(C)]
#[derive(Debug, Clone, Copy)]
struct OnlineModelConfig {
    transducer: TransducerModelConfig,
    paraformer: ParaformerModelConfig,
    zipformer2_ctc: Zipformer2CtcModelConfig,
    tokens: *const c_char,
    num_threads: c_int,
    provider: *const c_char,
    debug: c_int,
    model_type: *const c_char,
    modeling_unit: *const c_char,
    bpe_vocab: *const c_char,
    tokens_buf: *const c_char,
    tokens_buf_size: c_int,
    nemo_ctc: NemoCtcModelConfig,
    t_one_ctc: ToneCtcModelConfig,
}

/// SherpaOnnxKeywordSpotterConfig（64-bit 下 184 字节，见测试）。
#[repr(C)]
#[derive(Debug, Clone, Copy)]
struct KeywordSpotterConfig {
    feat_config: FeatureConfig,
    model_config: OnlineModelConfig,
    max_active_paths: c_int,
    num_trailing_blanks: c_int,
    keywords_score: c_float,
    keywords_threshold: c_float,
    keywords_file: *const c_char,
    keywords_buf: *const c_char,
    keywords_buf_size: c_int,
}

/// SherpaOnnxKeywordResult（64-bit 下 56 字节，见测试）。
/// 只读 `keyword` 字段（触发词原文）；其余字段不读。
#[repr(C)]
#[derive(Debug, Clone, Copy)]
struct KeywordResult {
    keyword: *const c_char,
    tokens: *const c_char,
    tokens_arr: *const *const c_char,
    count: c_int,
    timestamps: *mut c_float,
    start_time: c_float,
    json: *const c_char,
}

enum SpotterOpaque {}
enum StreamOpaque {}

type CreateSpotterFn = unsafe extern "C" fn(*const KeywordSpotterConfig) -> *const SpotterOpaque;
type DestroySpotterFn = unsafe extern "C" fn(*const SpotterOpaque);
type CreateStreamFn = unsafe extern "C" fn(*const SpotterOpaque) -> *const StreamOpaque;
type DestroyStreamFn = unsafe extern "C" fn(*const StreamOpaque);
type AcceptWaveformFn = unsafe extern "C" fn(*const StreamOpaque, c_int, *const c_float, c_int);
type IsReadyFn = unsafe extern "C" fn(*const SpotterOpaque, *const StreamOpaque) -> c_int;
type DecodeFn = unsafe extern "C" fn(*const SpotterOpaque, *const StreamOpaque);
type ResetStreamFn = unsafe extern "C" fn(*const SpotterOpaque, *const StreamOpaque);
type GetResultFn =
    unsafe extern "C" fn(*const SpotterOpaque, *const StreamOpaque) -> *const KeywordResult;
type DestroyResultFn = unsafe extern "C" fn(*const KeywordResult);

/// 从 .so 里解析出的函数表（libloading 符号，生命周期与 Library 绑定）。
/// 注意字段顺序：`_lib` 必须最后声明（struct 按声明顺序 drop，Library
/// 要最后释放，符号先于它失效才安全；符号本身 Drop 是空操作，但顺序要对）。
struct Api {
    create_spotter: libloading::Symbol<'static, CreateSpotterFn>,
    destroy_spotter: libloading::Symbol<'static, DestroySpotterFn>,
    create_stream: libloading::Symbol<'static, CreateStreamFn>,
    destroy_stream: libloading::Symbol<'static, DestroyStreamFn>,
    accept_waveform: libloading::Symbol<'static, AcceptWaveformFn>,
    is_ready: libloading::Symbol<'static, IsReadyFn>,
    decode: libloading::Symbol<'static, DecodeFn>,
    reset_stream: libloading::Symbol<'static, ResetStreamFn>,
    get_result: libloading::Symbol<'static, GetResultFn>,
    destroy_result: libloading::Symbol<'static, DestroyResultFn>,
    _lib: libloading::Library,
}

/// 动态加载的 sherpa-onnx KWS：一个 spotter + 一条流式 stream。
/// `feed_frame` 每 20ms 喂一帧 PCM，命中返回关键词原文，否则 None。
pub struct SherpaSpotter {
    api: Api,
    spotter: *const SpotterOpaque,
    stream: *const StreamOpaque,
    /// 路径 CString 必须一直活着：config 里的裸指针指向它们。
    _paths: Vec<CString>,
}

// spotter/stream 只在持有 `&mut self` 的录音任务里串行调用；
// Library 与裸指针不同线程共享时无数据竞争（调用本身已串行）。
unsafe impl Send for SherpaSpotter {}

impl SherpaSpotter {
    /// 默认 .so 候选名（按系统库路径搜索；音箱上由部署脚本放到固定位置）。
    pub const DEFAULT_LIB_NAMES: &'static [&'static str] =
        &["libsherpa-onnx-c-api.so", "libsherpa-onnx-c-api.so.2"];

    /// 打开模型。
    /// - `model_dir`：含 encoder/decoder/joiner .onnx + tokens.txt 的目录
    ///   （见 `scripts/download-kws-model.sh` 产出的布局；encoder 优先 int8，
    ///   没有 int8 时回退 fp32）。
    /// - `keywords_file`：`text2token` 生成的 keywords.txt（ppinyin 格式，
    ///   例如 `n ǐ h ǎo x iǎo l ǐ @你好小李`）。
    /// - `lib_path`：.so 路径；空串 = 按 DEFAULT_LIB_NAMES 依次尝试。
    ///   非空时只试该路径（失败即报错，不回退默认名，避免静默加载到
    ///   别的同名库）。
    /// - `num_threads`：ONNX Runtime 线程数（音箱上用 1）。
    pub fn open(
        model_dir: &str,
        keywords_file: &str,
        lib_path: &str,
        num_threads: i32,
    ) -> Result<Self> {
        let api = Self::load_api(lib_path)?;
        let dir = Path::new(model_dir);
        let (enc_int8_name, dec_name, join_name, tokens_name) = model_file_names();
        let encoder_int8 = dir.join(enc_int8_name);
        let encoder_fp32 = dir.join("encoder-epoch-12-avg-2-chunk-16-left-64.onnx");
        let encoder = if encoder_int8.exists() {
            encoder_int8
        } else if encoder_fp32.exists() {
            tracing::warn!(
                "sherpa int8 encoder not found, falling back to fp32 encoder (larger, slower)"
            );
            encoder_fp32
        } else {
            return Err(anyhow!(
                "sherpa KWS encoder not found in {model_dir} (looked for {})",
                encoder_int8.display()
            ));
        };
        let decoder = dir.join(dec_name);
        let joiner = dir.join(join_name);
        let tokens = dir.join(tokens_name);
        for (what, p) in [
            ("decoder", &decoder),
            ("joiner", &joiner),
            ("tokens.txt", &tokens),
        ] {
            if !p.exists() {
                return Err(anyhow!("sherpa KWS {what} not found: {}", p.display()));
            }
        }
        if !Path::new(keywords_file).exists() {
            return Err(anyhow!(
                "sherpa KWS keywords file not found: {keywords_file}"
            ));
        }
        Self::open_with_files(
            api,
            &encoder,
            &decoder,
            &joiner,
            &tokens,
            keywords_file,
            num_threads,
        )
    }

    fn load_api(lib_path: &str) -> Result<Api> {
        // 取符号并把借用期延长为 'static：符号与 Library 存在同一 struct 里，
        // Library 字段最后 drop，符号不可能先于 Library 失效。
        macro_rules! sym {
            ($lib:expr, $name:literal, $ty:ty) => {{
                let s: libloading::Symbol<'_, $ty> = unsafe { $lib.get($name) }.map_err(|e| {
                    anyhow!(
                        "sherpa symbol {} missing: {e}",
                        String::from_utf8_lossy($name)
                    )
                })?;
                unsafe {
                    std::mem::transmute::<
                        libloading::Symbol<'_, $ty>,
                        libloading::Symbol<'static, $ty>,
                    >(s)
                }
            }};
        }
        let mut candidates: Vec<String> = vec![];
        if !lib_path.is_empty() {
            // 显式路径只试它自己：失败即报错，不回退默认名（否则可能静默
            // 加载到系统里另一个同名库，且测试会因进程内已加载的库而假通过）。
            candidates.push(lib_path.to_string());
        } else {
            candidates.extend(Self::DEFAULT_LIB_NAMES.iter().map(|s| s.to_string()));
        }
        let mut last_err = String::new();
        for cand in &candidates {
            match unsafe { libloading::Library::new(cand) } {
                Ok(lib) => {
                    let api = Api {
                        create_spotter: sym!(
                            &lib,
                            b"SherpaOnnxCreateKeywordSpotter\0",
                            CreateSpotterFn
                        ),
                        destroy_spotter: sym!(
                            &lib,
                            b"SherpaOnnxDestroyKeywordSpotter\0",
                            DestroySpotterFn
                        ),
                        create_stream: sym!(
                            &lib,
                            b"SherpaOnnxCreateKeywordStream\0",
                            CreateStreamFn
                        ),
                        destroy_stream: sym!(
                            &lib,
                            b"SherpaOnnxDestroyOnlineStream\0",
                            DestroyStreamFn
                        ),
                        accept_waveform: sym!(
                            &lib,
                            b"SherpaOnnxOnlineStreamAcceptWaveform\0",
                            AcceptWaveformFn
                        ),
                        is_ready: sym!(&lib, b"SherpaOnnxIsKeywordStreamReady\0", IsReadyFn),
                        decode: sym!(&lib, b"SherpaOnnxDecodeKeywordStream\0", DecodeFn),
                        reset_stream: sym!(&lib, b"SherpaOnnxResetKeywordStream\0", ResetStreamFn),
                        get_result: sym!(&lib, b"SherpaOnnxGetKeywordResult\0", GetResultFn),
                        destroy_result: sym!(
                            &lib,
                            b"SherpaOnnxDestroyKeywordResult\0",
                            DestroyResultFn
                        ),
                        _lib: lib,
                    };
                    tracing::info!("sherpa-onnx C API loaded from {cand}");
                    return Ok(api);
                }
                Err(e) => {
                    last_err = format!("{cand}: {e}");
                }
            }
        }
        Err(anyhow!(
            "cannot load libsherpa-onnx-c-api.so (tried: {}); last error: {last_err}",
            candidates.join(", ")
        ))
    }

    #[allow(clippy::too_many_arguments)]
    fn open_with_files(
        api: Api,
        encoder: &Path,
        decoder: &Path,
        joiner: &Path,
        tokens: &Path,
        keywords_file: &str,
        num_threads: i32,
    ) -> Result<Self> {
        let c_encoder = path_cstring(encoder)?;
        let c_decoder = path_cstring(decoder)?;
        let c_joiner = path_cstring(joiner)?;
        let c_tokens = path_cstring(tokens)?;
        let c_keywords = CString::new(keywords_file)
            .map_err(|_| anyhow!("keywords file path contains NUL: {keywords_file}"))?;
        let c_provider = CString::new("cpu").unwrap();
        let c_model_type = CString::new("").unwrap();
        let c_modeling_unit = CString::new("").unwrap();
        let c_bpe_vocab = CString::new("").unwrap();
        // 注意：config 里的裸指针指向下面这些 CString，`paths` 必须与
        // spotter 同寿命（存在 Self._paths 里），不能是临时量。
        let config = KeywordSpotterConfig {
            feat_config: FeatureConfig {
                sample_rate: 16_000,
                feature_dim: 80,
            },
            model_config: OnlineModelConfig {
                transducer: TransducerModelConfig {
                    encoder: c_encoder.as_ptr(),
                    decoder: c_decoder.as_ptr(),
                    joiner: c_joiner.as_ptr(),
                },
                paraformer: ParaformerModelConfig {
                    encoder: std::ptr::null(),
                    decoder: std::ptr::null(),
                },
                zipformer2_ctc: Zipformer2CtcModelConfig {
                    model: std::ptr::null(),
                },
                tokens: c_tokens.as_ptr(),
                num_threads,
                provider: c_provider.as_ptr(),
                debug: 0,
                model_type: c_model_type.as_ptr(),
                modeling_unit: c_modeling_unit.as_ptr(),
                bpe_vocab: c_bpe_vocab.as_ptr(),
                tokens_buf: std::ptr::null(),
                tokens_buf_size: 0,
                nemo_ctc: NemoCtcModelConfig {
                    model: std::ptr::null(),
                },
                t_one_ctc: ToneCtcModelConfig {
                    model: std::ptr::null(),
                },
            },
            max_active_paths: 4,
            num_trailing_blanks: 1,
            // 与 Python 冒烟实验一致的默认值（见 kws/README.md）：
            // score 1.0 / threshold 0.25 时 5/5 正样本检出、5/5 负样本静默。
            keywords_score: 1.0,
            keywords_threshold: 0.25,
            keywords_file: c_keywords.as_ptr(),
            keywords_buf: std::ptr::null(),
            keywords_buf_size: 0,
        };
        // SAFETY：config 字段全是合法 CString 裸指针或 null；spotter 非空才继续。
        let spotter = unsafe { (api.create_spotter)(&config) };
        if spotter.is_null() {
            return Err(anyhow!(
                "SherpaOnnxCreateKeywordSpotter returned NULL (model files: encoder={} decoder={} joiner={} tokens={} keywords={})",
                encoder.display(),
                decoder.display(),
                joiner.display(),
                tokens.display(),
                keywords_file
            ));
        }
        // SAFETY：spotter 刚创建成功；stream 非空才继续。
        let stream = unsafe { (api.create_stream)(spotter) };
        if stream.is_null() {
            unsafe { (api.destroy_spotter)(spotter) };
            return Err(anyhow!("SherpaOnnxCreateKeywordStream returned NULL"));
        }
        tracing::info!(
            "sherpa KWS loaded: encoder={} keywords={}",
            encoder.display(),
            keywords_file
        );
        Ok(Self {
            api,
            spotter,
            stream,
            _paths: vec![
                c_encoder,
                c_decoder,
                c_joiner,
                c_tokens,
                c_keywords,
                c_provider,
                c_model_type,
                c_modeling_unit,
                c_bpe_vocab,
            ],
        })
    }

    /// 喂一帧 20ms PCM（320 采样点 @16kHz）；命中返回关键词原文。
    /// 命中后自动 reset stream，下一句可独立检出。
    pub fn feed_frame(&mut self, pcm: &[i16; FRAME_SAMPLES]) -> Result<Option<String>> {
        let samples: Vec<c_float> = pcm.iter().map(|s| (*s as c_float) / 32768.0).collect();
        // SAFETY：stream 由 open() 创建且未销毁；samples 长度 320，指针有效。
        unsafe {
            (self.api.accept_waveform)(
                self.stream,
                16_000,
                samples.as_ptr(),
                FRAME_SAMPLES as c_int,
            );
            while (self.api.is_ready)(self.spotter, self.stream) != 0 {
                (self.api.decode)(self.spotter, self.stream);
            }
            let result = (self.api.get_result)(self.spotter, self.stream);
            if result.is_null() {
                return Ok(None);
            }
            let keyword = if (*result).keyword.is_null() {
                String::new()
            } else {
                CStr::from_ptr((*result).keyword)
                    .to_string_lossy()
                    .into_owned()
            };
            (self.api.destroy_result)(result);
            if keyword.is_empty() {
                return Ok(None);
            }
            (self.api.reset_stream)(self.spotter, self.stream);
            Ok(Some(keyword))
        }
    }
}

impl Drop for SherpaSpotter {
    fn drop(&mut self) {
        // SAFETY：指针由 open() 创建、未提前释放；先毁 stream 再毁 spotter。
        unsafe {
            if !self.stream.is_null() {
                (self.api.destroy_stream)(self.stream);
            }
            if !self.spotter.is_null() {
                (self.api.destroy_spotter)(self.spotter);
            }
        }
    }
}

fn path_cstring(p: &Path) -> Result<CString> {
    let s = p
        .to_str()
        .ok_or_else(|| anyhow!("model path is not valid UTF-8: {}", p.display()))?;
    CString::new(s).map_err(|_| anyhow!("model path contains NUL: {s}"))
}

/// 模型目录默认布局的文件名（供 `SherpaSpotter::open` 与下载脚本共用）。
pub fn model_file_names() -> (&'static str, &'static str, &'static str, &'static str) {
    (
        "encoder-epoch-12-avg-2-chunk-16-left-64.int8.onnx",
        "decoder-epoch-12-avg-2-chunk-16-left-64.onnx",
        "joiner-epoch-12-avg-2-chunk-16-left-64.onnx",
        "tokens.txt",
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::mem::size_of;
    use std::path::PathBuf;

    #[test]
    fn c_api_struct_sizes_match_c_header() {
        // C 探针（sherpa-onnx 2.0.1 c-api.h，x86_64）实测值；
        // 32-bit ARM 上指针 4 字节，尺寸不同是正常的，故只在 64-bit 断言。
        #[cfg(target_pointer_width = "64")]
        {
            assert_eq!(size_of::<FeatureConfig>(), 8);
            assert_eq!(size_of::<TransducerModelConfig>(), 24);
            assert_eq!(size_of::<ParaformerModelConfig>(), 16);
            assert_eq!(size_of::<Zipformer2CtcModelConfig>(), 8);
            assert_eq!(size_of::<NemoCtcModelConfig>(), 8);
            assert_eq!(size_of::<ToneCtcModelConfig>(), 8);
            assert_eq!(size_of::<OnlineModelConfig>(), 136);
            assert_eq!(size_of::<KeywordSpotterConfig>(), 184);
            assert_eq!(size_of::<KeywordResult>(), 56);
            assert_eq!(std::mem::offset_of!(KeywordSpotterConfig, model_config), 8);
            assert_eq!(
                std::mem::offset_of!(KeywordSpotterConfig, keywords_file),
                160
            );
            assert_eq!(std::mem::offset_of!(OnlineModelConfig, tokens), 48);
        }
        // 32-bit ARM（音箱目标）：指针 4 字节，全部字段 4 字节对齐，无填充。
        // 手算：OnlineModelConfig = 12+8+4+4+4+4+4+4+4+4+4+4+4+4 = 68；
        // KeywordSpotterConfig = 8+68+4+4+4+4+4+4+4 = 104；
        // KeywordResult = 4*7 = 28。
        // 在 x86_64 宿主机上跑 cargo test 时本分支不执行；ARM 交叉测试
        // （或真机）跑到它时才生效——写在这里是把 32-bit 布局钉死，
        // 免得以后有人只按 64-bit 的 136/184/56 去排错。
        #[cfg(target_pointer_width = "32")]
        {
            assert_eq!(size_of::<FeatureConfig>(), 8);
            assert_eq!(size_of::<TransducerModelConfig>(), 12);
            assert_eq!(size_of::<ParaformerModelConfig>(), 8);
            assert_eq!(size_of::<Zipformer2CtcModelConfig>(), 4);
            assert_eq!(size_of::<NemoCtcModelConfig>(), 4);
            assert_eq!(size_of::<ToneCtcModelConfig>(), 4);
            assert_eq!(size_of::<OnlineModelConfig>(), 68);
            assert_eq!(size_of::<KeywordSpotterConfig>(), 104);
            assert_eq!(size_of::<KeywordResult>(), 28);
            assert_eq!(std::mem::offset_of!(KeywordSpotterConfig, model_config), 8);
            assert_eq!(
                std::mem::offset_of!(KeywordSpotterConfig, keywords_file),
                92
            );
            assert_eq!(std::mem::offset_of!(OnlineModelConfig, tokens), 24);
        }
    }

    /// 读 16-bit 单声道 16kHz WAV（测试夹具用，不做通用音频解析）。
    fn read_test_wav(path: &Path) -> Vec<i16> {
        let bytes = std::fs::read(path).unwrap();
        assert!(bytes.len() > 44, "wav too short: {}", path.display());
        assert_eq!(&bytes[0..4], b"RIFF", "not a RIFF wav: {}", path.display());
        assert_eq!(&bytes[8..12], b"WAVE", "not a WAVE wav: {}", path.display());
        // 找 data chunk（跳过 fmt 等 chunk；Piper/scipy 产的 wav 结构简单）。
        let mut off = 12;
        let mut data: Option<&[u8]> = None;
        while off + 8 <= bytes.len() {
            let id = &bytes[off..off + 4];
            let size = u32::from_le_bytes(bytes[off + 4..off + 8].try_into().unwrap()) as usize;
            if id == b"data" {
                data = Some(&bytes[off + 8..(off + 8 + size).min(bytes.len())]);
                break;
            }
            off += 8 + size + (size % 2);
        }
        let data = data.expect("no data chunk in wav");
        assert_eq!(data.len() % 2, 0, "odd wav data length");
        data.as_chunks::<2>()
            .0
            .iter()
            .map(|c| i16::from_le_bytes([c[0], c[1]]))
            .collect()
    }

    /// Rust FFI 端到端：用真实模型 + 真实 TTS 样本跑 SherpaSpotter。
    /// 需要环境变量（实验夹具在仓库外，不提交模型/样本进仓库）：
    ///   SHERPA_KWS_TEST_MODEL_DIR（含 encoder/decoder/joiner/tokens.txt）
    ///   SHERPA_KWS_TEST_LIB（libsherpa-onnx-c-api.so 路径）
    ///   SHERPA_KWS_TEST_SAMPLES（含 pos1.wav / neg3.wav，16kHz 单声道）
    ///   SHERPA_KWS_TEST_KEYWORDS（keywords.txt，含“你好小李”一行）
    /// 未设时跳过（打印 SKIP，不算失败）；设了就必须全过。
    #[test]
    fn sherpa_detects_nihaoxiaoli_tts_sample() {
        let model_dir = match std::env::var("SHERPA_KWS_TEST_MODEL_DIR") {
            Ok(v) => v,
            Err(_) => {
                eprintln!("SKIP: SHERPA_KWS_TEST_MODEL_DIR not set (needs real model dir)");
                return;
            }
        };
        let lib = match std::env::var("SHERPA_KWS_TEST_LIB") {
            Ok(v) => v,
            Err(_) => {
                eprintln!("SKIP: SHERPA_KWS_TEST_LIB not set (needs libsherpa-onnx-c-api.so)");
                return;
            }
        };
        let samples = match std::env::var("SHERPA_KWS_TEST_SAMPLES") {
            Ok(v) => v,
            Err(_) => {
                eprintln!("SKIP: SHERPA_KWS_TEST_SAMPLES not set");
                return;
            }
        };
        let keywords = match std::env::var("SHERPA_KWS_TEST_KEYWORDS") {
            Ok(v) => v,
            Err(_) => {
                eprintln!("SKIP: SHERPA_KWS_TEST_KEYWORDS not set");
                return;
            }
        };
        let samples_dir = PathBuf::from(&samples);

        // 正样本：Piper TTS“你好小李”（1.24s）+ 1s 静音尾巴（与 Python 实验一致）。
        let mut spotter = SherpaSpotter::open(&model_dir, &keywords, &lib, 1)
            .expect("SherpaSpotter::open must succeed with real model+lib");
        let pos = read_test_wav(&samples_dir.join("pos1.wav"));
        let mut hit: Option<String> = None;
        let feed = |s: &mut SherpaSpotter, pcm: &[i16]| -> Option<String> {
            let mut out = None;
            for frame in pcm.as_chunks::<FRAME_SAMPLES>().0 {
                if let Some(kw) = s.feed_frame(frame).unwrap() {
                    out = Some(kw);
                }
            }
            out
        };
        if let Some(kw) = feed(&mut spotter, &pos) {
            hit = Some(kw);
        }
        if hit.is_none() {
            // 尾部静音：关键词后的帧让解码器吐出结果（真实流式使用中一直有帧）。
            let silence = vec![0i16; FRAME_SAMPLES * 50];
            if let Some(kw) = feed(&mut spotter, &silence) {
                hit = Some(kw);
            }
        }
        assert_eq!(hit.as_deref(), Some("你好小李"), "pos1.wav must trigger");

        // 负样本：同一 TTS 声音说的无关句子，不应检出。
        let mut spotter2 = SherpaSpotter::open(&model_dir, &keywords, &lib, 1)
            .expect("second SherpaSpotter::open must succeed");
        let neg = read_test_wav(&samples_dir.join("neg3.wav"));
        let mut hit2: Option<String> = None;
        if let Some(kw) = feed(&mut spotter2, &neg) {
            hit2 = Some(kw);
        }
        if hit2.is_none() {
            let silence = vec![0i16; FRAME_SAMPLES * 50];
            if let Some(kw) = feed(&mut spotter2, &silence) {
                hit2 = Some(kw);
            }
        }
        assert_eq!(hit2, None, "neg3.wav must stay silent");
    }

    #[test]
    fn sherpa_open_missing_lib_reports_clear_error() {
        let err = match SherpaSpotter::open(
            "/tmp/no-such-model-dir",
            "/tmp/no-such-keywords.txt",
            "/tmp/no-such-libsherpa.so",
            1,
        ) {
            Ok(_) => panic!("open with a missing .so must fail"),
            Err(e) => e,
        };
        assert!(
            err.to_string().contains("libsherpa-onnx-c-api"),
            "unexpected error: {err}"
        );
    }

    #[test]
    fn sherpa_open_missing_model_reports_clear_error() {
        // .so 存在但模型目录不存在 → 应报模型文件缺失，而不是段错误。
        let lib = match std::env::var("SHERPA_KWS_TEST_LIB") {
            Ok(v) => v,
            Err(_) => {
                eprintln!("SKIP: SHERPA_KWS_TEST_LIB not set");
                return;
            }
        };
        let err = match SherpaSpotter::open(
            "/tmp/no-such-model-dir",
            "/tmp/no-such-keywords.txt",
            &lib,
            1,
        ) {
            Ok(_) => panic!("open with a missing model dir must fail"),
            Err(e) => e,
        };
        assert!(
            err.to_string().contains("not found"),
            "unexpected error: {err}"
        );
    }
}
