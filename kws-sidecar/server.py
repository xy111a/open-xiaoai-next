#!/usr/bin/env python3
"""KWS 边车服务（open-xiaoai-next 服务端唤醒检测）。

音箱只采音；Hub 解码 Opus 后把 16kHz 单声道 PCM（int16 LE，20ms/帧）
经 WebSocket 发到这里做唤醒检测。推理逻辑复用实验
``~/workspace/.kws-exp/run_kws_test.py``（sherpa-onnx 2.0.1，
sherpa-onnx-kws-zipformer-wenetspeech-3.3M，关键词“你好小李”），
包括“喂完音频后补静音帧再排空”的做法——模型需要关键词后的
若干帧（trailing blanks）才能吐出结果；真实使用中麦克风会持续
送入静音帧，所以这里不需要测试脚本里那种显式补 1s 静音。

协议（已定，不许改）：
  WS  ws://127.0.0.1:10400/v1/kws（部署时 Hub 与边车都走 host 网络，
  见 deploy/docker-compose.yml；容器名 kws-sidecar 在 host 网络下解析不到）
  Hub -> 边车 首条：{"type":"hello","device_id":"...","sample_rate":16000}
  Hub -> 边车 二进制帧：PCM int16 LE 单声道 16kHz（20ms = 640 字节）
  边车 -> Hub {"type":"ready"} / {"type":"wake","device_id","keyword",
                 "rms","timestamp_ms"} / {"type":"error","message"}
  HTTP GET /health -> 200 {"status":"ok","model_loaded":true}（模型未
                 加载好时 503；模型加载失败则进程直接非零退出）

并发模型：单事件循环内串行解码；每 device_id 独立的 KeywordSpotter
流状态（stream），互不干扰；连接断开即丢弃该设备流状态，重连从零开始。
"""
import argparse
import asyncio
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

# 模型文件（int8 encoder；与 client/scripts/download-kws-model.sh 一致）
ENCODER_INT8 = "encoder-epoch-12-avg-2-chunk-16-left-64.int8.onnx"
DECODER = "decoder-epoch-12-avg-2-chunk-16-left-64.onnx"
JOINER = "joiner-epoch-12-avg-2-chunk-16-left-64.onnx"
TOKENS = "tokens.txt"
KEYWORDS = "keywords.txt"

REQUIRED_MODEL_FILES = [ENCODER_INT8, DECODER, JOINER, TOKENS]


def load_spotter(model_dir, keywords_file, threshold, score, num_threads):
    """加载 KeywordSpotter；缺文件或加载失败直接抛异常（调用方非零退出）。"""
    missing = [f for f in REQUIRED_MODEL_FILES
               if not os.path.isfile(os.path.join(model_dir, f))]
    if missing:
        raise FileNotFoundError(
            f"model dir {model_dir} missing files: {', '.join(missing)}")
    if not os.path.isfile(keywords_file):
        raise FileNotFoundError(f"keywords file not found: {keywords_file}")
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
             "keywords=%s)", time.perf_counter() - t0, threshold, score,
             keywords_file)
    return spotter


class DeviceStream:
    """一台设备的独立 KWS 流状态。"""

    def __init__(self, spotter, device_id):
        self.spotter = spotter
        self.device_id = device_id
        self.stream = spotter.create_stream()
        self.rms_frames = deque(maxlen=RMS_WINDOW_FRAMES)
        self.wake_count = 0

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
    def __init__(self, spotter):
        self.spotter = spotter
        self.model_loaded = spotter is not None
        self.devices = {}  # device_id -> DeviceStream

    # ---- HTTP（经 websockets 的 process_request 拦截）----
    async def process_request(self, connection, request: Request):
        if request.path == "/health":
            if self.model_loaded:
                body = json.dumps(
                    {"status": "ok", "model_loaded": True}).encode()
                return Response(200, "OK", Headers([("Content-Type",
                                                      "application/json")]),
                                body)
            body = json.dumps(
                {"status": "error", "model_loaded": False}).encode()
            return Response(503, "Service Unavailable",
                            Headers([("Content-Type", "application/json")]),
                            body)
        if request.path == "/v1/kws":
            return None  # 继续 WebSocket 握手
        return Response(404, "Not Found", Headers(), b"not found")

    # ---- WebSocket ----
    async def handler(self, ws):
        device_id = None
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
            if device_id in self.devices:
                LOG.warning("device %s reconnected while old stream alive; "
                            "dropping old stream", device_id)
            stream = DeviceStream(self.spotter, device_id)
            self.devices[device_id] = stream
            LOG.info("device connected: %s", device_id)
            await ws.send(json.dumps({"type": "ready"}))

            async for message in ws:
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
                    keyword = stream.feed(pcm[i:i + FRAME_SAMPLES])
                    if keyword:
                        event = {
                            "type": "wake",
                            "device_id": device_id,
                            "keyword": keyword,
                            "rms": stream.rms,
                            "timestamp_ms": int(time.time() * 1000),
                        }
                        await ws.send(json.dumps(event, ensure_ascii=False))
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
            if device_id is not None and self.devices.get(device_id) is stream:
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
    try:
        spotter = load_spotter(args.model_dir, keywords_file,
                               args.threshold, args.score, args.num_threads)
    except Exception:  # noqa: BLE001 - 模型故障必须明确报错退出，不静默跑
        LOG.exception("FATAL: failed to load KWS model from %s; refusing "
                      "to start without a model", args.model_dir)
        sys.exit(1)
    server = Server(spotter)
    async with serve(server.handler, args.host, args.port,
                     process_request=server.process_request,
                     max_size=1 << 20):
        LOG.info("kws-sidecar listening on ws://%s:%d/v1/kws "
                 "(health: http://%s:%d/health)",
                 args.host, args.port, args.host, args.port)
        await asyncio.Future()  # run forever


def main():
    asyncio.run(amain())


if __name__ == "__main__":
    main()
