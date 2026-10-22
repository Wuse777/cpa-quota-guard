"""Opt-in Linux/root acceptance test using a separate temporary systemd service.

Never uses or restarts the production CPA unit/config/auth directory.
Uses an empty auth directory and sends no upstream requests.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

import yaml


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--plugin", required=True, type=Path)
    ap.add_argument("--cpa", default="/opt/cliproxyapi/cli-proxy-api")
    ap.add_argument("--panel", type=Path, default=Path("/opt/cliproxyapi/static/management.html"))
    ap.add_argument("--report", type=Path, default=Path("test-results/install-integration-report.json"))
    args = ap.parse_args()
    if os.geteuid() != 0:
        raise SystemExit("This isolated systemd test requires root")
    if not args.panel.is_file():
        ap.error("--panel must point to the verified CPAMC 1.26.5 HTML")
    work = Path(tempfile.mkdtemp(prefix="quota-guard-install-acceptance-"))
    unit_name = "quota-guard-install-acceptance"
    unit_file = Path("/run/systemd/system") / (unit_name + ".service")
    config = work / "config.yaml"
    (work / "auths").mkdir()
    (work / "static").mkdir()
    shutil.copy2(args.panel, work / 'static/management.html')
    key = "isolated-install-acceptance-only"
    (work / "key").write_text(key)
    (work / "key").chmod(0o600)
    config.write_text(yaml.safe_dump({
        "config-version": 8, "server": {"host": "127.0.0.1", "port": 18417},
        "management": {"secret-key": key, "disable-auto-update-panel": True, "allow-remote": False},
        "oauth": {"auth-dir": str(work / "auths")},
        "plugins": {"enabled": False, "dir": str(work / "plugins")}}))
    source = Path(__file__).parents[1]
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    checks = []

    def get(path):
        req = urllib.request.Request("http://127.0.0.1:18417" + path,
                                     headers={"Authorization": "Bearer " + key})
        with opener.open(req, timeout=3) as response:
            return json.load(response)

    def ready():
        for _ in range(50):
            try:
                get("/v0/management/config")
                return
            except OSError:
                time.sleep(.2)
        raise RuntimeError("isolated CPA did not become ready")

    def run(*args):
        subprocess.run(args, check=True, capture_output=True, text=True)

    created = False
    try:
        # Exclusive create prevents replacing any pre-existing test unit.
        with unit_file.open("x") as f:
            f.write("[Unit]\nDescription=Isolated quota guard installer acceptance\n"
                    "[Service]\nType=simple\nWorkingDirectory=" + str(work) + "\n"
                    "Environment=MANAGEMENT_STATIC_PATH=" + str(work / "static") + "\n"
                    "ExecStart=" + args.cpa + " --local-model --config " + str(config) + "\n")
        created = True
        run("systemctl", "daemon-reload")
        run("systemctl", "start", unit_name)
        ready()
        before = config.read_bytes()
        assert yaml.safe_load(before)["management"]["secret-key"].startswith("$2")
        checks.append("baseline isolated CPA running with hashed management key")
        cmd = [sys.executable, str(source / "install.py"), "--config", str(config),
               "--binary", str(args.plugin.resolve()), "--panel", str(work / "static/management.html"), "--state-dir", str(work / "state"),
               "--backup-dir", str(work / "backups"), "--service", unit_name,
               "--service-user", "root", "--management-key-file", str(work / "key")]
        run(*cmd, "--apply")
        status = get("/v0/management/plugins/quota-guard/status")
        assert status["version"] == "0.2.1" and status["policy"]["mode"] == "observe"
        assert status["accounts"] == []
        checks.append("installer copies real shared library, restarts isolated unit and verifies registration")
        # Persist a policy through the real plugin so rollback can prove it remains intact.
        p = status["policy"]
        req = urllib.request.Request("http://127.0.0.1:18417/v0/management/plugins/quota-guard/policy",
            data=json.dumps(p).encode(), method="PUT",
            headers={"Authorization": "Bearer " + key, "Content-Type": "application/json"})
        with opener.open(req, timeout=3) as response:
            assert response.status == 200
        saved = (work / "state/policy.json").read_bytes()
        backup, = (work / "backups").iterdir()
        run(*cmd, "--rollback", str(backup))
        ready()
        assert config.read_bytes() == before
        assert not (work / "plugins/quota-guard.so").exists()
        assert (work / "state/policy.json").read_bytes() == saved
        try:
            get("/v0/management/plugins/quota-guard/status")
            raise AssertionError("plugin route remained registered after rollback")
        except urllib.error.HTTPError as error:
            assert error.code == 404
        checks.append("rollback restores exact config, removes plugin, retains policy and restarts isolated unit")
        report = {"checks": checks, "count": len(checks),
                  "plugin_sha256": hashlib.sha256(args.plugin.read_bytes()).hexdigest(),
                  "production_unit_touched": False}
        args.report.parent.mkdir(parents=True, exist_ok=True)
        args.report.write_text(json.dumps(report, indent=2))
        for check in checks:
            print("PASS", check)
        print("INSTALL_INTEGRATION_OK", len(checks))
    finally:
        if created:
            subprocess.run(["systemctl", "stop", unit_name], check=False, capture_output=True)
            unit_file.unlink()
            subprocess.run(["systemctl", "daemon-reload"], check=False, capture_output=True)


if __name__ == "__main__":
    main()
