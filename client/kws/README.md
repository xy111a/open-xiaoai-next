# KWS：“你好小李”唤醒词（sherpa-onnx）

状态（2026-11-17）：**可行（合成语音冒烟通过）**。模型能检出“你好小李”，
Rust 侧已用 feature `sherpa-kws` 接入（`src/sherpa.rs`，默认不开）。
**未证明**：真人唤醒率、远场/电视噪声下表现、ARMv7 真机性能与延迟。
这三项仍 BLOCKED，需真机 + 真人录音。

## 模型

| 项 | 内容 |
|---|---|
| 模型 | `sherpa-onnx-kws-zipformer-wenetspeech-3.3M-2024-01-01` |
| 来源 | sherpa-onnx 官方文档（k2-fsa.github.io/sherpa/onnx/kws/pretrained_models/） |
| 下载地址 | `https://github.com/k2-fsa/sherpa-onnx/releases/download/kws-models/sherpa-onnx-kws-zipformer-wenetspeech-3.3M-2024-01-01.tar.bz2`（GitHub k2-fsa/sherpa-onnx releases，tag `kws-models`） |
| 训练数据 | WenetSpeech L（10,000 小时中文语音） |
| 参数 | 3.3M；zipformer transducer；拼音（声母+韵母）建模单元 |
| 许可 | Apache License 2.0（模型包内 README.md） |
| SHA256（tarball） | `b2f7c89690dc8ce4c6ed6afeab7cd800c36ad1421fb6b6302b4a4b194cf7f35f`（2026-11-17 下载实测；上游重新打包会变） |
| 关键词机制 | 开放词表（open vocabulary）：换唤醒词不用重新训练，只换 `keywords.txt`（`text2token` 生成，ppinyin 格式） |

部署用的文件（int8 编码器，共 5,708,052 字节 ≈ 5.4MB）：

| 文件 | 字节 | 说明 |
|---|---|---|
| `encoder-epoch-12-avg-2-chunk-16-left-64.int8.onnx` | 4,777,666 | int8 编码器（chunk-16，延迟约 320ms） |
| `decoder-epoch-12-avg-2-chunk-16-left-64.onnx` | 675,349 | fp32 解码器（int8 收益小，官方只提供 fp32） |
| `joiner-epoch-12-avg-2-chunk-16-left-64.onnx` | 253,410 | fp32 joiner |
| `tokens.txt` | 1,627 | token 表 |
| `keywords.txt` | 37 | `n ǐ h ǎo x iǎo l ǐ @你好小李` |

另有 fp32 编码器（12,152,560 字节）可用 `--fp32` 保留；实验里 int8 与 fp32
检出结果相同（见下），默认只发 int8。参考实现（idootop/open-xiaoai
`examples/kws/`，本仓库 `references/`）用的也是同一模型 + 同一
keywords.txt 格式（`n ǐ h ǎo x iǎo zh ì @你好小智` 之类）。

## 下载与校验

```bash
./scripts/download-kws-model.sh [目标目录] [--fp32]
# 默认目标 ./kws-models；WAKE_WORD 环境变量可改唤醒词（默认“你好小李”）
```

脚本做三件事：下载官方 tarball → SHA256 校验 → 只保留上表文件并生成
`keywords.txt`（优先调 `sherpa-onnx-cli text2token --tokens-type ppinyin` 现算；
无该工具且唤醒词就是“你好小李”时写入本次验证过的 token 行；其它词无工具则
直接失败）。模型文件不要提交进 git 仓库。

部署到音箱：

```bash
scp -r kws-models root@<音箱IP>:/data/open-xiaoai/kws/models
# client.toml：
kws_model_dir = "/data/open-xiaoai/kws/models"
kws_lib = "/data/open-xiaoai/kws/lib/libsherpa-onnx-c-api.so"  # 见下 ARM 部署
```

## Rust 接入（feature `sherpa-kws`，默认不开）

- `src/sherpa.rs`：sherpa-onnx C API（2.0.1 版 `c-api.h`）的最小 FFI 封装。
  用 `libloading` 运行时 `dlopen` `libsherpa-onnx-c-api.so`，不做链接期依赖，
  所以 ARMv7 交叉编译不受影响（feature 开了也只是多一个 dlopen）。
  `#[repr(C)]` 结构体已用 C 探针对照头文件锚定（64-bit：OnlineModelConfig=136、
  KeywordSpotterConfig=184、KeywordResult=56 字节，见 `sherpa.rs` 测试）。
- `src/kws.rs`：`Kws::new_with_sherpa(model_dir, keywords_file, lib_path, wake_word)`；
  `feed()` 每 20ms 帧过模型，命中返回 `WakeEvent`（`wake_word` 取检出的关键词原文）。
  推理参数与实验一致：1 线程、score 1.0、threshold 0.25、max_active_paths 4、
  num_trailing_blanks 1。
- `main.rs`：feature 开时尝试加载 sherpa 后端；模型/.so 缺失或加载失败时
  **不再回退纯 RMS，而是非零退出**（P1-40：`Kws::new` 在 feature 开时
  直接 Err，`?` 向上传；静默跑 RMS 等于永远不唤醒，还看不出来）。
  feature 不开时行为与原来完全一致。
- `config.rs`：新增 `kws_model_dir`（默认 `/data/open-xiaoai/kws/models`）、
  `kws_lib`（默认空串 = 按默认名在系统库路径找）；旧 client.toml 不写这两个
  字段时走默认值，不会被拖垮。

宿主机验证（需真实模型 + .so + 样本，夹具在仓库外，用环境变量指）：

```bash
cargo test --features sherpa-kws
# 端到端（真实模型 + 真实 TTS 样本，见下实验）：
SHERPA_KWS_TEST_MODEL_DIR=<模型目录> \
SHERPA_KWS_TEST_LIB=<libsherpa-onnx-c-api.so 路径> \
SHERPA_KWS_TEST_SAMPLES=<样本目录，含 pos1.wav / neg3.wav> \
SHERPA_KWS_TEST_KEYWORDS=<keywords.txt> \
cargo test --features sherpa-kws sherpa
# 未设环境变量时端到端测试打印 SKIP，不算失败；默认 cargo test 不跑它。
```

## 实验报告（2026-11-17，VM 上实测）

结论先行：**模型能检出“你好小李”——仅对合成语音成立。**
这不是真人唤醒率，不是真机通过，不要这样对外写。

### 方法

- 引擎：sherpa-onnx 2.0.1（pip），`KeywordSpotter`，WenetSpeech 模型，
  fp32 chunk-16 编码器为主，int8 编码器对照；1 线程；score 1.0 / threshold 0.25
  （另测 threshold 0.15 / 0.35、score 2.0）。
- 关键词文件：`keywords.txt` 只有一行 `n ǐ h ǎo x iǎo l ǐ @你好小李`
  （`sherpa-onnx-cli text2token --tokens-type ppinyin` 生成）。
- 样本：Piper TTS（`piper-tts` 1.8.0，中文女声 `zh_CN-huayan-medium`，
  22050Hz → scipy 重采样 16kHz 单声道），共 10 个基础样本 + 3 个加噪/噪声样本。
  **只有一个合成音色，无男声、无真人、无远场、无电视/音乐底噪。**
  edge-tts（微软在线 TTS，本想做第二音色）连不上
  `speech.platform.bing.com:443`（SSL 握手被出口代理挡住），未用上。
- 喂法：与音箱管线一致，20ms（320 采样）一块流式喂；喂完补 1s 静音再结束
  （真实使用中麦克风会一直送帧；**不补静音时裸喊“你好小李”会漏检**——
  第一轮实验 5 个正样本只检出 2 个，补静音后 5/5；这是 transducer 需要
  关键词后若干帧才能吐结果，不是模型不认识词）。
- 对照：模型自带 `test_wavs`（3/4/5.wav）+ 自带 keywords，
  6 个期望关键词（文森特卡索、法国、蒋友伯、女儿、周望军、落实）全部检出，
  证明测试 harness 本身没错。

### 结果

| 组 | 样本 | 结果 |
|---|---|---|
| 正样本 5/5 | `你好小李`（裸喊） | 检出（喂入 1.44s 处，音频 1.24s + 0.2s 静音后） |
| | `你好小李，打开客厅灯` | 检出（1.12s） |
| | `你好小李，今天天气怎么样` | 检出（1.44s） |
| | `你好小李`（语速 ×1.18） | 检出（1.45s） |
| | `你好小李`（语速 ×0.8） | 检出（1.75s） |
| 负样本 5/5 静默 | `你好`（半句） | 未检出 ✓ |
| | `小李，你在吗`（只喊小李） | 未检出 ✓ |
| | `今天天气真好，我们去爬山吧` | 未检出 ✓ |
| | `小爱同学，打开灯`（别的唤醒词） | 未检出 ✓ |
| | `你好小明，请帮我查一下明天的天气` | 未检出 ✓ |
| 变体 | int8 编码器 | 5/5 检出、5/5 静默（与 fp32 相同） |
| | threshold 0.15 / 0.35、score 2.0 | 均为 5/5、5/5（本组样本内不敏感） |
| 加噪 | `你好小李` + 白噪声 SNR 15dB / 5dB | 均检出（1.44s）；2s 纯白噪声未检出 ✓ |
| 延迟 | 检出时刻 − 关键词结束 | 约 0.1–0.3s（chunk-16 标称延迟 320ms；关键词约在音频前 1s 内说完） |
| 性能（x86_64 VM） | 模型加载 0.7–1.05s（1 线程）；RTF ≈ 0.02–0.06（约为实时 16–50 倍） | 仅供参考，ARMv7 上会慢得多，未测 |

Rust FFI 端到端（`cargo test --features sherpa-kws sherpa`，真实模型 + 真实
`.so` + 真实 TTS 样本）：`pos1.wav` 检出“你好小李”，`neg3.wav` 静默，
结构体尺寸测试与缺失库/缺失模型报错测试均通过。

### 局限（写清楚，不粉饰）

1. 样本量小：正 5 + 负 5（+ 加噪 3），全是同一个 Piper 合成女声。
   换音色、换人、换口音、远场 1m/3m、电视声、音乐声下表现**一概未知**。
2. 阈值未调优：0.15–0.35 在本组样本内结果相同，不代表阈值不重要；
   真人语料到了之后要按 FR-01（1m/3m 唤醒率 ≥95%，电视声下 20min 零误唤醒）重调。
3. 延迟是 VM 上的数：ARMv7（Amlogic A113X，单核弱）上的加载时间与 RTF 未测；
   参考实现称“模型加载较慢”，要有心理准备。
4. 内存：int8 模型文件 5.4MB；运行时 sherpa-onnx 官方与参考实现称约 20MB，
   256MB 机型可跑，128MB 机型会被杀掉（Killed）——本项目两台都是 256MB。

## ARM 部署（P1-40 部分修复，2026-11-17）

`build-armv7.sh` 现默认开 `--features sherpa-kws`，并在末尾断言产物含
sherpa 标记（`src/sherpa.rs` 的 `SHERPA_KWS_MARKER` 与 C API 符号名）；
feature 开时 KWS 后端缺失，Client 直接非零退出，不再静默跑 RMS
（`src/kws.rs` 的 `Kws::new` 在 feature 开时返回 Err，`main.rs` 的 `?`
向上传）。**但这只修了“二进制含不含 KWS”的 half：真机加载仍 BLOCKED，
原因有两层，都是 2026-11-17 实测的：**

### 第 1 层：官方 ARM .so 的版本要求高于音箱

官方 sherpa-onnx v2.0.1 确实有 ARM 32-bit 预编译包
（`sherpa-onnx-v2.0.1-linux-arm-gnueabihf-shared.tar.bz2`），本仓库已用
`scripts/download-kws-lib.sh` 取到（SHA256 校验 + 10 个必需导出符号核对，
见 `kws/vendor-arm/SOURCE.md`）：

| 文件 | 字节 | SHA256 |
|---|---|---|
| `libsherpa-onnx-c-api.so` | 4,733,132 | `46d52505…8bc6b`（全值见 SOURCE.md） |
| `libonnxruntime.so` | 20,617,061 | `4636872e…72dc`（全值见 SOURCE.md） |

但它要求 `log/pow/exp@GLIBC_2.29` 与 `GLIBCXX_3.4.29`，而音箱
（LX06 1.94.13 / OH2P 1.58.6 原厂 rootfs 实测）最高只有 glibc 2.25、
libstdc++ 3.4.22（`libstdc++.so.6.0.22`）。直接放上去 `dlopen` 会报
`version 'GLIBC_2.29' not found`。`libonnxruntime.so` 本身只要求
GLIBC_2.4，没问题；卡的是 sherpa 那层。

### 第 2 层：musl 静态 Client 根本不能 dlopen

当前 ARM Client 是 `armv7-unknown-linux-musleabihf` 静态链接（`file`
显示 statically linked，无 dynamic section）。实测：x86_64 上同构的
musl 静态程序 `dlopen` 宿主 glibc `libsherpa-onnx-c-api.so` 直接失败
`Dynamic loading not supported`（exit 2），而 glibc 程序同条件 `dlopen`
成功并解析到 `SherpaOnnxCreateKeywordSpotter`（exit 0）。
即：**哪怕拿到版本兼容的 .so，不改 Client 链接方式也加载不了。**

出路（二选一，需协调人决策）：
- (a) 把 Client 改成动态链接 musl（或 static-PIE）+ 交叉编译 musl 版
  sherpa/ORT（ORT 没有 musl 预编译，需从源码编，工作量大）；或
- (b) 把 Client 改成 `armv7-unknown-linux-gnueabihf`（glibc 2.25 目标，
  zig 可指定 `-target arm-linux-gnueabihf.2.25`）+ 从源码重编 sherpa
  （复用 `kws/vendor-arm/` 里官方 `libonnxruntime.so`，它只要求
  GLIBC_2.4）。音箱自带 glibc 2.25 与 `/usr/lib/libasound.so.2`，
  glibc 路线有现成地基。

### 过渡路线（TRANSITIONAL）：参考 kws 二进制 sidecar

> **作废（2026-11-17）：服务端 KWS 已落地，唤醒检测移出音箱、放到 NAS 端
> `kws-sidecar` 边车（Hub 解码 Opus 后转发 PCM，见 `open-xiaoai-next/
> TEST_REPORT.md`“服务端唤醒”一节与 `kws-sidecar/README.md`），本节的参考
> kws 二进制 sidecar + `KwsLogMonitor` 尾随日志的旁路方案就此作废，不再
> 接线进 `main.rs`、不再部署到音箱。作废原因：服务端 KWS 复用已验证的
> 同一 sherpa 模型与关键词，终结了 ARM `.so` 加载问题，128MB 机型也能走
> 同一条路，且音频不出局域网、仍本地离线；`KwsLogMonitor` 代码本身保留
> （不删除），仅不再作为唤醒路径；A 路（源码重编 ORT+sherpa）保留为未来
> 可选项，本地 KWS 代码同样不删除。以下原文保留作记录，不再执行。

在上面两项解决前，真机唤醒走参考实现 sidecar（已验证能在音箱上跑，
不是猜测）：

1. 取包：`./scripts/download-kws-reference.sh`（Gitee
   `idootop/artifacts` releases，tag `open-xiaoai-kws` 的 `kws.tar.gz`，
   tarball SHA256 `690a38b9…07b9`，全值见 `kws/vendor-arm/SOURCE.md`）。
2. 包里**没有 `.so`**：只有一个 ARM glibc 可执行文件 `kws`
   （11,243,180 字节，最高只要求 GLIBC_2.18）+ zipformer2 布局模型
   （`encoder.onnx` 等，与本项目 transducer 布局的 `kws-models/` 不同，
   不要混用）。它不能被 Rust `dlopen`。
3. 用法：在音箱上作 sidecar 独占麦克风跑唤醒，把
   `<timestamp_ms>@<关键词>` 写进 `/tmp/open-xiaoai/kws.log`
   （`__STARTED__` 表示启动完成），Rust 侧用 `src/kws.rs` 的
   `KwsLogMonitor` 尾随该日志取唤醒事件（行格式、去重语义与参考实现
   `references/open-xiaoai/packages/client-rust/src/services/monitor/kws.rs`
   一致；日志里没有 RMS，`KwsLogMonitor` 不编造 RMS）。
4. 未验证、仍 TODO：sidecar 的拉起/保活、它与 Client 的 ALSA 争用
   （sidecar 占麦克风时 Client 自己的采集任务会退出）、以及
   `KwsLogMonitor` 接线进 `main.rs`。这三项都需要真机。

部署到音箱（待真机验证）：

```bash
scp -r kws-models root@<音箱IP>:/data/open-xiaoai/kws/models
# Rust 直连（上面两层解决后才可用）：
scp kws/vendor-arm/libsherpa-onnx-c-api.so kws/vendor-arm/libonnxruntime.so root@<音箱IP>:/data/open-xiaoai/kws/lib/
# client.toml：
kws_model_dir = "/data/open-xiaoai/kws/models"
kws_lib = "/data/open-xiaoai/kws/lib/libsherpa-onnx-c-api.so"  # Rust 直连用
# 过渡 sidecar：见上；client.toml 的 kws_model_dir/kws_lib 在过渡期不起作用
```

部署后在音箱上复测：加载时间、RTF、内存占用、真人唤醒率（FR-01）。

## 替代路线（若 sherpa 路线在真机上失败才启用）

以下只做调研与流程准备，**没有替 bai 注册任何账号**。

### A. Porcupine 自定义唤醒词（Picovoice，需 bai 的账号）

- 是什么：商用 on-device 唤醒词引擎；自定义词在 Picovoice Console 网页上
  输入文字即生成 `.ppn` 模型文件（按平台区分，ARM 32-bit 有对应平台选项），
  运行时 100% 本地，约 1MB/词，CPU 占用很低。
- 注册流程（bai 自己做）：`https://console.picovoice.ai/signup` → 邮箱 +
  密码注册 → 验证邮箱 → 登录 `https://console.picovoice.ai/` → 取 AccessKey
  （许可证/激活 token，不是联网 API key；初始化引擎时要传，之后完全离线）。
- 免费额度（多方资料一致的大意，具体以 Console 内页面为准）：免费 tier 可做
  个人/小项目使用，无需信用卡；每月活跃用户 ≤3（免费 tier 的计数口径是
  “30 天内初始化过引擎的独立设备/应用/浏览器实例”）；自定义唤醒词免费 tier
  可做（有资料称上限 3 个自定义词）；超出后收费较高（有第三方报价称下一档
  $899/月起，以官网 pricing 页为准）。
- 接入点（本项目）：`Kws` 后端再加一个 Porcupine 实现（Rust 有
  `porcupine` crate / C API 动态加载，模式与 `src/sherpa.rs` 相同）；
  模型文件 `.ppn` 随 OTA 下发；AccessKey 放 client.toml（不进仓库）。
  注意中文：Console 自定义词支持多语言，中文（普通话）是否在列表内以下单时
  Console 实际显示为准。
- 何时用：sherpa 在 ARM 真机上跑不动（加载太慢/内存超/延迟超），或真人
  唤醒率调不上去时。

### B. openWakeWord 训练（需 bai 的录音）

- 是什么：开源（Apache 2.0 代码；预训练模型为 CC BY-NC-SA 4.0，非商业——
  自训模型归属与合规使用前再确认）on-device 唤醒词，TensorFlow Lite/ONNX，
  产出单个 `.onnx`（约 200KB），CPU 实时运行资源占用极小。
- 录音要求（bai 自己录）：16kHz、16-bit、单声道 WAV，每条 0.5–2s，只说
  “你好小李”；安静房间录，口齿清晰、音量正常；**正样本 50–100 条**
  （多遍录，语速/语气/轻重稍有变化更好；另建议 50–100 条近音否定句，
  如“你好小明”“小李”）；另需约 1,000 条正样本总量时可用 TTS 合成补齐
  （社区做法：3,000–50,000 条合成 + 20–100 条真人，真人样本权重更高），
  负样本约 10,000 条（通用语音语料，训练脚本自带流程拉取）。
- 训练流程：上游 `dscripka/openWakeWord` 的
  `notebooks/automatic_model_training.ipynb`（或社区 `openwakeword-trainer`
  13 步流水线，Piper TTS 合成 + 噪声/混响增强）→ Colab GPU 约 30–60 分钟
  （完整流程有称 4–6 小时，取决于样本量）→ 产出 `<name>.onnx` → 阈值调优
  （真人模型常用 0.5–0.8；纯合成模型常需 0.3–0.4）→ 用 500+ 正样本与
  200 小时级负音频做验收（FR-01 同标准）。
- 接入点（本项目）：与 sherpa 相同的 feature 门控位置再加一个
  openWakeWord 后端（ONNX Runtime 推理，模型极小）；或先用 openWakeWord
  的 Python 推理在 NAS 上验证效果再移植。
- 何时用：sherpa 与 Porcupine 都不满足（例如只要完全开源自有模型、
  或要针对 bai 本人声音特调）时。成本最高（录音 + 训练 + 调阈值），
  不作为首选。
