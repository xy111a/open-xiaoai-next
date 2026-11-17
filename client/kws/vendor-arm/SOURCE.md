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

1. **glibc / libstdc++ 版本不够**：该 .so 要求 `log/pow/exp@GLIBC_2.29` 与
   `GLIBCXX_3.4.29`；音箱（LX06 1.94.13 / OH2P 1.58.6 原厂 rootfs 实测）
   最高只有 glibc 2.25、libstdc++ 3.4.22（`libstdc++.so.6.0.22`）。
   直接放上去 `dlopen` 会报 `version 'GLIBC_2.29' not found`。
   `libonnxruntime.so` 本身只要求 GLIBC_2.4，没问题；卡的是 sherpa 那层。
2. **musl 静态 Client 根本不能 dlopen**：当前 ARM Client 是
   `armv7-unknown-linux-musleabihf` 静态链接（`file` 显示 statically
   linked，无 dynamic section）。实测：x86_64 上同构的 musl 静态程序
   `dlopen` 宿主 glibc `libsherpa-onnx-c-api.so` 直接失败
   `Dynamic loading not supported`（exit 2），而 glibc 程序同条件
   `dlopen` 成功并解析到 `SherpaOnnxCreateKeywordSpotter`（exit 0）。
   即：**哪怕拿到版本兼容的 .so，不改 Client 链接方式也加载不了**。

出路（二选一，需协调人决策，见 `../README.md` §ARM 部署）：
- (a) 把 Client 改成动态链接 musl（或 static-PIE）+ 交叉编译 musl 版
  sherpa/ORT；或
- (b) 把 Client 改成 `armv7-unknown-linux-gnueabihf`（glibc 2.25 目标，
  zig 可指定 `-target arm-linux-gnueabihf.2.25`）+ 从源码重编 sherpa
  （复用本目录官方 `libonnxruntime.so`，它只要求 GLIBC_2.4）。
  音箱自带 glibc 2.25 与 `/usr/lib/libasound.so.2`，glibc 路线有现成地基。

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
