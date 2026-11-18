# 上机包（Release）— open-xiaoai-next 首刷 + NAS 部署

> 2026-11-18，第三轮审计 P1 修复轮（分支 `fix-p1-round3`，F-1/F-2/F-3 修完重编）。
> 本文件只写路径、SHA256、核对命令与顺序；**不含任何密码、Token、证书、密钥**——
> Token 在 `deploy/.env` 里（gitignore，不进仓库、不进包，刷机电脑上现场填）。
> 实际文件在仓库外：`~/workspace/speaker-open-xiaoai-rebuild/release/`（见下文“包清单”）。

## 0. 镜像已是现版（2026-11-18 P1 修复轮重编）

包内两份补丁镜像是 2026-11-18 第三轮审计 P1 修复轮重编的（SHA 见 §1，
`flash-check.sh` §4b 四路默认/`--strict` 均 exit 0）。含全部现版补丁：
inittab 三行控制台均走密码登录 `/bin/login`（F-2）、asound `.bak` 原厂备份
（P2-33）、rc.local init 包装器校验 + SHA256 pin 必填（F-3，缺 pin 即拒绝
执行，安装流程见 `firmware-patch/README.md`“/data/init.sh 安装流程”）、mTLS
客户端证书。
**root 密码未换**：重编时新镜像的 shadow root 行沿用了旧镜像的 `$6$`
hash，bai 手里已有的密码继续有效，未生成新密码。
旧 SHA（LX06 `7dafb950…05e21`、OH2P `fe2bab99…5c926`，2026-11-18 P2 轮打的）已作废；
更旧的（LX06 `7266aba9…d97b3`、OH2P `139de363…6415`，2026-11-17 打的）同样作废。

## 1. 包清单（每件东西的路径、大小、SHA256、核对命令）

| # | 东西 | 包内路径 | 大小（字节） | SHA256 | 核对命令 |
|---|---|---|---|---|---|
| 1 | LX06 补丁镜像 | `images/lx06_1.94.13-root-patched.squashfs` | 38,457,344 | `1a5c27603d7ab8064841f3e970258ef39ae754c2847a46fc0529168c250d0281` | `sha256sum images/lx06_1.94.13-root-patched.squashfs` |
| 2 | OH2P 补丁镜像 | `images/oh2p_1.58.6-root-patched.squashfs` | 30,179,328 | `8db02670f0c81bda5059e5a0fe2cad5b6045f4afe252a310353c7ac3acf9d197` | `sha256sum images/oh2p_1.58.6-root-patched.squashfs` |
| 3 | Hub 二进制 | `deploy/hub-linux-amd64` | 12,093,544 | `973a3b1fa2d34acab7a1feaa5c6fb146bc01167735b4c50ba408d1acd0213091` | `sha256sum deploy/hub-linux-amd64`；清单 `deploy/hub-linux-amd64.sha256`（`nas-install.sh` §0 会比对，不一致即 FAIL） |
| 4 | 部署包 | `deploy/`（docker-compose.yml / Dockerfile.hub / .dockerignore / snapserver.conf / .env.example / nas-install.sh / NAS_CHECKLIST.md / README.md） | — | — | `python3 -c "import yaml; yaml.safe_load(open('deploy/docker-compose.yml'))"`；`bash -n deploy/nas-install.sh` |
| 5 | kws-sidecar 构建源 | `kws-sidecar/`（Dockerfile / server.py / entrypoint.sh / requirements.txt / scripts/） | — | — | `ls kws-sidecar/` 四项都在；`nas-install.sh` §2 会打 tar 传到 NAS `$NAS_DIR/kws-sidecar` |
| 6 | 刷机 runbook | `FLASH_RUNBOOK.md` | — | — | 按 §8 检查清单逐条打勾 |
| 7 | NAS 检查单 | `NAS_CHECKLIST.md` | — | — | 按 §0–§5 逐条打勾 |
| 8 | 回滚检查单 | `ROLLBACK_CHECKLIST.md`（本包内，runbook §6.2 的单页版） | — | — | 刷机当天先填好再动手 |

核对镜像 SHA 时用完整 SHA256 比对，不要只比前缀。官方原厂包（回滚用）：
LX06 在 `firmware/lx06_1.94.13.bin`（43,478,328 字节，SHA256
`a56a38619fd98e467adb8c998757f80109c4dac89db48d62dae985e3062476ed`，已实测）；
OH2P 在 `firmware/oh2p_1.58.6.bin`（36,042,824 字节，SHA256
`1f351d879c06ff3bd7bd4b4c68e2c8fa45eee1a82be5839e57899ae8f9b7c27b`，2026-11-18
从 `work/` 拷入并核对，与白名单一致）。
`flash-check.sh` 会验原厂包。

## 2. 刷机顺序（先刷机，后部署；每一步的详细命令见 runbook）

1. 刷机电脑上核对 §1 的 SHA256（两份镜像 + Hub 二进制）。
2. `cd firmware-patch && ./scripts/flash-check.sh lx06 1.94.13`（或 `oh2p 1.58.6`），
   再用 `--strict` 跑一遍：SHA 不一致即 FAIL。§4b 内容验证缺标记即 FAIL
   （init 包装器本身缺失只警告——针对旧版镜像；.bak 缺失即 FAIL，当前重编镜像里两者都有）。
3. 按 `FLASH_RUNBOOK.md` §2 读当前启动槽 → §3 决策表定目标槽（按实测 `boot_part` 定，
   不要默认 boot0）→ §4 备份（NAS 侧 + 设备侧）→ §5 刷机（只写非当前槽的 system 分区，
   绝不写 bootloader / tpl / dtb / kernel / 当前槽）→ §6 刷后校验 + 回滚演练。
4. 刷后：开机、SSH 能进（root 密码是 2026-11-17 已交付的那个，重编未换）、Hub 能连上。
5. NAS 部署：`NAS_SSH=root@NAS_IP ./deploy/nas-install.sh`（只传文件+检查，
   汇总 `FAIL=0`），再 `NAS_SSH=root@NAS_IP ./deploy/nas-install.sh up`
   （传文件 + up + 逐项检查）。按 `NAS_CHECKLIST.md` §0–§5 逐条打勾。
6. 网易云扫码：去 Hub Web 面板扫码（见 `deploy/README.md`“网易云扫码登录”）。

## 3. 回滚（出问题时不要现找）

- 首选：翻回未动过的槽重启（runbook §6.2 回滚演练，本包 `ROLLBACK_CHECKLIST.md`）。
- 第二道保险：用 §1 的官方原厂包恢复 system 槽（runbook §1.2 的段 + §7.3）。
- Hub OTA 更新只走 Hub 自有 OTA；**绝不复用原厂 `bin/flash.sh`**（会重写 u-boot，
  补丁镜像无小米签名、走不通原厂 OTA）。Hub OTA 的 `VerifyTimeout` 是 30 秒：
  设备须在 30 秒内回 hello，否则任务标失败——先看设备日志再决定是否重推，
  不要连续重推（见 `FLASH_RUNBOOK.md` §1.5）。

## 4. 版本记录

| 日期 | 内容 |
|---|---|
| 2026-11-17 | 补丁镜像重编（P0-2 修复后）：LX06 `7266aba9…d97b3`、OH2P `139de363…6415` |
| 2026-11-18 | Hub 从分支尖 996329c 重编：12,093,552 字节、`95535eee…ead7de`（融合 B + P1 + P2 Hub 改动后）；digest 钉死（compose 5 个 + Dockerfile.hub 基础镜像）；nas-install.sh 补 kws-sidecar 发货与 Hub 清单校验 |
| 2026-11-18 | Hub 从分支尖 afcdcf0 再重编：12,093,544 字节、`973a3b1f…3091`（协调员收尾：删 streamFor 死代码 + kws 排空移到 hello 前）；清单已同步更新 |
