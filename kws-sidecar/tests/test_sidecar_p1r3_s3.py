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
import subprocess
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
    """S-3(1)：显式配置在解析层仍有效——KWS_HOST 环境变量与 --host
    命令行都能覆盖默认。注意：解析通过不等于能启动，2026-11-18 bai
    定之后非回环地址在 amain() 里启动即拒绝（见本文件末尾的守门
    测试）；本测试只钉解析行为。"""
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
    """S-3(2)：server.py 全文仍无 TLS/证书逻辑（2026-11-18 bai 定接受
    回环 + token、不加 TLS），且“边车不启用 TLS、依赖 127.0.0.1 绑定”
    已写为正式安全假设进 README（含风险边界与失效条件），状态已决，
    非回环地址启动即拒绝。"""
    text = (SIDECAR_DIR / "server.py").read_text(encoding="utf-8")
    assert not re.search(r"(?i)\bssl\b", text), \
        "server.py must contain no TLS logic yet (S-3 decided: loopback + token, no TLS)"
    readme = README.read_text(encoding="utf-8")
    assert "不启用 TLS" in readme
    assert "127.0.0.1" in readme
    assert "已决" in readme, \
        "README security assumption must record the 2026-11-18 decision"
    assert "待 bai 定夺" not in readme and "待定决定" not in readme, \
        "README must no longer say the S-3 decision is pending"
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

# ---------------- S-3 已决（2026-11-18）：非回环启动即拒绝 ----------------

def test_decisions6_s3_loopback_guard_accepts_loopback():
    """S-3 已决：回环地址放行——127.0.0.0/8、::1 与 localhost。"""
    is_loopback = sidecar_server._is_loopback_host
    assert is_loopback("127.0.0.1")
    assert is_loopback("127.0.0.2")
    assert is_loopback("127.255.255.255")
    assert is_loopback("::1")
    assert is_loopback("localhost")


def test_decisions6_s3_loopback_guard_refuses_non_loopback():
    """S-3 已决：非回环拒绝——0.0.0.0、局域网 IP、未指定地址、空串、
    解析失败的主机名。"""
    is_loopback = sidecar_server._is_loopback_host
    assert not is_loopback("0.0.0.0")
    assert not is_loopback("192.168.1.10")
    assert not is_loopback("10.0.0.2")
    assert not is_loopback("::")
    assert not is_loopback("")
    assert not is_loopback("nonexistent.invalid")


def _run_server_with_host(host, port):
    """以给定 --host 起 server 子进程（不加载模型：守门在 Token/
    模型加载之前，配错地址应秒退）。返回 CompletedProcess。"""
    env = dict(os.environ)
    env.pop("KWS_HOST", None)
    return subprocess.run(
        [sys.executable, str(SIDECAR_DIR / "server.py"),
         "--host", host, "--port", str(port)],
        capture_output=True, text=True, timeout=60, env=env)


def test_decisions6_s3_non_loopback_host_refuses_startup():
    """S-3 已决：--host 0.0.0.0 启动即非零退出，stderr 点名回环要求；
    且守门在 Token 校验之前（不配 Token 也应先报回环错，不是 Token 错）。"""
    proc = _run_server_with_host("0.0.0.0", 10499)
    assert proc.returncode == 1, \
        f"non-loopback host must exit 1, got {proc.returncode}: {proc.stderr}"
    assert "FATAL" in proc.stderr
    assert "loopback" in proc.stderr
    assert "0.0.0.0" in proc.stderr


def test_decisions6_s3_loopback_host_passes_guard_to_token_check():
    """S-3 已决：--host 127.0.0.1 能过守门，接着才报 Token 未配
    （证明守门放行后流程继续走到下一道校验，不是笼统拒绝一切）。"""
    proc = _run_server_with_host("127.0.0.1", 10499)
    assert proc.returncode == 1
    assert "KWS token" in proc.stderr, \
        f"loopback host must pass the guard and fail on token: {proc.stderr}"
    assert "loopback" not in proc.stderr
