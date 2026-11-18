#!/usr/bin/env python3
"""KWS 边车服务（open-xiaoai-next 服务端唤醒检测）。

音箱只采音；Hub 解码 Opus 后把 16kHz 单声道 PCM（int16 LE，20ms/帧）
经 WebSocket 发到这里做唤醒检测。推理逻辑复用实验
``~/workspace/.kws-exp/run_kws_test.py``（sherpa-onnx 2.0.1，
sherpa-onnx-kws-zipformer-wenetspeech-3.3M，关键词“你好小李”），
包括“喂完音频后补静音帧再排空”的做法——模型需要关键词后的
若干帧（trailing blanks）才能吐出结果；真实使用中麦克风会持续
送入静音帧，所以这里不需要测试脚本里那种显式补 1s 静音。

协议（已定，不许改；2026-11-17 P1 修复后 hello 增加 token 字段）：
  WS  ws://127.0.0.1:10400/v1/kws（部署时 Hub 与边车都走 host 网络，
  见 deploy/docker-compose.yml；容器名 kws-sidecar 在 host 网络下解析不到）
  Hub -> 边车 首条：{"type":"hello","device_id":"...","sample_rate":16000,
                     "token":"<与 Hub 的 OPEN_XIAOAI_TOKEN 同一机密>"}
  鉴权：hello 的 token 须与边车启动时传入的 Token 一致（常数时间比较），
  缺失/错误即 error + 1008 关闭；边车未配 Token（<16 位亦然）时拒绝启动。
  同 device_id 重连：旧连接被关闭（1000 "replaced by new connection"），
  旧流状态丢弃，旧连接此后不再产生 wake。
  连接数上限：默认 16，超限即 error + 1013 关闭。
  Hub -> 边车 二进制帧：PCM int16 LE 单声道 16kHz（20ms = 640 字节）
  边车 -> Hub {"type":"ready"} / {"type":"wake","device_id","keyword",
                 "rms","rms_peak","timestamp_ms"} / {"type":"error","message"}
                 （rms 是检出时最近 1s 的 RMS 均值；rms_peak 是同一窗口
                 内单帧 RMS 的峰值，不被 trailing 静音稀释，供仲裁选用，
                 见 SC-P2-2）
  HTTP GET /health -> 200 {"status":"ok","model_loaded":true,
                 "keywords":1,"devices":0,"uptime_s":...,"total_wakes":0,
                 "device_wakes":{}}；
                 模型未加载、关键词表为空或推理连续失败时 503 + reason。
                 模型加载失败则进程直接非零退出。

并发模型：单事件循环内串行解码；每 device_id 独立的 KeywordSpotter
流状态（stream），互不干扰；连接断开即丢弃该设备流状态，重连从零开始；
同一 device_id 重连时旧连接被主动关闭、旧流丢弃（不再出 wake）。

安全假设（S-3，2026-11-18 第三轮审计 P1，未决 TLS 前的正式假设，
详见 README.md“安全假设”）：本服务不启用 TLS，只监听明文 ws，
默认只绑 127.0.0.1 回环（--host / KWS_HOST 默认值），流量不出回环；
hello 的 token 以明文 JSON 传输，隔离只靠回环绑定 + token 本身。
任何 NAS 本机进程都可直连 127.0.0.1:10400。bai 在“接受回环假设”
与“给边车加 TLS”之间定夺后，本假设即失效（选 TLS 则 Hub 的指纹
pin 才真正跑通；wss:// 一配就连不上，是 fail-closed，不是 bug）。
"""
import argparse
import asyncio
import hmac
import json
import logging
import math
import os
import re
import sys
import time
from collections import deque

import numpy as np
import sherpa_onnx
import websockets
from websockets.asyncio.server import serve
from websockets.http11 import Headers, Request, Response

LOG = logging.getLogger("kws-sidecar")

SAMPLE_RATE = 16000
FRAME_SAMPLES = 320          # 20ms @ 16kHz
FRAME_BYTES = FRAME_SAMPLES * 2
RMS_WINDOW_FRAMES = 50       # wake 事件的 rms 取最近 1s 的帧 RMS 合成

TOKEN_MIN_LENGTH = 16        # 与 Hub 的 OPEN_XIAOAI_TOKEN 规则一致（>=16 位）
DEFAULT_MAX_CONNECTIONS = 16  # WS 连接数上限（每设备一路流，2 台设备余量充足）
MAX_CONSECUTIVE_STREAM_ERRORS = 5  # 连续推理失败达此数即标记服务不健康（/health 503）

# SC-P2-1：单帧排空循环的 decode 硬上限。is_ready 恒真时无上限循环会
# 堵住整个单事件循环（全部设备停摆）；超限即记 error 并 reset 该流。
MAX_DECODE_PER_FRAME = 32
# SC-P2-4：连接断开时补喂的 trailing 静音帧数（25 帧 = 500ms；实测
# 关键词后约 10 帧静音即出结果，25 帧留足余量）。
FLUSH_SILENCE_FRAMES = 25
# SC-P2-5：device_id 字符集与长度上限（原样进日志会伪造日志行）。
# 字符集含 "."——现有测试与 Hub 的设备 id 形如 pos-pos1.wav（含点），
# 拒绝点会破坏既有协议；点本身不影响日志单行性。
DEVICE_ID_RE = re.compile(r"[A-Za-z0-9_.-]{1,64}")
# SC-P2-6：阈值/分数的合法范围（与 tests/bootstrap.py 的校验同源）。
DEFAULT_THRESHOLD = 0.25     # 实验验证值（2026-11-17）
DEFAULT_SCORE = 1.0          # 实验验证值（2026-11-17）

# 模型文件（int8 encoder；与 client/scripts/download-kws-model.sh 一致）
ENCODER_INT8 = "encoder-epoch-12-avg-2-chunk-16-left-64.int8.onnx"
DECODER = "decoder-epoch-12-avg-2-chunk-16-left-64.onnx"
JOINER = "joiner-epoch-12-avg-2-chunk-16-left-64.onnx"
TOKENS = "tokens.txt"
KEYWORDS = "keywords.txt"

REQUIRED_MODEL_FILES = [ENCODER_INT8, DECODER, JOINER, TOKENS]


def count_keywords(keywords_file):
    """校验关键词表：至少含一行非空、含 '@' 的有效行；返回有效行数。

    空表/全注释/无 '@' 的行都会抛 ValueError（调用方 FATAL 退出，
    绝不带病上线——空表时 sherpa 恒无检出，/health 却会报 200，
    即 SC-P1-2 的 fail-open）。
    """
    count = 0
    with open(keywords_file, encoding="utf-8") as f:
        for lineno, raw in enumerate(f, 1):
            line = raw.strip()
            if not line or line.startswith("#"):
                continue
            tokens, sep, _word = line.partition("@")
            if not sep or not tokens.strip():
                raise ValueError(
                    f"keywords file {keywords_file}:{lineno}: invalid line "
                    f"(expected '<tokens> @<word>'): {raw.strip()!r}")
            count += 1
    if count == 0:
        raise ValueError(
            f"keywords file {keywords_file} has no valid keyword lines; "
            "refusing to start with an empty keyword table")
    return count


def load_spotter(model_dir, keywords_file, threshold, score, num_threads):
    """加载 KeywordSpotter；缺文件/空词表/加载失败直接抛异常（调用方非零退出）。

    返回 (spotter, keywords_count)。
    """
    missing = [f for f in REQUIRED_MODEL_FILES
               if not os.path.isfile(os.path.join(model_dir, f))]
    if missing:
        raise FileNotFoundError(
            f"model dir {model_dir} missing files: {', '.join(missing)}")
    if not os.path.isfile(keywords_file):
        raise FileNotFoundError(f"keywords file not found: {keywords_file}")
    keywords_count = count_keywords(keywords_file)
    t0 = time.perf_counter()
    spotter = sherpa_onnx.KeywordSpotter(
        tokens=os.path.join(model_dir, TOKENS),
        encoder=os.path.join(model_dir, ENCODER_INT8),
        decoder=os.path.join(model_dir, DECODER),
        joiner=os.path.join(model_dir, JOINER),
        keywords_file=keywords_file,
        num_threads=num_threads,
        keywords_threshold=threshold,
        keywords_score=score,
    )
    LOG.info("model loaded in %.2fs (encoder=int8, threshold=%s, score=%s, "
             "keywords=%s, keyword_lines=%d)", time.perf_counter() - t0,
             threshold, score, keywords_file, keywords_count)
    return spotter, keywords_count


def _safe_device_id(value):
    """日志用的 device_id 单行化（SC-P2-5）：去 \r/\n、截断 64 字符。
    合法 id 原样通过；hello 校验前的原始输入只在报错路径进日志，
    绝不原样输出（防日志行伪造）。"""
    text = value if isinstance(value, str) else repr(value)
    return text.replace("\r", "?").replace("\n", "?")[:64]


class DeviceStream:
    """一台设备的独立 KWS 流状态。"""

    def __init__(self, spotter, device_id):
        self.spotter = spotter
        self.device_id = device_id
        self.stream = spotter.create_stream()
        self.rms_frames = deque(maxlen=RMS_WINDOW_FRAMES)
        self.wake_count = 0
        # SC-P2-7：延迟统计（每帧 20ms）。frames_since_wake 是上次
        # 检出后喂入的帧数——关键词结束到检出的帧数的近似（检出越晚
        # 数越大）；frames_at_last_wake 在检出瞬间冻结，供日志用。
        self.frames_fed = 0
        self.frames_since_wake = 0
        self.frames_at_last_wake = 0
        # 被同 device_id 的新连接取代时置 True：旧 handler 见此标志即停喂，
        # 且即使算出结果也不再发 wake（SC-P1-1 的双保险）。
        self.replaced = False

    def feed(self, pcm_int16):
        """喂一帧 PCM；返回检出的关键词（无检出返回 None）。"""
        samples = pcm_int16.astype(np.float32) / 32768.0
        self.rms_frames.append(float(np.sqrt(np.mean(samples ** 2))))
        self.frames_fed += 1
        self.frames_since_wake += 1
        self.stream.accept_waveform(SAMPLE_RATE, samples)
        # 复用实验的排空逻辑：is_ready -> decode -> get_result。
        # SC-P2-1：排空循环加硬上限——某路流的 is_ready 若因模型/
        # 音频异常恒为真，无上限循环会堵住整个单事件循环；超限即
        # 记 error 并 reset 该设备流（自愈），不让整车停摆。
        decodes = 0
        while self.spotter.is_ready(self.stream):
            if decodes >= MAX_DECODE_PER_FRAME:
                LOG.error("device %s: is_ready still true after %d "
                          "decodes; resetting stream", self.device_id,
                          MAX_DECODE_PER_FRAME)
                self.spotter.reset_stream(self.stream)
                break
            self.spotter.decode_stream(self.stream)
            decodes += 1
        result = self.spotter.get_result(self.stream)
        if result:
            self.spotter.reset_stream(self.stream)
            self.wake_count += 1
            self.frames_at_last_wake = self.frames_since_wake
            self.frames_since_wake = 0
            return result
        return None

    def flush_on_disconnect(self, silence_frames=FLUSH_SILENCE_FRAMES):
        """断开时补喂静音并做最后一次检出（SC-P2-4）：模型需要关键词
        后约 200ms trailing 音频才吐结果；设备恰在关键词尾音处断线
        时，不 flush 就会丢这句唤醒。连接已断，wake 事件无处可发，
        检出时调用方只记一条日志供排障。返回检出的关键词或 None；
        推理失败返回 None，不抛异常（finally 里调用必须安全）。"""
        silence = np.zeros(FRAME_SAMPLES, dtype=np.int16)
        for _ in range(silence_frames):
            try:
                keyword = self.feed(silence)
            except Exception:
                LOG.warning("device %s: flush on disconnect failed",
                            self.device_id, exc_info=True)
                return None
            if keyword:
                return keyword
        return None

    @property
    def rms(self):
        if not self.rms_frames:
            return 0.0
        return round(math.sqrt(
            sum(r * r for r in self.rms_frames) / len(self.rms_frames)), 3)

    @property
    def rms_peak(self):
        """窗口内单帧 RMS 的峰值（SC-P2-2）：rms 是 1s 均值，会被
        trailing 静音稀释（检出越晚稀释越多）；峰值不受检出延迟
        影响，仲裁比较响度时应优先用它。"""
        if not self.rms_frames:
            return 0.0
        return round(max(self.rms_frames), 3)


class Server:
    def __init__(self, spotter, keywords_count=0, token=None,
                 max_connections=DEFAULT_MAX_CONNECTIONS):
        self.spotter = spotter
        self.keywords_count = keywords_count
        self.token = token  # 与 Hub 的 OPEN_XIAOAI_TOKEN 同一机密；绝不记日志
        self.max_connections = max_connections
        self.model_loaded = spotter is not None
        self.devices = {}  # device_id -> (DeviceStream, ws)
        self.connections = 0  # 当前 WS 连接数（含未完成 hello 的半开连接）
        self.started_at = time.monotonic()
        self.total_wakes = 0
        self._consecutive_stream_errors = 0
        self._fatal_error = None  # 置起后 /health 即 503（fail-closed，需重启恢复）

    def health_reason(self):
        """None 表示健康；否则返回 503 应报告的原因字符串。"""
        if not self.model_loaded or self.spotter is None:
            return "model not loaded"
        if self.keywords_count <= 0:
            return "keywords table is empty"
        if self._fatal_error is not None:
            return self._fatal_error
        return None

    def note_feed_success(self):
        self._consecutive_stream_errors = 0

    def note_feed_error(self):
        self._consecutive_stream_errors += 1
        if self._consecutive_stream_errors >= MAX_CONSECUTIVE_STREAM_ERRORS:
            self._fatal_error = (
                "repeated stream errors "
                f"({self._consecutive_stream_errors} consecutive "
                "decode failures)")
            LOG.error("marking sidecar unhealthy: %s", self._fatal_error)

    # ---- HTTP（经 websockets 的 process_request 拦截）----
    async def process_request(self, connection, request: Request):
        if request.path == "/health":
            reason = self.health_reason()
            body_dict = {
                "model_loaded": bool(self.model_loaded),
                "keywords": self.keywords_count,
                "devices": len(self.devices),
                "uptime_s": round(time.monotonic() - self.started_at, 1),
                "total_wakes": self.total_wakes,
                # SC-P2-7：每设备累计唤醒次数（此前 wake_count 自增后
                # 无任何读取方，是死指标）；排障时可直接看出哪台设备
                # 流异常（某设备 wakes 远高于同屋另一台即疑似误触发）。
                "device_wakes": {did: stream.wake_count
                                 for did, (stream, _ws)
                                 in self.devices.items()},
            }
            if reason is None:
                body_dict["status"] = "ok"
                body = json.dumps(body_dict).encode()
                return Response(200, "OK", Headers([("Content-Type",
                                                      "application/json")]),
                                body)
            body_dict["status"] = "error"
            body_dict["reason"] = reason
            body = json.dumps(body_dict).encode()
            return Response(503, "Service Unavailable",
                            Headers([("Content-Type", "application/json")]),
                            body)
        if request.path == "/v1/kws":
            return None  # 继续 WebSocket 握手
        return Response(404, "Not Found", Headers(), b"not found")

    # ---- WebSocket ----
    async def handler(self, ws):
        device_id = None
        stream = None
        # 连接数上限：在事件循环内同步检查 + 计数，无竞态；半开连接也计入。
        self.connections += 1
        if self.connections > self.max_connections:
            LOG.warning("connection limit reached (%d); refusing new "
                        "connection", self.max_connections)
            try:
                await self._send_error(ws, "too many connections")
                await ws.close(code=1013, reason="too many connections")
            except websockets.ConnectionClosed:
                pass
            finally:
                self.connections -= 1
            return
        try:
            # SC-P2-3：hello 超时是预期内的协议行为（半开连接/端口扫描），
            # 不是内部异常——单独捕获：记一条 warning，发 "hello timeout"
            # 后以 1008 关闭，不打 traceback、不误导客户端。
            try:
                hello_raw = await asyncio.wait_for(ws.recv(), timeout=10)
            except asyncio.TimeoutError:
                LOG.warning("hello timeout: no hello within 10s; closing "
                            "connection")
                try:
                    await self._send_error(ws, "hello timeout")
                    await ws.close(code=1008, reason="hello timeout")
                except websockets.ConnectionClosed:
                    pass
                return
            if not isinstance(hello_raw, str):
                await self._send_error(
                    ws, "first message must be a JSON hello text frame")
                await ws.close(code=1002, reason="hello must be text JSON")
                return
            try:
                hello = json.loads(hello_raw)
            except json.JSONDecodeError:
                await self._send_error(ws, "hello is not valid JSON")
                await ws.close(code=1002, reason="hello is not valid JSON")
                return
            if hello.get("type") != "hello":
                await self._send_error(
                    ws, "first message type must be 'hello'")
                await ws.close(code=1002, reason="first message not hello")
                return
            device_id = hello.get("device_id")
            sample_rate = hello.get("sample_rate")
            # SC-P2-5：device_id 只判“非空字符串”不够——超长 id 可进字典
            # 与日志，含 \n 的 id 可伪造日志行。限长 64、字符集限
            # [A-Za-z0-9_.-]，非法即 error + 1002 关闭。
            if (not isinstance(device_id, str)
                    or not DEVICE_ID_RE.fullmatch(device_id)):
                await self._send_error(
                    ws, "hello.device_id must be 1-64 chars of "
                        "[A-Za-z0-9_.-]")
                await ws.close(code=1002, reason="bad device_id")
                return
            if sample_rate != SAMPLE_RATE:
                await self._send_error(
                    ws, f"sample_rate must be {SAMPLE_RATE}, got "
                        f"{sample_rate!r}")
                await ws.close(code=1002, reason="bad sample_rate")
                return
            # 鉴权（SC-P1-3）：hello 须带与边车 Token 一致的 token，常数时间
            # 比较；缺失/错误即 error + 1008 关闭。Token 值绝不记日志、
            # 不回显。注意：鉴权在取代旧连接之前——未通过鉴权的连接
            # 杀不掉合法旧连接。
            token = hello.get("token")
            if (not isinstance(token, str) or self.token is None
                    or not hmac.compare_digest(token, self.token)):
                await self._send_error(ws, "authentication failed")
                await ws.close(code=1008, reason="bad token")
                return
            if device_id in self.devices:
                old_stream, old_ws = self.devices[device_id]
                # 先标记取代（旧 handler 见标志即停喂、不再发 wake），
                # 再尽力关闭旧连接；关闭超时也不阻塞新连接——
                # replaced 标志本身已保证旧流不再出 wake。
                old_stream.replaced = True
                LOG.warning("device %s reconnected; closing old connection",
                            device_id)
                try:
                    await asyncio.wait_for(
                        old_ws.close(code=1000,
                                     reason="replaced by new connection"),
                        timeout=5)
                except (asyncio.TimeoutError, websockets.ConnectionClosed):
                    LOG.warning("device %s: old connection close timed out "
                                "or already closed; old stream already "
                                "invalidated", device_id)
            stream = DeviceStream(self.spotter, device_id)
            self.devices[device_id] = (stream, ws)
            LOG.info("device connected: %s", device_id)
            await ws.send(json.dumps({"type": "ready"}))

            async for message in ws:
                if stream.replaced:
                    break
                if isinstance(message, str):
                    await self._send_error(
                        ws, "unexpected text frame after hello; "
                            "audio must be binary PCM frames")
                    continue
                if len(message) % 2 != 0:
                    await self._send_error(
                        ws, f"odd-length PCM frame: {len(message)} bytes")
                    continue
                pcm = np.frombuffer(message, dtype=np.int16)
                # 按 20ms 切块喂入（Hub 约定 640 字节/帧；超长帧也照此切分）
                for i in range(0, len(pcm), FRAME_SAMPLES):
                    try:
                        keyword = stream.feed(pcm[i:i + FRAME_SAMPLES])
                    except Exception:
                        self.note_feed_error()
                        raise
                    self.note_feed_success()
                    if keyword:
                        # 取代后旧流即使算出结果也不发 wake（双保险）。
                        if stream.replaced or self.devices.get(
                                device_id, (None,))[0] is not stream:
                            break
                        event = {
                            "type": "wake",
                            "device_id": device_id,
                            "keyword": keyword,
                            "rms": stream.rms,
                            # SC-P2-2：同时报峰值——rms 是 1s 均值，会被
                            # trailing 静音稀释；仲裁比较响度时应优先用
                            # rms_peak。
                            "rms_peak": stream.rms_peak,
                            "timestamp_ms": int(time.time() * 1000),
                        }
                        await ws.send(json.dumps(event, ensure_ascii=False))
                        self.total_wakes += 1
                        # SC-P2-7：wake_count 此前是死指标——日志行写明
                        # “自该设备连接以来的第 N 次”与“上次检出后喂入
                        # 的帧数”（每帧 20ms，检出延迟的近似度量）。
                        LOG.info("wake: device=%s keyword=%s rms=%s "
                                 "rms_peak=%s wake_no=%d "
                                 "frames_since_wake=%d",
                                 device_id, keyword, event["rms"],
                                 event["rms_peak"], stream.wake_count,
                                 stream.frames_at_last_wake)
        except websockets.ConnectionClosed:
            pass
        except Exception:  # noqa: BLE001 - 任何异常都不能让服务静默死连接
            # SC-P2-5：此处 device_id 可能是 hello 校验前的原始输入，
            # 单行化后再进日志。
            LOG.exception("error handling device %s",
                          _safe_device_id(device_id))
            try:
                await self._send_error(ws, "internal error")
            except websockets.ConnectionClosed:
                pass
        finally:
            self.connections -= 1
            if device_id is not None:
                entry = self.devices.get(device_id)
                if entry is not None and entry[0] is stream:
                    # SC-P2-4：断开时补喂静音做最后一次检出（模型需要
                    # 关键词后约 200ms trailing 音频才吐结果）；检出只
                    # 记日志——连接已断，wake 事件无处可发。被取代的旧
                    # 流不 flush（其音频已过期、状态已作废）。
                    if stream is not None and not stream.replaced:
                        lost = stream.flush_on_disconnect()
                        if lost:
                            LOG.warning(
                                "wake lost on disconnect: device=%s "
                                "keyword=%s (keyword ended right before "
                                "disconnect; no connection left to deliver "
                                "the wake event)", device_id, lost)
                    del self.devices[device_id]
                    LOG.info("device disconnected, stream state dropped: %s",
                             device_id)

    @staticmethod
    async def _send_error(ws, message):
        try:
            await ws.send(json.dumps({"type": "error", "message": message}))
        except websockets.ConnectionClosed:
            pass


def build_arg_parser():
    p = argparse.ArgumentParser(description="KWS sidecar for open-xiaoai-next")
    # S-3：默认只绑回环（127.0.0.1）。边车不启用 TLS（见 README
    # “安全假设”），回环绑定是唯一的隔离；需局域网暴露时再显式改
    # --host / KWS_HOST。serve() 只绑 args.host 一个地址，无其他监听。
    p.add_argument("--host", default=os.environ.get("KWS_HOST", "127.0.0.1"),
                   help="listen address (default 127.0.0.1, loopback only; "
                        "the sidecar has no TLS, see README security "
                        "assumption)")
    p.add_argument("--port", type=int,
                   default=int(os.environ.get("KWS_PORT", "10400")))
    p.add_argument("--model-dir",
                   default=os.environ.get("KWS_MODEL_DIR", "/data/kws/models"))
    p.add_argument("--keywords-file",
                   default=os.environ.get("KWS_KEYWORDS_FILE", ""))
    p.add_argument("--threshold", type=float,
                   default=float(os.environ.get(
                       "KWS_THRESHOLD", str(DEFAULT_THRESHOLD))))
    p.add_argument("--score", type=float,
                   default=float(os.environ.get(
                       "KWS_SCORE", str(DEFAULT_SCORE))))
    p.add_argument("--num-threads", type=int,
                   default=int(os.environ.get("KWS_NUM_THREADS", "1")))
    p.add_argument("--token",
                   default=os.environ.get("OPEN_XIAOAI_TOKEN", ""),
                   help="WS hello 鉴权 Token；与 Hub 的 OPEN_XIAOAI_TOKEN "
                        "同一机密（>=16 位）。未配时拒绝启动。")
    p.add_argument("--max-connections", type=int,
                   default=int(os.environ.get("KWS_MAX_CONNECTIONS", "16")),
                   help="WS 连接数上限；超限的新连接以 1013 拒绝。")
    p.add_argument("--log-level",
                   default=os.environ.get("LOG_LEVEL", "INFO"))
    return p


async def amain():
    args = build_arg_parser().parse_args()
    logging.basicConfig(
        level=getattr(logging, args.log_level.upper(), logging.INFO),
        format="%(asctime)s %(levelname)s %(name)s %(message)s")
    keywords_file = args.keywords_file or os.path.join(args.model_dir,
                                                       KEYWORDS)
    # SC-P2-6：阈值/分数启动期校验——配错时边车要么永不唤醒、要么狂
    # 误触发，且 sherpa 未必 clamp；越界即 FATAL 退出，不静默跑。
    # 与 tests/bootstrap.py 的 _verify_thresholds() 同源（同一环境
    # 变量、同一合法范围），两边改阈值不会悄悄分叉。
    if not 0 < args.threshold <= 1:
        LOG.error("FATAL: --threshold / KWS_THRESHOLD must be in (0, 1]; "
                  "got %s", args.threshold)
        sys.exit(1)
    if args.score < 0:
        LOG.error("FATAL: --score / KWS_SCORE must be >= 0; got %s",
                  args.score)
        sys.exit(1)
    if (args.threshold, args.score) != (DEFAULT_THRESHOLD, DEFAULT_SCORE):
        LOG.warning("KWS threshold/score differ from the experimentally "
                    "verified values (threshold=%s, score=%s): "
                    "threshold=%s score=%s; tests/bootstrap.py reads the "
                    "same env vars for sample verification, keep them in "
                    "sync", DEFAULT_THRESHOLD, DEFAULT_SCORE,
                    args.threshold, args.score)
    # 鉴权 Token 是启动前置条件：未配或太短即拒绝启动（SC-P1-3 fail-closed，
    # 与 Hub 的 OPEN_XIAOAI_TOKEN 规则一致；Token 值绝不记日志）。
    token = args.token
    if not token or len(token) < TOKEN_MIN_LENGTH:
        LOG.error("FATAL: refusing to start without a KWS token "
                  "(pass --token or set OPEN_XIAOAI_TOKEN to at least %d "
                  "characters; reuse the Hub's OPEN_XIAOAI_TOKEN value)",
                  TOKEN_MIN_LENGTH)
        sys.exit(1)
    try:
        spotter, keywords_count = load_spotter(
            args.model_dir, keywords_file,
            args.threshold, args.score, args.num_threads)
    except Exception:  # noqa: BLE001 - 模型故障必须明确报错退出，不静默跑
        LOG.exception("FATAL: failed to load KWS model from %s; refusing "
                      "to start without a model", args.model_dir)
        sys.exit(1)
    server = Server(spotter, keywords_count=keywords_count, token=token,
                    max_connections=args.max_connections)
    async with serve(server.handler, args.host, args.port,
                     process_request=server.process_request,
                     max_size=1 << 20):
        LOG.info("kws-sidecar listening on ws://%s:%d/v1/kws "
                 "(health: http://%s:%d/health, keywords=%d, "
                 "max_connections=%d)",
                 args.host, args.port, args.host, args.port,
                 keywords_count, args.max_connections)
        await asyncio.Future()  # run forever


def main():
    asyncio.run(amain())


if __name__ == "__main__":
    main()
