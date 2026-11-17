"""download-ota.py 的测试：签名算法、SHA256 白名单、链接白名单、流式下载。

硬要求：不发任何真实网络请求——urllib.request.urlopen 全部用 monkeypatch
mock 掉；time.time 固定。
"""
import base64
import hashlib
import json
import sys

import pytest

import download_ota
from download_ota import (
    build_ota_url,
    download_file,
    fetch_ota_info,
    validate_link,
    verify_file,
    verify_local,
)

# 固定时间点：2026-11-17 11:33:20 CST 对应的 epoch 秒（测试内约定值）
FIXED_TIME_S = 1763350000.0
FIXED_TIME_MS = 1763350000000

# REA_ANALYSIS.md 记录的算法（测试内独立重算，不直接抄实现）：
#   otaInfo = channel=release&filterID=&locale=zh_CN&model=lx06
#             &time=<ms>&version=1.94.13&8007236f-a2d6-4847-ac83-c49395ad6d65
#   s = md5(base64(otaInfo))
EXPECTED_OTA_INFO = (
    "channel=release&filterID=&locale=zh_CN&model=lx06"
    f"&time={FIXED_TIME_MS}&version=1.94.13&8007236f-a2d6-4847-ac83-c49395ad6d65"
)
EXPECTED_S = hashlib.md5(base64.b64encode(EXPECTED_OTA_INFO.encode())).hexdigest()
EXPECTED_URL = (
    "https://api.miwifi.com/rs/grayupgrade/v2/lx06?model=lx06&version=1.94.13"
    f"&channel=release&filterID=&locale=zh_CN&time={FIXED_TIME_MS}&s={EXPECTED_S}"
)


def _payload_expected(payload: bytes, version: str = "9.9.9"):
    """给 monkeypatch 用的白名单条目：按 fake payload 实算哈希。"""
    return {("lx06", version): {
        "sha256": hashlib.sha256(payload).hexdigest(),
        "md5": hashlib.md5(payload).hexdigest(),
        "size": len(payload),
    }}


def _fake_fetch(link, md5, size, version="9.9.9"):
    def _fetch(model, ver, channel="release", sn=""):
        return {"data": {"currentInfo": {
            "size": size, "link": link, "hash": md5, "toVersion": version}}}
    return _fetch


class _FakeHTTPResp:
    """模拟 urlopen 返回的 HTTP 响应：按块读，读完返回 b''。"""

    def __init__(self, payload: bytes, status: int = 200):
        self._payload = payload
        self.status = status
        self._pos = 0

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False

    def read(self, n=-1):
        if self._pos >= len(self._payload):
            return b""
        chunk = (self._payload[self._pos:self._pos + n] if n > 0
                 else self._payload[self._pos:])
        self._pos += len(chunk)
        return chunk


def _fake_urlopen(payload, seen=None):
    def _urlopen(url_or_req, timeout=30):
        if seen is not None:
            seen.append(url_or_req)
        return _FakeHTTPResp(payload)
    return _urlopen


def _fake_scripts_dir(monkeypatch, tmp_path):
    """把 download_ota.__file__ 指到临时目录，使 main() 的输出落进 tmp work。"""
    fake_scripts = tmp_path / "scripts"
    fake_scripts.mkdir()
    monkeypatch.setattr(download_ota, "__file__",
                        str(fake_scripts / "download-ota.py"))
    return tmp_path / "work"


def test_build_ota_url_matches_rea_algorithm():
    """对已知输入算出的 otaInfo、s 与完整 URL 与 REA 记录一致（API 走 https）。"""
    got = build_ota_url("lx06", "1.94.13", "release", "", now_ms=FIXED_TIME_MS)
    assert got["ota_info"] == EXPECTED_OTA_INFO
    assert got["s"] == EXPECTED_S
    assert got["url"] == EXPECTED_URL
    assert got["url"].startswith("https://api.miwifi.com/")


def test_fetch_ota_info_uses_ms_timestamp_and_mocked_network(monkeypatch):
    """fetch 时 time.time() 被固定，URL 里 time 是毫秒；urlopen 被 mock。"""
    monkeypatch.setattr(download_ota.time, "time", lambda: FIXED_TIME_S)
    seen_urls = []

    class FakeResp:
        def __enter__(self):
            return self

        def __exit__(self, *exc):
            return False

        def read(self):
            return json.dumps({"code": "0", "data": {"currentInfo": {
                "size": 43478328,
                "link": "https://example.invalid/x.bin",
                "hash": "b202de2bbe149ce884aa500911151b95",
                "toVersion": "1.94.13"}}}).encode()

    def fake_urlopen(url, timeout=30):
        seen_urls.append(url)
        return FakeResp()

    monkeypatch.setattr(download_ota.urllib.request, "urlopen", fake_urlopen)
    info = fetch_ota_info("lx06", "1.94.13")
    assert len(seen_urls) == 1
    assert seen_urls[0] == EXPECTED_URL
    assert info["data"]["currentInfo"]["hash"] == "b202de2bbe149ce884aa500911151b95"


def test_verify_file_ok_and_mismatches(tmp_path):
    """SHA256 对上通过；SHA256/MD5/大小任一对不上都抛 ValueError。"""
    p = tmp_path / "x.bin"
    p.write_bytes(b"hello firmware")
    sha256 = hashlib.sha256(b"hello firmware").hexdigest()
    md5 = hashlib.md5(b"hello firmware").hexdigest()
    assert verify_file(p, sha256, md5, len(b"hello firmware")) == (
        sha256, len(b"hello firmware"))
    with pytest.raises(ValueError, match="SHA256 mismatch"):
        verify_file(p, "0" * 64, md5)
    with pytest.raises(ValueError, match="MD5 mismatch"):
        verify_file(p, sha256, "0" * 32)
    with pytest.raises(ValueError, match="大小不符"):
        verify_file(p, sha256, md5, 999)


def test_main_rejects_non_whitelisted_version(monkeypatch, tmp_path):
    """P0-5：不在白名单的版本在发任何网络请求之前就拒绝。"""
    _fake_scripts_dir(monkeypatch, tmp_path)

    def _no_network(*a, **k):
        raise AssertionError("白名单外版本不应发网络请求")

    monkeypatch.setattr(download_ota, "fetch_ota_info", _no_network)
    monkeypatch.setattr(download_ota.urllib.request, "urlopen", _no_network)
    monkeypatch.setattr(sys, "argv", ["download-ota.py", "lx06", "9.9.9"])
    with pytest.raises(ValueError, match="不在白名单"):
        download_ota.main()


def test_main_rejects_http_link(monkeypatch, tmp_path):
    """P0-5：服务端返回 http:// 链接时拒绝下载，且不写任何文件。"""
    work = _fake_scripts_dir(monkeypatch, tmp_path)
    payload = b"fake firmware bytes"
    monkeypatch.setattr(download_ota, "EXPECTED", _payload_expected(payload))
    monkeypatch.setattr(
        download_ota, "fetch_ota_info",
        _fake_fetch("http://cdn.cnbj1.fds.api.mi-img.com/x.bin",
                    hashlib.md5(payload).hexdigest(), len(payload)))
    monkeypatch.setattr(download_ota.urllib.request, "urlopen",
                        _fake_urlopen(payload))
    monkeypatch.setattr(sys, "argv", ["download-ota.py", "lx06", "9.9.9"])
    with pytest.raises(ValueError, match="拒绝非 https"):
        download_ota.main()
    assert not (work / "lx06_9.9.9.bin").exists()
    assert not (work / "lx06_9.9.9.bin.tmp").exists()


def test_main_rejects_untrusted_link_host(monkeypatch, tmp_path):
    """P0-5：https 但 host 不在白名单时拒绝下载（file:// 同理）。"""
    work = _fake_scripts_dir(monkeypatch, tmp_path)
    payload = b"fake firmware bytes"
    monkeypatch.setattr(download_ota, "EXPECTED", _payload_expected(payload))
    md5 = hashlib.md5(payload).hexdigest()
    for bad_link in ("https://evil.example/x.bin",
                     "https://cdn.cnbj1.fds.api.mi-img.com.evil.example/x.bin",
                     "file:///tmp/x.bin"):
        monkeypatch.setattr(download_ota, "fetch_ota_info",
                            _fake_fetch(bad_link, md5, len(payload)))
        monkeypatch.setattr(sys, "argv", ["download-ota.py", "lx06", "9.9.9"])
        with pytest.raises(ValueError, match="拒绝非白名单 host|拒绝非 https"):
            download_ota.main()
    assert not (work / "lx06_9.9.9.bin").exists()
    assert not (work / "lx06_9.9.9.bin.tmp").exists()


def test_main_rejects_server_md5_mismatch(monkeypatch, tmp_path):
    """P0-5：服务端自带的 MD5 与白名单不一致时，下载前就拒绝。"""
    work = _fake_scripts_dir(monkeypatch, tmp_path)
    payload = b"fake firmware bytes"
    monkeypatch.setattr(download_ota, "EXPECTED", _payload_expected(payload))
    monkeypatch.setattr(
        download_ota, "fetch_ota_info",
        _fake_fetch("https://cdn.cnbj1.fds.api.mi-img.com/x.bin",
                    "0" * 32, len(payload)))
    monkeypatch.setattr(download_ota.urllib.request, "urlopen",
                        _fake_urlopen(payload))
    monkeypatch.setattr(sys, "argv", ["download-ota.py", "lx06", "9.9.9"])
    with pytest.raises(ValueError, match="与白名单不一致"):
        download_ota.main()
    assert not (work / "lx06_9.9.9.bin").exists()


def test_main_sha256_mismatch_cleans_tmp(monkeypatch, tmp_path):
    """P0-5：下载后 SHA256 对不上时抛错，且 .tmp 被删、最终文件不落盘。"""
    work = _fake_scripts_dir(monkeypatch, tmp_path)
    payload = b"fake firmware bytes"
    # 白名单里 MD5/大小与 payload 一致，但 SHA256 故意写错：走到下载后校验才失败。
    bad = _payload_expected(payload)
    bad[("lx06", "9.9.9")]["sha256"] = "0" * 64
    monkeypatch.setattr(download_ota, "EXPECTED", bad)
    monkeypatch.setattr(
        download_ota, "fetch_ota_info",
        _fake_fetch("https://cdn.cnbj1.fds.api.mi-img.com/x.bin",
                    hashlib.md5(payload).hexdigest(), len(payload)))
    monkeypatch.setattr(download_ota.urllib.request, "urlopen",
                        _fake_urlopen(payload))
    monkeypatch.setattr(sys, "argv", ["download-ota.py", "lx06", "9.9.9"])
    with pytest.raises(ValueError, match="SHA256 mismatch"):
        download_ota.main()
    assert not (work / "lx06_9.9.9.bin").exists()
    assert not (work / "lx06_9.9.9.bin.tmp").exists()


def test_download_file_failure_cleans_tmp(monkeypatch, tmp_path):
    """P0-5：下载中途断开时，临时文件被删掉，不留截断的 .tmp。"""
    tmp = tmp_path / "x.bin.tmp"

    class _BreakingResp(_FakeHTTPResp):
        def read(self, n=-1):
            raise ConnectionError("模拟断网")

    monkeypatch.setattr(download_ota.urllib.request, "urlopen",
                        lambda url_or_req, timeout=30: _BreakingResp(b""))
    with pytest.raises(ConnectionError):
        download_file("https://cdn.cnbj1.fds.api.mi-img.com/x.bin", tmp)
    assert not tmp.exists()


def test_main_success_path_mocked(monkeypatch, tmp_path):
    """main() 全流程（mock 网络）：流式下载 → SHA256 校验 → rename 落盘，无 .tmp 残留。"""
    work = _fake_scripts_dir(monkeypatch, tmp_path)
    payload = b"fake firmware bytes"
    monkeypatch.setattr(download_ota, "EXPECTED", _payload_expected(payload))
    monkeypatch.setattr(
        download_ota, "fetch_ota_info",
        _fake_fetch("https://cdn.cnbj1.fds.api.mi-img.com/x.bin",
                    hashlib.md5(payload).hexdigest(), len(payload)))
    monkeypatch.setattr(download_ota.urllib.request, "urlopen",
                        _fake_urlopen(payload))
    monkeypatch.setattr(sys, "argv", ["download-ota.py", "lx06", "9.9.9"])
    download_ota.main()  # 不抛错即通过
    out = work / "lx06_9.9.9.bin"
    assert out.exists()
    assert out.read_bytes() == payload
    assert not (work / "lx06_9.9.9.bin.tmp").exists()


def test_validate_link():
    """链接白名单：https + 小米 CDN host 通过，其余拒绝。"""
    assert validate_link(
        "https://cdn.cnbj1.fds.api.mi-img.com/xiaoqiang/rom/lx06/x.bin")
    for bad in ("http://cdn.cnbj1.fds.api.mi-img.com/x.bin",
                "https://evil.example/x.bin",
                "https://cdn.cnbj1.fds.api.mi-img.com.evil.example/x.bin",
                "file:///tmp/x.bin",
                "ftp://cdn.cnbj1.fds.api.mi-img.com/x.bin"):
        with pytest.raises(ValueError):
            validate_link(bad)


def test_verify_local_and_cli(monkeypatch, tmp_path):
    """verify-bin / verify-sqfs 子命令：对上返回 0，对不上返回 1。"""
    payload = b"fake firmware bytes"
    p = tmp_path / "x.bin"
    p.write_bytes(payload)
    monkeypatch.setattr(download_ota, "EXPECTED", _payload_expected(payload))
    assert verify_local("lx06", "9.9.9", p, "bin")[0] == hashlib.sha256(
        payload).hexdigest()
    assert download_ota._cli(["download-ota.py", "verify-bin",
                              "lx06", "9.9.9", str(p)]) == 0
    # 白名单外版本：verify 也拒绝
    assert download_ota._cli(["download-ota.py", "verify-bin",
                              "lx06", "8.8.8", str(p)]) == 1
    # 内容被改：SHA256 对不上
    p.write_bytes(b"tampered")
    assert download_ota._cli(["download-ota.py", "verify-bin",
                              "lx06", "9.9.9", str(p)]) == 1
    # verify-sqfs 用 EXPECTED_SQFS；这里没有 9.9.9 的 sqfs 条目，应拒绝
    assert download_ota._cli(["download-ota.py", "verify-sqfs",
                              "lx06", "9.9.9", str(p)]) == 1


def test_whitelist_covers_real_versions():
    """白名单里的两个已验证版本哈希与 2026-11-17 复核值一致（防手滑改错）。"""
    assert download_ota.EXPECTED[("lx06", "1.94.13")]["sha256"] == (
        "a56a38619fd98e467adb8c998757f80109c4dac89db48d62dae985e3062476ed")
    assert download_ota.EXPECTED[("lx06", "1.94.13")]["md5"] == (
        "b202de2bbe149ce884aa500911151b95")
    assert download_ota.EXPECTED[("lx06", "1.94.13")]["size"] == 43478328
    assert download_ota.EXPECTED[("oh2p", "1.58.6")]["sha256"] == (
        "1f351d879c06ff3bd7bd4b4c68e2c8fa45eee1a82be5839e57899ae8f9b7c27b")
    assert download_ota.EXPECTED[("oh2p", "1.58.6")]["md5"] == (
        "eb86c0c12ea118fb701433bf009a180c")
    assert download_ota.EXPECTED[("oh2p", "1.58.6")]["size"] == 36042824
    assert download_ota.EXPECTED_SQFS[("lx06", "1.94.13")]["sha256"] == (
        "317a54909f12b63f22128965bf9005c06435d15f61c4202bb88c4524bf1e2ef4")
    assert download_ota.EXPECTED_SQFS[("oh2p", "1.58.6")]["sha256"] == (
        "c33dfc21a2a3093b6c4e41f3bf0738e1ff21daf59bbccc5203863bdb62dfa493")
