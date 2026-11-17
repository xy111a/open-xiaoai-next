# client/kws/vendor-arm/ — ARM 版 KWS 原生件（P1-40）

本目录放 ARM（32-bit armv7, gnueabihf）原生件。**二进制不要提交进 git 仓库**
（与模型文件同例）：丢了用 `../scripts/download-kws-lib.sh` /
`../scripts/download-kws-reference.sh` 现下现验。

## 1. 官方 sherpa-onnx ARM 预编译库（Rust 直连用，当前仍 BLOCKED）

| 文件 | 字节 | SHA256 |
|---|---|---|
| `libsherpa-onnx-c-api.so` | 4,733,132 | `46d5250538b5bcf97d921204f7fe61eca7bac119ad2dcb1c95b557a314d8bc6b` |
| `libonnxruntime.so` | 20,617,061 | `4636872e9b50f985b7ecaa55a6829dee81bdb96f77010b5bc0a4059e51d872dc` |

来源：sherpa-onnx 官方 GitHub releases，v2.0.1，
`sherpa-onnx-v2.0.1-linux-arm-gnueabihf-shared.tar.bz2`（tarball SHA256
`099c6bc956b26df246712ff9902419ff9cbc938c014177ea3e205b32675b48d5`），
Apache License 2.0。下载脚本：`../scripts/download-kws-lib.sh`（下载 →
SHA256 校验 tarball → 只取上面两个 .so → 校验 .so SHA256 → `nm -D`
核对 10 个必需导出符号）。

**为什么它现在还不能用（2026-11-17 实测，写清楚，不粉饰）：**

1. **glibc / libstdc++ 版本不够**：**两个 .so 都**要求 GLIBC 最高
   `2.34` 与 GLIBCXX 最高 `3.4.29`（2026-11-17 对本目录两个 .so
   `readelf -V` 实测确认；`libonnxruntime.so` 最高也是 `GLIBC_2.34`，不是
   早先写的 GLIBC_2.4——**“ORT 只要求 GLIBC_2.4”已被证伪**，见
   `AUDIT_ROUND2.md` P1-N3）；音箱（LX06 1.94.13 / OH2P 1.58.6
   原厂 rootfs 实测）最高只有 glibc 2.25、libstdc++ 3.4.22
   （`libstdc++.so.6.0.22`）。直接放上去 `dlopen` 会报
   `version 'GLIBC_2.34' not found`（旧版 v1.10.34/v1.9.29 要求 2.29，
   同样超标）。两个 .so 都超标，**不只是 sherpa 那层**。
2. **musl 静态 Client 根本不能 dlopen**（历史记录，本项已随构建目标
   改变而过时）：早先 ARM Client 默认是
   `armv7-unknown-linux-musleabihf` 静态链接（`file` 显示 statically
   linked，无 dynamic section）。实测：x86_64 上同构的 musl 静态程序
   `dlopen` 宿主 glibc `libsherpa-onnx-c-api.so` 直接失败
   `Dynamic loading not supported`（exit 2），而 glibc 程序同条件
   `dlopen` 成功并解析到 `SherpaOnnxCreateKeywordSpotter`（exit 0）。
   即：**哪怕拿到版本兼容的 .so，不改 Client 链接方式也加载不了**。
   当前构建目标已是 `armv7-unknown-linux-gnueabihf`（动态链接，
   `build-armv7-gnueabihf.sh`），musl 静态脚本
   （`build-armv7.sh`）仅作回退；**链接方式这层障碍已不存在，
   剩下第 1 条的 glibc 版本问题**。

出路（二选一，需协调人决策，见 `../README.md` §ARM 部署）：
- (a) 把 Client 改成动态链接 musl（或 static-PIE）+ 交叉编译 musl 版
  sherpa/ORT；或
- (b) 从源码重编 sherpa **与** ORT（**不能复用本目录官方
  `libonnxruntime.so`，它同样要求 GLIBC 2.34**），或 KWS 继续放
  NAS 边车（`kws-sidecar`，当前唯一能工作的唤醒路径，见 `TEST_REPORT.md`
  “服务端唤醒”一节）。音箱自带 glibc 2.25 与 `/usr/lib/libasound.so.2`，
  glibc 路线有现成地基。

## 2. 参考实现 kws 二进制（过渡，TRANSITIONAL）

| 文件 | 字节 | SHA256 |
|---|---|---|
| `reference-kws.tar.gz`（整包） | 9,483,096 | `690a38b94159f30f600494d5d6501f22083cbf6a967512833fb8e216707b07b9` |
| `reference/kws`（解出） | 11,243,180 | `efc1bbbf7611416ae7d8b895a84a26ec606abc7bc312d31d4001982b7397541f` |

来源：Gitee `idootop/artifacts` releases，tag `open-xiaoai-kws`，
`https://gitee.com/idootop/artifacts/releases/download/open-xiaoai-kws/kws.tar.gz`
（2026-11-17 下载实测；上游更新会变，脚本会校验失败并提示更新）。
下载脚本：`../scripts/download-kws-reference.sh`。
上游出处：idootop/open-xiaoai `examples/kws/`（`init.sh`/`boot.sh`，
本仓库 `references/open-xiaoai/examples/kws/`）。

实测属性（`file` / `readelf`）：
- ELF 32-bit LSB ARM EABI5，动态链接，interpreter `/lib/ld-linux-armhf.so.3`；
- 最高只要求 GLIBC_2.18，无 GLIBCXX 高版本要求（C++ 部分静态链接，
  内含 ONNX Runtime 符号），**与音箱 glibc 2.25 兼容**；
- 需要 `libasound.so.2`（音箱 `/usr/lib/` 自带）；
- **不含任何 `.so`**：它是一个可执行文件，不导出 `SherpaOnnx*` 符号，
  不能被 Rust `dlopen`。过渡用法是 sidecar：它独占麦克风跑唤醒，
  把 `<timestamp_ms>@<关键词>` 写进 `/tmp/open-xiaoai/kws.log`
  （`__STARTED__` 表示启动完成），Rust 侧用 `src/kws.rs` 的
  `KwsLogMonitor` 尾随该日志取唤醒事件（见 `../README.md` §ARM 部署）。
  > **作废（2026-11-17）**：服务端 KWS 已落地（NAS `kws-sidecar`），
  > 本节的参考 kws 二进制 sidecar + `KwsLogMonitor` 旁路方案就此作废，
  > 不再接线、不再部署到音箱（同 `../README.md` 的作废说明）。
- 包内 `reference/models/` 是 zipformer2 布局（`encoder.onnx` 等，
  与本项目 transducer 布局的 `encoder-epoch-12-...int8.onnx` 不同），
  sidecar 只能用它自己的模型目录，不要与 `kws-models/` 混用。

## 部署路径（音箱上，待真机验证）

```sh
scp client/kws/vendor-arm/libonnxruntime.so client/kws/vendor-arm/libsherpa-onnx-c-api.so root@<音箱IP>:/data/open-xiaoai/kws/lib/
# 过渡 sidecar：
scp client/kws/vendor-arm/reference-kws.tar.gz root@<音箱IP>:/data/open-xiaoai/kws/
# 音箱上：tar -xzf /data/open-xiaoai/kws/reference-kws.tar.gz -C /data/open-xiaoai/kws/reference
# client.toml：kws_lib = "/data/open-xiaoai/kws/lib/libsherpa-onnx-c-api.so"
```

`libonnxruntime.so` 必须与 `libsherpa-onnx-c-api.so` 同目录（后者
`DT_NEEDED libonnxruntime.so` 且 `RPATH $ORIGIN`）。
