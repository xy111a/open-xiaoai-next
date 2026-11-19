"""kws-sidecar 第三轮审计 P1 修复轮回归测试（S-2）。

S-2：全新卷直接 `compose up` 不 crash-loop——Dockerfile 建目录 +
改属主（copy-up）、entrypoint 下载前可写预检、compose 注释纠正、
NAS_CHECKLIST.md §8 冒烟项。

⚠️ 合成语音测试，非真人唤醒率（沿用本目录既有说明）。
本机无 docker：真实 `docker compose up` 全新卷实测做不了，记
BLOCKED（留待 NAS 首次部署时按 NAS_CHECKLIST.md §8 验证）；这里
只验证 Dockerfile / entrypoint / 文档接线。
"""
import os
import shutil
import subprocess
import sys
from pathlib import Path

import pytest

from conftest import SIDECAR_DIR

REPO_DIR = SIDECAR_DIR.parent
COMPOSE = REPO_DIR / "deploy" / "docker-compose.yml"
CHECKLIST = REPO_DIR / "deploy" / "NAS_CHECKLIST.md"
ENTRYPOINT = SIDECAR_DIR / "entrypoint.sh"
DOCKERFILE = SIDECAR_DIR / "Dockerfile"


def _write_regen_stub(tmp_path):
    """写一个 regen 替身脚本（只 exit 0），供 entrypoint 测试用。
    entrypoint 的 happy-path 测试只验证“走到 exec”，不验证 regen 本身；
    真 regen 脚本在 CI 里会走 sherpa-onnx-cli 分支，而测试用的空
    tokens.txt 会让 text2token 失败——所以这里用替身隔离。"""
    stub = tmp_path / "regen-stub.sh"
    stub.write_text("#!/bin/bash\n# stub: simulate successful keywords.txt regeneration\nexit 0\n",
                    encoding="utf-8")
    stub.chmod(0o755)
    return stub

REQUIRED_MODEL_FILES = (
    "encoder-epoch-12-avg-2-chunk-16-left-64.int8.onnx",
    "decoder-epoch-12-avg-2-chunk-16-left-64.onnx",
    "joiner-epoch-12-avg-2-chunk-16-left-64.onnx",
    "tokens.txt",
    "keywords.txt",
)


# ---------------- S-2：Dockerfile 建目录 + 改属主 ----------------

def test_p1r3_s2_dockerfile_creates_model_dir_with_correct_owner():
    """S-2(a)：Dockerfile 有 mkdir + chown 1000:1000，且在 VOLUME
    声明之前（VOLUME 之后再改目录内容会被丢弃，顺序错了等于没修）。"""
    text = DOCKERFILE.read_text(encoding="utf-8")
    assert "RUN mkdir -p /data/kws/models && chown 1000:1000 /data/kws/models" \
        in text, "Dockerfile must mkdir + chown the model dir (S-2)"
    mkdir_at = text.index("mkdir -p /data/kws/models")
    volume_at = text.index("VOLUME /data/kws/models")
    assert mkdir_at < volume_at, \
        "mkdir + chown must come BEFORE the VOLUME declaration"


def test_p1r3_s2_compose_comment_matches_dockerfile():
    """S-2：compose 注释不再把卷属主全推给 nas-install.sh，写明
    新卷属主来自 Dockerfile 的 mkdir + chown（copy-up），并指向
    NAS_CHECKLIST.md §8 的冒烟项；旧的失实说法已删掉。"""
    text = COMPOSE.read_text(encoding="utf-8")
    assert "mkdir -p /data/kws/models && chown 1000:1000" in text, \
        "compose comment must name the Dockerfile mkdir + chown (S-2)"
    assert "copy-up" in text
    assert "NAS_CHECKLIST.md §8" in text
    assert "模型卷属主由 nas-install.sh §4 的 chown one-shot 负责" not in text, \
        "stale claim (nas-install.sh as the sole owner mechanism) must go"


def test_p1r3_s2_checklist_has_fresh_volume_smoke_item():
    """S-2：NAS_CHECKLIST.md 有“不用 nas-install.sh、只 compose up
    全新卷”的冒烟项（§8），且明确只删 kws-data 卷、不许 `down -v`。"""
    text = CHECKLIST.read_text(encoding="utf-8")
    assert "## 8. S-2 冒烟" in text
    assert "docker volume rm" in text
    assert "down -v" in text


def _run_entrypoint_copy(tmp_path, model_dir, extra_env=None):
    """拷一份 entrypoint.sh 到 tmp 后跑（不改仓库里的原文件）；
    返回 CompletedProcess。只传最小环境，避免宿主环境变量干扰。"""
    copy = tmp_path / "entrypoint.sh"
    shutil.copy(ENTRYPOINT, copy)
    env = {"PATH": os.environ["PATH"], "KWS_MODEL_DIR": str(model_dir),
           "REGEN_KEYWORDS_SCRIPT": str(_write_regen_stub(tmp_path))}
    if extra_env:
        env.update(extra_env)
    return subprocess.run(["bash", str(copy)], capture_output=True,
                          text=True, timeout=30, env=env)


def test_p1r3_s2_entrypoint_syntax_ok():
    """S-2：entrypoint.sh 语法检查通过。"""
    proc = subprocess.run(["bash", "-n", str(ENTRYPOINT)],
                          capture_output=True, text=True)
    assert proc.returncode == 0, proc.stderr


def test_p1r3_s2_entrypoint_precheck_fails_fast_on_bad_model_dir(tmp_path):
    """S-2(b)：模型目录根本不是目录（如被写成文件）时，entrypoint
    在下载前就 FATAL 退出，不走到 download-model.sh（旧行为会在
    脚本深处的 cp 上失败，报错难读）。"""
    not_a_dir = tmp_path / "models"
    not_a_dir.write_bytes(b"not a directory")
    proc = _run_entrypoint_copy(tmp_path, not_a_dir)
    assert proc.returncode != 0
    combined = proc.stdout + proc.stderr
    assert "FATAL" in combined
    assert str(not_a_dir) in combined
    assert "downloading" not in combined, \
        "precheck must fire before any download attempt"


@pytest.mark.skipif(os.geteuid() == 0,
                    reason="root bypasses file permissions; -w is always "
                           "true, the unwritable-dir path cannot be "
                           "exercised as root")
def test_p1r3_s2_entrypoint_precheck_fails_on_unwritable_model_dir(tmp_path):
    """S-2(b)：模型目录不可写（全新卷仍是 root 属主时 uid 1000 看到
    的样子）时，entrypoint 给可读 FATAL（含修复命令）再退出。"""
    model_dir = tmp_path / "models"
    model_dir.mkdir()
    os.chmod(model_dir, 0o555)
    try:
        proc = _run_entrypoint_copy(tmp_path, model_dir)
    finally:
        os.chmod(model_dir, 0o755)
    assert proc.returncode != 0
    combined = proc.stdout + proc.stderr
    assert "FATAL" in combined
    assert "is not writable" in combined
    assert "chown" in combined, \
        "FATAL must tell the operator how to fix the volume ownership"
    assert "downloading" not in combined


def test_p1r3_s2_entrypoint_happy_path_reaches_exec(tmp_path):
    """S-2：模型齐全且目录可写时，预检不拦路，entrypoint 走到启动
    （exec 行用 echo 替身验证，不真起 server.py——/app 路径只在
    容器里存在）。"""
    model_dir = tmp_path / "models"
    model_dir.mkdir()
    for name in REQUIRED_MODEL_FILES:
        (model_dir / name).write_bytes(b"")
    copy = tmp_path / "entrypoint.sh"
    shutil.copy(ENTRYPOINT, copy)
    text = copy.read_text(encoding="utf-8")
    assert 'exec python /app/server.py "$@"' in text
    copy.write_text(text.replace(
        'exec python /app/server.py "$@"',
        'echo ENTRYPOINT-EXEC-REACHED "$@"'))
    env = {"PATH": os.environ["PATH"], "KWS_MODEL_DIR": str(model_dir),
           "REGEN_KEYWORDS_SCRIPT": str(_write_regen_stub(tmp_path))}
    proc = subprocess.run(["bash", str(copy), "--help"],
                          capture_output=True, text=True, timeout=30,
                          env=env)
    assert proc.returncode == 0, proc.stdout + proc.stderr
    assert "KWS model ready" in proc.stdout + proc.stderr
    assert "ENTRYPOINT-EXEC-REACHED --help" in proc.stdout


