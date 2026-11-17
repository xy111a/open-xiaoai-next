# 第二轮审计 — firmware-patch / deploy / CI 面

- 审计员：firmware-patch + deploy + CI 面审计员
- 日期：2026-11-17（CST，晚）
- 审计对象：`open-xiaoai-next/`（分支 `audit-round2-ci-fix`）下的
  `firmware-patch/`（补丁脚本、`FLASH_RUNBOOK.md`、`tests/`）、`deploy/`（`docker-compose.yml`、
  `Dockerfile.hub`、`nas-install.sh`、`NAS_CHECKLIST.md`）、`.github/workflows/ci.yml`、
  `scripts/lint.sh`、`.golangci.yml`、`.semgrep/`，以及 `TEST_REPORT.md` 中与本面相关的行。
- 对照：首轮 `AUDIT_REPORT.md`（2026-11-17 13:19，108 项；A+B 只修了 12 项，其余约 96 项延期）、
  `PLAN.md` v4（FR/NFR）、`TEST_REPORT.md`。
- 方法：只读审计，未修改任何源代码（§5 的两处 CI 修复除外，见文末“CI 修复”一节）。
  逐文件读代码、grep 交叉验证调用点与挂载点；CI 失败根因独立复核 run 44063914126 的
  实际日志（`gh run view 44063914126 --log-failed`，见 §2）；基线测试实跑（见 §1）。
  `firmware-patch/work/`（官方固件解包内容）不在审计范围；真机项一律记 BLOCKED，不写成通过。
- 结论先行：**本面新发现 P0 共 0 项**；最严重的新问题是 **P1-新-1（Web Token 未接进部署，
  P0-1 修复的部署侧断裂）**；另有 P1 新问题 2 项、P2 新问题 6 项；首轮本面延期项
  复核：P1-33 仍在、P1-4 / P1-8（残留）/ P1-36 / P1-37 / P1-46 / P2-6 / P2-8 / P2-9 /
  P2-23 / P2-24（残留）/ P2-30 / P2-32（部分）/ P2-33 / P2-34 / P2-35 / P2-36 / P2-37 /
  P2-38 / P2-39（已缓解）/ P2-55 / P2-56 均未修；P1-8 与 P2-31 的核心部分已有缓解，
  见各条。

## 1. 基线测试结果（2026-11-17 晚，分支 audit-round2-ci-fix，commit 509e57a）

### 1.1 `python3 -m pytest firmware-patch/tests/ -q`（仓库根目录）

- 结果：**54 passed，1 failed，6 errors，耗时约 44–90 秒**（两次跑耗时不同，失败集稳定）。
- 失败的 1 项：`test_shell_scripts.py::test_unpack_with_ota_bin_env` —
  `unpack.sh` 在 `unsquashfs` 缺失处退出 1，断言 `assert r.returncode == 0` 失败。
  实际错误行（本机复跑输出）：
  `缺 unsquashfs（apt 装 squashfs-tools）`。
- 出错的 6 项（均为 fixture setup 失败，同一根因）：`test_unpack_real_firmware`、
  `test_unpack_squashfs_header_matches_rea`、`test_patch_sh_end_to_end`、
  `test_patch_sh_does_not_modify_source_rootfs`、`test_patch_rejects_tampered_bin`、
  `test_patch_rejects_tampered_sqfs_when_bin_absent` — 模块级 `unpacked_work` fixture
  跑 `unpack.sh` 时因缺 `unsquashfs` 退出 1。
- 根因是**本机环境缺失，不是代码回归**：`which unsquashfs mksquashfs` 为空；
  `TEST_REPORT.md` 记载 squashfs-tools 曾用本地缓存 `.deb` 装进 `/usr`，
  `/usr` 重启会丢，现已丢失；`sudo apt-get install -y squashfs-tools` 报
  `Package squashfs-tools is not available`（包列表过期），`apt-get update`
  又被另一进程（PID 17961，`apt-get update`，疑似并行审计员）持锁而无法执行。
  本机 54 项通过的测试（含全部 `test_patch_firmware.py` 与 `test_download_ota.py`）
  均真实通过；7 项失败/出错与固件 fixture 本身无关——本地
  `speaker-open-xiaoai-rebuild/firmware/rootfs` 与 `lx06_1.94.13.bin` 均存在。
- 与 CI 的区别：CI 的 Python 失败是 fixture **文件不存在**（见 §2.2），
  本机的 7 项是 **工具缺失**，两者根因不同，不要混为一谈。

### 1.2 `./scripts/lint.sh`（仓库根目录）

- 结果：**全部通过**（`lint: 全部通过`，exit 0）。
- 各步：gofmt ok；`go vet` ok；golangci-lint ok；规则 1 ok（45 项接线检查全部通过，
  其中 P1-9 的 `ota.ConfirmHello` / `ota.MarkRebooted` / `otaMgr.Sweep` 与 P1-15 的
  `intercom.Start` / `intercom.Sweep` 共 5 项以 `DEFERRED` 打印——deferred until after
  first gate，bai 2026-11-17 批复，条目保留在检查表里）；规则 2 ok；规则 3 Go 部分 ok
  （1 条 KNOWN-EXCEPTION：`hub/internal/music/music.go` 的 `SnapClient.call()` 的
  `ReadBytes` 无长度上限，暂缓审计项，只记录、不修）；Semgrep ok；`cargo fmt --check` ok；
  `cargo clippy --all-targets` ok（4 个警告，见 `/tmp/open-xiaoai-clippy.log`，
  警告不致命）。
- 注意：lint.sh 全绿只说明“lint 规则内无违规”，不说明 CI 能过——lint.sh **不跑任何测试**
  （`go test` / `cargo test` / `pytest` 均不在此脚本里，注释自述“由 ci.yml 跑”），
  且 lint.sh 跑的 `go vet` 用的是本机 `~/workspace/tools/libopus` 源码自编的
  libopus/libopusfile（`PKG_CONFIG_PATH` 由 `env.sh` 设），与 CI 的 apt 环境不同，
  所以本地 lint 全绿不能掩盖 §2 的两个 CI 失败。

### 1.3 补充核实（审计员亲测，非测试套件）

- `flash-check.sh lx06 1.94.13` / `oh2p 1.58.6`：本轮未重跑（缺 `unsquashfs` 时
  超块解析只警告、不拦截；但补丁镜像 SHA 与 runbook 一致，见下）。
- 磁盘上的补丁镜像 SHA256 与 `FLASH_RUNBOOK.md` §1 表格及 `flash-check.sh` 内
  `KNOWN_PATCHED_SHA256` 三方一致（`sha256sum` 实测）：
  LX06 `7266aba99b769947bd3ce937c23f0dc0443ad52883f04e4992eed45c0d7d97b3`（38,453,248 字节），
  OH2P `139de363d39d0a5472bf9b00c015effd9a24c379ca5fa2436cc21c43e8706415`（30,179,328 字节）。
  旧 SHA（LX06 `cc36b28e…54f06d`、OH2P `db00add0…f0423`）已被取代，见 §4.4。
- `deploy/hub-linux-amd64` 经 `ldd` 核实只依赖 `libc.so.6` / `libm.so.6`，
  无 `libopus` 动态依赖——`Dockerfile.hub` 注释“Opus 已静态编入，不需要 libopus”
  属实；`TEST_REPORT.md` Phase 0 表“NAS 部署时 Hub 镜像里同样要装 libopus”
  与之矛盾，见 §4.4。

## 2. CI 失败根因独立复核（run 44063914126）

简报称 Go 失败是 `go vet` 时 `Package opusfile was not found`、Python 失败是
firmware-patch 测试在 setup 时 FileNotFoundError（缺固件 rootfs）。审计员独立拉取
`gh run view 44063914126 --log-failed`（2,719 行）核实：**两项均属实，且细节比简报更丰富**。

- 4 个 job 结论：`Go（test + vet + golangci-lint + 规则 1/2/3）| failure`；
  `Python（pytest）| failure`；`Rust（test + fmt + clippy）| success`；
  `Semgrep（规则 3 Rust/Python / 4 / 5）| success`。

### 2.1 Go job — 确认：`go vet` 时 `Package opusfile was not found`

实际错误行（日志原文）：

```
# gopkg.in/hraban/opus.v2
# [pkg-config --cflags  -- opus opus opus opus opusfile opusfile]
Package opusfile was not found in the pkg-config search path.
Perhaps you should add the directory containing `opusfile.pc'
to the PKG_CONFIG_PATH environment variable
Package 'opusfile' not found
```

- 失败点是 `go vet ./...` 步骤（`hub/`），在拉取 `gopkg.in/hraban/opus.v2` 依赖后、
  编译 `hub/internal/opus`（cgo）时触发；`go test`、`golangci-lint`、规则 1/2/3
  检查脚本均未跑到（同一 job 内 `go vet` 先失败，后续步骤未执行）。
- 根因确认：ci.yml 的安装行只装了 `libopus-dev`（`sudo apt-get update &&
  sudo apt-get install -y libopus-dev pkg-config`），而 `hraban/opus.v2` 的
  cgo 绑定同时要 `opus` 与 `opusfile` 两个 pkg-config 包（`opusfile.pc`
  由 `libopusfile-dev` 提供）。本机能过是因为 `~/workspace/tools/libopus`
  是源码自编的 libopus 1.5.2 + libopusfile 0.12（`TEST_REPORT.md` Phase 0），
  与 CI 的 apt 环境不同——属于“本机有、CI 无”的环境差，不是代码问题。
- 修复方向与简报一致：apt 安装行加 `libopusfile-dev`（见 §5）。

### 2.2 Python job — 确认：FileNotFoundError，缺固件 rootfs 与官方 .bin

实际汇总行（日志原文）：`24 failed, 24 passed, 1 skipped, 12 errors in 1.65s`，
随后 `##[error]Process completed with exit code 1.`。

- 错误分三类，全部是 fixture 引用的文件在 CI checkout 里不存在：
  1. `FileNotFoundError: [Errno 2] No such file or directory:
     '/home/runner/work/open-xiaoai-next/firmware/rootfs'` — 20 项 FAILED
     （`test_patch_firmware.py` 内直接调 `copy_rootfs()` 的测试，如
     `test_patch_ssh_standalone`、`test_patch_login_custom_password_env`、
     `test_cli_on_real_copy`、`test_patch_certs_installs_into_rootfs` 等）
     与 6 项 ERROR（模块级 `patched_rootfs` fixture setup 失败，如
     `test_patch_ssh`、`test_patch_login`、`test_patch_disable_ota`、
     `test_patch_start`、`test_patch_asound`、`test_patch_libxaudio`）。
  2. `FileNotFoundError: [Errno 2] No such file or directory:
     '/home/runner/work/open-xiaoai-next/firmware/lx06_1.94.13.bin'` —
     6 项 ERROR（模块级 `unpacked_work` fixture 的 `shutil.copyfile(FIRMWARE_BIN, …)`，
     如 `test_unpack_real_firmware`、`test_patch_sh_end_to_end`、
     `test_patch_rejects_tampered_bin`）与 2 项 FAILED
     （`test_unpack_rejects_tampered_bin`、`test_flash_check_rejects_tampered_image`，
     后者缺的是 `firmware-patch/work/lx06_1.94.13/root.squashfs`）。
  3. `test_unpack_with_ota_bin_env` FAILED（AssertionError）：`unpack.sh` 报
     `找不到 /home/runner/work/open-xiaoai-next/firmware/lx06_1.94.13.bin，
     先跑 download-ota.py`；`test_flash_check_ok_on_real_work` FAILED（AssertionError）：
     `flash-check.sh` 报 `FAIL 找不到补丁镜像 …/firmware-patch/work/lx06_1.94.13/root-patched.squashfs，
     先跑 patch.sh lx06 1.94.13`（其余 .bin / root.squashfs / mico_version 三项只打 WARN）。
- 根因确认：`firmware-patch/tests/conftest.py` 把固件定位在仓库**之外**——
  `PROJECT = FIRMWARE_PATCH.parents[1]` 即 `speaker-open-xiaoai-rebuild/`，
  `FIRMWARE_ROOTFS = PROJECT / "firmware" / "rootfs"`，
  `FIRMWARE_BIN = PROJECT / "firmware" / "lx06_1.94.13.bin`；
  而 CI 只 checkout `open-xiaoai-next/` 一个仓库，`speaker-open-xiaoai-rebuild/firmware/`
  （原厂固件解包目录，含 `rootfs/` 与 `lx06_1.94.13.bin`）根本不在仓库里。
  另：`firmware-patch/work/`（解包/补丁产物）被 `.gitignore` 明确排除
  （`firmware-patch/work/`），`git ls-files firmware-patch/work` 为空——
  所以依赖 `work/` 已交付产物的 `test_flash_check_ok_on_real_work` 与
  `test_flash_check_rejects_tampered_image` 在任何干净 checkout 里都必然失败，
  与固件 rootfs 无关，是第三类缺失。
- 简报未提到的两点：
  - 24 passed 的是通过的纯逻辑测试（`test_download_ota.py` 全套、
    `sha512_crypt` 纯函数测试、不依赖 fixture 的 shell 语法/参数校验测试等），
    说明失败面恰好等于“依赖外部固件材料的测试”，没有波及无关测试。
  - **kws-sidecar 的测试步骤在该 run 里一次都没执行**：Python job 内两个步骤
    是顺序的，`firmware-patch` 的 `pytest` 步骤 exit 1 后，整个 job 直接失败，
    后面的 `pytest（kws-sidecar）` 步骤被跳过——CI 日志全文 `grep kws-sidecar`
    命中 0 行。ci.yml 里该步骤确实存在（见 §3.3），但本次运行没有证据表明它能过；
    固件失败还**掩盖**了边车测试的结果。见 P1-新-3。

## 3. CI 文件本身（ci.yml 与 lint.sh 的一致性）

### P1-新-2：CI 的 Go job 与 lint.sh 的 `go vet` 环境差是结构性的，不只缺一个包

- 位置：`.github/workflows/ci.yml:28-31`（Go job 安装行）、`scripts/lint.sh`（`go vet` 步骤）、
  `hub/`（`go.mod` 依赖 `gopkg.in/hraban/opus.v2`）。
- 证据：CI 只装 `libopus-dev`；本机 `env.sh` 设 `PKG_CONFIG_PATH` 指向源码自编的
  libopus 1.5.2 + libopusfile 0.12（`TEST_REPORT.md` Phase 0）。本次是 `opusfile.pc`
  缺失；即使补上，CI 与本机仍有版本差（apt 的 libopus 1.4.x/1.5.x vs 自编 1.5.2，
  以及未来 `libopusfile` 版本差），`go test ./internal/opus` 的往返误差断言
  （相对 RMS 误差 <10%）理论上可能在 CI 抖动。
- 影响：中。每次新增 cgo 系统依赖都会先在本机通过、再在 CI 失败，修一次、漏一次。
- 建议：§5 先加 `libopusfile-dev` 止血；后续把 CI 的 Go 系统依赖钉成与本机一致的
  版本（或在 ci.yml 注释里写明“本机用 `~/workspace/tools/libopus` 自编，CI 用 apt，
  新增 cgo 依赖须两边同步”），并把 `go vet` 失败时 `go test` 也被跳过的问题记下
  （job 内步骤顺序依赖，见 P1-新-3 同类问题）。

### P1-新-3：Python job 把固件测试与 kws-sidecar 测试串在一个 job 里，前者失败会掩盖后者

- 位置：`.github/workflows/ci.yml:52-72`（python job，两个 `pytest` 步骤顺序执行）。
- 证据：run 44063914126 日志全文无 `kws-sidecar` 字样（见 §2.2）；kws-sidecar 测试
  本身是自给自足的（`kws-sidecar/tests/bootstrap.py`：缺模型就下载，`KWS_TEST_ASSETS`
  指向 `/tmp/kws-test-assets`，见 ci.yml 注释），与固件测试无任何依赖关系，
  却被固件失败连带跳过。
- 影响：中。CI 红了之后无法从本次运行判断边车是否也坏了；修固件的人与修边车的人
  互相阻塞在同一个 job 结论里。
- 建议：把 `pytest（kws-sidecar）` 拆成独立 job（或至少给固件步骤加
  `continue-on-error: true` 并让 job 结论反映两步的各自结果）。本轮任务只批准了
  §5 的两处修复，未做此拆分——留给协调员决定。

### P2-新-1：CI 与 lint.sh 互相缺项，本地全绿 ≠ CI 全绿

- 证据对照（均实测读文件）：
  - lint.sh 有 `gofmt -l hub scripts`，CI 的 Go job 没有 gofmt 检查——一个
    未格式化的 Go 改动可通过 CI，却在本地 lint 失败。
  - lint.sh 不跑任何测试（自述“由 ci.yml 跑”），CI 不跑 `scripts/lint.sh`
    本身——`check-wiring.go` 等规则 1/2/3 脚本在 CI 里是直接 `go run` 的，
    与 lint.sh 的调用一致，这部分没漂移；但 `golangci-lint` 在 CI 里由
    `golangci-lint-action@v6`（version v1.64.8）跑，在 lint.sh 里由本机
    `~/workspace/tools/bin/golangci-lint` 跑，版本未钉死一致。
  - CI 的 Go job 里 `golangci-lint-action` 在 run 44063914126 留了一条警告：
    `Restore cache failed: Dependencies file is not found in
    /home/runner/work/open-xiaoai-next/open-xiaoai-next. Supported file pattern: go.sum`——
    `go.sum` 实际在 `hub/go.sum`（`hub/go.mod` 同目录存在），action 默认在仓库根找，
    缓存未命中（只影响速度，不影响结论）。
  - CI 的 Rust job 跑 `cargo test --locked` + fmt + clippy；lint.sh 的 Rust 部分
    只跑 fmt + clippy（无 test）——两者互补但无单一入口能一次跑全。
- 影响：低。两边各漏一角，迟早有人在其中一边翻车。
- 建议：CI 增加 `gofmt -l` 步骤（或直接跑 `scripts/lint.sh go`）；给
  `golangci-lint-action` 显式配 `working-directory` 相关的缓存路径或接受该警告；
  长远可让 CI 直接跑 `scripts/lint.sh`（需先解决 lint.sh 对 `~/workspace/tools`
  绝对路径的依赖）。

### P2-新-2：CI 的 Python job 只装 `pytest`，未声明固件测试的其它前置

- 位置：`.github/workflows/ci.yml:63-67`。
- 证据：固件测试实际还需要 `unblob`、`unsquashfs`、`mksquashfs`、`file` 等系统工具
  （`test_shell_scripts.py` 的 `unpacked_work` fixture 跑 `unpack.sh`，后者调
  `unblob` 与 `unsquashfs`；`patch.sh` 调 `mksquashfs`）。CI 的 ubuntu-latest
  镜像默认没有这些工具，且 §5 的跳过方案实施后，这些测试在 CI 里会被跳过而非执行——
  跳过是正确选择，但 ci.yml 里没有任何注释说明“固件端到端测试在 CI 里不跑、
  在哪跑”（应在有固件材料的本机跑）。另外 `python -m pip install --quiet pytest`
  未钉版本，也未装 `pyyaml`（当前固件测试只用标准库，暂时不需要；若后续测试读
  YAML 会静默失败）。
- 影响：低。主要是可维护性：后来者会在 CI 里看到一片 skipped 而不知道去哪补跑。
- 建议：在 ci.yml 的 pytest 步骤上方加注释，写明固件端到端测试的跳过原因与
  本地补跑命令（`python3 -m pytest firmware-patch/tests/ -q`，需固件材料与
  squashfs-tools）；`pytest` 钉一个版本范围。

### 已核实无问题的项（CI 面）

- kws-sidecar 测试步骤**确实在 python job 里**（ci.yml:68-72）：工作目录
  `kws-sidecar`，`KWS_TEST_ASSETS=/tmp/kws-test-assets`，装 `ffmpeg`，装
  `requirements.txt` + `requirements-dev.txt` + `piper-tts`，跑
  `python -m pytest tests/ -q`。与 `kws-sidecar/tests/bootstrap.py` 的自举逻辑
  （缺模型即下载，见 `download-model.sh`，SHA256 已 pin）一致。
  但如 §2.2 所述，该步骤在 run 44063914126 里未被执行。
- Semgrep job 与 lint.sh 的 semgrep 调用一致（同为
  `semgrep --config .semgrep/rules.yml --error --exclude '*_test.go'
  --exclude '*/tests/*' --exclude 'test_*.py' hub client/src firmware-patch/scripts`），
  且在 run 44063914126 里 success。
- Rust job 在 run 44063914126 里 success；`cargo test --locked` 与
  `client/Cargo.lock` 一致（本轮未动 Rust 面）。

## 4. 审计发现（按 P0/P1/P2）

### P0 — 新发现 0 项

本面复核未发现新的 P0 级问题。P0-2 / P0-5 / P0-6 的 A+B 修复经复核均已落地
且证据成立（见 §4.0）；首轮 P0-1 / P0-3 / P0-4 不属本面，由其他审计员复核。
下面 P1-新-1 虽是最严重的新问题，但定级为 P1 而非 P0：它是 fail-closed 的
功能断裂（Web 面板不可用），不造成变砖、数据丢失或未授权访问。

### §4.0 A+B 修复复核（本面 5 项，均已落地）

- **P0-2（默认 root 密码）— 已修，复核通过。** `patch_firmware.py` 已无
  `DEFAULT_PASSWORD`；`_password()` 只从环境变量 `SSH_PASSWORD` 或交互式
  `getpass` 读，`≥16 位`，未设且非交互时直接失败退出（`patch_firmware.py:253-267`）；
  hash 为 sha512crypt（`$6$`，5000 轮，随机 16 字符盐，`patch_firmware.py:180-223`，
  注释写明已用 openssl 逐字节校验）；`main()` 不打印 hash（`patch_all` 返回的
  `result` 不含 hash，`patch_firmware.py:690-720`）；`firmware-patch/README.md:34`
  已改为“无默认值：`SSH_PASSWORD` 未设时补丁直接失败退出”。本轮基线
  `test_patch_firmware.py` 全过（含 `test_cli_never_prints_login_hash`、
  `test_patch_login_same_password_random_salt`）。
- **P0-5（OTA 明文 HTTP + 自指 MD5）— 已修，复核通过。** `download-ota.py:45`
  的 API 已是 `https://api.miwifi.com/…`；`EXPECTED` 白名单为强制门
  （`download-ota.py:49-91`，不在白名单的 `(model, version)` 拒绝下载）；
  下载链接强制 `https` + host 白名单（`check_download_link`，`:95-98`）；
  `verify-bin` / `verify-sqfs` 子命令已提供（`:253-255`），且 `unpack.sh:40`
  与 `patch.sh:74-83` 的入口校验均以它为唯一源（不另写哈希表，避免两处漂移）。
  本轮基线 `test_download_ota.py` 全过。
- **P0-6（刷机流程）— 已修，复核通过（文档与干跑部分；真机部分仍 BLOCKED）。**
  `FLASH_RUNBOOK.md` 已写全：只写非当前槽、刷前 `flash-check.sh`、读 `boot_part`、
  备份 boot1、刷后校验、回滚演练（§6.2 要求首刷当天必须做一次回滚演练，
  “没做过回滚演练的刷机算没做完”）；`flash-check.sh` 的 `KNOWN_PATCHED_SHA256`
  与 runbook §1 表格及磁盘实测三方一致（见 §1.3）。真机步骤（读 `boot_part`、
  备份分区、刷后校验、回滚演练）仍 BLOCKED，无真机证据。
- **P1-31（hub-data 卷挂错位置）— 已修，复核通过。** compose 现为
  `hub-data:/app/hub-data`（`docker-compose.yml:36`）；Hub 的 `DataDir` 默认
  `./hub-data`（`hub/internal/config/config.go:96,366`），容器 `WORKDIR /app`
  （`Dockerfile.hub`），三方一致。`NAS_CHECKLIST.md` §7 已补重启后不丢的冒烟步骤，
  但**未在真 NAS 上验证（BLOCKED）**。
- **P1-32（config.yaml `:ro` 挂载）— 已修，复核通过。** compose 现为
  `../hub/config.yaml:/app/config.yaml`（无 `:ro`，`docker-compose.yml:33`），
  与 `Dockerfile.hub` 注释一致。同样只在 NAS 上待验证（BLOCKED）。

### P1 — 新发现

#### P1-新-1：P0-1 修复后，`OPEN_XIAOAI_WEB_TOKEN` 从未接进部署：NAS 上的 Web 面板注定 401，OTA 下发入口随之不可用

- 位置：`deploy/docker-compose.yml`（hub 服务 `environment`）、`deploy/.env.example`、
  `deploy/nas-install.sh`、`.github/workflows/ci.yml` 无关；服务端
  `hub/internal/web/web.go:57-59,82-83`。
- 证据（审计员 grep 实测）：
  - 全仓库 `grep -rn "WEB_TOKEN" deploy/ hub/config.yaml hub/cmd/hub/main.go` **零命中**；
    `deploy/.env.example` 只列 `NAS_IP / MUSIC_FOLDER / HASS_TOKEN / LLM_API_KEY /
    OPEN_XIAOAI_TOKEN`，无 `OPEN_XIAOAI_WEB_TOKEN`；
    `docker-compose.yml` 的 hub `environment` 只传 `HASS_TOKEN / LLM_API_KEY /
    OPEN_XIAOAI_TOKEN` 三项，未传 Web Token。
  - 服务端行为：`web.go:82-83` 从环境变量 `OPEN_XIAOAI_WEB_TOKEN` 读 token，
    为空时 `/api/*` 一律 401（fail closed，注释 `web.go:57-59` 自述）。
  - 后果链：NAS 部署的 Hub 永远以空 Web Token 启动 → `/api/status`、
    `/api/devices`、`/api/config`、`/api/ota` 全部 401 → Web 面板不可用，
    FR-11 的一键 OTA 下发（走 `POST /api/ota`）在 NAS 上无入口。
- 影响：高（部署面）。P0-1 修的是“未鉴权”，修完后部署物没跟上，Web 功能从
  “不安全地可用”变成了“安全地不可用”。更糟的是检查本身也被拖下水：
  `nas-install.sh` §5 用无 token 的 `curl http://127.0.0.1:8080/api/status`
  判“Hub Web 面板正常”——curl 对 401 也 exit 0，检查恒为 PASS，是**虚假检查**
  （见 P1-新-2 的同类）；`NAS_CHECKLIST.md` §3 预期 8080 返回
  “JSON，含 `"wake_word":"你好小李"`”，§6.1、§7.2、§7.3 的无 token `curl
  /api/devices` 在新版 Hub 上实际返回 401 JSON，清单与代码已脱节。
- 修复建议：三处同步补：（a）`.env.example` 加 `OPEN_XIAOAI_WEB_TOKEN`
  （强随机 ≥16 位，与 `OPEN_XIAOAI_TOKEN` 独立）；（b）compose 的 hub
  `environment` 加 `- OPEN_XIAOAI_WEB_TOKEN=${OPEN_XIAOAI_WEB_TOKEN}`；
  （c）`nas-install.sh` §0 预检查加 Web Token 的存在与强度校验（与
  `OPEN_XIAOAI_TOKEN` 同规则），§5 的 8080 检查改为带
  `Authorization: Bearer $OPEN_XIAOAI_WEB_TOKEN` 并断言 HTTP 200；
  （d）`NAS_CHECKLIST.md` §3/§6.1/§7.2/§7.3 的 curl 命令补 token 用法。
  注意：本轮任务只批准 §5 的两处 CI 修复，**未修此项**，只报告。

#### P1-新-2：`nas-install.sh` 的多处检查是虚假 PASS（401 也算通过；端口检查漏 4399；`sed` 改 HA 地址无校验）

- 位置：`deploy/nas-install.sh` §1（端口循环）、§3（`sed` 改 config）、§5（逐项检查）。
- 证据：
  - §5 的 `curl -s -m 5 http://127.0.0.1:8080/api/status` 与
    `curl -s -m 5 http://127.0.0.1:3000/` 均只看 curl 退出码，不看 HTTP 状态码——
    401 / 404 / 500 照样 exit 0、照样 `ok`。与 P1-新-1 叠加：8080 检查在新版 Hub 上
    必然 401 却必然 PASS。
  - §5 的 MA 检查用了 `-w '%{http_code}'` 并 `grep -q 200`，是正确的写法——
    说明 8080/3000 两处是遗漏，不是全脚本的风格问题。
  - §1 的端口占用循环是 `for p in 10300 10200 10400 1883 8095 3000 8080`——
    **漏了 4399**（Hub 主端口，P2-24 首轮已指出“未查 4399 端口”，仍未修）。
    4399 被占时 Hub 启动即绑定失败、容器 crash-loop，脚本却在 §1 报全部空闲。
    §5 的端口连通检查同样只查 `10300 10200 1883 8095 3000`，无 4399、无 10400 TCP、
    无 8080——与 `NAS_CHECKLIST.md` §3 的端口表（有 4399 / 10400 / 8080）不一致。
  - §3 的 `sed -i 's#ws://192.168.1.10:8123#ws://$NAS_IP:8123#'` 无任何事后校验：
    若 NAS 上的 config.yaml 里 HA 地址已不是 `192.168.1.10`（例如之前手工改过），
    sed 静默无操作，随后的 `grep -E 'music_assistant:|api:|url:|kws:|  url:'`
    只打印、不比对，脚本照常 `ok "config.yaml 已改 … HA 走 ws://$NAS_IP:8123"`。
    （P2-24 的“sed 改 HA 地址无校验”仍未修；另注意 `$NAS_IP` 在本地双引号内展开，
    若含 `/` 或 `&` 会破坏 sed 替换式——`NAS_IP` 正常是 IP，影响有限。）
- 影响：中高。部署脚本的核心价值是“FAIL 即停、PASS 即真”；虚假 PASS 会让
  操作员在 Web 401、4399 被占、HA 地址未改的情况下一路走到 §6 网易云扫码，
  再回头排查重得多。
- 修复建议：8080/3000 检查改用 `-w '%{http_code}'` 断言 200（8080 须带
  Web Token，见 P1-新-1）；§1 与 §5 的端口表补 4399（占用检查）与 10400/8080
  （连通检查），与 checklist §3 对齐；§3 的 sed 后加 `grep -q "ws://$NAS_IP:8123"`
  断言，失败即 FAIL。

#### P1-33（首轮延期，复核：仍在）：`nas-install.sh` 每次覆盖 NAS 端 `config.yaml`，`.bak` 不是旧文件备份

- 位置：`deploy/nas-install.sh` §2（`scp hub/config.yaml "$NAS_SSH:$NAS_DIR/hub/config.yaml"`）、
  §3（`cp $NAS_DIR/hub/config.yaml $NAS_DIR/hub/config.yaml.bak`）。
- 证据：顺序是先 scp 覆盖、后 cp 备份——`.bak` 拷的是**刚传上去的本机文件**，
  不是被覆盖掉的 NAS 旧文件。NAS 端经语音认领的房间（`config.Devices` 落盘）
  与经 Web 面板改的配置，在每次重跑 `nas-install.sh` 时都会被本机
  `hub/config.yaml` 的旧副本整体覆盖且无法从 `.bak` 找回。
- 影响：高（数据丢失，每次重部署都发生）。与 P1-31/P1-32 已修形成反差：
  卷与挂载修好了，部署脚本仍会在每次传文件时把数据清掉。
- 修复建议：§2 传文件前先把 NAS 上的旧 `config.yaml` 拉回本机（或在 NAS 端
  `cp config.yaml config.yaml.bak.$(date +%Y%m%d%H%M%S)` 再 scp）；或改传文件为
  “NAS 上不存在时才传，存在时只做 §3 的 sed 调整”。本轮未修，只报告。

### P1 — 首轮延期复核（仍在，未修）

- **P1-4（compose 全量发布无鉴权服务 + 全部 `:latest` 无 digest）— 仍在。**
  `docker-compose.yml` 现状：whisper `rhasspy/wyoming-whisper:latest` 发布
  `10300:10300`、piper `:latest` 发布 `10200:10200`、mosquitto
  `eclipse-mosquitto:latest` 发布 `1883:1883` 且仍未挂载任何配置文件/密码文件、
  music-assistant `:latest` 发布 `8095:8095`、netease-api `:latest` 发布
  `3000:3000`、snapserver/mpd 走 host 网络；`Dockerfile.hub` 仍为
  `debian:bookworm-slim` 无 digest。kws-sidecar 与 hub 走 host 网络是 2026-11-17
  有意的统一寻址决策（compose 内长注释说明 127.0.0.1 寻址），不是疏忽；
  但 Wyoming/MQTT/MA/网易云向局域网发布且无鉴权的部分未动。另注意：
  `NAS_CHECKLIST.md` §5.2 要求在 MA 界面把 API base URL 填
  `http://netease-api:3000`（容器名解析）——MA 与 netease-api 同在 compose
  默认 bridge 网络，容器名解析成立，与 hub 的 host 网络问题是两回事，
  不要混淆。
- **P1-8（`unpack.sh` / `patch.sh` 无哈希校验，`find | head -1` 任选）— 核心已修，残留两处。**
  核心已修：`unpack.sh:40` 与 `patch.sh:74-83` 入口均调
  `download-ota.py verify-bin/verify-sqfs` 做白名单校验（P0-5 的连带修复，
  本轮基线 `test_unpack_rejects_tampered_bin` 等全过）。残留：
  （a）`unpack.sh:52-55` 仍用 `find "$OUT-extract" -name 'root.squashfs' | head -1`
  与 `find … -name 'mico_version' | head -1`——unblob 从被篡改包里 carve 出多个同名
  文件时挑选仍是任意的，应改为“恰好一个，多个即失败”；
  （b）`unpack.sh` 不清理旧的 `$OUT-extract` / `$OUT/rootfs` 就重跑，
  `unsquashfs -f` 不删除新包里已不存在的旧文件，旧版文件可残留进 rootfs 再被
  `patch.sh` 复制打包；`patch.sh:62-67` 的 `ORIG_SQFS` 回退分支仍有
  `find "$DIR"-extract … | head -1`；
  （c）`unpack.sh` 只 `cat` 打印 `mico_version`，不比对它是否含入参 `$VERSION`
  （`flash-check.sh` 会比对，但那是事后检查，不是解包时的门）。
- **P1-36（串口被改成免密 root shell）— 仍在。** `patch_firmware.py` 的
  `patch_login` 仍把 inittab 改为 `ttyS0::askfirst:/bin/ash --login`
  （`patch_firmware.py:386`，`patch.sh` §6 的验证 grep 亦以此行为补丁标记）。
  首轮定级 P1 的理由（拆机串口即 root，无需密码）未变；本面只报告，不评价
  是否应改为保留 `askfirst:/bin/login`。
- **P1-37（mosquitto 1883 疑似匿名开放，且 Hub 根本不用 MQTT）— 仍在，需 NAS 核实。**
  compose 仍发布 `1883:1883`、无任何 mosquitto 配置卷；
  `NAS_CHECKLIST.md` 故障速查已写明“Hub 当前代码里没有 MQTT 引用，mosquitto
  只是预留（设备注册表 + HA 发现），不起作用不影响 Hub 跑”——与首轮审计一致。
  匿名与否取决于镜像默认配置，仓库里无据可查，仍需 NAS 实测（BLOCKED）。
- **P1-46（部署物无时区）— 仍在。** `docker-compose.yml` 与 `Dockerfile.hub`
  全文无 `TZ`（审计员 grep 确认）；容器默认 UTC，循环提醒与勿扰时段会整体错 8 小时。
  修复是一行：各服务加 `TZ=Asia/Shanghai`（或 `environment: - TZ=Asia/Shanghai`），
  但本轮未修，只报告。

### P2 — 新发现

#### P2-新-3：`TEST_REPORT.md` 的 P1-40 行（L166）仍留着已被证伪的“ORT 只要求 GLIBC_2.4”说法

- 位置：`TEST_REPORT.md:166`（“审计修复”节 P1-40 行，方案 (b) 括号内）：
  “复用官方 `libonnxruntime.so`，它只要求 GLIBC_2.4”。
- 证据：同一文件 L235-236（“P1-40 方案 B — .so 勘测与证伪”节，“前提纠正（协调员实测）”）
  已用 `readelf -V` 证伪——同捆 `libonnxruntime.so` 的 GLIBC 最高需求是 **2.34**
  （2.28/2.34/2.32/2.33/2.29/2.27）、GLIBCXX 最高 3.4.29；“B 原义证伪：
  重编 sherpa C API + 复用官方 libonnxruntime.so 不成立”。
  旧说法与纠正相隔 70 行，读前半部分的人会按错误前提做决策（例如以为方案 B
  只差重编 sherpa C API）。
- 影响：低（文档）。但任务简报点名了这一处，且它是“已被证伪却仍留着”的典型文档滞后。
- 修复建议：在 L166 该句后加删除线/勘误注，或直接改为“（该前提已被证伪：
  官方 libonnxruntime.so 要求 GLIBC 2.34，见本文件‘前提纠正’节）”。本轮未修，只报告。

#### P2-新-4：`TEST_REPORT.md` 多处旧数未随重编更新，与 runbook / checklist 脱节

- 位置与证据：
  - L143（“第二轮复核人重跑结果”）：LX06 镜像 SHA 仍写 `cc36b28e…54f06d`、
    OH2P 镜像 `db00add0…f0423`、OH2P 镜像 30,175,232 字节、ARM 产物 2,852,620 字节、
    `deploy/hub-linux-amd64` 11,534,848 字节——均已被取代：新 SHA 为
    LX06 `7266aba9…d97b3`（38,453,248 字节）、OH2P `139de363…6415`
    （30,179,328 字节，见 runbook §1 与 §1.3 实测）；ARM 默认交付已是 gnueabihf
    2,435,964 字节（L286）；hub 二进制已是 11,747,288 字节、SHA `1e4f6f6c…c80a`
    （`NAS_CHECKLIST.md` 镜像核实表已更新，TEST_REPORT 未同步）。
  - L117（Phase 4 表“一键 OTA”行）：“状态机 Push→Verifying→OK/Failed、超时扫荡、
    Web 下发均验证 | 通过 | 真机刷机验证；回滚本身是设备端 boot1 行为”——
    与首轮审计 P1-9 的结论冲突：`ConfirmHello` / `MarkRebooted` / `Sweep`
    在生产代码里零调用，OTA 任务永久停在 `pushing`，回滚闭环是零实现而非
    “只差真机验证”。该行把“包内单测通过”写成了接近“功能通过”。
  - Phase 4 表“对讲全双工 | 未做”与第二轮 B 行“对讲全双工已做（`go test` 88 通过）”
    自相矛盾——前者是旧表未更新，后者是新行；以 B 行为准，但读者无法自行判断。
  - 计数滞后：多处仍写 `go test 88 通过 / cargo test 37 / pytest 36`，
    而“审计修复”节协调员重跑已是 `go test 131 / cargo test 46 / pytest 61`。
    旧数本身是 dated 记录可以理解，但 P1-40 行与 L143 的 SHA 是会被直接拿去
    刷机的数，必须改。
- 影响：低到中（文档）。SHA 旧值若被误用，`flash-check.sh` 只打 WARN 不拦截
  （见 P2-新-5），操作员可能按旧 SHA 去核对新镜像而误判。
- 修复建议：L143 与 L166 的旧 SHA/旧说法改为新值并标注取代关系；L117 的 OTA 行
  改为“包内状态机单测通过；生产接线零调用（P1-9），回滚闭环未实现”；
  “对讲全双工”旧行标注已被第二轮 B 行取代。本轮未修，只报告。

#### P2-新-5：`flash-check.sh` 的补丁镜像 SHA 不一致只警告、不失败，且不校验镜像内的补丁内容

- 位置：`firmware-patch/scripts/flash-check.sh`（`KNOWN_PATCHED_SHA256` 比对分支）、
  `firmware-patch/scripts/patch.sh` §6（打包后验证）。
- 证据：
  - flash-check.sh：`PSHA` 与 `KNOWN_PATCHED_SHA256` 不一致时只 `warn`
    “补丁镜像 SHA256 与记录值不同（镜像可能已重编）”，`FAIL` 保持 0，脚本 exit 0。
    设计意图是“镜像重编后值会变”，runbook §1 也写“以重编后 flash-check.sh 打印的值为准”。
    但这意味着：一次静默重编（例如另一位修复工人改了 `patch_firmware.py` 后重编，
    或产物被替换）只产生一行 WARN，操作员在 §0.2 的“SHA 必须与表格一致”与脚本的
    “不一致只警告”之间得不到强制门。
  - flash-check.sh §4 只验补丁镜像的存在、大小 <40MB、魔数 `hsqs`、
    `unsquashfs -s` 可解析——**不验镜像里的补丁是否都在**（不解开看 shadow/PAM/
    dropbear/asound）。`patch.sh` §6 的解开验证也只查 11 个文件存在 + 3 处 grep
    （dropbear / inittab / asound），不查 shadow 的 `$6$`、PAM 的 pam_unix、
    OTA 禁用（bin/ota、flash.sh、wireless）、`/data/init.sh` 启动行、证书文件、
    文件权限（见首轮 P2-6，仍未修）。
- 影响：低到中。正常流程下 `patch.sh` §6 已挡掉大部分坏镜像；flash-check 的
  WARN-only 是最后一道人为核对，不是自动门。风险场景是“重编后未更新 runbook
  表格 + 操作员没看 WARN 就刷”。
- 修复建议（二选一）：（a）flash-check 加 `--strict` 模式（SHA 不一致即 FAIL，
  刷机时默认用 strict，重编后由重编人同步更新 runbook 表格与脚本内白名单再刷）；
  或（b）保持 WARN，但在 runbook §8 清单里把“补丁镜像 SHA 与 §1 表格一致，
  或已记明重编来源”列为必勾项并要求附 flash-check 输出（现 §8 已有该行，
  属于流程补强）。另把 `patch.sh` §6 的验证扩展到 shadow/PAM/OTA 禁用
  （P2-6 的原建议）。本轮未修，只报告。

#### P2-新-6：`patch.sh` 头注释仍写“刷 boot0，保留 boot1 回滚”，与 runbook 的“以实测 boot_part 为准”冲突

- 位置：`firmware-patch/scripts/patch.sh:8`（注释）：
  “A/B 分区：system0 / system1；刷 boot0（补丁系统），boot1 保留原厂系统用于回滚”；
  对照 `FLASH_RUNBOOK.md` §0.3 与 §3 决策表（boot_part 实测为 boot0 时目标是
  boot1/system1；未实测时不刷，例外才按默认写 system0/boot0 并记明）。
- 影响：低（文档）。`patch.sh` 本身不写分区（只产镜像），注释不会直接导致误刷；
  但它是操作员会读到的第二份“槽位说法”，与 runbook 的决策表并列时会造成混淆。
- 修复建议：把该行注释改为“目标槽以 FLASH_RUNBOOK.md §3 决策表为准（按实测
  boot_part 定），不要默认 boot0”。本轮未修，只报告。

#### P2-新-7：`nas-install.sh` 与 `NAS_CHECKLIST.md` 的端口表、容器数、检查口径三处不一致

- 证据（均实测读文件）：
  - 容器数：脚本 §5 写“9 个容器都应为 Up”，checklist §2.2 也写 9 个——一致，
    但脚本 §5 只 `docker compose ps` 打印、不数数，`ok` 是无条件打印的
    （`$SSH "… ps …" && ok "见上：9 个容器都应为 Up…"`——ps 成功即 ok，
    不校验行数/状态）。
  - 端口表：脚本 §1 占用检查（10300/10200/10400/1883/8095/3000/8080）与
    §5 连通检查（10300/10200/1883/8095/3000）与 checklist §3
    （10300/10200/10400/1883/8095/3000/8080/1704/1705/4399）三张表各不相同
    （见 P1-新-2）。
  - 网易云检查：首轮 P1-34 称“网易云检查恒为 PASS”——本轮复核：脚本 §5 的
    netease 检查是 `$SSH "curl -s -m 5 http://127.0.0.1:3000/ | head -c 200; echo"
    && ok "NeteaseCloudMusicApi 有响应（见上）"`——curl 只要能连上（哪怕返回
    404/500）即 ok，且 `head -c 200` 的输出不校验内容；与 8080 检查同类问题，
    仍是弱检查（首轮 P1-34 未修）。
- 影响：低。主要是检查口径不统一，逐项对着清单手工核对时才发现。
- 修复建议：脚本 §5 的 ps 检查改为数 Up 的容器数（`docker compose ps --format
  '{{.Status}}' | grep -c '^Up'` 应为 9）；三张端口表统一为 checklist §3 的
  完整表；netease 检查断言 HTTP 200 或响应含预期字段。本轮未修，只报告。

#### P2-新-8：lint 规则覆盖面缺口（5 处，均实测）

- 位置：`scripts/lint.sh`、`.golangci.yml`、`.semgrep/rules.yml`、
  `.github/workflows/ci.yml`。
- 证据：
  1. **lint.sh 不覆盖 Python 测试与 shell 脚本 lint**：`scripts/lint.sh` 的
     注释自述“测试（go test / cargo test / pytest）不在此脚本里”，且无
     `shellcheck` / `bash -n` 步骤——shell 脚本的语法只靠
     `test_shell_scripts.py::test_shell_scripts_syntax`（pytest 内 `bash -n`）
     间接覆盖，CI 里 pytest 失败时连语法检查也被跳过（run 44063914126 里
     `test_shell_scripts_syntax` 虽在 24 passed 内，但这是运气——它不依赖 fixture）。
  2. **Semgrep 规则 3 的 Rust 分支自认缺口**：`.semgrep/rules.yml` 注释写明
     `vec![x; n]` 未覆盖（“实测 semgrep 1.185.0 的 Rust 宏模式无法可靠表达”，
     “仓库现状无变量长度的 vec![x; n]，先不覆盖”）——现状无违规，但规则本身
     有已知洞，需后续换 clippy 自定义 lint 或等 semgrep 修复。
  3. **规则 1 的 allowlist 是“打印 DEFERRED 即通过”**：`check-wiring.go` 对
     P1-9 / P1-15 的 5 项接线缺失打印 `DEFERRED … deferred until after first gate`
     后返回通过（本轮 lint 输出实测）。机制本身是 bai 批复的暂缓安排，没错；
     但风险是 allowlist 没有过期提醒——第一关口后若忘记摘掉，lint 会继续全绿。
     建议在 allowlist 条目旁加“摘掉条件：第一关口后接线完成即删”，或在
     `TEST_REPORT.md` 的暂缓项里挂钩。
  4. **规则 3 Go 部分有 1 条 KNOWN-EXCEPTION 且无过期条件**：
     `SnapClient.call()` 的 `ReadBytes` 无长度上限（lint 输出实测，
     `KNOWN-EXCEPTION rule3 … P1-1 同类`）。与第 3 条同类风险：例外永久化。
  5. **`.golangci.yml` 未开 `gosec` / `noctx` 等**：当前启用
     `govet / staticcheck / gofmt / goimports / ineffassign / unused / errcheck`
     （`.golangci.yml` 实测）。`errcheck` 已在本轮补过 3 处 `_ =`（见 TEST_REPORT
     “审计修复”节），但 `gosec`（弱加密/硬编码凭据等）未启用——P0-2 类问题
     （硬编码默认密码）若重现，golangci 不会报警；不过 semgrep 规则 4
     （Token/密码禁默认值）覆盖了同类形状，属于分工而非完全缺失。
- 影响：低。lint 本轮全绿是真实的；以上是“规则本身的已知洞”，不是“规则被绕过”。
- 修复建议：lint.sh 加 `bash -n`（或 shellcheck，若可用）步骤；给规则 1 allowlist
  与规则 3 KNOWN-EXCEPTION 加摘掉条件与日期；评估 `gosec` 是否启用（与 semgrep
  规则 4 去重后决定）。本轮未修，只报告。

### P2 — 首轮延期复核（仍在，未修）

- **P2-6（`patch.sh` 打包后验证不完整）— 仍在。** §6 只查 11 个文件存在 +
  3 处 grep（见 P2-新-5），不查 shadow/PAM/禁 OTA 与文件权限。另：验证用的
  `unsquashfs -d … -f -no-exit-code` 会掩盖解包错误（P2-39，见下）。
- **P2-8（`patch_firmware.py` 的 `--password` 与 hash 进进程参数/日志）— 已缓解大半。**
  首轮指 `--password` 命令行参数；现 `patch_firmware.py` 已无 `--password`
  参数（`main()` 只有 `--model / --rootfs / --cert-dir`，`:720-730`），密码只走
  环境变量或 `getpass`。残留：`patch.sh` 仍用环境变量 `SSH_PASSWORD` 传密码
  （进程环境对同机其他进程可见，`/proc/<pid>/environ` 仅同用户可读，影响有限）；
  `patch_firmware.py` 注释与 README 的示例行仍出现 `SSH_PASSWORD=<密码>` 字样
  （`patch_firmware.py:22,267`）——是文档示例，不是代码行为，风险低。
- **P2-9（版本号校验正则未转义 `.`，可含 `/` 造成路径穿越）— 仍在。**
  `unpack.sh:24-27`、`patch.sh:37-40`、`flash-check.sh:30-33` 三处共用
  `case "$VERSION" in [0-9]*.[0-9]*.[0-9]*)`——`.` 未转义匹配任意单字符，
  `[0-9]*` 可空，实测模式可接受 `1/2/3`（`.` 匹配 `/`）等非版本号，
  随后 `DIR="$WORK/${MODEL}_${VERSION}"` 会把 `/` 带进路径。缓解因素：
  `unpack.sh` / `patch.sh` 在建 `OUT` 目录前先跑 `verify-bin` 白名单校验，
  不在 `EXPECTED` 里的版本会被拒绝，所以穿越在正常流程里到不了写操作；
  `flash-check.sh` 无白名单门，但它只读文件，最多查不到文件而 FAIL。
  定级维持 P2（纵深防御缺口）。
- **P2-23（`download-ota.py` 无参数校验、无下载超时）— 部分仍在。**
  下载超时：`fetch_ota_info` 的 `urlopen(url, timeout=30)` 与下载的流式实现
  已有超时（首轮 P0-5 修复的连带，`download-ota.py:60-61` 附近原问题已改）；
  参数校验：`build_ota_url(model, version, …)` 仍不校验 model/version 格式，
  但 `verify_local` / `EXPECTED` 白名单会在非法版本上拒绝，下载路径的
  `fetch_ota_info` 前也有白名单门（`download-ota.py:86-91` 报错含白名单列表）。
  残留主要是 CLI 参数校验的纵深问题，定级 P2。
- **P2-24（`nas-install.sh` 多处检查水分）— 部分仍在。** 未查 4399 端口
  （见 P1-新-2）、容器计数靠肉眼（见 P2-新-7）、sed 改 HA 地址无校验
  （见 P1-新-2）、远端命令注入面（`$NAS_DIR` / `$NAS_IP` 未做校验即拼进
  `$SSH "…"` 远程命令；`NAS_IP` 若含 `;` 可注入——`NAS_IP` 来自本机 `.env`
  或环境变量，是操作员自供值，影响有限，定级 P2）。
- **P2-30（`mksquashfs` 参数与原厂不完全一致，镜像增大且无预警阈值）— 仍在。**
  `patch.sh:118-121` 的 `mksquashfs` 只取 `file` 读出的 `-comp` 与 `-b`
  （其余参数 `-noappend -all-root -always-use-fragments -no-xattrs -no-exports`
  是写死的）；与原厂参数是否完全一致未在仓库内留证据；只有 `>= 40MB` 的
  硬失败，无“例如 >38MB 即警告”的预警阈值。
- **P2-31（`patch.sh` 里 `file` 解析的显式检查是死代码）— 未复现，现代码的检查是活的。**
  `patch.sh:104-112` 的 `[ -n "$COMPRESSION" ] || fail` 与
  `[ -n "$BLOCKSIZE" ] || fail` 均有实际分支，`file` 输出缺对应字段时会
  fail；`COMPRESSION` / `BLOCKSIZE` 在 `mksquashfs` 调用里真实使用。
  首轮的“死代码”说法在本轮代码上不成立，建议销项或降级（需首轮审计员确认
  当时所指是否为另一处已改掉的代码）。
- **P2-32（`patch_firmware.py` 不可重复执行，非幂等）— 部分仍在。**
  `patch_all`（`patch_firmware.py:690-720`）先 `preflight` 再逐项应用，
  无回滚（见 P1-30 同类）；`check_ssh` 要求原厂 `enable=0` 行仍在，
  对已打过补丁的 rootfs 重跑会在 `patch_ssh` 处 `PatchError`；
  `check_login` 则已接受 `$6$`（已打补丁态）。正常流程下 `patch.sh` 每次
  `rm -rf squashfs-root` 后从干净解包复制，影响有限；直接重跑
  `patch_firmware.py` 会失败，属于“非幂等”仍在。
- **P2-33（asound 改动无备份，设备端不可逆）— 仍在。** `patch_asound`
  （`patch_firmware.py:530-560`）原地 `_write` 两份 asound 配置，无 `.bak`；
  恢复只能靠 `unpack.sh` 从官方 `.bin` 重解（流程上可行，但不是“备份”）。
- **P2-34（`/data/init.sh` 是无校验的开机 root 钩子）— 仍在。**
  `patch_start`（`patch_firmware.py:492-512`）在 `rc.local` 里加
  `[ -f "/data/init.sh" ] && sh /data/init.sh >/dev/null 2>&1 &`；
  `/data/init.sh` 本身不在补丁里创建、无签名/哈希校验，任何能写 `/data/init.sh`
  的一方（后续客户端安装流程、或拿到 `/data` 写权限者）即得开机 root 执行。
  按设计它是客户端安装的钩子，但“无校验”是事实，定级维持 P2。
- **P2-35（OTA 禁用有残留路径未验证，疑似）— 仍在，需真机核实。**
  `patch_disable_ota` 改 `bin/ota`、`bin/flash.sh`、`etc/init.d/wireless`
  （`patch_firmware.py:440-490`），仓库内无证据表明存在其他 OTA 入口；
  是否有残留路径只能真机核实（BLOCKED）。
- **P2-36（`Dockerfile.hub` 无 USER/HEALTHCHECK、无 `.dockerignore`、未 pin）— 仍在。**
  `Dockerfile.hub` 全文：`FROM debian:bookworm-slim`（无 digest）、无 `USER`、
  无 `HEALTHCHECK`、无 `.dockerignore`（构建上下文 `deploy/` 含 `.env`——
  注意：`.env` 不会被 COPY 进镜像（Dockerfile 只 COPY 二进制），但构建上下文
  仍会把 `.env` 发给 Docker daemon，属信息暴露面；另 `deploy/hub-linux-amd64`
  是 gitignored 的构建产物，`docker compose build` 在干净 checkout 里会因缺
  二进制而失败，`nas-install.sh` §0 的预检查挡了这一层）。
- **P2-37（compose 无资源限制、无 user）— 仍在。** 全 compose 无任何
  `mem_limit / cpus / user:`；hub 以 root 跑在 host 网络下（与 P1-4 同根）。
- **P2-38（Hub OTA 30s 超时偏短；未来复用 flash.sh 会重写 u-boot）— 仍在。**
  OTA 30 秒超时是 `hub/internal/ota` 的常量（非本面代码），runbook 未提及；
  `flash.sh` 复用风险是厂商代码风险，首刷不走该路径（runbook §1 已写明
  “补丁镜像没有小米签名，走不通”原厂 OTA 路径）。
- **P2-39（`unpack.sh` 的 `unsquashfs -no-exit-code` 会掩盖解包错误）— 已缓解。**
  `unpack.sh` 与 `patch.sh` 仍用 `-no-exit-code`，但其后均有产物校验
  （`unpack.sh` 查 `rootfs` 目录与 `etc/init.d/dropbear / bin/ota / bin/flash.sh`
  等关键文件；`patch.sh` §6 解开验证 11 个文件 + 3 处 grep）。残留风险只剩
  “关键文件都在、但其他文件损坏”的部分解包，定级维持 P2 偏低。
- **P2-55（部署物缺失：`.env.example` 不存在、二进制会悄悄过期、sed 静默无操作）— 部分仍在。**
  `.env.example` **已存在**（`deploy/.env.example`，含 NAS_IP / MUSIC_FOLDER /
  HASS_TOKEN / LLM_API_KEY / OPEN_XIAOAI_TOKEN——但缺 `OPEN_XIAOAI_WEB_TOKEN`，
  见 P1-新-1）；“二进制会悄悄过期”仍在（`deploy/hub-linux-amd64` 无版本/哈希记录
  机制，`NAS_CHECKLIST.md` 镜像核实表手写记录了 11,747,288 字节与 SHA
  `1e4f6f6c…c80a`，但无自动核对）；sed 静默无操作仍在（见 P1-新-2）。
- **P2-56（Web 面板：GET 配置暴露内部拓扑、未匹配路径都返回 200 首页）— 仍在（非本面代码，部署侧可见）。**
  服务端 `hub/internal/web` 的行为未在本轮复核（属 Web 面审计员范围）；
  部署侧的可见部分是：`nas-install.sh` §5 与 checklist 的 8080 检查均不校验
  响应体，无法发现该问题。本面只记录，不展开。

### P1-30 备注（`patch_all` 中途失败无回滚；`_write` 非原子写）

- 首轮 P1-30 属 firmware-patch 面，延期未修，本轮复核确认仍在：
  `_write`（`patch_firmware.py:97-98`）是直写 `path.write_text(...)`，无
  临时文件 + rename；`patch_all`（`:690-720`）逐项顺序写，中途任一步
  `PatchError` 即退出，已写的前几项留在 rootfs 副本里，无回滚。
  缓解因素：`patch.sh` 每次从干净解包复制（`rm -rf squashfs-root` 后 `cp -a`），
  且失败时删半成品镜像（`fail()` + `trap ERR`），所以半成品不会进刷机流程；
  但“补丁中途失败的 rootfs 副本是半打状态”本身仍是事实，重跑 `patch.sh`
  可以恢复，`patch_firmware.py` 直接重跑则会因 `check_ssh` 失败（见 P2-32）。
  定级维持 P1（首轮定级），未升级。

## 5. CI 修复（已实施，分支 audit-round2-ci-fix，未 push、未开 PR）

任务批准的两处修复均已做。改动清单：

### 5.1 Go job：apt 安装行加 `libopusfile-dev`

- 文件：`.github/workflows/ci.yml`（Go job “安装 libopus”步骤）。
- 改动前：`sudo apt-get update && sudo apt-get install -y libopus-dev pkg-config`
- 改动后：`sudo apt-get update && sudo apt-get install -y libopus-dev libopusfile-dev pkg-config`
- 依据：§2.1 的独立复核——`go vet` 在编译 `gopkg.in/hraban/opus.v2` 时报
  `Package opusfile was not found in the pkg-config search path`，`opusfile.pc`
  由 `libopusfile-dev` 提供；日志无其他根因（`opus.pc` 本身已找到，
  `libopus-dev` 正常安装）。

### 5.2 Python job：依赖固件 rootfs fixture 的测试在缺失时 `pytest.skip`（带明确理由）

- 文件：`firmware-patch/tests/conftest.py`、`firmware-patch/tests/test_patch_firmware.py`、
  `firmware-patch/tests/test_shell_scripts.py`。
- 做法（按任务要求选跳过方案，不删测试、不静默跳过、不把 rootfs 塞进仓库、
  不让 CI 现场下载解固件）：
  - `conftest.py` 新增两个模块级标志与辅助函数：`HAS_FIRMWARE_ROOTFS` /
    `HAS_FIRMWARE_BIN`（文件存在性检查）与 `require_firmware_rootfs()` /
    `require_firmware_bin()`（缺失时 `pytest.skip("…")`，理由写明缺失路径与
    “固件材料不在仓库里，本地跑需 speaker-open-xiaoai-rebuild/firmware/”）。
    另新增 `HAS_PATCHED_WORK`（`firmware-patch/work/<型号>_<版本>/root-patched.squashfs`
    是否存在）与 `require_patched_work()`，覆盖依赖 `work/` 已交付产物的测试。
  - `test_patch_firmware.py`：模块级 `patched_rootfs` fixture 与所有直接调
    `copy_rootfs()` 的测试在入口先调 `require_firmware_rootfs()`；
    不依赖 fixture 的纯逻辑测试（`sha512_crypt` 等）不动，CI 里继续跑。
  - `test_shell_scripts.py`：模块级 `unpacked_work` fixture 与
    `test_unpack_with_ota_bin_env`、`test_unpack_rejects_tampered_bin`、
    `test_patch_rejects_tampered_bin`、`test_patch_rejects_tampered_sqfs_when_bin_absent`
    等入口先调 `require_firmware_bin()`；`test_flash_check_ok_on_real_work` 与
    `test_flash_check_rejects_tampered_image` 入口先调 `require_patched_work()`。
  - 跳过是显式的：pytest 输出里显示为 `SKIPPED` 并带理由（`pytest -q` 的
    `s` 计数与 `-rs` 可见），不是静默跳过。
- kws-sidecar 测试步骤确认仍在 python job 里（ci.yml:68-72，未动），且与固件
  步骤无依赖——但如 P1-新-3 所述，两步同 job 的结构问题未在本轮改动。
- YAML 校验：`python3 -c "import yaml; yaml.safe_load(open('.github/workflows/ci.yml'))"`
  通过（见 §5.3）。

### 5.3 本地验证结果（2026-11-17 晚，全部实跑）

- YAML 校验通过：`python3 -c "import yaml; yaml.safe_load(open('.github/workflows/ci.yml'))"`
  打印 `YAML OK`。
- 包名核实：`apt-cache policy libopusfile-dev` 在 Ubuntu 24.04 上有候选
  `0.12-4build3`（与本机自编 libopusfile 0.12 同版本线）；`libopus-dev`
  候选 `1.4-1build1`。
- **CI 条件模拟**（干净 checkout，无固件材料、无 work/）：把仓库复制到
  `/tmp/ci-sim/open-xiaoai-next`（排除 `firmware-patch/work/`），跑
  `python3 -m pytest firmware-patch/tests/ -q -rs`，结果
  **24 passed, 37 skipped in 0.94s**——与 run 44063914126 的
  `24 passed, 24 failed, 1 skipped, 12 errors` 逐项对应
  （24 failed + 12 errors + 1 skipped = 37 skipped），0 failed、0 errors；
  `-rs` 输出里每条 skip 都带明确理由（如“缺固件 rootfs fixture：
  /tmp/ci-sim/firmware/rootfs 不存在——原厂固件不在仓库里；本地跑需先把固件解包到
  speaker-open-xiaoai-rebuild/firmware/rootfs”）。
- **本地有 fixture 时照常执行**：仓库原位（固件材料齐全）跑
  `python3 -m pytest firmware-patch/tests/ -q -rs`，结果
  **61 passed in 134.65s**（0 failed、0 errors、0 skipped）。
  注：本轮基线时本机缺 `unsquashfs` 导致 7 项失败/出错，验证前已装好
  squashfs-tools 4.6.1（`apt-get update` 由并行进程完成持锁后释放，
  随后 `apt-get install -y squashfs-tools` 成功）；61 passed 是装好工具后的
  完整结果，不是基线时的 54 passed。
- **kws-sidecar 测试仍过**：`kws-sidecar/.venv/bin/python -m pytest tests/ -q`
  结果 **16 passed in 14.26s**（默认复用 `~/workspace/.kws-exp` 已验证资产，
  未重新下载模型）。注：系统 python 缺 sherpa-onnx，须用 `.venv` 的 python 跑，
  与 CI（pip 装 requirements）环境不同；本轮未改边车代码，只确认未被连带破坏。
- **未验证**：GitHub Actions 上的实际运行（须等协调员开 PR 后 CI 跑）；
  `go vet` 在装 `libopusfile-dev` 后的 CI 环境里是否通过，只能以 CI 实跑为准，
  本地 `go vet` 全绿用的是自编 libopus，不能冒充 CI 结果。

## 附：分级统计

| 分级 | 首轮本面延期项（复核仍在） | 本轮新发现 | 其中已修（§5 除外） |
|---|---|---|---|
| P0 | 0（P0-2/P0-5/P0-6 已修，见 §4.0） | 0 | — |
| P1 | P1-33、P1-4、P1-8（残留）、P1-36、P1-37、P1-46、P1-30 | P1-新-1、P1-新-2 | 0 |
| P2 | P2-6、P2-8（残留）、P2-9、P2-23（残留）、P2-24（残留）、P2-30、P2-32（部分）、P2-33、P2-34、P2-35、P2-36、P2-37、P2-38、P2-39（已缓解）、P2-55（部分）、P2-56 | P2-新-1、P2-新-2、P2-新-3、P2-新-4、P2-新-5、P2-新-6、P2-新-7、P2-新-8 | 0（P2-31 未复现，建议销项） |

最严重的 3 项摘要：
1. **P1-新-1**：`OPEN_XIAOAI_WEB_TOKEN` 未接进部署——NAS 上的 Web 面板注定 401，
   FR-11 OTA 下发入口不可用；nas-install 与 checklist 的 8080 检查随之失效。
2. **P1-33（延期）**：`nas-install.sh` 每次覆盖 NAS 端 `config.yaml`，`.bak`
   拷的是刚传上去的本机文件——房间认领与 Web 改配置在每次重部署时丢失且无法找回。
3. **P1-新-2**：`nas-install.sh` 多处虚假 PASS（401 也算通过、漏 4399、sed 无校验）——
   部署脚本“PASS 即真”的承诺不成立，问题会被拖到扫码/联调阶段才暴露。
