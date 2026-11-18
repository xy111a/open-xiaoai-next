# 第三轮审计：固件 / 部署面分工报告

审计分支 `audit-round3`（main `3a9d1d9`，PR #4 已合入），工作目录干净、审计过程未改任何文件，未动 `release/`。镜像用 `~/workspace/tools/squashfs-tools/usr/bin/unsquashfs` 解到 `/tmp` 核查。

## 镜像实测基线（全部一致）

- LX06 `lx06_1.94.13-root-patched.squashfs`：38,457,344 字节，SHA256 `7dafb950…05e21` ✓；OH2P：30,179,328 字节，SHA256 `fe2bab99…5c926` ✓。与 `work/` 产物、`FLASH_RUNBOOK.md` §1 表、`flash-check.sh` 内 `KNOWN_PATCHED_SHA256` 三处一致；`flash-check.sh --strict` 两型号均 exit 0。
- P1-36：`ttyS0::askfirst:/bin/login` 在镜像内 ✓；PAM `libmico` 已注释、`pam_unix` 已启用 ✓；shadow `$6$` + 0600 ✓；dropbear 三门控已删 ✓；OTA 三处禁用在镜像内验证 ✓（`bin/ota` 两函数头 `return 1`、`flash.sh` `upgrade_param_check` 头 `return 1`、`wireless` `ota()` 头 `echo "download_fail"` + `exit 3`）；LX06 `libxaudio_engine.so` 内 `hw:0,3` 计数 0、`noop` 计数 1 ✓。
- P2-33：`etc/asound.conf.bak` 与 `etc/asound.conf.dts.bak` 经 `cmp` 与原厂 `work/<型号>/rootfs` 逐字节相同，两型号 ✓。
- P2-34：`/etc/open-xiaoai/init.sh` 在两镜像内存在，755、root 属主，两镜像 SHA256 相同，`rc.local:4` 接线 ✓。
- mTLS：`ca.crt`/`client.crt` 存在，`client.key` 0600，两镜像内三文件 SHA256 完全相同、与 `work/certs` 一致，有效期至 2036 ✓。
- **核实限度（按任务要求写明）**：root 密码 hash 与旧镜像的一致性**无法比对**——旧镜像（SHA `7266aba9…`/`139de363…`）已不在磁盘上，无任何旧 hash 记录可供对照。只观察到两份新镜像的盐互不相同（各自独立生成或各自沿用各自旧 hash 均可解释，无法区分）。

## P0（部署前必须修）：0 项

## P1（重要缺陷）：3 项

**F-1：打包后验证三处标记为空转——原厂镜像也能通过（patch.sh §6 / flash-check.sh §4b）**
结论：三处“补丁标记”检查无法区分已打补丁与原厂镜像，打包验证形同虚设。
证据：对原厂 `firmware-patch/work/<型号>/rootfs` 实测——`etc/init.d/dropbear` 原厂第 104 行已有 `mkdir -p /data/etc/dropbear`（在 `keygen()` 内，原厂自带）；`etc/init.d/wireless` 原厂已有 1 处 `echo "download_fail"`（curl 失败分支）；OH2P 原厂 `etc/asound.conf` 已有 2 处 `slave.pcm Capture`。而 `patch.sh:247,252,229` 与 `flash-check.sh:180,196,206` 恰好用这三处 `grep -q` 作“已打补丁”断言。另 `inittab` 的 `ttyS0 login` 检查同理（原厂即有，仅能作回归 guard）。 live 镜像本身是正确打过补丁的（直接核查了门控删除与函数头），且 `patch_firmware.py` 的进程内校验是正确的，所以这是纵深防御失效，不是 live 缺陷。
修复：改断言为补丁特有标记——dropbear 查三门控已删（`_require_not_in` 同款）；wireless 查 `ota()` 头后 60 字符内同时有 `echo "download_fail"` 与 `exit 3`（照抄 `patch_firmware.py:patch_disable_ota` 的校验）；OH2P asound 查 Capture 块参数或与 `.bak` 的差异。两处脚本同步改，并补回归测试（拿原厂 rootfs 跑 §6 应 FAIL）。

**F-2：inittab 仍有两处免密 root shell——tts/0 与 tty1 的 `/bin/ash --login`，只守住了 ttyS0**
结论：P1-36 只修了串口三行中的一行；另两行无任何代码检查。
证据：两镜像 `/etc/inittab` 全文三行控制台——`tts/0::askfirst:/bin/ash --login`、`ttyS0::askfirst:/bin/login`、`tty1::askfirst:/bin/ash --login`；`patch.sh:236` §6 只断言 `ttyS0` 行，`patch_firmware.py:check_login`（:456-457）同样只要求 `ttyS0` 行。内核侧证据（降低定级为 P1 而非 P0 的原因）：解压 LX06 `boot.img` 内核（8,241,160 字节）实测——meson-uart 驱动注册的设备名是 `ttyS`（内核内 `ttyS\x00` 出现 2 次、`tts\x00` 出现 0 次），`tts/0` 在 4.9 内核 + devtmpfs 下大概率是不存在的死项；`tty1` 是虚拟终端，音箱无键盘/显示不可达。但以上均未在真机启动日志上证实。
修复：把三行统一改为 `/bin/login`（或在 `patch.sh` §6 增加“全表无免密 shell”断言）；真机首刷后核对启动日志确认 `/dev/tts/0` 不存在。

**F-3：P2-34 的 SHA256 pin 校验永不触发——没有任何写入方会产生 `/data/init.sh.sha256`**
结论：包装器三条校验中 pin 这一条是死代码，实际只剩属主 + 权限两条。
证据：全仓 grep `init.sh.sha256` 只有 `firmware-patch/scripts/patch_firmware.py:65,72,661`、`patch.sh:257` 与测试文件引用；没有任何客户端安装流程、脚本或文档写该文件；且 `/data/init.sh` 本身如何装到音箱上，在仓内也无文档（`firmware-patch/README.md` 只字未提安装流程）。包装器本身写得正确（`ls -ln` 取属主/权限串、第 6/9 位 `w` 拒写、符号链接因 `lrwxrwxrwx` 第 6 位为 `w` 被拒、pin 为空即拒），但 pin 的前置条件永远不成立。
修复：二选一——(a) 让客户端安装流程写 pin 文件并把“写 pin”写进安装文档；(b) 把 pin 改为必填（缺 pin 文件即拒绝执行）。并补一份 `/data/init.sh` 安装流程文档。

## P2（次要 / 改进）：8 项

**F-4：root hash“沿用旧镜像”的说法无法核实，且移植步骤未进脚本**
结论：文档宣称的 hash 复用无任何可验证锚点，重编时靠手工操作。
证据：`TEST_REPORT.md:597-598` 称重编用一次性随机密码走 `SSH_PASSWORD_FILE`，随后“把两份新镜像的 shadow root 行换回旧镜像的 `$6$ hash（逐字节比对验证一致）”；但 `patch.sh`/`patch_firmware.py` 内无任何移植 hash 的代码路径（`patch_firmware.py:sha512_crypt` 每次都用 `secrets` 新随机盐，见 `:284-296`），且旧镜像已不在。修复：把移植步骤脚本化（或至少把所用命令记录进 runbook）；首刷后用串口 + bai 已有密码实测登录一次（真机项）。

**F-5：`PATCH_NO_CERTS` 任意非空值都跳过证书，包括 `PATCH_NO_CERTS=0`**
证据：`patch.sh:80` 用 `[ -n "${PATCH_NO_CERTS:-}" ]` 判断。修复：只认 `=1`（`[ "${PATCH_NO_CERTS:-}" = "1" ]`）。

**F-6：P2-39 inode 硬门 fail-open——`unsquashfs -s` 读不到 inode 数时只警告跳过**
证据：`patch.sh:230-234` 与 `unpack.sh` 同款（`EXPECT_INODES` 为空时 `echo 警告…跳过数量硬门`）。机制本身已实测通过（LX06 2144/2144、OH2P 2105/2105，`find` 计数与 `unsquashfs -s` 一致）。修复：读不到即 FAIL（或至少 `--strict` 下 FAIL）。

**F-7：`firmware-patch/README.md:47` 仍写旧 rc.local 直行，与镜像实际不符**
证据：该行写 `etc/rc.local 增加 [ -f "/data/init.sh" ] && sh /data/init.sh …`，实际镜像是包装器行 `[ -x /etc/open-xiaoai/init.sh ] && /etc/open-xiaoai/init.sh …`。修复：同步为包装器行。

**F-8：两台音箱共用同一份 mTLS 客户端证书/私钥**
结论：家用可接受，但吊销无隔离。
证据：两镜像内 `ca.crt`/`client.crt`/`client.key` 的 SHA256 完全相同。修复：如需按设备吊销，`gen-certs.sh` 改为按设备签发独立 client 证书；至少在文档里写明共用身份的含义。

**F-9：原厂 `boot.img` ramdisk 内 `/init` 仍有免密兜底 shell（`exec sh`），补丁范围外**
证据：解压 LX06 `boot.img` ramdisk（2,802,688 字节）含 `"Failed to switch root, dropping to a shell"` + `exec sh`；squashfs 内 `/init` 是同一脚本的副本。仅在 `switch_root` 失败的启动失败路径可达，且 boot.img 不在本次补丁范围内。修复：暂接受并记录；要修需改 boot.img（签名/刷机流程另议）。

**F-10：每日 03:00 cron 仍跑 `/bin/ota slient` 向小米查升级**
结论：仅查询、不致刷机，但每日联网行为仍在。
证据：两镜像 `/etc/crontabs/root` 有 `* 3 * * * /bin/ota slient # check ota`；`bin/ota` 的 `check()` 只调 `matool_check_upgrade`（只读查询），`upgrade()` 头 `return 1` 已死。修复：接受现状，或在 `patch_firmware.py` 里删该 cron 行并记录。

**F-11：`FIXES_P2.md:130` 的 P2-39 实测数已过期（历史记录，非 live 断言）**
证据：该行写“补丁镜像 2141/2141”，现 LX06 实测 2144 inodes（`unsquashfs -s` 与 `find` 均为 2144）。修复：注上“2026-11-17 旧重编值”或更新数字。

## P2-35：BLOCKED（真机）——代码层面已无可做的项

三处禁用点均在镜像内验证；`boardupgrade.sh` 只被 `flash.sh` source，无其他调用方；`upgrade()` 内 `flash.sh` 调用在 `return 1` 之后不可达；`wireless` 的 `ota()` 经 `EXTRA_COMMANDS` 对外暴露但头行即 `exit 3`。残留真机核实三项：(a) `flash.sh` 主流程顶层的 `return 1` 在设备 busybox ash 下是否真退出脚本（若不退出会继续往下走 `upgrade_verify_image`/`board_system_upgrade`——原厂代码通篇用同一写法，大概率没问题，但 VM 内无法证明）；(b) 刷后 cron 的 `slient` 检查是否真不下载/不触发；(c) 串口密码登录 + `/dev/tts/0` 是否存在（与 F-2 同源）。

## 已核实无问题的 headline 项（抽查通过）

- patch.sh：证书默认装入、无目录直接失败 ✓（`patch.sh:74-86`）；P2-9 严格正则 ✓（三脚本同款）；P2-31 两处 `|| true` ✓（`:173-174`）；`trap ERR` + `fail()` 清理 `OUT`/`VERIFY_DIR`/`SQFS_ROOT` ✓；P2-30 38MB 预警 + 超原厂 2MB 警告 + `PATCH_XZ_BCJ` opt-in 默认关 ✓；P2-32 幂等 ✓（含 wireless latent bug 修复，镜像内 `ota()` 只有一个插入块）；P2-8 `SSH_PASSWORD_FILE` 优先 ✓（`patch_firmware.py:342-346`）；P2-6 §6 其余断言 ✓（除 F-1 的三处空转外）。
- download-ota.py P2-23 参数白名单 ✓；白名单 hash 实测通过（oh2p `.bin`、`lx06`/`oh2p` 原始 root.squashfs）✓。unpack.sh P2-9/P2-39 ✓。
- 部署：compose 5 个拉取镜像 + `Dockerfile.hub` 的 `debian:bookworm-slim@sha256:734a72b3…` 共 6 个 digest 全钉死，且与 Docker Hub 2026-11-18 实测 `latest` digest 逐一比对一致（whisper `3cc01f64…`、piper `333622ab…`、snapserver `20840a2b…`、mpd `6f8dbbb8…`、netease-api `ed9fc80e…`、debian `734a72b3…`，注释取值日期 2026-11-18 ✓）；`cap_drop: [ALL]` 7/7 服务 ✓；`user: "1000:1000"` 仅 Hub 与边车，其余 5 个仍 root 的原因与 cap_drop 补偿已写在 compose 头注释 ✓；`Dockerfile.hub` 有 appuser + HEALTHCHECK，`deploy/.dockerignore` 排除 `.env`/`*.md`/`*.sha256` ✓；`read_only` 暂缓现状已在注释记录 ✓。
- nas-install.sh：§0 Hub 清单校验 ✓（清单与二进制实测一致：12,093,544 字节 / `973a3b1f…3091`）；§5 容器数 7 检查 + 注册制日志行检查 ✓（缺失仅 WARN）；NAS_IP/NAS_DIR/NAS_SSH 三项输入正则 ✓；`remote_curl_api` 带引号 heredoc + NAS 端读 token ✓；P2-55 kws-sidecar 构建源发货 ✓（Dockerfile/server.py/entrypoint.sh/requirements.txt/scripts 齐全）。
- MA 残留：仅历史注释与文档说明，无服务/卷/端口/`MA_TOKEN` 的 live 引用 ✓。
- FW-P2-1 gofmt 进 CI ✓（`ci.yml:39-42`）；FW-P2-3 TEST_REPORT 勘误删除线 ✓（L166 `~~GLIBC_2.29~~` → 2.34 +“已被证伪”）；FW-P2-4 文档数值同步 ✓（Hub 12,093,544/`973a3b1f`、LX06 38,457,344/`7dafb950`、OH2P 30,179,328/`fe2bab99` 与实物一致，旧值均有取代标注）；FW-P2-6 patch.sh 头注释 ✓；FW-P2-7 checklist 同步 ✓（§4.1b `ws://127.0.0.1:10400`、§4.2 注册制、§1.1 边车源码发货、7 容器）；FW-P2-8 ✓（`scripts/check-vec-len.sh`、CI shell job、`check-wiring.go` 的 DEFERRED allowlist 已空且摘掉条件已写、`check-bounds.go` 已加摘掉条件）。
- P2-38 runbook §1.5 ✓（“绝不复用原厂 `bin/flash.sh` 推补丁镜像”），`VerifyTimeout` 30 秒与 `hub/internal/ota/ota.go:65` 一致 ✓。

## 计数与 headline 结论

- **P0：0 项 / P1：3 项（F-1、F-2、F-3）/ P2：8 项（F-4～F-11）/ BLOCKED（真机）：P2-35 及其 3 个子项**
- Headline：固件面**可以刷**——两份镜像内容核实全部通过（P1-36、P2-33、P2-34 三缺口确实在镜像里，证书、shadow、OTA 禁用、libxaudio 均就位），部署面 digest、cap_drop、user、清单校验、MA 清理也全部核实通过，P0 为零。但首刷前建议先修 F-1（否则下次重编的打包验证拦不住回归），并定夺 F-2（inittab 另两行）与 F-3（pin 死代码）；F-4 的 hash 复用只能留待首刷时用串口密码登录实测。
