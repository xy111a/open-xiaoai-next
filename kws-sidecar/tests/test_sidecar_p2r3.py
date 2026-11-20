"""kws-sidecar 第三轮审计 P2 边车面回归测试（S-4 ~ S-13）。

S-8 不在此修（P1 轮已改，三处回环已核实），但有静态守护测试：
三处默认值一旦改回 0.0.0.0 即失败。
"""
import asyncio
import hashlib
import json
import logging
import os
import re
import sys
import types
import urllib.request
from pathlib import Path

import numpy as np
import pytest
import websockets
from websockets.asyncio.server import serve

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import server as sidecar_server

import bootstrap
from conftest import (KEYWORDS_FILE, MODEL_DIR, SAMPLES_DIR, SIDECAR_DIR,
                      TEST_TOKEN)
from test_kws_sidecar import (load_wav_frames, open_client, stream_frames,
                              wait_for_wake)
from test_sidecar_p2 import _run_server_cli, _start_server_subprocess

FRAME_BYTES = 640
KEYWORD = "你好小李"
REPO_DIR = SIDECAR_DIR.parents[0]


# ---------------- 共用假 spotter ----------------

class _SilentFakeSpotter:
    """永不检出的假 spotter；记录 accept_waveform 调用次数。"""

    def __init__(self):
        self.accept_calls = 0
        self.reset_calls = 0

    def create_stream(self):
        stream = types.SimpleNamespace()
        spotter = self

        def accept(sr, samples):
            spotter.accept_calls += 1

        stream.accept_waveform = accept
        return stream

    def is_ready(self, stream):
        return False

    def decode_stream(self, stream):
        pass

    def get_result(self, stream):
        return ""

    def reset_stream(self, stream):
        self.reset_calls += 1


class _FlakyFakeSpotter(_SilentFakeSpotter):
    """前 fail_times 次 accept_waveform 抛错，之后正常（S-6）。"""

    def __init__(self, fail_times):
        super().__init__()
        self.fail_times = fail_times
        self.failures = 0

    def create_stream(self):
        stream = types.SimpleNamespace()
        spotter = self

        def accept(sr, samples):
            spotter.accept_calls += 1
            if spotter.failures < spotter.fail_times:
                spotter.failures += 1
                raise RuntimeError("simulated decode failure")

        stream.accept_waveform = accept
        return stream


class _TrailingSilenceSpotter(_SilentFakeSpotter):
    """只有 trailing 静音到达后才吐结果（S-10；复用 P2-4 的模型行为）。"""

    def create_stream(self):
        stream = types.SimpleNamespace(audio_frames=0,
                                       silence_after_audio=0,
                                       reported=False)
        spotter = self

        def accept(sr, samples):
            spotter.accept_calls += 1
            if float(np.max(np.abs(samples))) > 1e-9:
                stream.audio_frames += 1
                stream.silence_after_audio = 0
            elif stream.audio_frames:
                stream.silence_after_audio += 1

        stream.accept_waveform = accept
        return stream

    def get_result(self, stream):
        if (stream.audio_frames >= 1 and stream.silence_after_audio >= 1
                and not stream.reported):
            stream.reported = True
            return KEYWORD
        return ""


# ---------------- S-4：非 640 字节帧拒绝 + max_size 收紧 ----------------

def test_s4_max_ws_message_size_is_64k():
    """S-4：serve 的 max_size 已从 1MB 收紧到 64KB。"""
    assert sidecar_server.MAX_WS_MESSAGE_SIZE == 64 << 10


def test_s4_non_640_frame_rejected_and_connection_survives():
    """S-4：1280 字节帧与 641 字节帧都回 error + 丢弃、不喂入模型；
    连接不断，后续合法 640 字节帧仍正常喂入。"""
    spotter = _SilentFakeSpotter()

    async def run():
        srv = sidecar_server.Server(spotter, keywords_count=1,
                                    token=TEST_TOKEN)
        async with serve(srv.handler, "127.0.0.1", 10410,
                         process_request=srv.process_request,
                         max_size=sidecar_server.MAX_WS_MESSAGE_SIZE):
            ws = await websockets.connect(
                "ws://127.0.0.1:10410/v1/kws",
                max_size=sidecar_server.MAX_WS_MESSAGE_SIZE)
            await ws.send(json.dumps(
                {"type": "hello", "device_id": "dev-s4",
                 "sample_rate": 16000, "token": TEST_TOKEN}))
            first = json.loads(await asyncio.wait_for(ws.recv(), timeout=5))
            assert first.get("type") == "ready"
            await ws.send(b"\x00" * 1280)
            err = json.loads(await asyncio.wait_for(ws.recv(), timeout=5))
            assert err.get("type") == "error"
            assert "640" in err["message"]
            await ws.send(b"\x00" * 641)
            err = json.loads(await asyncio.wait_for(ws.recv(), timeout=5))
            assert err.get("type") == "error"
            assert spotter.accept_calls == 0, \
                "oversized frames must never reach the model"
            # 连接存活的证据：后续合法 640 字节帧仍正常喂入（若已断，
            # send 即抛 ConnectionClosed）。
            await ws.send(b"\x00" * FRAME_BYTES)
            await asyncio.sleep(0.3)
            assert spotter.accept_calls == 1
            await ws.close()

    asyncio.run(run())


# ---------------- S-5：分桶计数 + fatal 自动清除 ----------------

def test_s5_error_counts_are_per_device():
    """S-5：两台设备各 4 次错误不触发 fatal（全局计数会是 8 次）；
    同一设备第 5 次才置 fatal，且原因写明设备。"""
    srv = sidecar_server.Server(object(), keywords_count=1)
    for _ in range(4):
        srv.note_feed_error("dev-a")
        srv.note_feed_error("dev-b")
    assert srv.health_reason() is None
    srv.note_feed_error("dev-a")
    assert srv.health_reason() is not None
    assert "repeated stream errors" in srv.health_reason()
    assert "dev-a" in srv.health_reason()


def test_s5_success_on_one_device_does_not_reset_another():
    """S-5：健康设备的成功喂入不重置故障设备的分桶计数。"""
    srv = sidecar_server.Server(object(), keywords_count=1)
    for _ in range(4):
        srv.note_feed_error("dev-a")
    for _ in range(3):
        srv.note_feed_success("dev-b")
    assert srv.stream_error_count("dev-a") == 4
    srv.note_feed_error("dev-a")
    assert srv.health_reason() is not None


def test_s5_fatal_clears_after_consecutive_successes():
    """S-5：fatal 置起后连续 5 次成功喂入即自动清除（带日志），
    /health 恢复 200，不再粘住到重启。"""
    records = []

    class ListHandler(logging.Handler):
        def emit(self, record):
            records.append(record.getMessage())

    srv = sidecar_server.Server(object(), keywords_count=1)
    for _ in range(5):
        srv.note_feed_error("dev-a")
    assert srv.health_reason() is not None
    logger = logging.getLogger("kws-sidecar")
    handler = ListHandler()
    handler.setLevel(logging.DEBUG)
    old_level = logger.level
    logger.addHandler(handler)
    logger.setLevel(logging.DEBUG)
    try:
        for _ in range(4):
            srv.note_feed_success("dev-b")
            assert srv.health_reason() is not None
        srv.note_feed_success("dev-b")
    finally:
        logger.removeHandler(handler)
        logger.setLevel(old_level)
    assert srv.health_reason() is None
    assert any("clearing sidecar unhealthy flag" in r for r in records)


# ---------------- S-6：单帧异常复位流不断连接 ----------------

def test_s6_transient_feed_errors_reset_stream_not_connection():
    """S-6：前 2 帧喂入抛错 → 记 error、复位流、连接不断；
    第 3 帧正常喂入，reset 恰好调了 2 次。"""
    spotter = _FlakyFakeSpotter(fail_times=2)

    async def run():
        srv = sidecar_server.Server(spotter, keywords_count=1,
                                    token=TEST_TOKEN)
        async with serve(srv.handler, "127.0.0.1", 10411,
                         process_request=srv.process_request,
                         max_size=1 << 20):
            ws, first = await open_client("ws://127.0.0.1:10411/v1/kws",
                                          "dev-s6")
            assert first.get("type") == "ready"
            frame = b"\x00" * FRAME_BYTES
            await ws.send(frame)
            await ws.send(frame)
            await ws.send(frame)
            await asyncio.sleep(0.5)
            assert spotter.failures == 2
            assert spotter.reset_calls == 2
            assert spotter.accept_calls == 3
            await ws.close()

    asyncio.run(run())


def test_s6_five_consecutive_feed_errors_drop_connection():
    """S-6：同一设备连续 5 次喂入失败 → 第 5 次后断连接（与 S-5 联动），
    客户端收到 internal error 后连接关闭。"""
    spotter = _FlakyFakeSpotter(fail_times=100)

    async def run():
        srv = sidecar_server.Server(spotter, keywords_count=1,
                                    token=TEST_TOKEN)
        async with serve(srv.handler, "127.0.0.1", 10412,
                         process_request=srv.process_request,
                         max_size=1 << 20):
            ws, first = await open_client("ws://127.0.0.1:10412/v1/kws",
                                          "dev-s6-drop")
            assert first.get("type") == "ready"
            frame = b"\x00" * FRAME_BYTES
            for _ in range(5):
                await ws.send(frame)
            err = json.loads(await asyncio.wait_for(ws.recv(), timeout=5))
            assert err.get("type") == "error"
            with pytest.raises(websockets.ConnectionClosed):
                await asyncio.wait_for(ws.recv(), timeout=5)

    asyncio.run(run())


# ---------------- S-7：Dockerfile 钉 digest / apt 版本 / USER ----------------

def test_s7_dockerfile_pins_digest_apt_versions_and_user():
    """S-7：FROM 钉 digest（与 2026-11-18 Docker Hub API 查到的
    python:3.12-slim digest 一致）、apt 包钉版本、镜像内有 USER。"""
    text = (SIDECAR_DIR / "Dockerfile").read_text(encoding="utf-8")
    m = re.search(r"^FROM python:3\.12-slim@sha256:([0-9a-f]{64})\s*$",
                  text, re.M)
    assert m, "FROM must pin python:3.12-slim by digest (S-7)"
    assert m.group(1) == \
        "5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b", \
        "digest must match the 2026-11-18 Docker Hub API value"
    # apt 钉版本跟底座的 Debian 发行版走：2026-11-20 起是 trixie
    #（python:3.12-slim 从 bookworm 漂到 trixie，见 Dockerfile 注释）。
    assert "curl=8.14.1-2+deb13u7" in text
    assert "bzip2=1.0.8-6" in text
    lines = text.splitlines()
    user_line = next(i for i, l in enumerate(lines)
                     if l.strip() == "USER 1000:1000")
    entry_line = next(i for i, l in enumerate(lines)
                      if l.startswith("ENTRYPOINT"))
    chown_line = next(i for i, l in enumerate(lines)
                      if "chown 1000:1000 /data/kws/models" in l
                      and l.strip().startswith("RUN"))
    # USER 必须在 mkdir + chown（root 才能做）之后、ENTRYPOINT 之前
    assert chown_line < user_line < entry_line


# ---------------- S-8：已核实关闭（P1 轮已修）——守护测试 ----------------

def test_s8_loopback_defaults_guard(monkeypatch):
    """S-8（已核实关闭）：--host 默认、Dockerfile ENV、compose 环境变量
    三处都必须是 127.0.0.1；任一处改回 0.0.0.0 即失败。"""
    monkeypatch.delenv("KWS_HOST", raising=False)
    args = sidecar_server.build_arg_parser().parse_args([])
    assert args.host == "127.0.0.1"
    dockerfile = (SIDECAR_DIR / "Dockerfile").read_text(encoding="utf-8")
    assert "KWS_HOST=127.0.0.1" in dockerfile
    assert "0.0.0.0" not in dockerfile
    compose = (REPO_DIR / "deploy" / "docker-compose.yml").read_text(
        encoding="utf-8")
    assert "KWS_HOST=127.0.0.1" in compose


# ---------------- S-9：部分缺失时已存在文件也验 SHA ----------------

def test_s9_partial_missing_voice_still_verifies_existing(tmp_path,
                                                          monkeypatch):
    """S-9：缺一个语音文件时，现存的那个也要验 SHA——坏的删掉重下，
    缺的下载补齐（此前现存文件直接跳过校验）。"""
    good_a, good_b = b"voice-a-bytes", b"voice-b-bytes"
    src_a = tmp_path / "src-a.onnx"
    src_b = tmp_path / "src-b.onnx.json"
    src_a.write_bytes(good_a)
    src_b.write_bytes(good_b)
    monkeypatch.setattr(bootstrap, "VOICE_FILES", [
        ("a.onnx", src_a.as_uri()),
        ("a.onnx.json", src_b.as_uri()),
    ])
    monkeypatch.setattr(bootstrap, "VOICE_SHA256", {
        "a.onnx": hashlib.sha256(good_a).hexdigest(),
        "a.onnx.json": hashlib.sha256(good_b).hexdigest(),
    })
    root = tmp_path / "assets"
    tts_dir = root / "tts"
    tts_dir.mkdir(parents=True)
    (tts_dir / "a.onnx").write_bytes(b"corrupt")
    # a.onnx.json 缺失：此前 _ensure_voice 直接进下载循环，现存的
    # corrupt a.onnx 会被跳过校验用到底。
    assert not (tts_dir / "a.onnx.json").exists()
    bootstrap._ensure_voice(root)
    assert (tts_dir / "a.onnx").read_bytes() == good_a
    assert (tts_dir / "a.onnx.json").read_bytes() == good_b


# ---------------- S-10：flush 检出计入 total_wakes，device_wakes 保留 ----------------

def test_s10_flush_wake_counted_and_device_wakes_retained():
    """S-10：断开时 flush 检出的唤醒计入 total_wakes、单记 lost_wakes；
    断开后 /health 的 device_wakes 仍保留该设备的累计值。"""
    spotter = _TrailingSilenceSpotter()

    async def get_health(srv):
        resp = await srv.process_request(
            None, types.SimpleNamespace(path="/health"))
        return resp.status_code, json.loads(resp.body)

    async def run():
        srv = sidecar_server.Server(spotter, keywords_count=1,
                                    token=TEST_TOKEN)
        async with serve(srv.handler, "127.0.0.1", 10413,
                         process_request=srv.process_request,
                         max_size=1 << 20):
            ws, first = await open_client("ws://127.0.0.1:10413/v1/kws",
                                          "dev-flush-r3")
            assert first.get("type") == "ready"
            frame = np.full(320, 1000, dtype=np.int16).tobytes()
            for _ in range(3):
                await ws.send(frame)
            await ws.close()
            await asyncio.sleep(1.0)
        return srv

    srv = asyncio.run(run())
    assert srv.total_wakes == 1, "flush wake must count in total_wakes"
    assert srv.lost_wakes == 1
    status, body = asyncio.run(get_health(srv))
    assert status == 200
    assert body["total_wakes"] == 1
    assert body["lost_wakes"] == 1
    assert body["device_wakes"] == {"dev-flush-r3": 1}, \
        "device_wakes must retain disconnected devices (S-10)"


# ---------------- S-11：取代超时后旧 handler 被取消、连接数释放 ----------------

class _RawWS:
    """最小 WebSocket 客户端（裸 socket）：故意不回 close 握手，
    用于模拟“旧连接 close() 5 秒超时”的对端。"""

    def __init__(self):
        self.reader = None
        self.writer = None

    async def connect(self, host, port, path="/v1/kws"):
        import base64
        import os as _os
        self.reader, self.writer = await asyncio.open_connection(host, port)
        key = base64.b64encode(_os.urandom(16)).decode()
        req = (f"GET {path} HTTP/1.1\r\nHost: {host}:{port}\r\n"
               f"Upgrade: websocket\r\nConnection: Upgrade\r\n"
               f"Sec-WebSocket-Key: {key}\r\n"
               f"Sec-WebSocket-Version: 13\r\n\r\n")
        self.writer.write(req.encode())
        await self.writer.drain()
        head = await self.reader.readuntil(b"\r\n\r\n")
        assert b"101" in head.split(b"\r\n")[0], head

    async def send_text(self, text):
        import os as _os
        import struct
        payload = text.encode()
        mask = _os.urandom(4)
        header = bytes([0x81, 0x80 | len(payload)]) + mask
        masked = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
        self.writer.write(header + masked)
        await self.writer.drain()

    async def recv_text(self, timeout=5):
        """读一帧服务端文本帧；close/ping 帧跳过（不回）。"""
        while True:
            b1 = await asyncio.wait_for(self.reader.readexactly(1),
                                        timeout=timeout)
            b2 = await asyncio.wait_for(self.reader.readexactly(1),
                                        timeout=timeout)
            opcode = b1[0] & 0x0F
            ln = b2[0] & 0x7F
            if ln == 126:
                ln = int.from_bytes(
                    await self.reader.readexactly(2), "big")
            elif ln == 127:
                ln = int.from_bytes(
                    await self.reader.readexactly(8), "big")
            payload = await self.reader.readexactly(ln) if ln else b""
            if opcode == 0x1:
                return payload.decode()
            # opcode 0x8 close / 0x9 ping：一律不回（模拟挂住的旧连接）

    def close_socket(self):
        if self.writer is not None:
            self.writer.close()


def test_s11_replaced_connection_timeout_cancels_old_handler():
    """S-11：旧连接不回 close 握手时，取代方等 5 秒超时后显式取消旧
    handler——旧连接数名额释放，新连接正常工作，/health 的连接数
    只剩新连接（半开/已认证分开计）。"""
    spotter = _SilentFakeSpotter()

    async def run():
        srv = sidecar_server.Server(spotter, keywords_count=1,
                                    token=TEST_TOKEN)
        async with serve(srv.handler, "127.0.0.1", 10414,
                         process_request=srv.process_request,
                         max_size=1 << 20):
            old = _RawWS()
            await old.connect("127.0.0.1", 10414)
            await old.send_text(json.dumps(
                {"type": "hello", "device_id": "dev-s11",
                 "sample_rate": 16000, "token": TEST_TOKEN}))
            assert json.loads(await old.recv_text())["type"] == "ready"
            assert srv.connections == 1
            assert srv.authenticated_connections == 1
            # 新连接取代旧连接：旧端不回 close，server 侧 5 秒超时。
            # 注意不用 open_client：它等 ready 只等 5 秒，会与服务端
            # 的 5 秒关旧连接超时撞车；这里放宽到 15 秒。
            ws = await websockets.connect(
                "ws://127.0.0.1:10414/v1/kws", max_size=1 << 20)
            await ws.send(json.dumps(
                {"type": "hello", "device_id": "dev-s11",
                 "sample_rate": 16000, "token": TEST_TOKEN}))
            first = json.loads(
                await asyncio.wait_for(ws.recv(), timeout=15))
            assert first.get("type") == "ready"
            # 取代完成时旧 handler 可能已被取消（计数已释放）或仍在
            # 5 秒超时后的取消调度中——只断言不超过 2，不钉死等于 2。
            assert srv.connections in (1, 2)
            # 等旧 handler 被取消、计数释放（5 秒超时 + 调度余量）
            deadline = asyncio.get_running_loop().time() + 10
            while srv.connections != 1:
                if asyncio.get_running_loop().time() > deadline:
                    break
                await asyncio.sleep(0.2)
            assert srv.connections == 1, \
                f"old handler must release its connection slot, got " \
                f"connections={srv.connections}"
            assert srv.authenticated_connections == 1
            await ws.send(b"\x00" * FRAME_BYTES)
            await asyncio.sleep(0.3)
            assert spotter.accept_calls == 1, \
                "new connection must still feed audio after replacement"
            await ws.close()
            old.close_socket()

    asyncio.run(run())


# ---------------- S-12：score 上界 / 环境变量 FATAL / 线程数与端口范围 ----------------

@pytest.mark.parametrize("args", [
    ["--score", "10.1"],
    ["--score", "100"],
    ["--num-threads", "0"],
    ["--num-threads", "9"],
    ["--port", "0"],
    ["--port", "65536"],
])
def test_s12_out_of_range_score_threads_port_exit_nonzero(args):
    """S-12：score >10、线程数 ∉[1,8]、端口 ∉[1,65535] → FATAL 非零退出。"""
    proc = _run_server_cli(args)
    assert proc.returncode != 0
    assert "FATAL" in proc.stderr + proc.stdout


@pytest.mark.parametrize("env_extra", [
    {"KWS_SCORE": "abc"},
    {"KWS_THRESHOLD": "abc"},
    {"KWS_PORT": "abc"},
    {"KWS_NUM_THREADS": "abc"},
])
def test_s12_bad_env_value_fatal_without_traceback(env_extra):
    """S-12：非法环境变量值 → FATAL 干净退出，不抛 ValueError traceback。"""
    proc = _run_server_cli([], env_extra)
    assert proc.returncode != 0
    combined = proc.stderr + proc.stdout
    assert "FATAL" in combined
    assert "Traceback" not in combined


def test_s12_bad_env_value_raises_system_exit_in_parser(monkeypatch):
    """S-12：build_arg_parser 解析非法环境变量时直接 SystemExit(1)。"""
    monkeypatch.setenv("KWS_SCORE", "not-a-number")
    with pytest.raises(SystemExit):
        sidecar_server.build_arg_parser()


def test_s12_boundary_values_start(tmp_path):
    """S-12：边界合法值 score=10、num-threads=8 正常启动、/health 200。"""
    log_path = tmp_path / "server.log"
    proc, log_file, _url, health = _start_server_subprocess(
        10415, log_path,
        ["--score", "10", "--num-threads", "8", "--port", "10415"])
    try:
        with urllib.request.urlopen(health, timeout=5) as r:
            assert r.status == 200
    finally:
        proc.terminate()
        proc.wait(timeout=10)
        log_file.close()


# ---------------- S-13：wake 事件带 wake_no / frames_since_wake ----------------

def test_s13_wake_event_carries_wake_no_and_frames(kws_server):
    """S-13：真模型 wake 事件含 wake_no（第 1 次为 1）与
    frames_since_wake（≥0 的检出延迟帧数）；协议文档同步更新。"""
    frames = load_wav_frames(SAMPLES_DIR / "pos1.wav")

    async def run():
        ws, first = await open_client(kws_server["url"], "pos-s13")
        assert first.get("type") == "ready"
        wake_task = asyncio.create_task(wait_for_wake(ws, timeout=10))
        await stream_frames(ws, frames)
        wake = await wake_task
        await ws.close()
        return wake

    wake = asyncio.run(run())
    assert wake is not None
    assert wake.get("wake_no") == 1, wake
    assert "frames_since_wake" in wake
    assert isinstance(wake["frames_since_wake"], int)
    assert wake["frames_since_wake"] >= 0
    doc = (SIDECAR_DIR / "server.py").read_text(encoding="utf-8")
    assert "wake_no" in doc and "frames_since_wake" in doc
    readme = (SIDECAR_DIR / "README.md").read_text(encoding="utf-8")
    assert "wake_no" in readme and "frames_since_wake" in readme
