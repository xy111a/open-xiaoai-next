"""kws-sidecar 测试固件：起一个真实 server 子进程（加载实验模型）。

模型与样本默认复用实验目录 ~/workspace/.kws-exp（已验证的
sherpa-onnx-kws-zipformer-wenetspeech-3.3M + Piper TTS 合成样本）；
缺失时由 tests/bootstrap.py 补齐（下载模型、生成样本），不静默
跳过——补不齐就明确报错。环境变量 KWS_TEST_ASSETS 可指向另一
资产根目录（CI 用，见 bootstrap.py）。
测试端口 10401，与正式端口 10400 区分开。
"""
import json
import os
import subprocess
import sys
import time
import urllib.request
from pathlib import Path

import pytest

from bootstrap import resolve_assets

SIDECAR_DIR = Path(__file__).resolve().parents[1]
# 补齐资产（失败时抛 RuntimeError，不跳过）。默认根目录已有全部
# 资产时这里只做存在性检查，不下载、不重新生成。
MODEL_DIR, KEYWORDS_FILE, SAMPLES_DIR = resolve_assets()

TEST_PORT = 10401
TEST_URL = f"ws://127.0.0.1:{TEST_PORT}/v1/kws"
HEALTH_URL = f"http://127.0.0.1:{TEST_PORT}/health"


def _server_python():
    venv_python = SIDECAR_DIR / ".venv" / "bin" / "python"
    return str(venv_python) if venv_python.exists() else sys.executable


def _wait_healthy(proc, timeout=90):
    deadline = time.time() + timeout
    while time.time() < deadline:
        if proc.poll() is not None:
            raise RuntimeError(
                f"server exited early with code {proc.returncode}")
        try:
            with urllib.request.urlopen(HEALTH_URL, timeout=2) as r:
                if r.status == 200:
                    body = json.loads(r.read())
                    assert body.get("model_loaded") is True
                    return
        except Exception:
            time.sleep(0.5)
    raise RuntimeError("server did not become healthy in time")


@pytest.fixture(scope="session")
def kws_server():
    """起 server 子进程（session 级，模型只加载一次），测试结束关闭。

    资产由模块导入时的 resolve_assets() 保证存在（缺失已补齐，
    补不齐会直接报错，不会走到这里）。
    """
    env = dict(os.environ)
    cmd = [
        _server_python(),
        str(SIDECAR_DIR / "server.py"),
        "--host", "127.0.0.1",
        "--port", str(TEST_PORT),
        "--model-dir", str(MODEL_DIR),
        "--keywords-file", str(KEYWORDS_FILE),
        "--log-level", "WARNING",
    ]
    proc = subprocess.Popen(cmd, env=env, stdout=subprocess.PIPE,
                            stderr=subprocess.STDOUT, text=True)
    try:
        _wait_healthy(proc)
        yield {"port": TEST_PORT, "url": TEST_URL, "health": HEALTH_URL,
               "proc": proc}
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait(timeout=5)
