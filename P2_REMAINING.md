# 剩余 P2 清单（P2 全修轮工作表）

> 来源：AUDIT_REPORT.md（首轮 P2 56 项）+ AUDIT_ROUND2.md §5（新 P2 40 项）+ §6 复查表状态。
> P1 轮已修的 P2 不在表里（见 FIXES_ROUND2.md §2）。本文件是工作表，最终报告是 FIXES_P2.md。
> 状态栏：待修 = 本轮要修；核实 = 先核实是否已被 P1 轮/融合 B 顺带修掉或已失效，属实则在状态文件里写清证据、不要重复修。

## Hub 面

| 编号 | 标题（简） | §6 状态 / 备注 |
|---|---|---|
| P2-1 | 自签证书 10 年有效、SAN 只有回环、指纹校验忽略有效期与主机名 | 仍开 |
| P2-2 | Token 比较非恒定时间，hello 拒绝原因可区分枚举 | 部分修复（核实残留） |
| P2-3 | room_claim/hello/status 字段无长度与内容校验 | 仍开 |
| P2-4 | 配置与语音数据文件 0644、数据目录 0755 | 仍开 |
| P2-5 | Web PUT 与房间认领共用临时文件、不同指针写同一配置 | 仍开 |
| P2-7 | 上游 HTTP 响应体无上限读取，错误信息回显原文 | 仍开 |
| P2-12 | 紧急呼叫无二次确认 | 仍开 |
| P2-13 | 提醒调度器可重复 Start 双倍触发；Stop 从不调用 | 仍开 |
| P2-14 | 提醒 tick/Cancel 落盘错误被吞掉 | 仍开 |
| P2-15 | Memory.Set 秒级 key 碰撞 | 核实：P1-41 顺带改纳秒 key，核实是否闭合 |
| P2-17 | Wyoming 无超时、未知事件死循环 | 部分修复（核实残留） |
| P2-25 | 勿扰格式非法静默按“不在勿扰” | 仍开 |
| P2-26 | 安全事件判定只用子串匹配 | 仍开 |
| P2-27 | ByRoom 随机取一台；播种占位设备成幽灵行 | 仍开 |
| P2-28 | decoders 永不回收 | 仍开 |
| P2-44 | 提醒存储：每秒全量写盘、损坏文件静默丢全部、Stop 后无法再启动、到点回调阻塞调度 | 仍开 |
| P2-46 | ChatStream SSE 行缓冲 1MB 上限丢整段回答 | 仍开 |
| P2-47 | 自动发现 1024 goroutine、错误恒 nil、网段推导武断 | 核实：P1-44 已删自动扫描，是否失效 |
| P2-48 | 语音管线：TTS 尾音丢失、不校验合成采样率、同设备可并发跑两次回答 | 部分修复（核实残留） |
| P2-49 | 回家场景绕过高风险 domain allowlist | 仍开 |
| P2-51 | 注册表房间语义三处不一致 | 核实：P1-44 注册制 + P1-39 认领后残留 |
| P2-53 | 主循环收尾：time.Tick 泄漏、goroutine 里 Fatalf、信号处理不完整 | 仍开 |
| P2-56 | Web 面板：GET 配置暴露内部拓扑、未匹配路径都 200 首页 | 部分修复（核实残留） |
| N-P2-2 | kws.Client.Close 与 SendPCM/streamFor 竞态，可泄漏流 goroutine | 待修 |
| N-P2-3 | KWS 边车重连后断线期积压约 1s 陈旧 PCM 照常发，可幽灵唤醒 | 待修 |
| N-P2-4 | KWS wake 事件 device_id 轻信边车回传值 | 待修 |
| N-P2-5 | kws_status 最长滞后 30 秒 | 待修 |
| N-P2-8 | nas-install.sh Hub /api/status 检查恒 PASS | 待修（与固件面协调，文件归属 deploy/） |
| N-P2-10 | KWS 边车走 wss + 自签证书时 Dial 与健康检查双双失败，无配置可解 | 待修 |
| N-P2-12 | KWS 重连退避无 jitter，两台设备流可同步重连脉冲 | 待修 |
| N-P2-1 | -race 下 2 个测试失败（测试替身并发写） | 核实：PR #1 已修测试替身，确认闭合 |
| N-P2-6 | config.Validate 漏 hub.mpd；web_listen :8080 不拦截 | 已修（P1 轮，核实） |
| N-P2-7 | authFailures 永不清理 | 已修（P1 轮，核实） |
| N-P2-9 | hello_ok 写无 deadline | 已修（P1 轮，核实） |

## 客户端面

| 编号 | 标题（简） | §6 状态 / 备注 |
|---|---|---|
| P2-19 | 非推流时仍每帧编码 Opus，白白耗 CPU | 仍开 |
| P2-20 | KWS 推理失败每帧打一条 warn，日志刷屏 | 仍开 |
| P2-21 | Sherpa is_ready 循环无上限，模型卡住录音任务永久阻塞 | 仍开（注：server 模式下本地 KWS 不走此路径，核实残留） |
| P2-22 | AEC 零能量参考信号时 NLMS 步长爆炸 | 仍开（与任务三 Opus 回声接线一起看） |
| P2-40 | 录音任务死一次就永久失去唤醒/采集能力 | 仍开 |
| P2-42 | 配置缺省是静默的危险默认值 | 部分修复（核实残留） |
| P2-52 | build-armv7.sh 静默产出“无 ALSA、无 KWS”应急版并 exit 0 | 仍开 |
| P2-54 | 两个公开自由函数失败即 panic | 仍开 |
| P2-N1 | interrupt 只清 playing 标志、不停 ALSA，80ms 尾音仍放完 | 待修 |
| P2-N2 | 客户端对入站 WS 帧无大小限制 | 待修 |
| P2-N4 | Opus 编码失败被静默吞掉 | 待修 |
| P2-N5 | 录音任务无背压丢弃策略，audio_tx 满反压堵采集 | 待修 |
| P2-N6 | hello_ok/rpc 解析过宽容，缺字段静默接受空值 | 待修 |
| P2-N7 | 构建不可字节复现 | 待修 |

## kws-sidecar 面

| 编号 | 标题（简） | 备注 |
|---|---|---|
| SC-P2-1 | is_ready 排空循环无上限 | 待修 |
| SC-P2-2 | wake 事件 rms 是 1s 均值，会被 trailing 静音稀释 | 待修 |
| SC-P2-3 | hello 10s 超时走通用 except，全量 traceback + 误导性 internal error | 待修 |
| SC-P2-4 | 连接断开时不 flush，尾部关键词随流丢弃 | 待修 |
| SC-P2-5 | device_id 无长度/内容校验，原样进日志 | 待修 |
| SC-P2-6 | KWS_THRESHOLD / KWS_SCORE 无范围校验 | 待修 |
| SC-P2-7 | 可观测性缺口：wake_count 死指标，无指标端点、无延迟统计 | 待修 |
| SC-P2-8 | bootstrap：Piper 语音文件只 pin 版本 tag，未做 SHA 校验 | 待修 |
| SC-P2-9 | bootstrap：样本校验阈值写死，与服务端运行阈值可漂移 | 待修 |
| SC-P2-10 | bootstrap：默认资产根是共享实验目录，坏样本重抽会改写它 | 待修 |
| SC-P2-11 | bootstrap：10 次上限实际是 10 次检测 / 9 次重抽，文档说“重抽最多 10 次”差一 | 待修 |
| SC-P2-12 | 容器以 root 运行、无资源上限 | 待修（与 P1-4 收尾协调：compose 侧统一处理） |
| SC-P2-13 | download-model.sh 的 curl 无 --fail，HTTP 错误靠 SHA 兜底、报错误导 | 待修 |

## 固件 / 部署 / CI 面

| 编号 | 标题（简） | §6 状态 / 备注 |
|---|---|---|
| P2-6 | patch.sh 打包后验证不完整 | 仍开 |
| P2-8 | patch_firmware.py 的 --password 与 hash 进进程参数/日志 | 部分修复（核实残留） |
| P2-9 | 版本号校验正则未转义 .，可含 / 路径穿越 | 仍开 |
| P2-10 | Sherpa .so 路径任意指定，ARM 结构体尺寸未锚定 | 部分修复（核实：server 模式下本地 KWS 已不用，残留） |
| P2-23 | download-ota.py 无参数校验、无下载超时 | 部分修复（核实残留） |
| P2-24 | nas-install.sh 多处检查水分 | 部分修复（核实：FW-P1-2 + 融合 B 后的残留） |
| P2-29 | 发现探针用 Version: "probe" 覆盖真实版本号 | 核实：P1-44 已删自动扫描，是否失效 |
| P2-30 | mksquashfs 参数与原厂不完全一致，镜像增大且无预警阈值 | 仍开 |
| P2-32 | patch_firmware.py 不可重复执行（非幂等） | 部分修复（核实：Friday 7662dcf 修了 patch_login，检查其余函数残留） |
| P2-33 | asound 改动无备份，设备端不可逆 | 仍开 |
| P2-34 | /data/init.sh 是无校验的开机 root 钩子 | 仍开 |
| P2-35 | OTA 禁用有残留路径未验证 | 仍开 |
| P2-36 | Dockerfile.hub 无 USER/HEALTHCHECK、无 .dockerignore、未 pin | 仍开（与 P1-4 收尾同一处，一并做） |
| P2-37 | compose 无资源限制、无 user | 仍开（与 P1-4 收尾同一处，一并做） |
| P2-38 | Hub OTA 30s 超时偏短；复用 flash.sh 会重写 u-boot | 仍开 |
| P2-39 | unpack.sh 的 unsquashfs -no-exit-code 会掩盖解包错误 | 部分修复（核实残留） |
| P2-55 | 部署物缺失：.env.example 不存在、二进制悄悄过期、sed 静默无操作 | 部分修复（核实：.env.example 已有，残留二、三项） |
| FW-P2-1 | CI 与 lint.sh 互相缺项，本地全绿 ≠ CI 全绿 | 待修 |
| FW-P2-3 | TEST_REPORT.md P1-40 行仍留证伪说法 | 核实：P1 轮已加勘误标注，确认是否闭合 |
| FW-P2-4 | TEST_REPORT.md 多处旧数未随重编更新 | 待修 |
| FW-P2-5 | flash-check.sh 补丁镜像 SHA 不一致只警告、不失败，且不校验镜像内补丁内容 | 待修 |
| FW-P2-6 | patch.sh 头注释“刷 boot0，保留 boot1 回滚”与 runbook 冲突 | 待修 |
| FW-P2-7 | nas-install.sh 与 NAS_CHECKLIST.md 端口表、容器数、检查口径三处不一致 | 待修（核实：融合 B 改容器数 8→7 后的残留） |
| FW-P2-8 | lint 规则覆盖面缺口（5 处，均实测） | 待修 |
| FW-P2-2 | CI Python job 只装 pytest | 已修（P1 轮，核实） |

## 不在本表（另有任务）

- P2-31：建议销项，协调员（Friday）亲自复议，不在工人范围。
- P1-4 收尾：Hub host 网络、user:/cap_drop/read_only、digest 钉死（MA 8095 已随融合 B 失效）——固件/部署面工人做。
- Opus 回声接线：客户端面工人做（TEST_REPORT.md:292）。
- -race flake 复现：Hub 面工人做（kws 包多跑几遍）。
