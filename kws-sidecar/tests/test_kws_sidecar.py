"""kws-sidecar 端到端测试（真实 server + 真实模型，WebSocket 实测）。

⚠️ 合成语音测试，非真人唤醒率：全部音频样本由 Piper TTS
（zh_CN-huayan-medium）合成后重采样到 16kHz 单声道；默认用实验
目录 ~/workspace/.kws-exp/samples，缺失时由 tests/bootstrap.py
现生成（见 conftest.py；CI 用 KWS_TEST_ASSETS 指向的空目录验证过）。
这里只证明“模型能检出合成语音 / 对负样本静默”，绝不是真人唤醒率、
不是真机性能。
"""
import asyncio
import json
import subprocess
import sys
import urllib.request
import wave
from pathlib import Path

import pytest
import websockets

from conftest import KEYWORDS_FILE, MODEL_DIR, SAMPLES_DIR, SIDECAR_DIR

FRAME_BYTES = 640          # 20ms @ 16kHz int16 mono
TRAILING_SILENCE_FRAMES = 50  # 1s 静音：模型需要关键词后的 trailing blanks
KEYWORD = "你好小李"

POS_FILES = ["pos1.wav", "pos2.wav", "pos3.wav", "pos4_fast.wav",
             "pos5_slow.wav"]
NEG_FILES = ["neg1.wav", "neg2.wav", "neg3.wav", "neg4.wav", "neg5.wav",
             "neg_noise2s.wav"]


def load_wav_frames(path, limit_frames=None):
    """读 16kHz 单声道 16-bit wav，返回 640 字节/帧的 bytes 列表。"""
    with wave.open(str(path)) as w:
        assert w.getframerate() == 16000, path
        assert w.getnchannels() == 1, path
        assert w.getsampwidth() == 2, path
        pcm = w.readframes(w.getnframes())
    frames = [pcm[i:i + FRAME_BYTES]
              for i in range(0, len(pcm), FRAME_BYTES)]
    if limit_frames is not None:
        frames = frames[:limit_frames]
    return frames


async def open_client(url, device_id, sample_rate=16000):
    """hello 握手；返回 (ws, 首条响应 dict)。"""
    ws = await websockets.connect(url, max_size=1 << 20)
    await ws.send(json.dumps({"type": "hello", "device_id": device_id,
                              "sample_rate": sample_rate}))
    first = json.loads(await asyncio.wait_for(ws.recv(), timeout=5))
    return ws, first


async def stream_frames(ws, frames, trailing_silence=TRAILING_SILENCE_FRAMES):
    for f in frames:
        await ws.send(f)
    for _ in range(trailing_silence):
        await ws.send(b"\x00" * FRAME_BYTES)


async def wait_for_wake(ws, timeout=8):
    """等一条 wake 事件；超时返回 None（其余消息忽略）。"""
    try:
        while True:
            msg = json.loads(
                await asyncio.wait_for(ws.recv(), timeout=timeout))
            if msg.get("type") == "wake":
                return msg
    except asyncio.TimeoutError:
        return None


async def collect_for(ws, seconds):
    """在 seconds 内收到的全部 JSON 消息（超时即结束）。"""
    msgs = []
    loop = asyncio.get_running_loop()
    deadline = loop.time() + seconds
    while True:
        try:
            msg = json.loads(await asyncio.wait_for(
                ws.recv(), timeout=max(0.01, deadline - loop.time())))
            msgs.append(msg)
        except asyncio.TimeoutError:
            return msgs


# ---------------- 正样本：合成“你好小李”应出 wake ----------------

@pytest.mark.parametrize("fname", POS_FILES)
def test_positive_wake(kws_server, fname):
    """合成正样本 → 恰好一次 wake，keyword/设备回显正确。"""
    path = SAMPLES_DIR / fname
    assert path.is_file(), f"missing sample: {path}"
    frames = load_wav_frames(path)

    async def run():
        ws, first = await open_client(kws_server["url"], f"pos-{fname}")
        assert first.get("type") == "ready"
        wake_task = asyncio.create_task(wait_for_wake(ws, timeout=10))
        await stream_frames(ws, frames)
        wake = await wake_task
        await ws.close()
        return wake

    wake = asyncio.run(run())
    assert wake is not None, f"{fname}: no wake event for synthetic positive"
    assert wake["type"] == "wake"
    assert wake["device_id"] == f"pos-{fname}"
    assert wake["keyword"] == KEYWORD
    assert isinstance(wake["rms"], (int, float)) and 0 <= wake["rms"] <= 1
    assert isinstance(wake["timestamp_ms"], int) and wake["timestamp_ms"] > 0


# ---------------- 负样本：其它语音/静音不应出 wake ----------------

@pytest.mark.parametrize("fname", NEG_FILES)
def test_negative_silent(kws_server, fname):
    """合成负样本（半句/只喊小李/无关语音/别的唤醒词/噪声）→ 无 wake。"""
    path = SAMPLES_DIR / fname
    assert path.is_file(), f"missing sample: {path}"
    frames = load_wav_frames(path)

    async def run():
        ws, first = await open_client(kws_server["url"], f"neg-{fname}")
        assert first.get("type") == "ready"
        await stream_frames(ws, frames)
        msgs = await collect_for(ws, 1.5)
        await ws.close()
        return msgs

    msgs = asyncio.run(run())
    wakes = [m for m in msgs if m.get("type") == "wake"]
    assert wakes == [], f"{fname}: unexpected wake event(s): {wakes}"


# ---------------- 双设备并发隔离 ----------------

def test_two_devices_isolated(kws_server):
    """A 设备唤醒时，B 设备（只喂静音）收不到任何 wake 事件。"""
    frames = load_wav_frames(SAMPLES_DIR / "pos1.wav")

    async def run():
        ws_a, first_a = await open_client(kws_server["url"], "dev-A")
        ws_b, first_b = await open_client(kws_server["url"], "dev-B")
        assert first_a.get("type") == "ready"
        assert first_b.get("type") == "ready"

        async def feed_a():
            wake_task = asyncio.create_task(wait_for_wake(ws_a, timeout=10))
            await stream_frames(ws_a, frames)
            return await wake_task

        async def feed_b():
            await stream_frames(ws_b, [], trailing_silence=100)  # 2s 静音
            return await collect_for(ws_b, 1.0)

        wake_a, msgs_b = await asyncio.gather(feed_a(), feed_b())
        await ws_a.close()
        await ws_b.close()
        return wake_a, msgs_b

    wake_a, msgs_b = asyncio.run(run())
    assert wake_a is not None and wake_a["device_id"] == "dev-A"
    assert wake_a["keyword"] == KEYWORD
    assert [m for m in msgs_b if m.get("type") == "wake"] == []


# ---------------- 断开清理：重连不残留旧状态 ----------------

def test_reconnect_drops_old_state(kws_server):
    """同一 device_id：喂半句唤醒词后断开 → 重连后只喂后半句不出 wake；
    再喂完整合成唤醒词恰好出一次 wake（旧状态未残留、新流正常工作）。"""
    frames = load_wav_frames(SAMPLES_DIR / "pos1.wav")
    half = len(frames) // 2
    device_id = "dev-reconnect"

    async def run():
        # 第一段连接：只喂前半句（“你好小…”），不断言结果，直接断开
        ws, first = await open_client(kws_server["url"], device_id)
        assert first.get("type") == "ready"
        for f in frames[:half]:
            await ws.send(f)
        await ws.close()

        # 重连同一 device_id
        ws, first = await open_client(kws_server["url"], device_id)
        assert first.get("type") == "ready"
        # 只喂后半句：若旧状态残留可能拼出完整关键词；干净状态下不应触发
        await stream_frames(ws, frames[half:])
        msgs = await collect_for(ws, 1.5)
        assert [m for m in msgs if m.get("type") == "wake"] == [], \
            "stale stream state leaked across reconnect"
        # 再喂完整一句：新流应恰好检出一次
        wake_task = asyncio.create_task(wait_for_wake(ws, timeout=10))
        await stream_frames(ws, frames)
        wake = await wake_task
        await ws.close()
        return wake

    wake = asyncio.run(run())
    assert wake is not None and wake["device_id"] == device_id
    assert wake["keyword"] == KEYWORD


# ---------------- 协议校验：非法 hello / 健康检查 / 模型缺失 ----------------

def test_hello_wrong_sample_rate_gets_error_and_close(kws_server):
    """hello 的 sample_rate 不是 16000 → error 消息后连接关闭。"""
    async def run():
        ws = await websockets.connect(kws_server["url"], max_size=1 << 20)
        await ws.send(json.dumps({"type": "hello", "device_id": "dev-bad",
                                  "sample_rate": 8000}))
        first = json.loads(await asyncio.wait_for(ws.recv(), timeout=5))
        assert first.get("type") == "error"
        with pytest.raises(websockets.ConnectionClosed):
            await asyncio.wait_for(ws.recv(), timeout=5)

    asyncio.run(run())


def test_health_endpoint(kws_server):
    """GET /health → 200 {"status":"ok","model_loaded":true}。"""
    with urllib.request.urlopen(kws_server["health"], timeout=5) as r:
        assert r.status == 200
        body = json.loads(r.read())
    assert body == {"status": "ok", "model_loaded": True}


def test_model_missing_exits_nonzero(tmp_path):
    """模型目录缺文件 → 进程明确报错、非零退出（不静默跑）。"""
    empty_dir = tmp_path / "no-model"
    empty_dir.mkdir()
    venv_python = SIDECAR_DIR / ".venv" / "bin" / "python"
    cmd = [str(venv_python) if venv_python.exists() else sys.executable,
           str(SIDECAR_DIR / "server.py"),
           "--host", "127.0.0.1", "--port", "10499",
           "--model-dir", str(empty_dir),
           "--keywords-file", str(KEYWORDS_FILE)]
    proc = subprocess.run(cmd, capture_output=True, text=True, timeout=60)
    assert proc.returncode != 0, "server must exit nonzero without a model"
    assert "FATAL" in proc.stderr or "FATAL" in proc.stdout
