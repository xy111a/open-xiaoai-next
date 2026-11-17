# firmware-patch — 固件补丁流水线

OTA 下载 → 解包 → 打补丁 → 重新打包 → 刷机。只做 LX06 / OH2P。

## 流程

```
download-ota.py lx06 1.94.13   # 不用小米账号，签名算法是公开的
unpack.sh lx06 1.94.13         # unblob + unsquashfs → work/lx06_1.94.13/rootfs
patch.sh lx06 1.94.13          # 补丁 + 重新打包 → work/lx06_1.94.13/root-patched.squashfs
# 刷机：LX06 拆机走 Micro USB（Amlogic Flash Tool / macOS flash-tool）
#      OH2P 底部 Type-C 免拆
```

`patch.sh` 是 Phase 1 实现（不再是骨架）：校验入参 → 复制 rootfs 到
`work/<型号>_<版本>/squashfs-root`（绝不原地改解包结果）→ 调
`scripts/patch_firmware.py` 打补丁 → 用 `file` 读原始 root.squashfs 的
压缩方式与 blocksize → `mksquashfs` 以相同参数重新打包 → 大小校验
（< 40MB，超了就失败、不输出镜像）→ `unsquashfs` 解开验证关键文件
与补丁标记。任何一步失败都 exit 非零，并删掉半成品镜像。

## 参数

| 参数 | 值 | 来源 |
|---|---|---|
| LX06 官方固件 | 1.94.13，43,478,328 字节，MD5 `b202de2bbe149ce884aa500911151b95` | REA_ANALYSIS.md |
| OH2P 官方固件 | 1.58.6，36,042,824 字节，MD5 `eb86c0c12ea118fb701433bf009a180c` | REA_ANALYSIS.md |
| OTA 接口 | `api.miwifi.com/rs/grayupgrade/v2/{model}`，s = md5(base64(otaInfo))，后缀 `8007236f-a2d6-4847-ac83-c49395ad6d65`，**time 是毫秒时间戳**（ota.ts 用 `Date.now()`） | REA_ANALYSIS.md / ota.ts |
| 分区上限 | 40MB（0x02800000），超了变砖 | REA_ANALYSIS.md |
| A/B 分区 | system0 / system1；刷 boot0，boot1 留原厂系统回滚 | REA_ANALYSIS.md |
| 校验 | miso：RSA + SHA1，签名覆盖包体从偏移 0xc 起；公钥走 `/dev/miso` 内核驱动 | REA_ANALYSIS.md |
| DTS | 刷机后回不来（整条链断了），别修它；NAS 侧 EQ 补约 80% | REA_ANALYSIS.md |
| root.squashfs | Squashfs v4.0 xz，2137 inodes，37,500,699 字节，blocksize 131072，1348 个文件 | REA_ANALYSIS.md / 实测 |
| root 密码 | 无默认值：`SSH_PASSWORD` 未设时补丁直接失败退出；密码 ≥16 位，只从环境变量或交互式输入读，hash 为 sha512crypt（`$6$`，每次随机 16 字符盐，5000 轮），hash 永不打印 | openssl 实测 |
| libxaudio | `hw:0,3` 在 1.94.13 里唯一一次出现，偏移 773628，6 字节改为 `noop\x00\x00` | strings 实测 |

## 补丁内容（`scripts/patch_firmware.py`，纯 Python 自研实现）

效果移植自参考实现 `references/open-xiaoai/packages/client-patch/patches/`
（只读 Spec，移植的是“改哪些文件、改成什么样”，未复制代码）：

| 补丁 | 改动 |
|---|---|
| SSH | `etc/init.d/dropbear`：去掉 `enable=0` 返回、蓝牙模式不启动、`/data/ssh_en` 门控；启动时 `mkdir -p /data/etc/dropbear`，缺 host key 则 keygen |
| 登录 | `etc/shadow` root 密码换 sha512crypt（`$6$`，随机盐）；`etc/inittab` 的 ttyS0 改 `/bin/ash --login`；`etc/pam.d/common-auth` 注释 `libmico-pam.so`、启用 `pam_unix.so` |
| 禁 OTA | `bin/ota` 的 `download_upgrade()`/`upgrade()` 开头 `return 1`；`bin/flash.sh` 的 `upgrade_param_check()` 开头 `return 1`；`etc/init.d/wireless` 的 `ota()` 开头 `echo download_fail` + `exit 3` |
| 启动 | `etc/rc.local` 增加 `[ -f "/data/init.sh" ] && sh /data/init.sh >/dev/null 2>&1 &` |
| asound（LX06/OH2P） | `etc/asound.conf` 与 `etc/asound.conf.dts`：`pcm.mico_record`/`pcm.noop` 改走 `Capture`（dsnoop，hw:0,3，48kHz，S32_LE，8ch，period 384，buffer 6144）；`defaults.pcm.rate_converter "speexrate_medium"`（已有则不重复加） |
| libxaudio（仅 LX06） | `usr/lib/libxaudio_engine.so` 里 `hw:0,3` 的 6 字节改为 `noop\x00\x00`；OH2P 跳过这一步 |
| mTLS 证书（可选，`--cert-dir`） | 把 `gen-certs.sh` 生成的 `ca.crt`/`client.crt`/`client.key` 装进 `etc/open-xiaoai/tls/`（证书 0644、私钥 0600）；客户端凭此证书连 Hub :4399，Hub 校验客户端证书后才放行 |

每个补丁函数都是“检查前置条件 → 应用 → 校验结果”，可被 pytest 单独
调用和断言；`patch_all()` 先对全部步骤做 preflight（只读），任一前置
条件不满足就立即失败且不写任何文件。

## 工具链注意（实测踩过的坑）

- `unpack.sh` 调 unblob 用的是 `-e/--extract-dir`，unblob 26.6.4 没有
  `-x`（旧脚本的 `-x` 是错的，已修正）。
- unblob 在缺提取器（`sasquatch`、`7z`）时以非零退出，但
  `root.squashfs` 与 `mico_version` 仍会被正确 carve 出来——那两个
  提取器本就不需要（squashfs 内容由系统 `unsquashfs` 解，boot.img
  不参与打补丁）。`unpack.sh` 不以 unblob 的退出码为准，以产物校验为准。
- 容器里无 CAP_MKNOD，`unsquashfs` 建 `/dev/*` 设备节点会报
  `Operation not permitted`（非致命；原厂 rootfs 的 `dev/` 本就是空的，
  启动时由 devtmpfs 接管）。两处 `unsquashfs` 调用都加了
  `-no-exit-code`，真正的门是解开后的文件校验。
- `download-ota.py` 原先用秒级 `time.time()`，已按 ota.ts 与
  REA_ANALYSIS.md 修正为毫秒时间戳；输出目录由 `scripts/work` 改为
  `firmware-patch/work`（与 unpack.sh / patch.sh 一致）。
- `/tmp` 是 512MB tmpfs，装不下完整流程的多份 rootfs 副本；测试临时
  目录由 `tests/conftest.py` 改到 `firmware-patch/.pytest-tmp`
  （workspace 磁盘）。跑完测试可删该目录。
- squashfs-tools（mksquashfs/unsquashfs 4.6.1）需另装：本机后台 apt
  的 `apt-get update` 自 03:39 起卡住未完成，2026-11-17 12:00 左右
  检查时仍在跑；改用本地 apt 缓存里已有的
  `squashfs-tools_1:4.6.1-1build1_amd64.deb` 用 `dpkg -i` 装好
  （未改 apt 配置、未杀后台 apt 进程）。注意 `/usr` 重启会丢，
  重启后需重装。

## 测试

```bash
cd firmware-patch
python3 -m pytest tests/ -q
```

| 文件 | 覆盖 |
|---|---|
| `tests/test_download_ota.py` | 签名算法：固定 `time.time`，断言已知输入（lx06/1.94.13/release/sn=""）的 s 值与完整 URL 与 REA 记录一致；MD5 校验失败时 `main()` 抛错；全程 mock 网络（`urlopen`/`urlretrieve`），不发真实请求 |
| `tests/test_patch_firmware.py` | 在真实 `firmware/rootfs` 副本上跑真实补丁，断言每个被改文件的改后内容；sha512crypt 与 openssl 输出一致；前置条件不满足时失败且不写任何文件；OH2P 跳过 libxaudio；CLI 入口真实跑通 |
| `tests/test_shell_scripts.py` | 用真实 `lx06_1.94.13.bin` 重跑解包，核对 root.squashfs 37,500,699 字节、Squashfs v4.0 xz、2137 inodes；`patch.sh` 端到端：产出存在且 < 40MB，解开后关键补丁都在；`bash -n` 与 `py_compile` |

2026-11-17 实测：**35 项全过**（`34 passed` 之后补了 OTA_BIN 用例，
全套约 2 分钟）。补丁镜像实测 38,453,248 字节（原 root.squashfs
37,500,699 字节，+952,549），距 40MB 上限余 3,489,792 字节。

## 红线

- **免拆机伪造小米官方 OTA 不可能。** 私钥只在小米手里，公钥从内核驱动来，
  flash.sh 调的是不带 `-u` 的 `miso -v`，换公钥文件也没用。第一次必须拆机刷，
  之后全部走 Hub 自有 OTA。
- 刷机前 SSH 跑 `free -m` 确认 256MB；刷错型号固件（LX06/OH2P 混用）会变砖。

## BLOCKED / 待确认

- 无（本次 Phase 1 无需 bai 的硬件或账号）。以下是不阻塞本任务的已知事项：
  - OH2P 没有真实固件包：OH2P 的补丁逻辑只在 LX06 rootfs 副本上验证过
    （asound 同逻辑、跳过 libxaudio），端到端需等拿到 OH2P 1.58.6 官方包
    再跑 `unpack.sh oh2p 1.58.6` + `patch.sh oh2p 1.58.6`。
  - 后台 apt 的 `apt-get update` 自 03:39 起卡住（至 12:00 仍在跑），
    squashfs-tools 是用本地缓存 .deb 装的；若 VM 重启，`/usr` 下的
    mksquashfs/unsquashfs 会丢失，需重装。
  - 补丁镜像比原 root.squashfs 大 952,549 字节（38,453,248 vs
    37,500,699），仍在 40MB 内，余 3.49MB；原因未深究（可能是 mksquashfs
    默认 xz 字典与原厂打包参数的差异），不影响刷机。
