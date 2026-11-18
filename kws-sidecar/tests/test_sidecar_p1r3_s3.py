"""kws-sidecar 第三轮审计 P1 修复轮回归测试（S-3）。

S-3：只做不依赖 bai 决定的部分——KWS_HOST 默认改回环 + 安全假设
进文档；TLS 实现本身未做（等 bai 在“接受回环假设”与“给边车加
TLS”之间二选一），本文件不含任何 TLS 测试。

⚠️ 合成语音测试，非真人唤醒率（沿用本目录既有说明）。
"""
import asyncio
import json
import os
import re
import socket
import sys
from pathlib import Path

import pytest
import websockets

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import server as sidecar_server

from conftest import SIDECAR_DIR, TEST_TOKEN
from test_sidecar_p2 import _start_server_subprocess

DOCKERFILE = SIDECAR_DIR / "Dockerfile"
README = SIDECAR_DIR / "README.md"

# ---------------- S-3：默认只绑回环 + 安全假设进文档 ----------------

def test_p1r3_s3_host_default_is_loopback(monkeypatch):
    """S-3(1)：KWS_HOST 未设时 --host 默认 127.0.0.1，不再是 0.0.0.0。"""
    monkeypatch.delenv("KWS_HOST", raising=False)
    args = sidecar_server.build_arg_parser().parse_args([])
    assert args.host == "127.0.0.1"


def test_p1r3_s3_host_env_and_cli_override_still_work(monkeypatch):
    """S-3(1)：显式配置仍有效——KWS_HOST 环境变量与 --host 命令行
    都能覆盖默认（需局域网暴露时显式改，不静默改不回去）。"""
    monkeypatch.setenv("KWS_HOST", "0.0.0.0")
    args = sidecar_server.build_arg_parser().parse_args([])
    assert args.host == "0.0.0.0"
    args = sidecar_server.build_arg_parser().parse_args(
        ["--host", "127.0.0.1"])
    assert args.host == "127.0.0.1"


def test_p1r3_s3_dockerfile_default_is_loopback():
    """S-3(1)：Dockerfile 的 KWS_HOST 默认值也是 127.0.0.1，
    与 server.py 一致；0.0.0.0 不再作为默认出现。"""
    text = DOCKERFILE.read_text(encoding="utf-8")
    assert "KWS_HOST=127.0.0.1" in text
    assert "KWS_HOST=0.0.0.0" not in text


def test_p1r3_s3_no_tls_in_server_and_assumption_documented():
    """S-3(2)：server.py 全文仍无 TLS/证书逻辑（TLS 实现未做，等
    bai 定夺），且“边车不启用 TLS、依赖 127.0.0.1 绑定”已写为正式
    安全假设进 README（含风险边界与失效条件）。"""
    text = (SIDECAR_DIR / "server.py").read_text(encoding="utf-8")
    assert not re.search(r"(?i)\bssl\b", text), \
        "server.py must contain no TLS logic yet (S-3 decision pending)"
    readme = README.read_text(encoding="utf-8")
    assert "不启用 TLS" in readme
    assert "127.0.0.1" in readme
    assert "待 bai 定夺" in readme or "待定决定" in readme
    assert "本机进程" in readme, \
        "assumption must state the risk boundary (any local process)"
    assert "| `KWS_HOST` | `127.0.0.1` |" in readme


def _listening_addrs(port):
    """从 /proc/net/tcp 读本机处于 LISTEN 的 socket（IP:port 集合）；
    不依赖 ss，避免沙箱网络对“连 LAN IP”这类主动探测的干扰。"""
    addrs = set()
    with open("/proc/net/tcp", encoding="utf-8") as f:
        next(f)  # 表头
        for line in f:
            parts = line.split()
            local, state = parts[1], parts[3]
            if state != "0A":  # 0A = LISTEN
                continue
            ip_hex, port_hex = local.split(":")
            if int(port_hex, 16) != port:
                continue
            ip = socket.inet_ntoa(bytes.fromhex(ip_hex)[::-1])
            addrs.add(ip)
    return addrs


def test_p1r3_s3_server_binds_loopback_only(tmp_path):
    """S-3(1)：起来的 server 只在 127.0.0.1:10409 上 LISTEN，
    0.0.0.0 上没有同端口监听（回环绑定的实测；不用“连 LAN IP”
    探测——沙箱网关会代答 TCP，结论不可靠）。"""
    log_path = tmp_path / "server.log"
    proc, log_file, url, health = _start_server_subprocess(10409, log_path)
    try:
        addrs = _listening_addrs(10409)
        assert "127.0.0.1" in addrs, \
            f"server must listen on 127.0.0.1:10409, got {addrs}"
        assert "0.0.0.0" not in addrs, \
            f"server must NOT listen on 0.0.0.0:10409, got {addrs}"
        # 回环上的 /health 与 WS 本身是通的（helper 已等到 200 才返回，
        # 这里再确认一次 WS 握手能走通，排除“监听在但服务是死的”）。
        async def hello_ok():
            ws = await websockets.connect(url, max_size=1 << 20)
            await ws.send(json.dumps(
                {"type": "hello", "device_id": "loopback-check",
                 "sample_rate": 16000, "token": TEST_TOKEN}))
            first = json.loads(
                await asyncio.wait_for(ws.recv(), timeout=5))
            await ws.close()
            return first.get("type")

        assert asyncio.run(hello_ok()) == "ready"
    finally:
        proc.terminate()
        proc.wait(timeout=10)
        log_file.close()
