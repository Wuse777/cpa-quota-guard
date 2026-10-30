"""Exercise the built plugin inside an unmodified CPA 8.0.35 process.

Uses fake OAuth accounts, a local quota endpoint, and a local HTTPS CONNECT proxy.
No model request or credential is sent to an external service.
Run on Linux with Python 3, PyYAML, OpenSSL and the release CPA binary.
"""
import argparse
import base64
import hashlib
import http.client
import http.server
import json
import os
from pathlib import Path
import shutil
import signal
import socketserver
import ssl
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request
from datetime import datetime, timedelta, timezone
import sys

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
from cpamc_bridge import inject_session_bridge

import yaml

KEY = "quota-guard-integration-only"
ACCESS = "quota-guard-test-client"
BASE = "http://127.0.0.1:18317"
quota = {"a.json": 10, "b.json": 80, "g.json": 70}
failures = set()
model_calls = []
reset_cards = {}
reset_calls = []
reset_failure = False


class Probe(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        name = self.path.rsplit("/", 1)[-1]
        if "/codex-reset/" in self.path:
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps({"credits": reset_cards.get(name, [])}).encode())
            return
        if name in failures:
            self.send_response(503)
            self.end_headers()
            return
        remaining = quota.get(name, 80)
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        if "/antigravity/" in self.path:
            data = {"groups": [{"displayName": "Claude", "buckets": [
                {"window": "WEEKLY", "remainingFraction": remaining / 100,
                 "resetTime": "2099-01-01T00:00:00Z"}]}]}
        else:
            data = {"rate_limit": {"primary_window": {
                "used_percent": 100 - remaining, "limit_window_seconds": 18000},
                "secondary_window": {"used_percent": 10, "limit_window_seconds": 604800}}}
        self.wfile.write(json.dumps(data).encode())

    def do_POST(self):
        if "/codex-reset/" in self.path and self.path.endswith("/consume"):
            name = self.path.split("/")[-2]
            payload = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))))
            reset_calls.append((name, payload["redeem_request_id"]))
            if reset_failure:
                self.send_response(403)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(b'{"error":{"code":"policy_denied","message":"fixture reset rejected"}}')
                return
            cards = reset_cards.get(name, [])
            if cards:
                cards.pop(0)
                quota[name] = 100
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"code":"reset"}')
            return
        self.do_GET()

    def log_message(self, *_):
        pass


class Proxy(socketserver.StreamRequestHandler):
    def handle(self):
        line = self.rfile.readline().decode().strip()
        while self.rfile.readline() not in (b"\r\n", b"\n", b""):
            pass
        if line != "CONNECT chatgpt.com:443 HTTP/1.1":
            self.wfile.write(b"HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
            return
        self.wfile.write(b"HTTP/1.1 200 Connection Established\r\n\r\n")
        self.wfile.flush()
        tls = self.server.tls_context.wrap_socket(self.connection, server_side=True)
        reader = tls.makefile("rb")
        try:
            request_line = reader.readline().decode().strip()
            headers = http.client.parse_headers(reader)
            body = reader.read(int(headers.get("Content-Length", "0")))
            who = headers.get("Authorization", "").removeprefix("Bearer fake-")
            model_calls.append(who)
            request = json.loads(body or b"{}")
            response = {"id": "resp_local", "object": "response", "status": "completed",
                        "model": request.get("model", "gpt-6.1-sol"),
                        "output": [{"id": "msg_local", "type": "message", "role": "assistant",
                                    "status": "completed", "content": [{"type": "output_text",
                                    "text": "selected:" + who, "annotations": []}]}],
                        "usage": {"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}
            events = [
                {"type": "response.output_text.delta", "delta": "selected:" + who,
                 "output_index": 0, "content_index": 0, "item_id": "msg_local"},
                {"type": "response.completed", "response": response},
            ]
            payload = "".join("data: " + json.dumps(e) + "\n\n" for e in events).encode()
            tls.sendall(b"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nConnection: close\r\nContent-Length: "
                        + str(len(payload)).encode() + b"\r\n\r\n" + payload)
        finally:
            reader.close()
            tls.close()


class ProxyServer(socketserver.ThreadingTCPServer):
    allow_reuse_address = True


def api(path, method="GET", payload=None, key=KEY):
    data = None if payload is None else json.dumps(payload).encode()
    headers = {"Content-Type": "application/json"}
    if key:
        headers["Authorization"] = "Bearer " + key
    req = urllib.request.Request(BASE + path, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=15) as r:
            raw = r.read()
            return r.status, json.loads(raw) if "json" in r.headers.get("Content-Type", "") else raw
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            raw = json.loads(raw)
        except ValueError:
            pass
        return e.code, raw


PREFIX = "/v0/management/plugins/quota-guard"


def status():
    code, data = api(PREFIX + "/status")
    assert code == 200, (code, data)
    return data


def wait_for(predicate, seconds=25):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        try:
            data = status()
            if predicate(data):
                return data
        except (OSError, AssertionError):
            pass
        time.sleep(.2)
    raise AssertionError("timed out waiting for fixture state")


def save(policy):
    code, data = api(PREFIX + "/policy", "PUT", policy)
    assert code == 200, (code, data)
    return data


def refresh():
    assert api(PREFIX + "/refresh", "POST", {})[0] == 202


def generate(model, stream=False):
    return api("/v1/responses", "POST", {"model": model, "input": "local test", "stream": stream}, ACCESS)


def main():
    global reset_failure
    parser = argparse.ArgumentParser()
    parser.add_argument("--plugin", required=True)
    parser.add_argument("--cpa", default="/opt/cliproxyapi/cli-proxy-api")
    parser.add_argument("--panel", type=Path, default=Path("/opt/cliproxyapi/static/management.html"),
                        help="verified CPAMC HTML, required only for --serve browser acceptance")
    parser.add_argument("--report", type=Path, default=Path("test-results/integration-report.json"))
    parser.add_argument("--serve", action="store_true")
    args = parser.parse_args()
    work = Path(tempfile.mkdtemp(prefix="quota-guard-integration-"))
    auths = work / "auths"
    auths.mkdir()
    plugin_dir = work / "plugins"
    plugin_dir.mkdir()
    shutil.copy2(args.plugin, plugin_dir / "quota-guard.so")
    cert, private = work / "ca.crt", work / "ca.key"
    subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes",
                    "-days", "1", "-subj", "/CN=chatgpt.com", "-addext", "subjectAltName=DNS:chatgpt.com",
                    "-keyout", str(private), "-out", str(cert)], check=True, capture_output=True)
    for name, provider, priority in [("a.json", "codex", 10), ("b.json", "codex", 0),
                                     ("g.json", "antigravity", 0)]:
        item = {"type": provider, "access_token": "fake-" + name, "email": name + "@example.invalid",
                "priority": priority, "expired": "2099-01-01T00:00:00Z", "plan_type": "pro"}
        if provider == "antigravity":
            item["project_id"] = "local-fixture-project"
        (auths / name).write_text(json.dumps(item))
    static = work / "static"
    static.mkdir()
    panel = args.panel
    if panel.exists():
        (static / "management.html").write_bytes(inject_session_bridge(panel.read_bytes()))
    elif args.serve:
        parser.error("--serve requires --panel pointing to the verified CPAMC 1.26.5 HTML")
    config = {"config-version": 8, "server": {"host": "127.0.0.1", "port": 18317},
              "management": {"allow-remote": False, "secret-key": KEY, "disable-auto-update-panel": True},
              "access": {"api-keys": [ACCESS]}, "oauth": {"auth-dir": str(auths)},
              "requests": {"proxy-url": "http://127.0.0.1:18319", "request-retry": 0},
              "plugins": {"enabled": True, "dir": str(plugin_dir), "configs": {"quota-guard": {
                  "enabled": True, "priority": 100, "state_dir": str(work / "state"),
                  "management_url": BASE, "management_key_file": str(work / "management.key"),
                  "test_probe_base": "http://127.0.0.1:18318"}}}}
    (work / "management.key").write_text(KEY)
    (work / "management.key").chmod(0o600)
    config_path = work / "config.yaml"
    config_path.write_text(yaml.safe_dump(config))
    probe_server = http.server.ThreadingHTTPServer(("127.0.0.1", 18318), Probe)
    proxy_server = ProxyServer(("127.0.0.1", 18319), Proxy)
    proxy_server.daemon_threads = True
    proxy_server.tls_context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    proxy_server.tls_context.load_cert_chain(cert, private)
    for server in (probe_server, proxy_server):
        threading.Thread(target=server.serve_forever, daemon=True).start()
    env = dict(os.environ, SSL_CERT_FILE=str(cert), MANAGEMENT_STATIC_PATH=str(static))
    log = open(work / "cpa.log", "w")
    proc = None
    results = []

    def start():
        return subprocess.Popen([args.cpa, "--local-model", "--config", str(config_path)],
                                cwd=work, env=env, stdout=log, stderr=subprocess.STDOUT)

    def check(name, condition):
        assert condition, name
        results.append(name)
        print("PASS", name, flush=True)

    try:
        proc = start()
        s = wait_for(lambda s: len(s["accounts"]) == 3 and all(a["buckets"] for a in s["accounts"]))
        check("native plugin registration and background queries", s["version"] == "0.2.2")
        plugins = api("/v8/management/plugins")[1]
        check("CPAMC plugin metadata and menu", any(p.get("id") == "quota-guard" and p.get("menus") for p in plugins["plugins"]))
        check("anonymous status rejected", api(PREFIX + "/status", key="")[0] == 401)
        check("anonymous mutation rejected", api(PREFIX + "/policy", "PUT", {}, key="")[0] == 401)
        page_code, page = api("/v0/resource/plugins/quota-guard/index", key="")
        check("public resource is static HTML without accounts or credentials",
              page_code == 200 and b"a.json" not in page and KEY.encode() not in page)
        s = status()
        p = s["policy"]
        p["rules"] = [{"account": "a.json", "enabled": True, "min_remaining_percent": 20},
                      {"account": "b.json", "enabled": True, "min_remaining_percent": 20},
                      {"account": "g.json", "enabled": True, "min_remaining_percent": 40}]
        s = save(p)
        check("observe mode marks low account without enforcing", s["policy"]["mode"] == "observe" and any(a["would_block"] for a in s["accounts"]))
        models = api("/v1/models", key=ACCESS)[1]["data"]
        ids = [m["id"] for m in models]
        model = next((m for m in ids if m == "gpt-6.1-sol"), next(m for m in ids if m.startswith("gpt-")))
        code, output = generate(model)
        check("observe preserves native request path", code == 200 and model_calls[-1] == "a.json")
        p = status()["policy"]
        p["mode"] = "enforce"
        save(p)
        for _ in range(3):
            code, output = generate(model)
            check("low high-priority account skipped", code == 200 and model_calls[-1] == "b.json")
        code, output = generate(model, True)
        check("streaming uses eligible account", code == 200 and model_calls[-1] == "b.json" and b"selected:b.json" in output)
        p = status()["policy"]
        stale = json.loads(json.dumps(p))
        p["rules"][1]["min_remaining_percent"] = 90
        save(p)
        before = len(model_calls)
        code, output = generate(model)
        check("all accounts protected returns explicit error without upstream call",
              code >= 400 and "quota_guard_protected" in json.dumps(output) and len(model_calls) == before)
        check("concurrent policy edits produce conflict", api(PREFIX + "/policy", "PUT", stale)[0] == 409)
        p = status()["policy"]
        p["rules"][1]["min_remaining_percent"] = 20
        save(p)
        quota["a.json"] = 26
        refresh()
        wait_for(lambda s: any(a["account"]["name"] == "a.json" and a["buckets"][0]["remaining_percent"] == 26 for a in s["accounts"]))
        generate(model)
        check("recovery gap avoids premature reuse", model_calls[-1] == "b.json")
        quota["a.json"] = 30
        refresh()
        wait_for(lambda s: any(a["account"]["name"] == "a.json" and not a["would_block"] for a in s["accounts"]))
        generate(model)
        check("fresh quota recovery restores higher-priority account", model_calls[-1] == "a.json")
        quota["a.json"] = 10
        refresh()
        wait_for(lambda s: any(a["account"]["name"] == "a.json" and a["would_block"] for a in s["accounts"]))
        failures.add("a.json")
        refresh()
        wait_for(lambda s: any(a["account"]["name"] == "a.json" and a.get("error") for a in s["accounts"]))
        generate(model)
        check("query failure retains last valid protection", model_calls[-1] == "b.json")
        failures.clear()
        saved_revision = status()["policy"]["revision"]
        proc.terminate()
        proc.wait(timeout=15)
        proc = start()
        s = wait_for(lambda s: len(s["accounts"]) == 3 and all(a["buckets"] for a in s["accounts"]))
        check("policy survives real CPA restart", s["policy"]["revision"] == saved_revision and s["policy"]["mode"] == "enforce")
        generate(model)
        check("protection survives CPA restart", model_calls[-1] == "b.json")
        def card(identity, hours):
            return {"id": identity, "status": "available", "reset_type": "codex_rate_limits",
                    "expires_at": (datetime.now(timezone.utc) + timedelta(hours=hours)).isoformat()}
        # Disable the account before publishing its due card: the scheduler is
        # independent of PUT policy and can otherwise redeem between these steps.
        p = status()["policy"]
        p["auto_reset"]["accounts"] = {"b.json": False}
        save(p)
        reset_cards["a.json"] = [card("due-card", 5), card("future-card", 48)]
        reset_cards["b.json"] = [card("disabled-card", 4)]
        refresh()
        s = wait_for(lambda s: any(e["state"] == "success" for e in s["reset_events"]))
        check("server automatically redeems due card without an open panel", len(reset_calls) == 1 and reset_calls[0][0] == "a.json")
        check("redemption uses CPA management forwarding and persists upstream status",
              "/v8/management/requests/api-call" in (work / "cpa.log").read_text()
              and any(e.get("result", {}).get("management_status") == 200
                      and e.get("result", {}).get("http_status") == 200 for e in s["reset_events"]))
        check("future cards and disabled account are preserved", len(reset_cards["a.json"]) == 1 and len(reset_cards["b.json"]) == 1)
        s = wait_for(lambda s: any(a["account"]["name"] == "a.json" and not a["would_block"] for a in s["accounts"]))
        generate(model)
        check("successful reset refreshes real CPA routing and quota", model_calls[-1] == "a.json")
        proc.terminate()
        proc.wait(timeout=15)
        proc = start()
        s = wait_for(lambda s: len(s["accounts"]) == 3 and all(a["buckets"] for a in s["accounts"]))
        check("durable reset journal prevents redemption after restart", len(reset_calls) == 1 and any(e["state"] == "success" for e in s["reset_events"]))
        reset_failure = True
        reset_cards["a.json"].insert(0, card("rejected-card", 3))
        refresh()
        def reset_account(snapshot):
            return next(a for a in snapshot["accounts"] if a["account"]["name"] == "a.json")["reset"]
        s = wait_for(lambda s: reset_account(s).get("last_result", {}).get("code") == "policy_denied")
        check("real CPA forwards and journals nested upstream errors",
              reset_account(s)["last_result"]["http_status"] == 403
              and "fixture reset rejected" in reset_account(s)["error"])
        refresh()
        s = wait_for(lambda s: "fixture reset rejected" in reset_account(s).get("error", ""))
        check("successful card list does not clear failed redemption", reset_account(s)["error"] != "")
        proc.terminate()
        proc.wait(timeout=15)
        proc = start()
        s = wait_for(lambda s: any(a.get("reset", {}).get("last_result", {}).get("code") == "policy_denied" for a in s["accounts"]))
        check("redemption failure survives real CPA restart", reset_account(s)["last_result"]["http_status"] == 403)
        # Leave a safe observation fixture for the browser acceptance test.
        p = status()["policy"]
        p["mode"] = "observe"
        save(p)
        report = {"checks": results, "count": len(results), "plugin_sha256": hashlib.sha256(Path(args.plugin).read_bytes()).hexdigest()}
        args.report.parent.mkdir(parents=True, exist_ok=True)
        args.report.write_text(json.dumps(report, indent=2))
        print("INTEGRATION_OK", len(results), "workdir", work, flush=True)
        if args.serve:
            print("BROWSER_FIXTURE_READY", BASE, flush=True)
            try:
                while proc.poll() is None:
                    time.sleep(1)
            except KeyboardInterrupt:
                pass
    finally:
        if proc and proc.poll() is None:
            proc.terminate()
            try:
                proc.wait(timeout=15)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait()
        for server in (probe_server, proxy_server):
            server.shutdown()
            server.server_close()
        log.close()


if __name__ == "__main__":
    main()
