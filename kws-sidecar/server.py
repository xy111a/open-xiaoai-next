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
                 "rms","timestamp_ms"} / {"type":"error","message"}
  HTTP GET /health -> 200 {"status":"ok","model_loaded":true,
                 "keywords":1,"devices":0,"uptime_s":...,"total_wakes":0}；
                 模型未加载、关键词表为空或推理连续失败时 503 + reason。
                 模型加载失败则进程直接非零退出。

并发模型：单事件循环内串行解码；每 device_id 独立的 KeywordSpotter
流状态（stream），互不干扰；连接断开即丢弃该设备流状态，重连从零开始；
同一 device_id 重连时旧连接被主动关闭、旧流丢弃（不再出 wake）。
"""
import argparse
import asyncio
import hmac
import json
import logging
import math
import os
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


class DeviceStream:
    """一台设备的独立 KWS 流状态。"""

    def __init__(self, spotter, device_id):
        self.spotter = spotter
        self.device_id = device_id
        self.stream = spotter.create_stream()
        self.rms_frames = deque(maxlen=RMS_WINDOW_FRAMES)
        self.wake_count = 0
        # 被同 device_id 的新连接取代时置 True：旧 handler 见此标志即停喂，
        # 且即使算出结果也不再发 wake（SC-P1-1 的双保险）。
        self.replaced = False

    def feed(self, pcm_int16):
        """喂一帧 PCM；返回检出的关键词（无检出返回 None）。"""
        samples = pcm_int16.astype(np.float32) / 32768.0
        self.rms_frames.append(float(np.sqrt(np.mean(samples ** 2))))
        self.stream.accept_waveform(SAMPLE_RATE, samples)
        # 复用实验的排空逻辑：is_ready -> decode -> get_result
        while self.spotter.is_ready(self.stream):
            self.spotter.decode_stream(self.stream)
        result = self.spotter.get_result(self.stream)
        if result:
            self.spotter.reset_stream(self.stream)
            self.wake_count += 1
            return result
        return None

    @property
    def rms(self):
        if not self.rms_frames:
            return 0.0
        return round(math.sqrt(
            sum(r * r for r in self.rms_frames) / len(self.rms_frames)), 3)


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
            hello_raw = await asyncio.wait_for(ws.recv(), timeout=10)
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
            if not isinstance(device_id, str) or not device_id:
                await self._send_error(
                    ws, "hello.device_id must be a non-empty string")
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
                            "timestamp_ms": int(time.time() * 1000),
                        }
                        await ws.send(json.dumps(event, ensure_ascii=False))
                        self.total_wakes += 1
                        LOG.info("wake: device=%s keyword=%s rms=%s",
                                 device_id, keyword, event["rms"])
        except websockets.ConnectionClosed:
            pass
        except Exception:  # noqa: BLE001 - 任何异常都不能让服务静默死连接
            LOG.exception("error handling device %s", device_id)
            try:
                await self._send_error(ws, "internal error")
            except websockets.ConnectionClosed:
                pass
        finally:
            self.connections -= 1
            if device_id is not None:
                entry = self.devices.get(device_id)
                if entry is not None and entry[0] is stream:
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
    p.add_argument("--host", default=os.environ.get("KWS_HOST", "0.0.0.0"))
    p.add_argument("--port", type=int,
                   default=int(os.environ.get("KWS_PORT", "10400")))
    p.add_argument("--model-dir",
                   default=os.environ.get("KWS_MODEL_DIR", "/data/kws/models"))
    p.add_argument("--keywords-file",
                   default=os.environ.get("KWS_KEYWORDS_FILE", ""))
    p.add_argument("--threshold", type=float,
                   default=float(os.environ.get("KWS_THRESHOLD", "0.25")))
    p.add_argument("--score", type=float,
                   default=float(os.environ.get("KWS_SCORE", "1.0")))
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
