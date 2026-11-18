"""kws-sidecar P2 全修轮回归测试（SC-P2-1 ~ SC-P2-13，SC-P2-12 除外）。

SC-P2-12（容器 root 运行、无资源上限）不归边车面改（compose 加固由
固件面统一做），本文件不含该项测试。

⚠️ 合成语音测试，非真人唤醒率：正样本由 Piper TTS
（zh_CN-huayan-medium）合成；只证明“修复的逻辑在合成语音上生效”，
绝不是真人唤醒率、不是真机性能。
"""
import asyncio
import hashlib
import json
import logging
import os
import re
import shutil
import subprocess
import sys
import threading
import time
import types
import urllib.request
import http.server
from pathlib import Path

import numpy as np
import pytest
import websockets
from websockets.asyncio.server import serve

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import server as sidecar_server

import bootstrap
from conftest import (KEYWORDS_FILE, MODEL_DIR, SAMPLES_DIR, SIDECAR_DIR,
                      TEST_TOKEN, _server_python)
from test_kws_sidecar import (load_wav_frames, open_client, stream_frames,
                              wait_for_wake)

FRAME_BYTES = 640
KEYWORD = "你好小李"


def _start_server_subprocess(port, log_path, extra_args=None):
    """起一个真实 server 子进程，日志写文件；返回 (proc, url, health)。"""
    cmd = [_server_python(),
           str(SIDECAR_DIR / "server.py"),
           "--host", "127.0.0.1", "--port", str(port),
           "--model-dir", str(MODEL_DIR),
           "--keywords-file", str(KEYWORDS_FILE),
           "--token", TEST_TOKEN,
           "--log-level", "INFO"]
    if extra_args:
        cmd += extra_args
    log_file = open(log_path, "w", encoding="utf-8")
    proc = subprocess.Popen(cmd, stdout=log_file, stderr=subprocess.STDOUT,
                            text=True)
    health = f"http://127.0.0.1:{port}/health"
    deadline = time.time() + 90
    try:
        while time.time() < deadline:
            if proc.poll() is not None:
                raise RuntimeError(
                    f"server exited early with {proc.returncode}")
            try:
                with urllib.request.urlopen(health, timeout=2) as r:
                    if r.status == 200:
                        return proc, log_file, \
                            f"ws://127.0.0.1:{port}/v1/kws", health
            except Exception:
                time.sleep(0.5)
        raise RuntimeError("server did not become healthy in time")
    except Exception:
        proc.terminate()
        proc.wait(timeout=10)
        log_file.close()
        raise


# ---------------- SC-P2-1：is_ready 排空循环有上限 ----------------

class _AlwaysReadySpotter:
    """is_ready 恒真的假 spotter（模拟模型/音频异常卡住的排空循环）。"""

    def __init__(self):
        self.decode_calls = 0
        self.reset_calls = 0

    def create_stream(self):
        return types.SimpleNamespace(
            accept_waveform=lambda sr, samples: None)

    def is_ready(self, stream):
        return True

    def decode_stream(self, stream):
        self.decode_calls += 1

    def get_result(self, stream):
        return ""

    def reset_stream(self, stream):
        self.reset_calls += 1


def test_p2_1_decode_loop_capped():
    """SC-P2-1：is_ready 恒真时 feed 最多 decode 32 次即 reset 流、
    返回（不永久堵住事件循环）。"""
    spotter = _AlwaysReadySpotter()
    stream = sidecar_server.DeviceStream(spotter, "dev-cap")
    result = stream.feed(np.zeros(320, dtype=np.int16))
    assert result is None
    assert spotter.decode_calls == sidecar_server.MAX_DECODE_PER_FRAME
    assert spotter.reset_calls == 1


# ---------------- SC-P2-2：wake 事件带 rms_peak ----------------

def test_p2_2_rms_peak_not_diluted(kws_server):
    """SC-P2-2：wake 事件含 rms_peak；pos1 实测峰值帧 RMS 0.311
    （第 27/63 帧，在检出窗口内），而 1s 均值 rms 约 0.137——峰值
    不被 trailing 静音稀释，仲裁应优先用它。"""
    frames = load_wav_frames(SAMPLES_DIR / "pos1.wav")

    async def run():
        ws, first = await open_client(kws_server["url"], "pos-peak")
        assert first.get("type") == "ready"
        wake_task = asyncio.create_task(wait_for_wake(ws, timeout=10))
        await stream_frames(ws, frames)
        wake = await wake_task
        await ws.close()
        return wake

    wake = asyncio.run(run())
    assert wake is not None
    assert "rms_peak" in wake, "wake event must carry rms_peak (SC-P2-2)"
    assert 0 <= wake["rms"] <= wake["rms_peak"] <= 1
    assert wake["rms_peak"] >= 0.25, \
        f"rms_peak {wake['rms_peak']} should be near the pos1 peak 0.311"
    assert wake["rms_peak"] > wake["rms"], \
        "rms must be diluted by trailing silence while rms_peak is not"


# ---------------- SC-P2-3：hello 超时不再走通用 except ----------------

def test_p2_3_hello_timeout_message_and_no_traceback(tmp_path):
    """SC-P2-3：建连后 10s 不发 hello → 收到 "hello timeout"（不是
    "internal error"）、1008 关闭；服务端日志有 warning、无 traceback。"""
    log_path = tmp_path / "server.log"
    proc, log_file, url, _health = _start_server_subprocess(10404, log_path)
    try:
        async def run():
            ws = await websockets.connect(url, max_size=1 << 20)
            first = json.loads(
                await asyncio.wait_for(ws.recv(), timeout=15))
            assert first.get("type") == "error"
            assert first.get("message") == "hello timeout", \
                f"expected 'hello timeout', got {first!r}"
            with pytest.raises(websockets.ConnectionClosed) as exc:
                await asyncio.wait_for(ws.recv(), timeout=5)
            return exc.value.rcvd.code

        code = asyncio.run(run())
        assert code == 1008
    finally:
        proc.terminate()
        proc.wait(timeout=10)
        log_file.close()
    log = log_path.read_text(encoding="utf-8")
    assert "hello timeout" in log
    assert "Traceback" not in log, \
        "hello timeout must not dump a traceback"


# ---------------- SC-P2-4：断开时 flush 尾部关键词 ----------------

class _FakeStream:
    """accept_waveform 会记录音频帧 / trailing 静音帧的假流。"""

    def __init__(self):
        self.audio_frames = 0
        self.silence_after_audio = 0
        self.reported = False

    def accept_waveform(self, sr, samples):
        if float(np.max(np.abs(samples))) > 1e-9:
            self.audio_frames += 1
            self.silence_after_audio = 0
        elif self.audio_frames:
            self.silence_after_audio += 1


class _TrailingSilenceSpotter:
    """只有 trailing 静音到达后才吐结果的假 spotter（模拟真模型
    “关键词后约 200ms 才出结果”的行为）。"""

    def create_stream(self):
        return _FakeStream()

    def is_ready(self, stream):
        return False

    def decode_stream(self, stream):
        pass

    def get_result(self, stream):
        if (stream.audio_frames >= 1 and stream.silence_after_audio >= 1
                and not stream.reported):
            stream.reported = True
            return KEYWORD
        return ""

    def reset_stream(self, stream):
        stream.reported = True


def test_p2_4_flush_catches_keyword_at_disconnect_fake_spotter():
    """SC-P2-4（接线）：只喂音频、不喂 trailing 静音就断开——flush 补喂
    静音后检出关键词，服务端记 "wake lost on disconnect" 日志。"""
    records = []

    class ListHandler(logging.Handler):
        def emit(self, record):
            records.append(record.getMessage())

    async def run():
        srv = sidecar_server.Server(_TrailingSilenceSpotter(),
                                    keywords_count=1, token=TEST_TOKEN)
        logger = logging.getLogger("kws-sidecar")
        handler = ListHandler()
        handler.setLevel(logging.DEBUG)
        logger.addHandler(handler)
        logger.setLevel(logging.DEBUG)
        try:
            async with serve(srv.handler, "127.0.0.1", 10406,
                             process_request=srv.process_request,
                             max_size=1 << 20):
                ws = await websockets.connect(
                    "ws://127.0.0.1:10406/v1/kws", max_size=1 << 20)
                await ws.send(json.dumps(
                    {"type": "hello", "device_id": "dev-flush",
                     "sample_rate": 16000, "token": TEST_TOKEN}))
                first = json.loads(
                    await asyncio.wait_for(ws.recv(), timeout=5))
                assert first.get("type") == "ready"
                frame = np.full(320, 1000, dtype=np.int16).tobytes()
                for _ in range(3):
                    await ws.send(frame)
                await ws.close()
                await asyncio.sleep(1.0)
        finally:
            logger.removeHandler(handler)

    asyncio.run(run())
    assert any("wake lost on disconnect" in r for r in records), \
        f"flush did not log the lost wake; records: {records}"


def test_p2_4_flush_catches_keyword_at_disconnect_real_model(tmp_path):
    """SC-P2-4（真模型）：喂完整 pos1（63 帧）但不喂 trailing 静音就
    断开——探针已证实纯音频喂完不检出、补 9 帧静音才检出；flush 补
    25 帧静音必须在日志里留下 "wake lost on disconnect"。"""
    log_path = tmp_path / "server.log"
    proc, log_file, url, _health = _start_server_subprocess(10405, log_path)
    try:
        frames = load_wav_frames(SAMPLES_DIR / "pos1.wav")

        async def run():
            ws, first = await open_client(url, "dev-flush-real")
            assert first.get("type") == "ready"
            for f in frames:
                await ws.send(f)
            await ws.close()
            await asyncio.sleep(1.5)

        asyncio.run(run())
    finally:
        proc.terminate()
        proc.wait(timeout=10)
        log_file.close()
    log = log_path.read_text(encoding="utf-8")
    assert "wake lost on disconnect" in log, \
        f"flush did not catch the keyword at disconnect; log tail:\n{log[-2000:]}"


# ---------------- SC-P2-5：device_id 长度/内容校验 ----------------

@pytest.mark.parametrize("bad_id", [
    "x" * 65,                 # 超长
    "bad\nid",                # 含换行（日志行伪造）
    "bad\rid",                # 含回车
    "bad id!",                # 含空格与非法字符
    "bad/id",
    "",                       # 空
])
def test_p2_5_bad_device_id_rejected(kws_server, bad_id):
    """SC-P2-5：非法 device_id → error 后连接关闭（1002）。"""
    async def run():
        ws = await websockets.connect(kws_server["url"], max_size=1 << 20)
        await ws.send(json.dumps(
            {"type": "hello", "device_id": bad_id, "sample_rate": 16000,
             "token": TEST_TOKEN}))
        first = json.loads(await asyncio.wait_for(ws.recv(), timeout=5))
        assert first.get("type") == "error"
        with pytest.raises(websockets.ConnectionClosed):
            await asyncio.wait_for(ws.recv(), timeout=5)

    asyncio.run(run())


def test_p2_5_dotted_device_id_still_accepted(kws_server):
    """SC-P2-5 回归：含 "." 的 device_id（如既有测试用的 pos-pos1.wav）
    必须通过——点不影响日志单行性，拒绝它会破坏既有协议。"""
    async def run():
        ws, first = await open_client(kws_server["url"], "pos-pos1.wav")
        assert first.get("type") == "ready"
        await ws.close()

    asyncio.run(run())


def test_p2_5_safe_device_id_logging():
    """SC-P2-5：日志用 device_id 单行化（去 \\r/\\n、截断 64 字符）。"""
    assert sidecar_server._safe_device_id("a\nb\rc") == "a?b?c"
    assert sidecar_server._safe_device_id("x" * 100) == "x" * 64
    assert sidecar_server._safe_device_id("dev-ok_1") == "dev-ok_1"


# ---------------- SC-P2-6：KWS_THRESHOLD / KWS_SCORE 范围校验 ----------------

def _run_server_cli(args, env_extra=None, timeout=60):
    cmd = [_server_python(),
           str(SIDECAR_DIR / "server.py"),
           "--host", "127.0.0.1", "--port", "10407",
           "--model-dir", str(MODEL_DIR),
           "--keywords-file", str(KEYWORDS_FILE),
           "--token", TEST_TOKEN] + args
    env = dict(os.environ)
    env.pop("KWS_THRESHOLD", None)
    env.pop("KWS_SCORE", None)
    if env_extra:
        env.update(env_extra)
    return subprocess.run(cmd, capture_output=True, text=True,
                          timeout=timeout, env=env)


@pytest.mark.parametrize("args,env_extra", [
    (["--threshold", "5.0"], None),
    (["--threshold", "0"], None),
    (["--threshold", "-0.5"], None),
    (["--score", "-1"], None),
    ([], {"KWS_THRESHOLD": "2"}),
    ([], {"KWS_SCORE": "-0.1"}),
])
def test_p2_6_out_of_range_threshold_score_exits_nonzero(args, env_extra):
    """SC-P2-6：阈值越出 (0, 1] / 分数 <0 → FATAL 非零退出，不静默跑。"""
    proc = _run_server_cli(args, env_extra)
    assert proc.returncode != 0
    combined = proc.stderr + proc.stdout
    assert "FATAL" in combined


def test_p2_6_valid_threshold_score_starts(tmp_path):
    """SC-P2-6：合法阈值/分数（0.25/1.0）正常启动、/health 200。"""
    log_path = tmp_path / "server.log"
    proc, log_file, _url, health = _start_server_subprocess(
        10408, log_path, ["--threshold", "0.25", "--score", "1.0"])
    try:
        with urllib.request.urlopen(health, timeout=5) as r:
            assert r.status == 200
    finally:
        proc.terminate()
        proc.wait(timeout=10)
        log_file.close()


# ---------------- SC-P2-7：wake_count 不再是死指标 ----------------

class _CountingSpotter:
    """第 3 次 get_result 才吐关键词的假 spotter。"""

    def __init__(self):
        self.calls = 0

    def create_stream(self):
        return types.SimpleNamespace(
            accept_waveform=lambda sr, samples: None)

    def is_ready(self, stream):
        return False

    def decode_stream(self, stream):
        pass

    def get_result(self, stream):
        self.calls += 1
        return KEYWORD if self.calls == 3 else ""

    def reset_stream(self, stream):
        pass


def test_p2_7_wake_count_and_frames_wired():
    """SC-P2-7：检出后 wake_count 自增、frames_at_last_wake 记下
    “上次检出后喂入的帧数”（第 3 帧检出 → 3 帧）。"""
    stream = sidecar_server.DeviceStream(_CountingSpotter(), "dev-count")
    for _ in range(2):
        assert stream.feed(np.zeros(320, dtype=np.int16)) is None
    assert stream.wake_count == 0
    assert stream.feed(np.zeros(320, dtype=np.int16)) == KEYWORD
    assert stream.wake_count == 1
    assert stream.frames_at_last_wake == 3


def test_p2_7_health_reports_device_wakes():
    """SC-P2-7：/health 含 device_wakes（每设备累计唤醒次数）。"""
    async def get_health(srv):
        resp = await srv.process_request(
            None, types.SimpleNamespace(path="/health"))
        return resp.status_code, json.loads(resp.body)

    srv = sidecar_server.Server(object(), keywords_count=1)
    srv.devices["dev-x"] = (types.SimpleNamespace(wake_count=3), None)
    status, body = asyncio.run(get_health(srv))
    assert status == 200
    assert body["device_wakes"] == {"dev-x": 3}
    assert body["total_wakes"] == 0  # 全局计数仍在 /health 里


# ---------------- SC-P2-8：Piper 语音文件 SHA 校验 ----------------

def test_p2_8_download_verifies_sha(tmp_path):
    """SC-P2-8：_download 带正确 SHA 时写入；SHA 不符时删文件重报。"""
    src = tmp_path / "voice.onnx"
    src.write_bytes(b"fake-voice-bytes")
    sha = hashlib.sha256(b"fake-voice-bytes").hexdigest()
    url = src.as_uri()
    dest = tmp_path / "dl" / "voice.onnx"
    bootstrap._download(url, dest, sha256=sha)
    assert dest.read_bytes() == b"fake-voice-bytes"
    bad = tmp_path / "dl2" / "voice.onnx"
    with pytest.raises(RuntimeError, match="SHA256"):
        bootstrap._download(url, bad, sha256="0" * 64)
    assert not bad.exists(), "corrupt download must be deleted, not kept"


def test_p2_8_corrupt_voice_file_redownloaded(tmp_path, monkeypatch):
    """SC-P2-8：已存在的语音文件 SHA 不符 → 删掉重下，不静默用坏文件。"""
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
    (tts_dir / "a.onnx.json").write_bytes(good_b)
    bootstrap._ensure_voice(root)
    assert (tts_dir / "a.onnx").read_bytes() == good_a
    assert (tts_dir / "a.onnx.json").read_bytes() == good_b


# ---------------- SC-P2-9：样本校验阈值与服务端同源 ----------------

def test_p2_9_thresholds_from_env(monkeypatch):
    """SC-P2-9：_verify_thresholds 读 KWS_THRESHOLD / KWS_SCORE，
    默认 0.25/1.0；非法值直接报错。"""
    monkeypatch.delenv("KWS_THRESHOLD", raising=False)
    monkeypatch.delenv("KWS_SCORE", raising=False)
    assert bootstrap._verify_thresholds() == (0.25, 1.0)
    monkeypatch.setenv("KWS_THRESHOLD", "0.3")
    monkeypatch.setenv("KWS_SCORE", "2.5")
    assert bootstrap._verify_thresholds() == (0.3, 2.5)
    monkeypatch.setenv("KWS_THRESHOLD", "2")
    with pytest.raises(RuntimeError, match="KWS_THRESHOLD"):
        bootstrap._verify_thresholds()
    monkeypatch.setenv("KWS_THRESHOLD", "0.25")
    monkeypatch.setenv("KWS_SCORE", "-1")
    with pytest.raises(RuntimeError, match="KWS_SCORE"):
        bootstrap._verify_thresholds()
    monkeypatch.setenv("KWS_SCORE", "abc")
    with pytest.raises(RuntimeError):
        bootstrap._verify_thresholds()


# ---------------- SC-P2-10：重抽前备份原样本 ----------------

def test_p2_10_reroll_backs_up_original(tmp_path):
    """SC-P2-10：_backup_existing 把原文件拷为 <name>.bak-<时间戳>；
    不存在的文件返回 None。"""
    path = tmp_path / "pos3.wav"
    path.write_bytes(b"original-sample")
    backup = bootstrap._backup_existing(path)
    assert backup is not None
    assert backup.exists()
    assert backup.read_bytes() == b"original-sample"
    assert re.search(r"\.bak-\d{8}-\d{6}$", backup.name), backup.name
    assert bootstrap._backup_existing(tmp_path / "missing.wav") is None


# ---------------- SC-P2-11：重抽计数是 10 次重抽（11 次检测） ----------------

def _copy_samples_to(tmp_path):
    samples_dir = tmp_path / "samples"
    samples_dir.mkdir()
    for name in [n for n, _, _ in bootstrap.SAMPLE_SPECS] + [bootstrap.NOISE_FILE]:
        shutil.copy(SAMPLES_DIR / name, samples_dir / name)
    return samples_dir


def test_p2_11_ten_rerolls_then_success(tmp_path, monkeypatch):
    """SC-P2-11：pos1 前 10 次检测失败 → 恰好重抽 10 次，第 11 次
    检测通过、不报错（旧代码 10 次检测只含 9 次重抽）。"""
    samples_dir = _copy_samples_to(tmp_path)
    calls = {"pos1.wav": 0}
    synth_calls = []

    def fake_detect(spotter, path):
        name = Path(path).name
        if name == "pos1.wav":
            calls["pos1.wav"] += 1
            return calls["pos1.wav"] > 10
        if name.startswith("pos"):
            return True
        return False

    monkeypatch.setattr(bootstrap, "_detect", fake_detect)
    monkeypatch.setattr(bootstrap, "_synthesize_sample",
                        lambda *a: synth_calls.append(a[5].name))
    monkeypatch.setattr(bootstrap, "_sample_toolchain",
                        lambda root: ("piper", "ffmpeg", Path("voice.onnx")))
    bootstrap._verify_samples(tmp_path, MODEL_DIR, KEYWORDS_FILE,
                              samples_dir)
    assert calls["pos1.wav"] == 11, \
        f"expected 11 detections, got {calls['pos1.wav']}"
    assert synth_calls.count("pos1.wav") == 10, \
        f"expected 10 re-rolls, got {synth_calls.count('pos1.wav')}"
    # 重抽前备份了原文件（SC-P2-10 的联动）
    assert list(samples_dir.glob("pos1.wav.bak-*")), \
        "re-roll must back up the original sample first"


def test_p2_11_eleven_failures_raise_after_ten_rerolls(tmp_path, monkeypatch):
    """SC-P2-11：pos1 始终失败 → 10 次重抽（11 次检测）后明确报错，
    报错写明重抽/检测次数。"""
    samples_dir = _copy_samples_to(tmp_path)
    synth_calls = []

    def fake_detect(spotter, path):
        name = Path(path).name
        if name == "pos1.wav":
            return False
        if name.startswith("pos"):
            return True
        return False

    monkeypatch.setattr(bootstrap, "_detect", fake_detect)
    monkeypatch.setattr(bootstrap, "_synthesize_sample",
                        lambda *a: synth_calls.append(a[5].name))
    monkeypatch.setattr(bootstrap, "_sample_toolchain",
                        lambda root: ("piper", "ffmpeg", Path("voice.onnx")))
    with pytest.raises(RuntimeError) as exc:
        bootstrap._verify_samples(tmp_path, MODEL_DIR, KEYWORDS_FILE,
                                  samples_dir)
    assert "10 re-rolls" in str(exc.value)
    assert "11 detections" in str(exc.value)
    assert synth_calls.count("pos1.wav") == 10


# ---------------- SC-P2-13：download-model.sh curl --fail ----------------

def test_p2_13_script_uses_fail_and_reports_size():
    """SC-P2-13：脚本 curl 带 --fail；SHA 失败报错里打印文件大小。"""
    script = SIDECAR_DIR / "scripts" / "download-model.sh"
    text = script.read_text(encoding="utf-8")
    assert "--fail" in text, "curl must use --fail so HTTP errors fail fast"
    assert "stat -c%s" in text, \
        "SHA failure message must report the downloaded file size"
    proc = subprocess.run(["bash", "-n", str(script)],
                          capture_output=True, text=True)
    assert proc.returncode == 0, proc.stderr


class _QuietHandler(http.server.SimpleHTTPRequestHandler):
    def log_message(self, *args):
        pass


def test_p2_13_curl_fail_flag_rejects_http_error(tmp_path):
    """SC-P2-13：实测 curl --fail 的语义——404 时 exit 22；不带 --fail
    时 exit 0（旧脚本的误导根源：错误页面被当成模型包写盘）。"""
    handler = lambda *a, **k: _QuietHandler(
        *a, directory=str(tmp_path), **k)
    httpd = http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler)
    port = httpd.server_address[1]
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    try:
        url = f"http://127.0.0.1:{port}/does-not-exist.tar.bz2"
        r_fail = subprocess.run(
            ["curl", "-sSLf", "--retry", "0", "-o", os.devnull, url],
            capture_output=True, text=True)
        assert r_fail.returncode == 22, \
            f"curl --fail on 404 must exit 22, got {r_fail.returncode}"
        r_nofail = subprocess.run(
            ["curl", "-sSL", "--retry", "0", "-o", os.devnull, url],
            capture_output=True, text=True)
        assert r_nofail.returncode == 0, \
            "curl without --fail exits 0 on 404 (the old misleading path)"
    finally:
        httpd.shutdown()
        httpd.server_close()
