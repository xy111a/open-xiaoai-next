#!/usr/bin/env python3
"""Serve the extracted panel HTML + fake /api/* backends, then verify
over plain HTTP that the panel loads and every API it fetches returns
the documented JSON shape.

This does NOT execute the panel JS (no headless browser on this VM);
it proves the HTML + fake-backend contract, not rendering.
Run: python3 mock_panel_server_check.py
"""
import json
import re
import sys
import threading
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

HERE = Path(__file__).resolve().parent
WEB_GO = HERE.parent / "web" / "web.go"

src = WEB_GO.read_text()
anchor = "fmt.Fprint(w, `"
start = src.find(anchor) + len(anchor)
end = src.find("`", start)
PANEL = src[start:end]

FAKE = {
    "/api/status": {"wake_word": "你好小李", "rooms": ["客厅"], "devices": 2,
                    "online": 2, "kws_status": "ok", "kws_down": False,
                    "latest_latency": {"id": "lx06-1-1", "device_id": "lx06-1",
                                       "kind": "voice",
                                       "started_at": "2026-11-19T17:00:00+08:00",
                                       "total_ms": 3120,
                                       "stages": {"kws_ms": 120, "stt_ms": 900,
                                                  "llm_ms": 800, "tts_ms": 600},
                                       "ok": True}},
    "/api/devices": [{"ID": "lx06-1", "Model": "lx06", "Room": "客厅"}],
    "/api/health": {"services": [
        {"name": n, "container": "oxn-" + n, "kind": "http",
         "address": "127.0.0.1:1", "status": "ok" if i % 2 == 0 else "down",
         "latency_ms": 12, "detail": "HTTP 200" if i % 2 == 0 else "TCP 连接失败"}
        for i, n in enumerate(["hub", "whisper", "piper", "kws-sidecar",
                               "snapserver", "mpd", "netease-api"])],
        "checked_at": "2026-11-19T17:00:00+08:00",
        "healthy": 4, "total": 7, "down": 3},
    "/api/latency": {"interactions": [
        {"id": "lx06-1-1", "device_id": "lx06-1", "kind": "voice",
         "started_at": "2026-11-19T17:00:00+08:00", "total_ms": 3120,
         "stages": {"kws_ms": 120, "stt_ms": 900, "llm_ms": 800, "tts_ms": 600},
         "ok": True}],
        "count": 1},
}


class H(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/":
            body, ctype = PANEL.encode(), "text/html; charset=utf-8"
        elif self.path in FAKE:
            body = json.dumps(FAKE[self.path], ensure_ascii=False).encode()
            ctype = "application/json"
        else:
            self.send_response(404)
            self.end_headers()
            return
        self.send_response(200)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *a):
        pass


def main():
    srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    base = f"http://127.0.0.1:{srv.server_address[1]}"
    fails = []

    def check(name, cond, detail=""):
        print(("PASS " if cond else "FAIL ") + name + ((" — " + detail) if detail and not cond else ""))
        if not cond:
            fails.append(name)

    def get(path):
        with urllib.request.urlopen(base + path, timeout=5) as r:
            return r.status, r.headers.get("Content-Type", ""), r.read().decode()

    try:
        st, ct, body = get("/")
        check("GET / -> 200 text/html", st == 200 and "text/html" in ct, f"{st} {ct}")
        for frag in ("/api/status", "/api/devices", "/api/health", "/api/latency",
                     "系统健康", "语音延迟", "最近交互"):
            check(f"panel HTML contains {frag}", frag in body)
        # Every endpoint the panel JS fetches answers with valid JSON
        # carrying the keys the JS reads.
        for path, keys in (("/api/status", ["wake_word", "devices", "online",
                                            "kws_status", "latest_latency"]),
                           ("/api/health", ["services", "healthy", "total", "down"]),
                           ("/api/latency", ["interactions", "count"])):
            st, ct, body = get(path)
            try:
                data = json.loads(body)
                ok = st == 200 and all(k in data for k in keys)
            except Exception as e:
                ok, data = False, str(e)
            check(f"GET {path} -> 200 with keys {keys}", ok, f"{st} {data if not ok else ''}")
        st, ct, body = get("/api/devices")
        data = json.loads(body)
        check("GET /api/devices -> list with ID/Model/Room",
              isinstance(data, list) and data and all(
                  k in data[0] for k in ("ID", "Model", "Room")), body[:200])
        # Health payload has exactly the 7 containers, in compose order.
        st, ct, body = get("/api/health")
        names = [s["name"] for s in json.loads(body)["services"]]
        check("health lists all 7 containers in order",
              names == ["hub", "whisper", "piper", "kws-sidecar",
                        "snapserver", "mpd", "netease-api"], str(names))
    finally:
        srv.shutdown()
    print()
    if fails:
        print(f"{len(fails)} check(s) FAILED")
        return 1
    print("mock panel server checks passed (HTML + fake APIs over HTTP; JS execution not verified — no headless browser on this VM)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
