"""kws-sidecar 测试资产自举（bootstrap）：缺什么补什么，补完再跑测试。

资产根目录：
- 默认沿用实验目录 ``~/workspace/.kws-exp``（本地已有全部资产时，
  只做存在性检查，一个字节也不重新下载，行为与以前完全一致）。
- 环境变量 ``KWS_TEST_ASSETS`` 指向另一根目录时（CI 用），在该目录
  内按同样结构补齐，测试不依赖本机实验目录。

补齐的三类资产（结构与 ``.kws-exp`` 相同）：
- 模型：``models/sherpa-onnx-kws-zipformer-wenetspeech-3.3M-2024-01-01/``
  （encoder int8 / decoder / joiner / tokens.txt），缺失时调
  ``scripts/download-model.sh`` 下载（SHA256 已 pin 在脚本里）；
  脚本同时把 ``keywords.txt`` 写进模型目录。
- 关键词表：优先 ``<root>/keywords.txt``，其次模型目录内的
  ``keywords.txt``；都没有时写入 2026-11-17 实验验证过的 token 行
  （与 ``download-model.sh`` 的 fallback 完全相同）。
- 样本：``samples/`` 内 11 个 wav（Piper TTS zh_CN-huayan-medium
  合成，16kHz 单声道 16-bit），缺失时现生成：语音缺失时从
  HuggingFace（rhasspy/piper-voices v1.0.0）下载，``piper`` 缺失或
  ``ffmpeg`` 缺失时直接报错（不自动装包）。

任何一步失败都抛 ``RuntimeError``（写清缺什么、从哪下、装什么），
不静默跳过、不空断言。资产文件不写进仓库。

样本校验（verify-and-reroll）：Piper 合成是非确定性的——同一文本、
同一语音、同一版本，两次生成是不同的音频；实测 pos3（“你好小李，
今天天气怎么样”）约 1/7 的抽样检不出（关键词本身的渲染没问题，
是逗号后 prosody/停顿的差异让 transducer 的关键词路径丢了；
前缀 1.3s 单独喂是能检出的）。所以生成后用离线 spotter 逐个校验
（与测试同样的流式喂法：20ms 块 + 1s trailing 静音；正样本必须
检出、负样本必须静默），不合格的正/负样本重抽，最多 10 次；10 次
仍不合格就明确报错（不硬凑、不跳过）。噪声样本是确定性的（固定
种子），校验不过直接报错，不重抽。
"""
import os
import shutil
import subprocess
import sys
import urllib.request
import wave
from pathlib import Path

import numpy as np

SIDECAR_DIR = Path(__file__).resolve().parents[1]
WORKSPACE = SIDECAR_DIR.parents[2]  # ~/workspace
DEFAULT_ROOT = WORKSPACE / ".kws-exp"

MODEL_DIR_NAME = "sherpa-onnx-kws-zipformer-wenetspeech-3.3M-2024-01-01"
REQUIRED_MODEL_FILES = [
    "encoder-epoch-12-avg-2-chunk-16-left-64.int8.onnx",
    "decoder-epoch-12-avg-2-chunk-16-left-64.onnx",
    "joiner-epoch-12-avg-2-chunk-16-left-64.onnx",
    "tokens.txt",
]

# 2026-11-17 实验验证过的“你好小李”token 行（与
# scripts/download-model.sh 的 fallback 完全相同；其它唤醒词没有
# 验证过的行，不瞎写——缺失时直接报错）。
VERIFIED_KEYWORDS_LINE = "n ǐ h ǎo x iǎo l ǐ @你好小李\n"

VOICE_BASE = ("https://huggingface.co/rhasspy/piper-voices/resolve/"
              "v1.0.0/zh/zh_CN/huayan/medium/zh_CN-huayan-medium")
# (文件名, 下载地址)
VOICE_FILES = [
    ("zh_CN-huayan-medium.onnx", f"{VOICE_BASE}.onnx"),
    ("zh_CN-huayan-medium.onnx.json", f"{VOICE_BASE}.onnx.json"),
]

# (文件名, 文本, length-scale；None = 默认 1.0)。
# 文本与变体照抄 ~/workspace/.kws-exp/run_kws_test.py 的 SAMPLES 表；
# 语速变体：piper 的 length-scale 是时长倍数，语速×1.18 → 1/1.18，
# 语速×0.8 → 1/0.8。
SAMPLE_SPECS = [
    ("pos1.wav", "你好小李", None),
    ("pos2.wav", "你好小李，打开客厅灯", None),
    ("pos3.wav", "你好小李，今天天气怎么样", None),
    ("pos4_fast.wav", "你好小李", 1 / 1.18),
    ("pos5_slow.wav", "你好小李", 1 / 0.8),
    ("neg1.wav", "你好", None),
    ("neg2.wav", "小李，你在吗", None),
    ("neg3.wav", "今天天气真好，我们去爬山吧", None),
    ("neg4.wav", "小爱同学，打开灯", None),
    ("neg5.wav", "你好小明，请帮我查一下明天的天气", None),
]
NOISE_FILE = "neg_noise2s.wav"
NOISE_SECONDS = 2
NOISE_STD = 5000  # 与实验样本 neg_noise2s.wav 实测 RMS≈5012 一致
NOISE_SEED = 20261117

# 样本校验：最多重抽次数；阈值与分数取边车默认值（README 的
# KWS_THRESHOLD / KWS_SCORE，实验验证值）。
MAX_GENERATION_ATTEMPTS = 10
VERIFY_THRESHOLD = 0.25
VERIFY_SCORE = 1.0
TRAILING_SILENCE_FRAMES = 50  # 1s 静音，与测试的 stream_frames 一致


def assets_root() -> Path:
    """资产根目录：KWS_TEST_ASSETS 覆盖，否则默认实验目录。"""
    override = os.environ.get("KWS_TEST_ASSETS")
    return Path(override) if override else DEFAULT_ROOT


def _missing_model_files(model_dir: Path) -> list:
    return [f for f in REQUIRED_MODEL_FILES
            if not (model_dir / f).is_file()]


def _ensure_model(root: Path) -> Path:
    model_dir = root / "models" / MODEL_DIR_NAME
    missing = _missing_model_files(model_dir)
    if not missing:
        return model_dir
    script = SIDECAR_DIR / "scripts" / "download-model.sh"
    print(f"[bootstrap] model incomplete in {model_dir} "
          f"(missing: {', '.join(missing)}); running {script} ...",
          flush=True)
    try:
        proc = subprocess.run(
            ["bash", str(script), str(model_dir)],
            capture_output=True, text=True, timeout=600)
    except subprocess.TimeoutExpired as e:
        raise RuntimeError(
            "bootstrap: download-model.sh timed out after 600s; "
            f"model dir: {model_dir}") from e
    if proc.returncode != 0:
        raise RuntimeError(
            "bootstrap: download-model.sh failed "
            f"(exit {proc.returncode}); model dir: {model_dir}; "
            "check network access to github.com/k2-fsa/sherpa-onnx "
            "releases and the pinned SHA256 in scripts/download-model.sh.\n"
            f"--- script output (tail) ---\n"
            f"{(proc.stdout + proc.stderr)[-2000:]}")
    missing = _missing_model_files(model_dir)
    if missing:
        raise RuntimeError(
            "bootstrap: model still incomplete after download "
            f"(missing: {', '.join(missing)}); model dir: {model_dir}")
    return model_dir


def _ensure_keywords(root: Path, model_dir: Path) -> Path:
    for candidate in (root / "keywords.txt", model_dir / "keywords.txt"):
        if candidate.is_file():
            return candidate
    target = root / "keywords.txt"
    print(f"[bootstrap] keywords.txt missing; writing the verified "
          f"token line for 你好小李 to {target} ...", flush=True)
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(VERIFIED_KEYWORDS_LINE, encoding="utf-8")
    return target


def _download(url: str, dest: Path) -> None:
    dest.parent.mkdir(parents=True, exist_ok=True)
    print(f"[bootstrap] downloading {url} -> {dest} ...", flush=True)
    try:
        with urllib.request.urlopen(url, timeout=120) as r, \
                open(dest, "wb") as f:
            shutil.copyfileobj(r, f)
    except Exception as e:
        if dest.exists():
            dest.unlink()
        raise RuntimeError(
            f"bootstrap: failed to download {url}: {e}; "
            "check network access to huggingface.co") from e


def _ensure_voice(root: Path) -> Path:
    tts_dir = root / "tts"
    voice_onnx = tts_dir / VOICE_FILES[0][0]
    if all((tts_dir / name).is_file() for name, _ in VOICE_FILES):
        return voice_onnx
    for name, url in VOICE_FILES:
        dest = tts_dir / name
        if not dest.is_file():
            _download(url, dest)
    return voice_onnx


def _which_or_fail(name: str, install_hint: str) -> str:
    path = shutil.which(name)
    if path is None and name == "piper":
        # pip 装进当前测试 venv 时，piper 脚本与 python 在同一 bin
        # 目录；PATH 未必包含它（CI 里 setup-python 会加 PATH，
        # 本地 venv 不一定），兜底找 sys.executable 的同级目录。
        candidate = Path(sys.executable).parent / "piper"
        if candidate.is_file() and os.access(candidate, os.X_OK):
            path = str(candidate)
    if path is None:
        raise RuntimeError(
            f"bootstrap: required tool '{name}' not found on PATH; "
            f"{install_hint}")
    return path


def _synthesize_sample(piper: str, ffmpeg: str, voice_onnx: Path,
                       text: str, length_scale, out_path: Path) -> None:
    """piper 合成（22050Hz）→ ffmpeg 重采样到 16kHz 单声道 16-bit。"""
    import tempfile
    with tempfile.TemporaryDirectory() as tmp:
        raw = Path(tmp) / "raw.wav"
        cmd = [piper, "--model", str(voice_onnx),
               "--output_file", str(raw)]
        if length_scale is not None:
            cmd += ["--length-scale", f"{length_scale:.3f}"]
        proc = subprocess.run(cmd, input=text.encode("utf-8"),
                              capture_output=True, timeout=300)
        if proc.returncode != 0 or not raw.is_file():
            raise RuntimeError(
                "bootstrap: piper failed to synthesize "
                f"'{text}' (exit {proc.returncode}); "
                f"stderr: {proc.stderr.decode('utf-8', 'replace')[-1000:]}")
        proc = subprocess.run(
            [ffmpeg, "-y", "-v", "error", "-i", str(raw),
             "-ar", "16000", "-ac", "1", "-sample_fmt", "s16",
             str(out_path)],
            capture_output=True, text=True, timeout=120)
        if proc.returncode != 0 or not out_path.is_file():
            raise RuntimeError(
                "bootstrap: ffmpeg failed to resample to 16kHz for "
                f"{out_path.name} (exit {proc.returncode}); "
                f"stderr: {proc.stderr[-1000:]}")


def _write_noise(out_path: Path) -> None:
    rng = np.random.default_rng(NOISE_SEED)
    data = np.clip(rng.normal(0, NOISE_STD, NOISE_SECONDS * 16000),
                   -32768, 32767).astype(np.int16)
    with wave.open(str(out_path), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(16000)
        w.writeframes(data.tobytes())


def _sample_toolchain(root: Path):
    """生成样本的工具链：(piper, ffmpeg, voice_onnx)。"""
    piper = _which_or_fail(
        "piper", "install it first: pip install piper-tts")
    ffmpeg = _which_or_fail(
        "ffmpeg", "install it first: apt-get install -y ffmpeg")
    voice_onnx = _ensure_voice(root)
    return piper, ffmpeg, voice_onnx


def _ensure_samples(root: Path) -> Path:
    samples_dir = root / "samples"
    samples_dir.mkdir(parents=True, exist_ok=True)
    wanted = [name for name, _, _ in SAMPLE_SPECS] + [NOISE_FILE]
    missing = [name for name in wanted
               if not (samples_dir / name).is_file()]
    if not missing:
        return samples_dir
    print(f"[bootstrap] samples missing in {samples_dir}: "
          f"{', '.join(missing)}; generating ...", flush=True)
    piper, ffmpeg, voice_onnx = _sample_toolchain(root)
    _generate_missing(samples_dir, missing, piper, ffmpeg, voice_onnx)
    still = [name for name in wanted
             if not (samples_dir / name).is_file()]
    if still:
        raise RuntimeError(
            "bootstrap: samples still missing after generation: "
            f"{', '.join(still)}; samples dir: {samples_dir}")
    return samples_dir


def _generate_missing(samples_dir: Path, missing, piper, ffmpeg,
                      voice_onnx: Path) -> None:
    for name, text, length_scale in SAMPLE_SPECS:
        if name in missing:
            _synthesize_sample(piper, ffmpeg, voice_onnx, text,
                               length_scale, samples_dir / name)
    if NOISE_FILE in missing:
        _write_noise(samples_dir / NOISE_FILE)


def _detect(spotter, path: Path) -> bool:
    """离线 spotter 流式检一次：20ms 块 + 1s trailing 静音。

    与 tests/test_kws_sidecar.py 的 stream_frames / wait_for_wake
    同样的喂法（server 端是同一模型、同一阈值）。
    """
    with wave.open(str(path)) as w:
        data = np.frombuffer(w.readframes(w.getnframes()), dtype=np.int16)
    stream = spotter.create_stream()
    for i in range(0, len(data), 320):
        stream.accept_waveform(
            16000, data[i:i + 320].astype(np.float32) / 32768.0)
        while spotter.is_ready(stream):
            spotter.decode_stream(stream)
        if spotter.get_result(stream):
            return True
    for _ in range(TRAILING_SILENCE_FRAMES):
        stream.accept_waveform(16000, np.zeros(320, dtype=np.float32))
        while spotter.is_ready(stream):
            spotter.decode_stream(stream)
        if spotter.get_result(stream):
            return True
    return False


def _verify_samples(root: Path, model_dir: Path, keywords_file: Path,
                    samples_dir: Path) -> None:
    """逐个校验样本：正样本必须检出、负样本必须静默。

    不合格的可重抽样本（Piper 合成）重抽，最多 MAX_GENERATION_ATTEMPTS
    次；噪声样本是确定性的，校验不过直接报错。
    """
    try:
        import sherpa_onnx
    except ImportError as e:
        raise RuntimeError(
            "bootstrap: cannot verify samples, sherpa-onnx is not "
            "installed; install it first: "
            "pip install -r requirements.txt") from e
    spotter = sherpa_onnx.KeywordSpotter(
        tokens=str(model_dir / "tokens.txt"),
        encoder=str(model_dir / REQUIRED_MODEL_FILES[0]),
        decoder=str(model_dir / REQUIRED_MODEL_FILES[1]),
        joiner=str(model_dir / REQUIRED_MODEL_FILES[2]),
        keywords_file=str(keywords_file),
        keywords_threshold=VERIFY_THRESHOLD,
        keywords_score=VERIFY_SCORE, num_threads=1)
    pos = {name for name, _, _ in SAMPLE_SPECS if name.startswith("pos")}
    neg = {name for name, _, _ in SAMPLE_SPECS if name.startswith("neg")}
    neg.add(NOISE_FILE)
    piper = ffmpeg = voice_onnx = None  # 懒初始化，只在需要重抽时取
    for name in sorted(pos | neg):
        expect_wake = name in pos
        path = samples_dir / name
        attempts = 0
        while True:
            attempts += 1
            ok = _detect(spotter, path) == expect_wake
            if ok:
                break
            if name == NOISE_FILE:
                raise RuntimeError(
                    "bootstrap: deterministic noise sample "
                    f"{NOISE_FILE} failed verification "
                    f"(expected silent, got wake); this is a model/"
                    "threshold issue, not a bad draw — do not ignore")
            if attempts >= MAX_GENERATION_ATTEMPTS:
                raise RuntimeError(
                    "bootstrap: sample "
                    f"{name} failed verification {attempts} times "
                    f"(expected {'wake' if expect_wake else 'silent'}); "
                    "the model is marginal on this sentence — "
                    "do not silently pass, investigate before "
                    "changing the sample text")
            if piper is None:
                piper, ffmpeg, voice_onnx = _sample_toolchain(root)
            print(f"[bootstrap] sample {name} failed verification; "
                  f"re-rolling (attempt {attempts + 1}/"
                  f"{MAX_GENERATION_ATTEMPTS}) ...", flush=True)
            spec = next(s for s in SAMPLE_SPECS if s[0] == name)
            _synthesize_sample(piper, ffmpeg, voice_onnx, spec[1],
                               spec[2], path)


def resolve_assets():
    """补齐并返回 (model_dir, keywords_file, samples_dir)。

    失败时抛 RuntimeError（不跳过、不空断言）。样本生成后逐个离线
    校验（正样本必须检出、负样本必须静默），不合格的重抽。
    """
    root = assets_root()
    model_dir = _ensure_model(root)
    keywords_file = _ensure_keywords(root, model_dir)
    samples_dir = _ensure_samples(root)
    _verify_samples(root, model_dir, keywords_file, samples_dir)
    return model_dir, keywords_file, samples_dir
