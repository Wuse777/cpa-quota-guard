"""Install the built plugin on the Linux CPA host, with a reversible backup.

Default is a read-only plan. --apply installs and restarts CPA; --rollback BACKUP
restores the previous config/plugin and restarts CPA. Requires Python 3 + PyYAML.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import pwd
import shutil
import subprocess
import tempfile
import time
import urllib.request

import yaml

from cpamc_bridge import inject_session_bridge

VERSION = "0.2.2"


def atomic_write(path, data, mode=0o640, uid=None, gid=None):
    path = Path(path)
    fd, name = tempfile.mkstemp(prefix="." + path.name + "-", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as f:
            os.fchmod(f.fileno(), mode)
            if uid is not None:
                os.fchown(f.fileno(), uid, gid)
            f.write(data)
            f.flush()
            os.fsync(f.fileno())
        os.replace(name, path)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def restart(service):
    subprocess.run(["systemctl", "restart", service], check=True)
    subprocess.run(["systemctl", "is-active", "--quiet", service], check=True)


def verify_registration(port, key):
    # Ignore shell HTTP_PROXY for the local management endpoint.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    for attempt in range(20):
        try:
            req = urllib.request.Request(f"http://127.0.0.1:{port}/v0/management/plugins/quota-guard/status",
                                         headers={"Authorization": "Bearer " + key})
            with opener.open(req, timeout=3) as response:
                status = json.load(response)
                if response.status == 200 and status.get("version") == VERSION and "policy" in status:
                    return
        except (OSError, ValueError):
            time.sleep(.5)
    raise RuntimeError("Plugin did not register; restoring previous configuration")


def restore_plugin(target, backup, info):
    if info["had_plugin"]:
        meta = info.get("plugin_stat", {})
        atomic_write(target, (backup / "quota-guard.so").read_bytes(), meta.get("mode", 0o755),
                     meta.get("uid"), meta.get("gid"))
    elif target.exists():
        target.unlink()


def restore_panel(backup, info):
    if "panel" in info:
        meta = info["panel_stat"]
        atomic_write(Path(info["panel"]), (backup / "management.html").read_bytes(),
                     meta["mode"], meta["uid"], meta["gid"])


def restore_management_key(backup, info):
    if "management_key" not in info:
        return
    target = Path(info["management_key"])
    if info["had_management_key"]:
        meta = info["management_key_stat"]
        atomic_write(target, (backup / "cpa-management.key").read_bytes(),
                     meta["mode"], meta["uid"], meta["gid"])
    elif target.exists():
        target.unlink()


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--config", type=Path, default=Path("/etc/cliproxyapi/config.yaml"))
    ap.add_argument("--binary", type=Path, default=Path(__file__).parent / "dist/quota-guard.so")
    ap.add_argument("--panel", type=Path, default=Path("/opt/cliproxyapi/static/management.html"),
                    help="CPAMC 1.26.5 HTML to add authenticated plugin session support")
    ap.add_argument("--state-dir", type=Path, default=Path("/var/lib/cliproxyapi/quota-guard"))
    ap.add_argument("--proxy-url", help="defaults to CPA requests.proxy-url; use direct for no proxy")
    ap.add_argument("--service", default="cliproxyapi")
    ap.add_argument("--service-user", default="cliproxyapi")
    ap.add_argument("--backup-dir", type=Path, default=Path("/root/cliproxyapi-plugin-backups"))
    ap.add_argument("--management-key-file", type=Path, help="required on apply when config stores a hashed key")
    ap.add_argument("--apply", action="store_true")
    ap.add_argument("--rollback", type=Path)
    args = ap.parse_args()
    if args.apply and args.rollback:
        ap.error("--apply and --rollback are mutually exclusive")
    if args.rollback:
        backup = args.rollback.resolve()
        info = json.loads((backup / "manifest.json").read_text())
        config = Path(info["config"])
        target = Path(info["plugin"])
        st = config.stat()
        atomic_write(config, (backup / "config.yaml").read_bytes(), st.st_mode & 0o777,
                     st.st_uid, st.st_gid)
        restore_plugin(target, backup, info)
        restore_panel(backup, info)
        restore_management_key(backup, info)
        restart(info.get("service", "cliproxyapi"))
        print("Rollback complete; saved quota policy remains in state_dir.")
        return
    config = yaml.safe_load(args.config.read_text())
    if not isinstance(config, dict):
        raise SystemExit("CPA configuration must be a YAML object")
    plugins = config.get("plugins", {})
    if not isinstance(plugins, dict):
        raise SystemExit("Existing plugins configuration is invalid")
    if not isinstance(plugins.get("configs", {}), dict):
        raise SystemExit("Existing plugins.configs must be an object")
    for plugin_id, settings in plugins.get("configs", {}).items():
        if not isinstance(settings, dict):
            raise SystemExit("Existing plugin settings must be an object")
        if plugin_id != "quota-guard" and settings.get("enabled"):
            raise SystemExit("Another plugin is enabled; inspect scheduler compatibility before installation.")
    directory = Path(plugins.get("dir") or "/opt/cliproxyapi/plugins")
    if not directory.is_absolute():
        raise SystemExit("Set plugins.dir to an absolute path before using this installer.")
    artifact = args.binary.read_bytes()
    if not artifact.startswith(b"\x7fELF"):
        raise SystemExit("Expected a Linux ELF shared library")
    if args.state_dir.is_symlink():
        raise SystemExit("state_dir must not be a symlink")
    if not args.state_dir.is_absolute():
        raise SystemExit("state_dir must be an absolute path")
    target = directory / "quota-guard.so"
    management_key = args.state_dir / "cpa-management.key"
    if management_key.is_symlink():
        raise SystemExit("management key file must not be a symlink")
    panel_before = args.panel.read_bytes()
    panel_after = inject_session_bridge(panel_before)
    print(json.dumps({"config": str(args.config), "plugin": str(target),
                      "panel": str(args.panel), "panel_session_bridge": True,
                      "state_dir": str(args.state_dir), "sha256": hashlib.sha256(artifact).hexdigest(),
                      "restart": args.service, "initial_mode": "observe (or existing saved policy)"}, indent=2))
    if not args.apply:
        print("Plan only. Run again with --apply to install.")
        return
    key = config.get("management", {}).get("secret-key") or config.get("remote-management", {}).get("secret-key", "")
    if args.management_key_file:
        key = args.management_key_file.read_text().strip()
    if not key or key.startswith("$2"):
        raise SystemExit("Supply the actual management key via --management-key-file before applying.")
    account = pwd.getpwnam(args.service_user)
    created_plugin_dir = not directory.exists()
    directory.mkdir(parents=True, exist_ok=True)
    # Root's restrictive umask must not hide shared libraries from the CPA service user.
    if created_plugin_dir:
        directory.chmod(0o755)
    args.state_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
    os.chown(args.state_dir, account.pw_uid, account.pw_gid)
    os.chmod(args.state_dir, 0o700)
    args.backup_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
    backup = Path(tempfile.mkdtemp(prefix="quota-guard-", dir=args.backup_dir))
    shutil.copy2(args.config, backup / "config.yaml")
    shutil.copy2(args.panel, backup / "management.html")
    had_plugin = target.exists()
    if had_plugin:
        shutil.copy2(target, backup / "quota-guard.so")
    info = {"config": str(args.config.resolve()), "plugin": str(target), "had_plugin": had_plugin,
            "service": args.service}
    panel_stat = args.panel.stat()
    info["panel"] = str(args.panel.resolve())
    info["panel_stat"] = {"mode": panel_stat.st_mode & 0o777, "uid": panel_stat.st_uid, "gid": panel_stat.st_gid}
    if had_plugin:
        old = target.stat()
        info["plugin_stat"] = {"mode": old.st_mode & 0o777, "uid": old.st_uid, "gid": old.st_gid}
    info["management_key"] = str(management_key)
    info["had_management_key"] = management_key.exists()
    if management_key.exists():
        old_key = management_key.stat()
        info["management_key_stat"] = {"mode": old_key.st_mode & 0o777, "uid": old_key.st_uid, "gid": old_key.st_gid}
        shutil.copy2(management_key, backup / "cpa-management.key")
    (backup / "manifest.json").write_text(json.dumps(info))
    plugins["enabled"] = True
    plugins["dir"] = str(directory)
    entry = plugins.setdefault("configs", {}).setdefault("quota-guard", {})
    proxy = args.proxy_url if args.proxy_url is not None else config.get("requests", {}).get("proxy-url", config.get("proxy-url", ""))
    entry.update(enabled=True, priority=100, state_dir=str(args.state_dir), proxy_url=proxy)
    port = config.get("server", {}).get("port", config.get("port", 8317))
    entry.update(management_url=f"http://127.0.0.1:{port}", management_key_file=str(management_key))
    config["plugins"] = plugins
    config.setdefault("management", {})["disable-auto-update-panel"] = True
    st = args.config.stat()
    try:
        atomic_write(management_key, key.encode(), 0o600, account.pw_uid, account.pw_gid)
        atomic_write(target, artifact, 0o755)
        atomic_write(args.panel, panel_after, panel_stat.st_mode & 0o777, panel_stat.st_uid, panel_stat.st_gid)
        atomic_write(args.config, yaml.safe_dump(config, allow_unicode=True, sort_keys=False).encode(),
                     st.st_mode & 0o777, st.st_uid, st.st_gid)
        restart(args.service)
        port = config.get("server", {}).get("port", config.get("port", 8317))
        verify_registration(port, key)
    except Exception:
        atomic_write(args.config, (backup / "config.yaml").read_bytes(), st.st_mode & 0o777, st.st_uid, st.st_gid)
        restore_plugin(target, backup, info)
        restore_panel(backup, info)
        restore_management_key(backup, info)
        subprocess.run(["systemctl", "restart", args.service], check=False)
        raise
    print("Installed. Backup:", backup)
    print("CPAMC → 插件 → 账号额度保护。首次安装默认为只观察。")


if __name__ == "__main__":
    main()
