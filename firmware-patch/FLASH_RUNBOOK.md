# 刷机 Runbook — LX06 / OH2P 首刷补丁固件

> 状态：2026-11-17 编写。**真机步骤全部未验证**，每一步在真机上第一次执行时
> 都要按本文逐条打勾记录；任何一步与本文不符（分区名、boot_part 值、报错）
> 都先停手，不要凭记忆继续。
>
> 适用范围：只刷 `firmware-patch/work/<型号>_<版本>/root-patched.squashfs`
>（由 `patch.sh` 产出、经 `flash-check.sh` 干跑通过的镜像）。不适用于
> 官方 `.bin` 整包升级（那是另一条路：原厂 `bin/ota` + `miso` 签名校验，
> 补丁镜像没有小米签名，走不通）。

## 0. 红线（违反任何一条立即停手）

1. **只写非当前槽的 system 分区**。绝不写 bootloader / tpl / dtb / kernel
   分区，绝不写当前正在用的 system 分区。
2. **刷前必须先过 `flash-check.sh`**，且刷机电脑上的镜像 SHA256 必须与
   §1 表格一致（或与重编记录一致并写明重编来源）。
3. **boot_part 的值以真机实测为准**，不许凭“TEST_REPORT 说刷 boot0”就默认。
   TEST_REPORT 的“刷入 boot0，保留 boot1”是“当前槽为 boot1 时”的写法；
   若实测当前槽是 boot0，目标就是 boot1 / system1（见 §3 决策表）。
4. **不口头操作**：所有分区号、槽位、命令都以本 runbook 的命令实测输出为准，
   现场抄数进 §8 检查清单。
5. 刷机全程不断电、不拔数据线；笔记本用电源适配器供电，不要只用电池。

## 1. 材料与工具（版本钉死）

### 1.1 镜像（刷机电脑上先备好，先跑 `flash-check.sh`）

| 文件 | 大小（字节） | SHA256 |
|---|---|---|
| 官方 LX06 1.94.13 `.bin` | 43,478,328 | `a56a38619fd98e467adb8c998757f80109c4dac89db48d62dae985e3062476ed` |
| 官方 OH2P 1.58.6 `.bin` | 36,042,824 | `1f351d879c06ff3bd7bd4b4c68e2c8fa45eee1a82be5839e57899ae8f9b7c27b` |
| LX06 原始 root.squashfs | 37,500,699 | `317a54909f12b63f22128965bf9005c06435d15f61c4202bb88c4524bf1e2ef4` |
| OH2P 原始 root.squashfs | 29,953,626 | `c33dfc21a2a3093b6c4e41f3bf0738e1ff21daf59bbccc5203863bdb62dfa493` |
| **LX06 补丁镜像 root-patched.squashfs（2026-11-18 P1 修复轮重编后实测）** | **38,457,344** | **`1a5c27603d7ab8064841f3e970258ef39ae754c2847a46fc0529168c250d0281`** |
| **OH2P 补丁镜像 root-patched.squashfs（2026-11-18 P1 修复轮重编后实测）** | **30,179,328** | **`8db02670f0c81bda5059e5a0fe2cad5b6045f4afe252a310353c7ac3acf9d197`** |

> 旧 SHA（LX06 `7dafb9507bdc4894715f2978ffaf5fd6a11af18ad61ed268f48c6309f0a05e21`、OH2P `fe2bab99eb15e423b689c24bace6fec1b665b1073dc12ec1c9fa2e4433b5c926`，2026-11-18 P2 轮打的）已被取代：第三轮审计 P1 修复轮（F-1 打包验证改真标记、F-2 inittab 三行统一 `/bin/login`、F-3 init 包装器 SHA256 pin 改必填）改了补丁代码，2026-11-18 用现版 `patch.sh` 重编，SHA 变了，以上表为准（已用 `sha256sum` 在磁盘上核实）。更旧的 SHA（LX06 `7266aba99b769947bd3ce937c23f0dc0443ad52883f04e4992eed45c0d7d97b3`、OH2P `139de363d39d0a5472bf9b00c015effd9a24c379ca5fa2436cc21c43e8706415`，2026-11-17 打的；LX06 `cc36b28e…54f06d`、OH2P `db00add0…f0423`）同样作废。
> `flash-check.sh` 内部的 `KNOWN_PATCHED_SHA256` 已同步为上表新值（2026-11-18 P1 修复轮重编后核实：两型号默认与 `--strict` 四路均 exit 0，`PASS 补丁镜像 SHA256 与记录值一致`，无 WARN）。
>
> 【2026-11-18 P1 修复轮重编注：重编时 root 密码未换——重编前先把两份旧镜像的 shadow root 行记到本地 gitignore 文件 `firmware-patch/work/.root-hashes-backup`（永不进仓库），重编时用 `PATCH_REUSE_ROOT_HASH_FILE` 指向该 hash 沿用（见 `patch_firmware.py:_reuse_root_hash`），重编后逐字节比对验证 hash 与备份一致；bai 手里已有的密码继续有效，未生成新密码。重编内容：F-1（`patch.sh` §6 与 `flash-check.sh` §4b 的补丁标记断言改走 `patch_firmware.py:verify_markers`，只认补丁特有标记，原厂镜像必 FAIL）、F-2（三行控制台统一 `/bin/login`；`tts/0` 经内核实测是死项——meson-uart 只注册 `ttyS`，内核里无 `tts` 设备名，`/dev/tts/0` 不会生成；但纵深防御仍统一改掉）、F-3（init 包装器 SHA256 pin 改必填，缺 pin 即拒绝执行；安装流程见 `firmware-patch/README.md`“/data/init.sh 安装流程”）。】
>
> 【2026-11-18 P2 轮重编注：重编时 root 密码未换——新镜像的 shadow root 行沿用了旧镜像的 `$6$` hash（bai 手里已有的密码继续有效），未生成新密码；重编还修了两个管线缺口：(1) `patch.sh` 此前从不传 `--cert-dir`，直接跑它会产出无 mTLS 证书的镜像，现已默认用 `work/certs` 装入（缺目录直接失败，仅测试可设 `PATCH_NO_CERTS=1`）；(2) `patch.sh` §6 验 `.bak` 曾用 "slave.pcm Capture" 标记法，OH2P 原厂 asound.conf 里就有该行会误杀，现改为与原始 root.squashfs 逐字节比对。详见 `TEST_REPORT.md`“P2 全修轮”一节。】

补丁镜像上限 **40MB（0x02800000 = 41,943,040 字节）**，超了变砖。
`flash-check.sh` 会逐项核对上表，任一 FAIL 都不许刷。

> 补丁镜像若被重编（例如另一位修复工人在改 `patch_firmware.py`），SHA256
> 会变：以重编后 `flash-check.sh` 打印的值为准，并把新值与重编原因记进
> §8 检查清单，不要硬套上表。
>
> 【2026-11-18 P2 轮重编注：上表镜像已是 2026-11-18 用现版 `patch.sh` 重编的，
> 含 P1-36（inittab `/bin/login`）、P2-33（.bak）、P2-34（init 包装器）与 mTLS
> 证书；`flash-check.sh` §4b 四路（两型号 × 默认/`--strict`）均 exit 0。
> 旧的“镜像过期三缺口”二选一已失效，不再适用。】
>
> 【2026-11-18 P1 修复轮重编注：上表镜像已是 P1 修复轮重编的（F-1/F-2/F-3，
> 见 §1.1 表上方的重编注）；`flash-check.sh` §4b 四路（两型号 × 默认/`--strict`）
> 均 exit 0。`flash-check.sh` §4b 现要求：.bak 缺失即 FAIL（OH2P 的
> asound.conf 只能由 .bak 证明打过补丁）；init 包装器在而 pin 不是必填即 FAIL。】

### 1.2 恢复用原厂材料（回滚的第二道保险，第一道是未动过的槽，见 §6）

官方 `.bin` 本身是 miso 容器，不能直接当分区镜像刷；恢复时用 unblob
从它里面 carve 出的段（已随 `unpack.sh` 产出，位置
`work/<型号>_<版本>-extract/<型号>_<版本>.bin_extract/`）：

| 段 | LX06 1.94.13 | OH2P 1.58.6 |
|---|---|---|
| kernel（恢复 kernel 槽用，首刷用不到） | `boot.img`，5,288,767 字节，SHA256 `97b772225fe1c287a47c3add92565faf16e93178ce8fc247853a1021468586c7` | `boot.img.encrypt`，5,399,040 字节，SHA256 `7c0fa95e0bf6cc52657bb53ec6dfa179571769c63f486fd6ac0aa996f8ffb72e` |
| rootfs（恢复 system 槽用） | `root.squashfs`（同 §1.1） | `root.squashfs`（同 §1.1） |
| dtb / u-boot | `dtb.img`、`u-boot.bin.usb.bl2`、`u-boot.bin.usb.tpl` | `dtb.img.encrypt`、`u-boot.bin.encrypt.usb.bl2`、`u-boot.bin.encrypt.usb.tpl` |

**首刷不碰 dtb / u-boot / kernel**，所以正常情况下 §1.2 的 kernel/dtb
段用不上；它们只在“有人误写了这些分区”的事故恢复时才用（见 §7.3）。

### 1.3 刷机工具（钉死版本，二选一）

| 平台 | 工具 | 版本 / 来源 | 安装 |
|---|---|---|---|
| macOS | `references/open-xiaoai/packages/flash-tool` 的 `./flash`（基于 khadas `aml-flash-tool`，macOS 二进制来自 numbqq） | v1.0.0 | `brew install libusb-compat`；`chmod +x ./flash` |
| Windows | Amlogic Flash Tool（`update.exe`） | v6.0.0 | 解压到桌面并重命名为 `Amlogic_Flash_Tool_v6.0.0`；先运行 `AMLLogic driver installer.exe` 装 USB 驱动；用 Git Bash 进 `/bin` 目录操作 |

数据线：必须是能传数据的数据线（不只是充电线）。LX06：拆机，主板左上角
Micro USB 调试口（拆机参考：52audio 38303）。OH2P：底部 Type-C，免拆机。

### 1.4 干跑检查（刷机电脑上，VM 步骤已验证）

```bash
cd firmware-patch
./scripts/flash-check.sh lx06 1.94.13   # 或 oh2p 1.58.6
# 刷机当天用 --strict 再跑一遍：SHA 与 §1.1 记录值不一致即 FAIL（默认只警告）。
./scripts/flash-check.sh --strict lx06 1.94.13
```

全部 PASS（WARN 只提示、不拦截）才继续。把输出保存下来附在 §8 清单后。
注意：`--strict` 下 SHA 不一致即 FAIL；§4b 内容验证缺标记即 FAIL
（.bak 与 init 包装器缺失只警告——针对旧版补丁产物；2026-11-18 重编的
当前镜像里两者都有，不会触发该警告）。

### 1.5 Hub OTA 更新（首刷之后的事，不在首刷流程里）

- 首刷之后再推补丁镜像走 Hub 自有 OTA（`hub/internal/ota`）。**绝不复用
  原厂 `bin/flash.sh` 推补丁镜像**——它会重写 u-boot；补丁镜像没有小米
  签名，走不通原厂 OTA，复用它等于把设备变砖。
- Hub OTA 的 `VerifyTimeout` 是 **30 秒**（`hub/internal/ota/ota.go` 的
  `VerifyTimeout: 30s`，`PushTimeout` 10 分钟是另一项）：设备须在 30 秒内
  回 hello，否则任务标失败。设备回 hello 慢（首次启动、网络抖动）时任务
  会误判失败——先看设备日志再决定是否重推，不要连续重推。
`flash-check.sh` 只做本地校验：官方 `.bin` / 原始 root.squashfs 白名单
SHA256、补丁镜像大小 / 魔数 / `unsquashfs -s` 解析、mico_version 一致性。
设备侧步骤它验不了，见 §2–§6。

## 2. 刷前：读当前启动槽（真机，BLOCKED，未验证）

目的：确定哪个槽是当前槽，目标槽 = 非当前槽。槽位配对关系（已核对
原厂 `bin/boardupgrade.sh:222-242`）：**boot0 配 system0，boot1 配
system1**；`boot_part` 选中的就是当前槽。

### 2.1 首选：设备 SSH 上读（若能连上）

```sh
ssh root@<音箱 IP>
/usr/bin/fw_env -g boot_part    # 期望输出 boot0 或 boot1，二者必居其一
cat /proc/mtd                   # 记下 boot0/boot1/system0/system1 各自的 mtd 号与大小
```

- `fw_env` 在 LX06 / OH2P 原厂 rootfs 里都是 `/usr/bin/fw_env`。
- **预期困难（已核对代码，未在真机上确认）**：原厂 release 通道下
  dropbear 被 `/data/ssh_en`（或 `/data/.ssh_en`、`/tmp/ssh_en`）门控，
  默认不开 SSH；没有已公开的免拆机办法在原厂系统里建这个文件。
  所以这一步很可能连不上 SSH——**连不上是预期内的，不代表设备坏了**，
  按 §2.2 走，并在 §8 清单里记“SSH 不可用，boot_part 改由 §2.2 确认”。
- 若 SSH 能连上（例如这台之前已刷过补丁系统），同时做 §4 的设备侧备份。

### 2.2 备选：Windows 刷机工具在 u-boot 下读

```bat
./update.exe identify
./update.exe bulkcmd "printenv boot_part"
```

- `identify` 须显示版本号（如 `This firmware version is 0-7-0-16-0-0-0-0`）
  才算连上；不显示就重插电源重试（参考文档称刷写报错也可多试几次，
  但 `printenv` 这类只读命令失败时不要接着做写操作）。
- macOS 的 `./flash` 没有读环境变量的命令（只有 `connect` / `delay` /
  `switch` / `system`），所以 macOS 路径下 boot_part 只能走 §2.1；
  §2.1 也走不通时，**先不要猜槽位**：用 Windows 工具读一次再回来，
  或按 §3 的保守策略（见下）执行并在 §8 记明“boot_part 未实测”。

## 3. 目标槽决策表（按 §2 实测值填，不许跳过）

| 实测 `boot_part` | 当前槽 | 目标槽（写这里） | 刷机命令里的槽位 | 回滚槽（不动它） |
|---|---|---|---|---|
| `boot1` | boot1 / system1 | **boot0 / system0** | `switch boot0` + `system system0 …` | boot1 / system1 |
| `boot0` | boot0 / system0 | **boot1 / system1** | `switch boot1` + `system system1 …` | boot0 / system0 |
| 未实测（§2.1/§2.2 均失败） | 未知 | **不刷**，先解决读槽问题；只有一种例外见下 | — | — |

例外（仅当 Windows 工具也读不到、且必须当天刷时）：按参考实现
`references/open-xiaoai` 的默认流程写 **system0 + boot0**，并在 §8
清单里明确记“boot_part 未实测，按默认流程写 system0/boot0；若刷后不
开机，第一步就是按 §6.2 翻回另一槽”。**这是降级方案，不是常规方案。**

## 4. 刷前：备份（真机，BLOCKED，未验证）

### 4.1 NAS 侧备份（刷机电脑上就能做，必做）

确认 §1.1 + §1.2 的文件都在刷机电脑上，且 `flash-check.sh` 已通过。
这就是“官方包级”备份：即使设备侧备份做不了，凭这些文件 + 未动过的槽
也能恢复（§7）。

### 4.2 设备侧备份（SSH 能连上时才做；连不上就记 BLOCKED，见 §2.1）

在音箱上以 root 执行，把**四个分区**（两个 kernel + 两个 system）都读出来：

```sh
mkdir -p /data/backup-YYYYMMDD
cat /proc/mtd
# 按 /proc/mtd 的实际输出，把下面四行的 mtd 号换成实测值（示例为占位写法）：
dd if=/dev/mtd<N_boot0>   of=/data/backup-YYYYMMDD/boot0.img   bs=1M
dd if=/dev/mtd<N_boot1>   of=/data/backup-YYYYMMDD/boot1.img   bs=1M
dd if=/dev/mtd<N_system0> of=/data/backup-YYYYMMDD/system0.img bs=1M
dd if=/dev/mtd<N_system1> of=/data/backup-YYYYMMDD/system1.img bs=1M
cd /data/backup-YYYYMMDD && sha256sum * > SHA256SUMS && cat SHA256SUMS
```

- `/dev/mtdN`、`dd`、`sha256sum` 在 LX06 / OH2P 原厂 rootfs 里都有
  （`dd` 在 `/bin`，`sha256sum` 在 `/usr/bin`）。
- 备份完立即 `scp` 到 NAS/刷机电脑，**不要只留在音箱 `/data` 里**
  （刷机出问题时可能连不上它）。
- 校验：NAS 上对拷回的文件重算 SHA256，与音箱上 `SHA256SUMS` 逐项比对，
  一致才算备份完成。

## 5. 刷机步骤（真机，BLOCKED，未验证）

前置：§1 干跑通过、§2 槽位已定、§4.1 已做、补丁镜像已拷到刷机电脑。
全程只写 **§3 决策表定出的目标 system 分区**。

### 5.1 macOS（`./flash` v1.0.0）

```bash
cd references/open-xiaoai/packages/flash-tool
./flash connect
# 按提示拔掉音箱电源再重新插上，等设备连上
./flash delay 15
./flash switch <目标 boot 槽，如 boot0>     # 与 §3 决策表一致
./flash system <目标 system 分区，如 system0> /path/to/root-patched.squashfs
```

### 5.2 Windows（Amlogic Flash Tool v6.0.0，Git Bash 进 `/bin` 目录）

```bash
./update.exe identify
# 显示版本号才算连上；不显示就重插电源重试
./update.exe bulkcmd "setenv bootdelay 15"
./update.exe bulkcmd "setenv boot_part <目标 boot 槽，如 boot0>"
./update.exe bulkcmd "saveenv"
./update.exe partition <目标 system 分区，如 system0> /path/to/root-patched.squashfs
```

- 两条路的 `switch` / `setenv boot_part` 与 `system` / `partition`
  的槽位必须成对（boot0 配 system0，boot1 配 system1），且必须与 §3
  决策表一致；写之前大声念一遍槽位再回车。
- 若提示刷写错误：参考文档称“可以多试几次，不一定是真的无法刷机”；
  重试前先确认数据线插紧、设备已重新上电；**连续两次同一错误就停手，
  按 §7 排查，不要反复试到分区写坏**。
- 提示成功后：拔掉数据线和电源，重新插电重启。若无反应，等几分钟再
  重新上电；仍无反应按 §6.2 回滚，不要马上重刷。

## 6. 刷后校验与回滚演练（真机，BLOCKED，未验证）

### 6.1 刷后校验（缺一不可）

```sh
ssh root@<音箱 IP>   # 用 patch 时设定的 SSH 密码登录（密码由 patch 操作人设定，
                      # 记在密码管理器里；禁止使用仓库里曾出现过的默认口令）
/usr/bin/fw_env -g boot_part    # 必须等于 §3 的目标槽
cat /proc/mtd                   # 确认分区表与刷前一致（分区名与大小未变）
grep "option ROM" /usr/share/mico/version   # 仍为原厂版本号（如 1.94.13）——
                                            # 版本号没变是正常的：补丁只改了
                                            # rootfs 内容，不改版本号
head -c <补丁镜像字节数> /dev/mtd<N_目标system> | sha256sum
    # 必须等于 §1.1 的补丁镜像 SHA256（读回校验：只取前 N 字节比对，
    # N = 补丁镜像大小；分区剩余部分是擦除态填充，不参与比对）
```

功能校验（在 Hub 侧做）：音箱上线、唤醒词 `你好小李` 有反应、点歌能出声、
Hub 日志里能看到该设备。任一失败都先按 §6.2 回滚，再排查。

### 6.2 回滚演练（首刷当天必须做一次，不要留到以后）

1. 把 `boot_part` 翻回原来的槽（二选一）：
   - SSH 里：`/usr/bin/fw_env -s boot_part <原槽，如 boot1>`；
   - 或 USB 工具：macOS `./flash switch <原槽>`，Windows
     `./update.exe bulkcmd "setenv boot_part <原槽>"` + `bulkcmd "saveenv"`。
2. 断电重启，确认原厂系统能启动：`小爱同学`唤醒有反应（原厂唤醒词），
   且 SSH 连不上（原厂 dropbear 默认门控，见 §2.1——连不上是正常的）。
3. 确认回滚成功后，再把 `boot_part` 翻回目标槽（补丁系统），断电重启，
   重做 §6.1 校验。
4. 把两次翻槽的实测结果记进 §8 清单。**没做过回滚演练的刷机算没做完。**

### 6.3 刷后补备份（首刷成功后立即做）

用补丁系统的 SSH 把**未动过的那个槽**（回滚槽）的 kernel + rootfs
读出来，按 §4.2 同样的方法存 NAS 并验 SHA256。这样即使以后两个槽
都被写过，也有一份“设备实测原厂分区”备份。

## 7. 失败处理

| 现象 | 处理 |
|---|---|
| `identify` / `connect` 连不上 | 换数据线（确认能传数据）、重插电源重试、检查驱动（Windows 先装 `AMLLogic driver installer.exe`；macOS 确认 `libusb-compat` 已装）。只读命令失败时不要做写操作。 |
| 刷写报错 | 先重插电源重试一次；连续两次同一错误就停手，检查镜像 SHA256 与槽位是否写对，不要反复试。 |
| 刷后无反应（等几分钟重上电仍无） | 不要马上重刷：先用 USB 工具把 `boot_part` 翻回原槽（§6.2 的命令），断电重启，看原厂系统能不能起来。能起来 = 补丁槽有问题，按 §7 下一行处理。 |
| 目标槽系统损坏、原槽正常 | 原槽启动后，用 USB 工具把**原厂 root.squashfs**（§1.1，已验 SHA256）重新写进目标 system 分区：`./flash system <目标system> <原厂 root.squashfs>` 或 `./update.exe partition <目标system> <原厂 root.squashfs>`，再把 `boot_part` 指回目标槽验证；或直接留在原槽当原厂系统用。 |
| kernel 槽被误写（首刷正常流程不会发生） | 用 USB 工具写回原厂 kernel 段：`./update.exe partition <boot0 或 boot1> <boot.img（LX06）或 boot.img.encrypt（OH2P）>`（§1.2 的文件与 SHA256）。macOS `./flash` 没有写 kernel 分区的命令，这种情况改用 Windows 工具。 |
| bootloader / tpl / dtb 被误写 | **停手，不要自行修**：这是最危险的情况，记下当时执行的完整命令与输出，找协调人/原厂资料再定恢复方案；不要反复上电。 |

## 8. 执行检查清单（每次刷机填一份）

| 项 | 实测值 / 结果 | 打勾 |
|---|---|---|
| 型号 / 版本 | LX06 1.94.13 / OH2P 1.58.6（圈一） | ☐ |
| `flash-check.sh` 通过（附输出） | 通过 / 失败（失败不许刷） | ☐ |
| 补丁镜像 SHA256（实测） | | ☐ |
| 补丁镜像来源（本次 patch 产物 / 重编：重编原因 ） | | ☐ |
| 实测 `boot_part`（刷前） | boot0 / boot1 / 未实测（圈一） | ☐ |
| 目标槽（§3 决策表） | boot_ / system_ | ☐ |
| NAS 侧备份 §4.1 完成 | 是 / 否 | ☐ |
| 设备侧备份 §4.2（SSH 可用时） | 完成，SHA256 已比对 / SSH 不可用，记 BLOCKED | ☐ |
| 刷机工具与版本 | macOS ./flash v1.0.0 / Windows Amlogic v6.0.0（圈一） | ☐ |
| 刷写结果 | 成功 / 失败（写明报错原文） | ☐ |
| 刷后 `boot_part`（实测） | boot_ | ☐ |
| 刷后分区读回 SHA256 = 补丁镜像 SHA256 | 是 / 否 | ☐ |
| 功能校验（上线 / 唤醒 / 点歌） | 通过 / 失败 | ☐ |
| 回滚演练 §6.2（翻回原槽能进原厂系统） | 通过 / 未做（未做 = 刷机没做完） | ☐ |
| 刷后补备份 §6.3 | 完成 / 未做 | ☐ |
| 操作人 / 复核人 / 日期 | | ☐ |

## 附：命令速查（只列本 runbook 用到的，禁止口头补充）

```sh
# 设备上（root）
/usr/bin/fw_env -g boot_part                 # 读当前槽
/usr/bin/fw_env -s boot_part boot0           # 翻槽（boot0/boot1 按 §3 填）
cat /proc/mtd                                # 分区表
sha256sum <文件>                             # 算 SHA256
head -c <字节数> /dev/mtd<N> | sha256sum     # 分区读回校验

# 刷机电脑上
./scripts/flash-check.sh lx06 1.94.13       # 干跑检查
./flash connect | ./flash delay 15 | ./flash switch boot0
./flash system system0 root-patched.squashfs
./update.exe identify
./update.exe bulkcmd "setenv bootdelay 15"
./update.exe bulkcmd "setenv boot_part boot0"
./update.exe bulkcmd "saveenv"
./update.exe partition system0 root-patched.squashfs
```

依据：原厂 `bin/boardupgrade.sh`（槽位配对、boot_part 翻转、mtd 写入方式）、
原厂 `bin/flash.sh`（`upgrade_verify_image` 用 `miso -v` 验签——补丁镜像
无小米签名，所以首刷不走 OTA 路径）、`references/open-xiaoai` 与
`references/ownding-open-xiaoai` 的刷机文档（工具版本与命令）、
`firmware-patch/README.md`（镜像大小上限 40MB）。分区号（mtdN 的 N）
只能取真机 `cat /proc/mtd` 的实测值，本文不写任何推测的分区号。
