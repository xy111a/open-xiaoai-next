# HA 融合（b）：Hub 把音箱报给 HA 做 media_player 实体

Hub 每 15 秒把注册表里的每台音箱上报给 HA，一个音箱对应一个
`media_player.open_xiaoai_*` 实体，在 HA 的 UI 里能看到、在自动
化与模板里能用。上报走 HA REST API（`POST /api/states/<实体>`），
与 Hub 现有的 HA WebSocket 客户端共用同一个长期访问令牌
（`HASS_TOKEN`），但不依赖 WebSocket 是否连通。

> 状态：上报逻辑只在 stub HA（假 REST 端点）上验证过
> （`hub/internal/ha/mediaplayer_test.go`，含端到端同步测试），
> **尚未在真 NAS、真 HA 上验证过实体真的出现**。真机步骤见下面
> “在 NAS 上确认”与文末的待验证清单。

## 实体怎么来

| 项目 | 规则 |
|---|---|
| 实体 ID | `media_player.open_xiaoai_<设备 ID 的 slug>`，如设备 `lx06-192-168-1-20` → `media_player.open_xiaoai_lx06_192_168_1_20` |
| 显示名 | 有房间时是“客厅音箱”这种（房间认领后下次同步自动改名）；未认领时是 `open-xiaoai <设备 ID>` |
| state | `playing` / `paused` / `idle` / `unavailable`（见下表） |
| `volume_level` | 0–1（MPD 音量换算；MPD 未报音量时不上报） |
| `media_title` / `media_artist` | 当前曲名 / 歌手；没在放时不上报 |
| `supported_features` | 0——只上报状态，**不接控制**（见“现在不能做的事”） |

state 映射（MPD 是全屋唯一的播放源，Snapcast 同步播到所有
音箱，所以全部音箱共享同一份 MPD 状态，只在名字与在线状态上不同）：

| 情况 | state |
|---|---|
| 音箱在线，MPD 在放 | `playing` |
| 音箱在线，MPD 暂停 | `paused` |
| 音箱在线，MPD 停止 / 状态未知 | `idle` |
| 音箱离线（注册表里 Online=false） | `unavailable`（不报过期的 `playing`） |

## 开关与配置

`hub/config.yaml` 里 `home_assistant.media_player_enabled`，
**默认开**（不写就是开）；显式写 `false` 关闭上报：

```yaml
home_assistant:
  url: "ws://192.168.1.10:8123"
  media_player_enabled: true   # 默认开；写 false 关闭
```

其余不用配：上报地址由 `home_assistant.url` 换算（`ws://host:8123`
→ `http://host:8123`），令牌走环境变量 `HASS_TOKEN`，与现有
HA 连接同一套。注意：REST 上报不需要 WS 连通也能工作，但
`HASS_TOKEN` 为空时上报不会启用（启动日志会写明原因）。

上报节奏：启动即同步一次，之后每 15 秒一次；同一实体的
state/音量/曲名都没变时不发请求（不重复打 HA）；上报失败只记
日志、下一轮重试，不影响 Hub 主流程。

## 在 NAS 上确认

1. Hub 起好后等 15 秒，在 HA → 开发者工具 → 状态里搜
   `media_player.open_xiaoai_`，应看到每台已注册音箱一个实体。
2. 对音箱说“播放周杰伦的青花瓷”，等一轮同步（≤15 秒），实体
   应变 `playing`，`media_title` 为青花瓷；说“暂停”，应变
   `paused`；说“停止”，应变 `idle`。
3. 音箱断电/断网后下一轮同步，实体应变 `unavailable`。
4. 自动化示例（HA 里直接用，无需 Hub 侧配合）：
   - 触发：`media_player.open_xiaoai_lx06_192_168_1_20` 的 state
     变为 `playing`；动作：开客厅灯。实体 ID 按实际的改。

## 现在不能做的事（后续工作，不是 bug）

- **HA 里控制音箱还没接**：在 HA UI 里点播放/暂停、拖音量条，
  现在不会动音箱（`supported_features` 为 0，UI 上也不应出现
  可点的控制）。接控制的自然后续：Hub 订阅 HA 的
  `media_player` 服务调用（或 HA automation 调 Hub 的
  `/api/music/*`），把 play/pause/volume 映射到 MPD/Snapcast。
  做之前要先定鉴权与“谁是播放主控”的规则，别两边抢。
- **实体不属于任何集成配置项**：REST 上报的实体出现在 HA 的
  状态表里，可进 UI、自动化与模板，但没有设备页（HA 的
  WebSocket API 没有“设置任意实体状态”的命令，REST 是为这类
  外部上报设计的做法）。想要原生设备页需另写自定义集成。
- **音量与曲目是全屋共享的**：MPD 只有一个播放队列，Snapcast
  同步播到所有音箱，所以每台音箱的实体看到的是同一份音量与
  曲名；目前没有“只给卧室那台调音量”的分路（Snapcast 的单台
  音量在 Hub 里还没接出来）。

## 待真机验证（还没做过）

1. 真 NAS 上 Hub 启动后，HA 状态表里出现
   `media_player.open_xiaoai_*` 实体（每台已注册音箱一个）。
2. 放歌/暂停/停止三态在 15 秒内反映到 HA 实体上；离线音箱变
   `unavailable`。
3. HA 自动化能以这些实体为触发条件（例如 playing 时开灯）。
4. HA 重启后实体是否保留——REST 上报的状态在 HA 重启后会消失，
   下一轮 Hub 同步（≤15 秒）应重新出现；确认这个恢复过程。
