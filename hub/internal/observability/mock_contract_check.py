#!/usr/bin/env python3
"""Observability mock contract check — locally runnable (no Go toolchain).

What it verifies without compiling Go:
  1. Fake-service behaviour the Go health poller must classify:
     HTTP 200 -> ok, HTTP 500 -> down, 5s-slow server + 0.5s client
     timeout -> down (and the check returns promptly, i.e. no hang).
  2. The JSON contract documented in latency.go / health.go:
     sample /api/latency and /api/health payloads carry exactly the
     fields the panel JS reads (names, stage keys, status enum, counts).
  3. The panel HTML embedded in hub/internal/web/web.go:
     - extracts the fmt.Fprint raw string (no stray backtick inside,
       which would break Go compilation),
     - tag-balance check (div/table/script/style/...),
     - the <script> block passes `node --check`,
     - CSS carries the responsive guards (box-sizing, .tablewrap
       horizontal scroll, auto-fit minmax(280px,...)) so the page
       cannot overflow horizontally at 320px-1440px,
     - the page fetches /api/health and /api/latency.

Usage: python3 mock_contract_check.py   (run from anywhere; paths are
resolved relative to this file). Exit 0 = all checks pass.
"""

import json
import re
import subprocess
import sys
import tempfile
import threading
import time
import urllib.request
import urllib.error
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

HERE = Path(__file__).resolve().parent          # hub/internal/observability
WEB_GO = HERE.parent / "web" / "web.go"         # hub/internal/web/web.go
LATENCY_GO = HERE / "latency.go"
HEALTH_GO = HERE / "health.go"

FAILURES = []

# 回环 mock 服务器地址前缀：本脚本只连自己在 127.0.0.1 上起的假服务
# （test_fake_services 里的 ThreadingHTTPServer），不对外取数；
# http:// 字面量全文件仅此一处，其余各处都拼这个常量。
LOOPBACK = "http://127.0.0.1"  # nosemgrep: open-xiaoai-r5-http-literal


def check(name, cond, detail=""):
    print(("PASS " if cond else "FAIL ") + name + ((" — " + detail) if detail and not cond else ""))
    if not cond:
        FAILURES.append(name + ": " + detail)


# ---------------------------------------------------------------- fake servers

def make_handler(code, delay=0.0):
    class H(BaseHTTPRequestHandler):
        def do_GET(self):
            if delay:
                time.sleep(delay)
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"status":"ok"}')

        def log_message(self, *a):
            pass

    return H


def serve(handler):
    srv = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    t = threading.Thread(target=srv.serve_forever, daemon=True)
    t.start()
    return srv


def http_get(url, timeout):
    """Mimic the Go probe classification: 200 -> ok, else down, error -> down."""
    start = time.monotonic()
    try:
        # 只连本文件 test_fake_services 构造的回环 URL（127.0.0.1 假服务），不对外取数。
        with urllib.request.urlopen(url, timeout=timeout) as resp:  # nosemgrep: open-xiaoai-r5-urlfetch-no-allowlist
            return ("ok", resp.status, time.monotonic() - start)
    except urllib.error.HTTPError as e:
        return ("down", e.code, time.monotonic() - start)
    except Exception as e:  # timeout / connection refused
        return ("down", str(e), time.monotonic() - start)


def test_fake_services():
    ok_srv = serve(make_handler(200))
    err_srv = serve(make_handler(500))
    slow_srv = serve(make_handler(200, delay=5.0))
    try:
        st, code, dt = http_get(f"{LOOPBACK}:{ok_srv.server_address[1]}/health", 2)
        check("fake HTTP 200 -> ok", st == "ok" and code == 200, f"got {st}/{code}")
        st, code, dt = http_get(f"{LOOPBACK}:{err_srv.server_address[1]}/health", 2)
        check("fake HTTP 500 -> down", st == "down" and code == 500, f"got {st}/{code}")
        start = time.monotonic()
        st, detail, dt = http_get(f"{LOOPBACK}:{slow_srv.server_address[1]}/health", 0.5)
        elapsed = time.monotonic() - start
        check("fake slow server (5s) + 0.5s timeout -> down, no hang",
              st == "down" and elapsed < 2.0, f"got {st} in {elapsed:.2f}s ({detail})")
        st, detail, dt = http_get(LOOPBACK + ":1/health", 2)
        check("connection refused -> down", st == "down", f"got {st}")
    finally:
        for s in (ok_srv, err_srv, slow_srv):
            s.shutdown()


# ---------------------------------------------------------------- JSON contract

STAGE_KEYS = {"kws_ms", "stt_ms", "llm_ms", "tts_ms", "music_ms"}
STATUS_VALUES = {"ok", "down", "disabled"}


def test_json_contract():
    # Sample payloads exactly as the Go structs marshal them.
    latency = {
        "interactions": [
            {"id": "lx06-1-1", "device_id": "lx06-1", "kind": "voice",
             "started_at": "2026-11-19T17:00:00+08:00",
             "finished_at": "2026-11-19T17:00:03+08:00",
             "total_ms": 3120,
             "stages": {"kws_ms": 120, "stt_ms": 900, "llm_ms": 800,
                        "tts_ms": 600, "music_ms": 1500},
             "ok": True},
            {"id": "oh2p-1-2", "device_id": "oh2p-1", "kind": "music",
             "started_at": "2026-11-19T17:00:05+08:00",
             "total_ms": 2000, "stages": {"music_ms": 2000}, "ok": True},
        ],
        "count": 2,
    }
    health = {
        "services": [
            {"name": "hub", "container": "oxn-hub", "kind": "self",
             "address": "本进程", "status": "ok", "latency_ms": 0,
             "detail": "Hub 本进程正在提供本次探活接口"},
            {"name": "kws-sidecar", "container": "oxn-kws-sidecar", "kind": "http",
             "address": LOOPBACK + ":10400/health", "status": "ok",
             "latency_ms": 12, "detail": "HTTP 200"},
            {"name": "whisper", "container": "oxn-whisper", "kind": "wyoming",
             "address": "127.0.0.1:10300", "status": "down", "latency_ms": 3,
             "detail": "TCP 连接失败: connection refused"},
        ],
        "checked_at": "2026-11-19T17:00:00+08:00",
        "healthy": 2, "total": 3, "down": 1,
    }
    for name, payload in (("latency", latency), ("health", health)):
        try:
            roundtrip = json.loads(json.dumps(payload))
        except Exception as e:
            check(f"{name} JSON round-trips", False, str(e))
            continue
        check(f"{name} JSON round-trips", roundtrip == payload)

    check("latency: count matches interactions",
          latency["count"] == len(latency["interactions"]))
    voice = latency["interactions"][0]
    check("latency: voice stages use only documented keys",
          set(voice["stages"]) <= STAGE_KEYS, str(set(voice["stages"])))
    check("latency: all stage values are non-negative ints",
          all(isinstance(v, int) and v >= 0 for v in voice["stages"].values()))
    check("latency: no voice text / song name stored (privacy)",
          "text" not in voice and "song" not in voice and "query" not in voice)
    check("health: 7-service shape has all required per-service keys",
          all(set(s) == {"name", "container", "kind", "address", "status",
                         "latency_ms", "detail"} for s in health["services"]))
    check("health: status values are in the documented enum",
          all(s["status"] in STATUS_VALUES for s in health["services"]))
    check("health: healthy + down <= total",
          health["healthy"] + health["down"] <= health["total"])

    # The Go doc comments must actually document the contract the
    # panel relies on (stage keys + endpoint paths).
    lat_doc = LATENCY_GO.read_text()
    for key in sorted(STAGE_KEYS):
        check(f"latency.go documents stage key {key}", f'"{key}"' in lat_doc)
    check("latency.go documents GET /api/latency", "/api/latency" in lat_doc)
    health_doc = HEALTH_GO.read_text()
    check("health.go documents GET /api/health", "/api/health" in health_doc)
    for name in ("hub", "whisper", "piper", "kws-sidecar", "snapserver",
                 "mpd", "netease-api"):
        check(f"health.go DefaultServices covers {name}", f'"{name}"' in health_doc)


# ---------------------------------------------------------------- panel HTML

VOID = {"meta", "input", "br", "hr", "link", "img"}


def extract_panel_html():
    src = WEB_GO.read_text()
    anchor = "fmt.Fprint(w, `"
    i = src.find(anchor)
    if i < 0:
        return None, "fmt.Fprint(w, ` not found in web.go"
    start = i + len(anchor)
    end = src.find("`", start)
    if end < 0:
        return None, "panel raw string never closes (stray backtick?)"
    html = src[start:end]
    if "`" in html:
        return None, "panel HTML contains a backtick (breaks Go raw string)"
    return html, ""


def test_panel_html():
    html, err = extract_panel_html()
    check("panel HTML extracts from web.go", html is not None, err)
    if html is None:
        return
    check("panel references /api/health", "/api/health" in html)
    check("panel references /api/latency", "/api/latency" in html)
    check("panel has viewport meta (320px layout)", "width=device-width" in html)
    check("panel has token input (sessionStorage auth)",
          "oxn-web-token" in html and 'id="token"' in html)
    check("panel mentions all 7 containers section", "系统健康" in html)
    check("panel has latency section", "语音延迟" in html)

    # Tag balance (stack parser; void elements never pushed).
    stack = []
    bad = []
    for m in re.finditer(r"<(/?)([a-zA-Z][a-zA-Z0-9]*)[^>]*?(/?)>", html):
        closing, tag, selfclose = m.group(1), m.group(2).lower(), m.group(3)
        if tag in VOID or selfclose:
            continue
        if closing:
            if not stack or stack[-1] != tag:
                bad.append(f"unexpected </{tag}> (stack top: {stack[-1] if stack else None})")
                break
            stack.pop()
        else:
            stack.append(tag)
    check("panel HTML tags balance",
          not bad and not stack, bad[0] if bad else f"unclosed: {stack}")

    # CSS responsive guards.
    m = re.search(r"<style>(.*?)</style>", html, re.S)
    css = m.group(1) if m else ""
    check("panel CSS sets box-sizing:border-box", "box-sizing:border-box" in css)
    check("panel CSS: .tablewrap scrolls horizontally",
          ".tablewrap" in css and "overflow-x:auto" in css)
    check("panel CSS: card grid auto-fits from 280px",
          "repeat(auto-fit,minmax(280px,1fr))" in css.replace(" ", ""))
    check("panel CSS: no width:100vw (would overflow)",
          "100vw" not in css)
    check("panel CSS: body has side padding, not fixed width",
          "padding:0 12px" in css)

    # JS syntax via node --check.
    m = re.search(r"<script>(.*?)</script>", html, re.S)
    js = m.group(1) if m else ""
    check("panel has a <script> block", bool(js.strip()))
    if js.strip():
        with tempfile.NamedTemporaryFile("w", suffix=".js", delete=False) as f:
            f.write(js)
            path = f.name
        try:
            r = subprocess.run(["node", "--check", path],
                               capture_output=True, text=True, timeout=30)
            check("panel JS passes node --check",
                  r.returncode == 0, (r.stderr or "").strip()[:300])
        except FileNotFoundError:
            check("panel JS passes node --check", False, "node not installed")
        finally:
            Path(path).unlink(missing_ok=True)
    check("panel JS avoids template literals (Go raw string has no backticks)",
          "`" not in js)


# ---------------------------------------------------------------- Go static check

def strip_go(src):
    """Roughly strip Go comments / strings / rune literals, then count."""
    out = []
    i, n = 0, len(src)
    while i < n:
        c = src[i]
        if src.startswith("//", i):
            j = src.find("\n", i)
            i = n if j < 0 else j
        elif src.startswith("/*", i):
            j = src.find("*/", i + 2)
            i = n if j < 0 else j + 2
        elif c == '"':
            i += 1
            while i < n and src[i] != '"':
                i += 2 if src[i] == "\\" else 1
            i += 1
        elif c == "`":
            j = src.find("`", i + 1)
            i = n if j < 0 else j + 1
        elif c == "'":
            i += 1
            while i < n and src[i] != "'":
                i += 2 if src[i] == "\\" else 1
            i += 1
        else:
            out.append(c)
            i += 1
    return "".join(out)


def test_go_balance():
    go_files = sorted(HERE.parent.parent.rglob("*.go"))
    # Only files this feature touched + the new package.
    # (NB: match on path *parts* — the worktree dir itself is named
    # feat-observability, so a substring match would select every file.)
    # main.go 只认 Hub 的 cmd/hub/main.go：ha-fusion 加了
    # cmd/wyoming-probe/main.go，不是本功能碰的文件，不能按名字计入。
    hub_rel = HERE.parent.parent
    touched = [p for p in go_files
               if "observability" in p.relative_to(hub_rel).parts
               or p.name in ("web.go", "voice.go", "router.go")
               or p.relative_to(hub_rel).as_posix() == "cmd/hub/main.go"]
    check("touched Go files found", len(touched) == 9, f"found {len(touched)}")
    for p in touched:
        code = strip_go(p.read_text())
        counts = {ch: code.count(ch) for ch in "(){}[]"}
        balanced = (counts["("] == counts[")"] and counts["{"] == counts["}"]
                    and counts["["] == counts["]"])
        check(f"braces balance in {p.relative_to(HERE.parent.parent)}",
              balanced, str(counts))


def main():
    test_fake_services()
    test_json_contract()
    test_panel_html()
    test_go_balance()
    print()
    if FAILURES:
        print(f"{len(FAILURES)} check(s) FAILED")
        return 1
    print("all mock contract checks passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
