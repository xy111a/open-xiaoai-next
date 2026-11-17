#!/usr/bin/env python3
"""下载 LX06 / OH2P 官方 OTA 包（不用小米账号登录）。

原理（2026-11-16 在本机验证通过）：小米灰度升级接口的签名算法是公开的——
把 otaInfo 字符串 base64 后取 MD5 当 s 参数即可，不需要 MI_DID / MI_USER / MI_TOKEN。
  otaInfo = channel={channel}&filterID={sn}&locale=zh_CN&model={model}&time={time_ms}&version={version}&8007236f-a2d6-4847-ac83-c49395ad6d65
  s = md5(base64(otaInfo))
  URL = https://api.miwifi.com/rs/grayupgrade/v2/{model}?model=...&version=...&channel=...&filterID=...&locale=zh_CN&time={time_ms}&s={s}

注意 time 是毫秒时间戳（ota.ts 用 Date.now()；REA_ANALYSIS.md 记为 time=<ms>）。

信任链（P0-5 修复，2026-11-17）：
  1. API 走 HTTPS（2026-11-17 实测 api.miwifi.com 支持 HTTPS，证书校验通过，
     错误签名返回 HTTP 401，说明 TLS 层可达；之前是明文 HTTP）。
  2. 白名单 EXPECTED 是强制门：不在白名单的 (model, version) 在发任何网络
     请求之前就拒绝下载；新增版本必须先人工核实 SHA256 再更新白名单。
  3. SHA256 是权威校验（MD5 只作兼容参考）：下载后对文件算 SHA256，与白名单
     比对；服务端同一条响应里给的 MD5/大小只作预检，不作放行依据。
  4. 下载链接强制白名单 host + https scheme，拒绝 http://、file:// 等。
  5. 流式下载带超时，先写 .tmp，校验通过再 rename，失败删临时文件。

用法：
  python3 download-ota.py lx06 1.94.13    # 下载到 work/lx06_1.94.13.bin 并校验 SHA256
  python3 download-ota.py oh2p 1.58.6
  python3 download-ota.py verify-bin lx06 1.94.13 work/lx06_1.94.13.bin
  python3 download-ota.py verify-sqfs lx06 1.94.13 work/lx06_1.94.13/root.squashfs

已验证的包（2026-11-16 下载、2026-11-17 复核 SHA256）：
  lx06 1.94.13：43,478,328 字节，MD5 b202de2bbe149ce884aa500911151b95，
                 SHA256 a56a38619fd98e467adb8c998757f80109c4dac89db48d62dae985e3062476ed
  oh2p 1.58.6：36,042,824 字节，MD5 eb86c0c12ea118fb701433bf009a180c，
                 SHA256 1f351d879c06ff3bd7bd4b4c68e2c8fa45eee1a82be5839e57899ae8f9b7c27b
"""
import base64
import hashlib
import json
import os
import sys
import time
import urllib.parse
import urllib.request
from pathlib import Path

SECRET_SUFFIX = "8007236f-a2d6-4847-ac83-c49395ad6d65"
API = "https://api.miwifi.com/rs/grayupgrade/v2/{model}"

# 白名单：(model, version) -> 官方包指纹。唯一可信锚点；新增版本必须先
# 人工核实 SHA256（从小米官方渠道交叉确认）再加进来，绝不凭服务端返回值写。
EXPECTED = {
    ("lx06", "1.94.13"): {
        "sha256": "a56a38619fd98e467adb8c998757f80109c4dac89db48d62dae985e3062476ed",
        "md5": "b202de2bbe149ce884aa500911151b95",
        "size": 43478328,
    },
    ("oh2p", "1.58.6"): {
        "sha256": "1f351d879c06ff3bd7bd4b4c68e2c8fa45eee1a82be5839e57899ae8f9b7c27b",
        "md5": "eb86c0c12ea118fb701433bf009a180c",
        "size": 36042824,
    },
}

# 从已验证官方包解出的原始 root.squashfs（unpack.sh 产物核对用）。
# 来源：work/<型号>_<版本>/root.squashfs，与 *-extract/ 下 carve 出的同名文件
# SHA256 一致（2026-11-17 核对）；patch.sh 在找不到 .bin 时退而校验它。
EXPECTED_SQFS = {
    ("lx06", "1.94.13"): {
        "sha256": "317a54909f12b63f22128965bf9005c06435d15f61c4202bb88c4524bf1e2ef4",
        "size": 37500699,
    },
    ("oh2p", "1.58.6"): {
        "sha256": "c33dfc21a2a3093b6c4e41f3bf0738e1ff21daf59bbccc5203863bdb62dfa493",
        "size": 29953626,
    },
}

# 下载链接只允许这个 host（2026-11-17 实测 api.miwifi.com 返回的 link host，
# 与 changelogUrl 同域）。新增 CDN host 必须先人工确认是小米官方域名再加。
ALLOWED_LINK_HOSTS = frozenset({"cdn.cnbj1.fds.api.mi-img.com"})

CHUNK_SIZE = 1024 * 1024  # 流式下载分块：1MB


def expected(model: str, version: str) -> dict:
    """取白名单条目；不在白名单即拒绝（强制门，发网络请求之前调用）。"""
    try:
        return EXPECTED[(model, version)]
    except KeyError:
        raise ValueError(
            f"不在白名单：{model} {version}（只允许 "
            + "、".join(f"{m} {v}" for m, v in sorted(EXPECTED))
            + "；新版本需人工核实 SHA256 后更新 EXPECTED 再下载）")


def validate_link(link: str) -> str:
    """下载链接强制 https + 白名单 host；否则拒绝。返回原链接。"""
    u = urllib.parse.urlparse(link)
    if u.scheme.lower() != "https":
        raise ValueError(f"拒绝非 https 下载链接：{link!r}")
    host = (u.hostname or "").lower()
    if host not in ALLOWED_LINK_HOSTS:
        raise ValueError(
            f"拒绝非白名单 host 的下载链接：{link!r}（只允许 {sorted(ALLOWED_LINK_HOSTS)}）")
    return link


def build_ota_url(model: str, version: str, channel: str = "release",
                  sn: str = "", now_ms: int | None = None) -> dict:
    """按 REA_ANALYSIS.md 记录的算法算出 otaInfo、s 与完整 URL（纯函数，可测）。

    now_ms 为毫秒时间戳；不传时取当前时间（time.time() * 1000）。
    """
    if now_ms is None:
        now_ms = int(time.time() * 1000)
    ota_info = (
        f"channel={channel}&filterID={sn}&locale=zh_CN&model={model}"
        f"&time={now_ms}&version={version}&{SECRET_SUFFIX}"
    )
    s = hashlib.md5(base64.b64encode(ota_info.encode())).hexdigest()
    url = (
        f"{API.format(model=model)}?model={model}&version={version}"
        f"&channel={channel}&filterID={sn}&locale=zh_CN&time={now_ms}&s={s}"
    )
    return {"ota_info": ota_info, "s": s, "url": url}


def fetch_ota_info(model: str, version: str, channel: str = "release",
                   sn: str = "") -> dict:
    """问灰度升级接口拿当前版本信息（唯一发网络请求的地方）。"""
    url = build_ota_url(model, version, channel, sn)["url"]
    # nosemgrep 说明：url 由 build_ota_url 固定拼出（https://api.miwifi.com
    # 灰度接口，无外部输入），不在白名单校验之列；带 timeout=30。
    with urllib.request.urlopen(url, timeout=30) as r:  # nosemgrep: open-xiaoai-r5-urlfetch-no-allowlist
        return json.loads(r.read())


def hash_file(path: Path) -> tuple[str, str, int]:
    """流式算文件 SHA256、MD5 与大小（不一次性读进内存）。"""
    sha256 = hashlib.sha256()
    md5 = hashlib.md5()
    size = 0
    with open(path, "rb") as f:
        while True:
            chunk = f.read(CHUNK_SIZE)
            if not chunk:
                break
            sha256.update(chunk)
            md5.update(chunk)
            size += len(chunk)
    return sha256.hexdigest(), md5.hexdigest(), size


def verify_file(path: Path, expected_sha256: str,
                expected_md5: str | None = None,
                expected_size: int | None = None) -> tuple[str, int]:
    """校验文件 SHA256（权威）与可选的 MD5/大小；不符抛 ValueError。

    返回 (sha256, size)。
    """
    got_sha256, got_md5, size = hash_file(Path(path))
    if got_sha256 != expected_sha256:
        raise ValueError(
            f"SHA256 mismatch: got {got_sha256}, want {expected_sha256} ({path})")
    if expected_md5 is not None and got_md5 != expected_md5:
        raise ValueError(
            f"MD5 mismatch: got {got_md5}, want {expected_md5} ({path})")
    if expected_size is not None and size != expected_size:
        raise ValueError(f"大小不符：got {size}, want {expected_size} ({path})")
    return got_sha256, size


def verify_local(model: str, version: str, path: str | Path,
                 kind: str = "bin") -> tuple[str, int]:
    """按白名单校验一个本地文件：kind='bin' 查 EXPECTED，'sqfs' 查 EXPECTED_SQFS。

    供 unpack.sh / patch.sh / flash-check.sh 调用。成功打印 OK 并返回
    (sha256, size)；失败抛 ValueError。
    """
    table = EXPECTED if kind == "bin" else EXPECTED_SQFS
    try:
        exp = table[(model, version)]
    except KeyError:
        raise ValueError(
            f"不在白名单：{model} {version}（{kind} 校验无锚点，拒绝）")
    got_sha256, size = verify_file(Path(path), exp["sha256"],
                                   exp.get("md5"), exp["size"])
    print(f"{kind} OK: {path} sha256={got_sha256} size={size}")
    return got_sha256, size


def download_file(link: str, tmp_path: Path, timeout: int = 30) -> None:
    """流式下载到 tmp_path（带超时）；任何失败都删临时文件再抛错。"""
    req = urllib.request.Request(
        link, headers={"User-Agent": "open-xiaoai-next/download-ota.py"})
    try:
        # link 已由调用方 check_download_link() 校验（https + 白名单 host，
        # 见上），这里是校验通过后的实际下载；urlopen 带 timeout，流式写 .tmp。
        with urllib.request.urlopen(req, timeout=timeout) as r:  # nosemgrep: open-xiaoai-r5-urlfetch-no-allowlist
            status = getattr(r, "status", 200)
            if status != 200:
                raise ValueError(f"下载失败：HTTP {status} ({link})")
            with open(tmp_path, "wb") as f:
                while True:
                    chunk = r.read(CHUNK_SIZE)
                    if not chunk:
                        break
                    f.write(chunk)
    except BaseException:
        Path(tmp_path).unlink(missing_ok=True)
        raise


def main(argv: list[str] | None = None) -> None:
    if argv is None:
        argv = sys.argv[1:]
    model, version = argv[0], argv[1]
    channel = argv[2] if len(argv) > 2 else "release"
    sn = argv[3] if len(argv) > 3 else ""
    # 强制门：先查白名单再发任何网络请求。
    exp = expected(model, version)
    info = fetch_ota_info(model, version, channel, sn)
    cur = info["data"]["currentInfo"]
    link, md5, size = cur["link"], cur["hash"], int(cur["size"])
    # 服务端不得偷换版本；toVersion 缺失时跳过此项（兼容旧响应）。
    if cur.get("toVersion") is not None and cur["toVersion"] != version:
        raise ValueError(
            f"服务端返回的版本与请求不符：toVersion={cur['toVersion']!r}，"
            f"请求 {version!r}，拒绝下载")
    validate_link(link)
    # 预检：服务端自带的 MD5/大小必须先与白名单一致，否则不下载。
    # （这只是预检；放行依据永远是下载后对文件实算的 SHA256。）
    if md5 != exp["md5"] or size != exp["size"]:
        raise ValueError(
            f"服务端返回的 MD5/大小与白名单不一致：got ({md5}, {size})，"
            f"want ({exp['md5']}, {exp['size']})，拒绝下载")
    print(f"OTA: {model} {version}, size={size}, md5={md5}")
    print(f"link: {link}")

    out = Path(__file__).resolve().parent.parent / "work" / f"{model}_{version}.bin"
    out.parent.mkdir(exist_ok=True)
    tmp = out.with_name(out.name + ".tmp")
    download_file(link, tmp)
    print(f"downloaded: {tmp} ({tmp.stat().st_size} bytes)")
    try:
        got_sha256, got_size = verify_file(tmp, exp["sha256"], exp["md5"], exp["size"])
    except BaseException:
        tmp.unlink(missing_ok=True)
        raise
    os.replace(tmp, out)
    print(f"SHA256 OK: {got_sha256} ({got_size} bytes) -> {out}")


def _cli(argv: list[str]) -> int:
    """命令行分发：verify-bin / verify-sqfs 校验本地文件，其余走下载流程。"""
    if len(argv) >= 2 and argv[1] in ("verify-bin", "verify-sqfs"):
        kind = "bin" if argv[1] == "verify-bin" else "sqfs"
        try:
            verify_local(argv[2], argv[3], argv[4], kind)
        except (ValueError, FileNotFoundError, IndexError) as e:
            print(f"校验失败：{e}", file=sys.stderr)
            return 1
        return 0
    try:
        main(argv[1:])
    except (ValueError, KeyError, IndexError) as e:
        print(f"失败：{e}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(_cli(sys.argv))
