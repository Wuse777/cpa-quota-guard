"""Read-only plan/doctor by default; apply and rollback verify CPA recovery."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess
import tempfile
import time
import urllib.error
import urllib.request

import yaml
from cpamc_bridge import inject_session_bridge, panel_diagnostics

VERSION = "0.3.0"
ROOT = Path(__file__).resolve().parent


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
        directory = os.open(path.parent, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(name):
            os.unlink(name)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def management_get(port, key, path):
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    req = urllib.request.Request(f"http://127.0.0.1:{port}/v0/management/{path}",
                                 headers={"Authorization": "Bearer " + key})
    with opener.open(req, timeout=3) as response:
        return json.load(response), {name.lower(): value for name, value in response.headers.items()}


def restart(service):
    subprocess.run(["systemctl", "restart", service], check=True)
    subprocess.run(["systemctl", "is-active", "--quiet", service], check=True)


def verify_registration(port, key, expected_version=VERSION, plugin_present=True):
    """Read-only readiness check; never refreshes quotas or consumes cards."""
    last = "not ready"
    for _ in range(20):
        try:
            if plugin_present:
                status, _ = management_get(port, key, "plugins/quota-guard/status")
                if status.get("version") == expected_version and isinstance(status.get("policy"), dict):
                    return status
                last = "unexpected plugin version or policy"
            else:
                management_get(port, key, "config")
                try:
                    management_get(port, key, "plugins/quota-guard/status")
                except urllib.error.HTTPError as error:
                    if error.code == 404:
                        return None
                    raise
                last = "plugin route is still registered"
        except (OSError, ValueError) as error:
            last = "HTTP " + str(error.code) if isinstance(error, urllib.error.HTTPError) else type(error).__name__
        time.sleep(.5)
    raise RuntimeError("Management readiness failed: " + last)


def snapshot_runtime(port, key):
    _, headers = management_get(port, key, "config")
    baseline = {"port": port, "cpa_version": headers.get("x-cpa-version", "unknown"), "plugin_present": False}
    try:
        status, _ = management_get(port, key, "plugins/quota-guard/status")
        baseline.update(plugin_present=True, version=status.get("version"))
        if not baseline["version"]:
            raise RuntimeError("Existing plugin has no version; cannot verify recovery")
    except urllib.error.HTTPError as error:
        if error.code != 404:
            raise
    return baseline


def sha256(raw):
    return hashlib.sha256(raw).hexdigest()


def load_json(path):
    return json.loads(Path(path).read_text(encoding="utf-8")) if path else {}


def validate_artifact(binary, manifest_path=None):
    raw = Path(binary).read_bytes()
    if len(raw) < 20 or raw[:6] != b"\x7fELF\x02\x01" or raw[18:20] != b"\x3e\x00":
        raise ValueError("Expected an ELF64 little-endian Linux amd64 shared library")
    manifest_path = Path(manifest_path) if manifest_path else Path(binary).parent / "BUILD.json"
    manifest = load_json(manifest_path)
    if manifest.get("version") != VERSION or manifest.get("target") != "linux-amd64-glibc":
        raise ValueError("Build manifest version or target does not match this installer")
    if manifest.get("sha256") != sha256(raw):
        raise ValueError("Shared library SHA256 does not match BUILD.json")
    if manifest.get("abi_version") != 1 or manifest.get("schema_version") != 6:
        raise ValueError("Unsupported plugin ABI/schema in BUILD.json")
    if not manifest.get("commit") or not manifest.get("built_at"):
        raise ValueError("BUILD.json must identify its commit and build time")
    package_manifest = Path(binary).parent.parent / "MANIFEST.json"
    if package_manifest.exists():
        package = load_json(package_manifest)
        if package.get("version") != VERSION:
            raise ValueError("Package manifest version mismatch")
        for item in package.get("files", []):
            relative = Path(item["path"])
            path = package_manifest.parent / relative
            if relative.is_absolute() or ".." in relative.parts or any(
                    (package_manifest.parent / Path(*relative.parts[:i])).is_symlink()
                    for i in range(1, len(relative.parts) + 1)):
                raise ValueError("Invalid package manifest path")
            data = path.read_bytes()
            if len(data) != item["size"] or sha256(data) != item["sha256"]:
                raise ValueError("Package integrity check failed: " + item["path"])
    return raw, {name: manifest[name] for name in
                 ("version", "target", "commit", "built_at", "sha256", "abi_version", "schema_version")}


def plugin_preflight(config, directory, host_plugins=None, declarations=None):
    """Host flags are preferred; CPA 8.0.35 needs binary-bound declarations.

    A plugin name, menu or priority is never evidence that it has no scheduler.
    """
    host_entries = {item.get("id"): item for item in (host_plugins or {}).get("plugins", [])}
    declarations = (declarations or {}).get("plugins", {})
    result = []
    for plugin_id, settings in config.get("plugins", {}).get("configs", {}).items():
        if not isinstance(settings, dict):
            raise ValueError("Existing plugin settings must be an object")
        if plugin_id == "quota-guard" or not settings.get("enabled"):
            continue
        if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]*", plugin_id):
            raise ValueError("Invalid configured plugin ID")
        item = host_entries.get(plugin_id, {})
        # Discovery entries may carry default false flags before registration.
        # Only an active registration is evidence of implemented capabilities.
        scheduler = item.get("supports_scheduler") if item.get("registered") is True else None
        source = "host"
        if item.get("registered") is True and not isinstance(scheduler, bool) and isinstance(item.get("capabilities"), dict):
            scheduler = item["capabilities"].get("scheduler", False)
        path = Path(item.get("path") or directory / (plugin_id + ".so"))
        digest = sha256(path.read_bytes()) if path.is_file() and path.resolve().is_relative_to(directory.resolve()) else None
        if not isinstance(scheduler, bool):
            declaration = declarations.get(plugin_id, {})
            if digest and declaration.get("sha256") == digest and isinstance(declaration.get("scheduler"), bool):
                scheduler, source = declaration["scheduler"], "sha256-bound declaration"
            else:
                scheduler, source = None, "unknown; supply --plugin-capabilities bound to installed SHA256"
        result.append({"id": plugin_id, "scheduler": scheduler, "source": source,
                       "sha256": digest, "compatible": scheduler is False})
    return result


def policy_summary(state_dir, auto_reset=None, reset_accounts=()):
    path = state_dir / "policy.json"
    if path.exists():
        if auto_reset is not None or reset_accounts:
            raise ValueError("Existing policy is preserved; change automatic reset in CPAMC instead")
        value = load_json(path)
        auto = value.get("auto_reset", {"enabled": True, "before_hours": 6, "accounts": {}})
        return {"source": "existing policy; unchanged", "quota_mode": value.get("mode", "observe"),
                "auto_reset_enabled": auto.get("enabled", True),
                "include_new_accounts": auto.get("include_new_accounts", True),
                "auto_reset_before_hours": auto.get("before_hours", 6),
                "explicit_reset_accounts": sum(bool(v) for v in auto.get("accounts", {}).values())}, None
    if auto_reset == "on" and not reset_accounts:
        raise ValueError("First-install --auto-reset on also requires explicit --reset-account ACCOUNT")
    if reset_accounts and auto_reset != "on":
        raise ValueError("--reset-account requires --auto-reset on")
    value = load_json(ROOT / "policy.safe-start.example.json")
    value["auto_reset"].update(enabled=auto_reset == "on", include_new_accounts=False,
                                 accounts={name: True for name in reset_accounts})
    return {"source": "new safe policy", "quota_mode": value["mode"],
            "auto_reset_enabled": value["auto_reset"]["enabled"], "include_new_accounts": False,
            "auto_reset_before_hours": value["auto_reset"]["before_hours"],
            "explicit_reset_accounts": len(reset_accounts)}, value


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
        atomic_write(target, (backup / "cpa-management.key").read_bytes(), meta["mode"], meta["uid"], meta["gid"])
    elif target.exists():
        target.unlink()


def file_stat(path):
    st = Path(path).stat()
    return {"mode": st.st_mode & 0o777, "uid": st.st_uid, "gid": st.st_gid}


def recover(backup, info, key):
    config = Path(info["config"])
    meta = info.get("config_stat") or file_stat(config)
    atomic_write(config, (backup / "config.yaml").read_bytes(), meta["mode"], meta["uid"], meta["gid"])
    restore_plugin(Path(info["plugin"]), backup, info)
    restore_panel(backup, info)
    restore_management_key(backup, info)
    print("RECOVERY files restored; policy and journal retained.", flush=True)
    restart(info.get("service", "cliproxyapi"))
    baseline = info.get("baseline")
    if not baseline:
        raise RuntimeError("Old backup has no API baseline; verify its API manually after recovery")
    verify_registration(baseline["port"], key, baseline.get("version"), baseline["plugin_present"])
    print("RECOVERY service active and original API version verified.", flush=True)


def read_key(config, key_file=None):
    key = config.get("management", {}).get("secret-key") or config.get("remote-management", {}).get("secret-key", "")
    if key_file:
        key = Path(key_file).read_text().strip()
    return key if key and not key.startswith("$2") else None


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--config", type=Path, default=Path("/etc/cliproxyapi/config.yaml"))
    ap.add_argument("--binary", type=Path, default=ROOT / "dist/quota-guard.so")
    ap.add_argument("--artifact-manifest", type=Path, help="defaults to BUILD.json beside --binary")
    ap.add_argument("--panel", type=Path, default=Path("/opt/cliproxyapi/static/management.html"))
    ap.add_argument("--panel-extensions", type=Path, help="JSON declarations binding marked panel extensions to SHA256")
    ap.add_argument("--plugin-capabilities", type=Path, help="JSON capabilities bound to installed plugin binary SHA256")
    ap.add_argument("--state-dir", type=Path, default=Path("/var/lib/cliproxyapi/quota-guard"))
    ap.add_argument("--proxy-url", help="defaults to CPA requests.proxy-url; use direct for no proxy")
    ap.add_argument("--service", default="cliproxyapi")
    ap.add_argument("--service-user", default="cliproxyapi")
    ap.add_argument("--backup-dir", type=Path, default=Path("/root/cliproxyapi-plugin-backups"))
    ap.add_argument("--management-key-file", type=Path)
    ap.add_argument("--auto-reset", choices=("off", "on"), help="first installation only; default off")
    ap.add_argument("--reset-account", action="append", default=[], help="explicit first-install account with --auto-reset on")
    modes = ap.add_mutually_exclusive_group()
    modes.add_argument("--apply", action="store_true")
    modes.add_argument("--rollback", type=Path)
    modes.add_argument("--doctor", action="store_true")
    args = ap.parse_args()
    config = yaml.safe_load(args.config.read_text()) if args.config.exists() else {}
    if not isinstance(config, dict):
        raise SystemExit("CPA configuration must be a YAML object")
    key_error = None
    try:
        key = read_key(config, args.management_key_file)
    except OSError:
        if not args.doctor:
            raise
        key, key_error = None, "management key file unavailable"
    if args.rollback:
        backup = args.rollback.resolve()
        info = load_json(backup / "manifest.json")
        if not key:
            restored_key = backup / "cpa-management.key"
            key = read_key(yaml.safe_load((backup / "config.yaml").read_text()), restored_key if restored_key.exists() else None)
        if not key:
            raise SystemExit("Rollback verification needs --management-key-file containing the actual CPA key")
        print("RECOVERY backup: " + str(backup), flush=True)
        recover(backup, info, key)
        return
    plugins = config.get("plugins", {})
    if not isinstance(plugins, dict) or not isinstance(plugins.get("configs", {}), dict):
        raise SystemExit("Existing plugins configuration is invalid")
    directory = Path(plugins.get("dir") or "/opt/cliproxyapi/plugins")
    port = config.get("server", {}).get("port", config.get("port", 8317))
    diagnostics = {"installer_version": VERSION, "platform": platform.system(), "machine": platform.machine(),
                   "config_found": args.config.exists(), "management": "not queried; actual key unavailable"}
    if key_error:
        diagnostics["management"] = key_error
    host_plugins = None
    if key:
        try:
            host_plugins, headers = management_get(port, key, "plugins")
            diagnostics.update(management="reachable", cpa_version=headers.get("x-cpa-version", "unknown"),
                               cpa_commit=headers.get("x-cpa-commit", "unknown"))
        except (OSError, ValueError):
            diagnostics["management"] = "unavailable (read-only check)"
    extensions = load_json(args.panel_extensions)
    try:
        panel_raw = args.panel.read_bytes()
        diagnostics["panel"] = {"supported": False, "installed_sha256": sha256(panel_raw),
                                "bridge_present": b"quota-guard-session-bridge:start" in panel_raw}
        try:
            diagnostics["panel"] = panel_diagnostics(panel_raw, extensions)
        except ValueError as error:
            diagnostics["panel"]["error"] = str(error)
    except OSError as error:
        diagnostics["panel"] = {"supported": False, "error": str(error)}
    preflight = plugin_preflight(config, directory, host_plugins, load_json(args.plugin_capabilities))
    diagnostics["other_plugins"] = preflight
    try:
        artifact, build = validate_artifact(args.binary, args.artifact_manifest)
        diagnostics["build"] = build
    except (OSError, ValueError, KeyError) as error:
        artifact = None
        diagnostics["build"] = {"valid": False, "error": str(error)}
    if args.doctor:
        installed_binary = directory / "quota-guard.so"
        diagnostics["installed_binary_sha256"] = sha256(installed_binary.read_bytes()) if installed_binary.is_file() else None
        try:
            status, _ = management_get(port, key, "plugins/quota-guard/status") if key else ({}, {})
            diagnostics["running_build"] = status.get("build", {"version": status.get("version", "not available")})
        except (OSError, ValueError):
            diagnostics["running_build"] = {"available": False}
        print(json.dumps(diagnostics, indent=2, ensure_ascii=False))
        return
    if not args.config.exists():
        raise SystemExit("CPA configuration does not exist")
    if not directory.is_absolute() or not args.state_dir.is_absolute():
        raise SystemExit("Plugin and state directories must be absolute")
    if args.state_dir.is_symlink() or (args.state_dir / "cpa-management.key").is_symlink():
        raise SystemExit("State directory and management key must not be symlinks")
    if artifact is None:
        raise SystemExit("Artifact preflight failed: " + diagnostics["build"]["error"])
    panel_before = args.panel.read_bytes()
    panel_after = inject_session_bridge(panel_before, extensions)
    policy, initial_policy = policy_summary(args.state_dir, args.auto_reset, args.reset_account)
    print(json.dumps({"config": str(args.config), "plugin": str(directory / "quota-guard.so"),
                      "panel": str(args.panel), "state_dir": str(args.state_dir), "restart": args.service,
                      "policy": policy, "compatibility": diagnostics}, indent=2, ensure_ascii=False), flush=True)
    if not args.apply:
        print("Plan only. Unknown capabilities require a SHA256-bound declaration before --apply.")
        return
    if platform.system() != "Linux" or platform.machine().lower() not in ("x86_64", "amd64"):
        raise SystemExit("Installation requires Linux amd64/glibc")
    if any(not item["compatible"] for item in preflight):
        raise SystemExit("Plugin preflight blocked: scheduler conflict or unknown capabilities; see the plan")
    if not key:
        raise SystemExit("Supply the actual management key via --management-key-file before applying")
    baseline = snapshot_runtime(port, key)
    if str(baseline["cpa_version"]).lstrip("v") != "8.0.35":
        raise SystemExit("Unsupported CPA version; expected verified 8.0.35 before changing files")
    import pwd
    account = pwd.getpwnam(args.service_user)
    created_plugin_dir = not directory.exists()
    directory.mkdir(parents=True, exist_ok=True)
    if created_plugin_dir:
        directory.chmod(0o755)
    args.state_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
    os.chown(args.state_dir, account.pw_uid, account.pw_gid)
    os.chmod(args.state_dir, 0o700)
    args.backup_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
    backup = Path(tempfile.mkdtemp(prefix="quota-guard-", dir=args.backup_dir))
    print("BACKUP " + str(backup), flush=True)
    target = directory / "quota-guard.so"
    management_key = args.state_dir / "cpa-management.key"
    shutil.copy2(args.config, backup / "config.yaml")
    shutil.copy2(args.panel, backup / "management.html")
    info = {"config": str(args.config.resolve()), "config_stat": file_stat(args.config),
            "plugin": str(target), "had_plugin": target.exists(), "service": args.service,
            "panel": str(args.panel.resolve()), "panel_stat": file_stat(args.panel),
            "management_key": str(management_key), "had_management_key": management_key.exists(),
            "baseline": baseline}
    if target.exists():
        shutil.copy2(target, backup / "quota-guard.so")
        info["plugin_stat"] = file_stat(target)
    if management_key.exists():
        shutil.copy2(management_key, backup / "cpa-management.key")
        info["management_key_stat"] = file_stat(management_key)
    atomic_write(backup / "manifest.json", json.dumps(info).encode(), 0o600)
    print("BACKUP complete; starting file changes.", flush=True)
    plugins["enabled"] = True
    plugins["dir"] = str(directory)
    entry = plugins.setdefault("configs", {}).setdefault("quota-guard", {})
    proxy = args.proxy_url if args.proxy_url is not None else config.get("requests", {}).get("proxy-url", config.get("proxy-url", ""))
    entry.update(enabled=True, priority=100, state_dir=str(args.state_dir), proxy_url=proxy,
                 management_url=f"http://127.0.0.1:{port}", management_key_file=str(management_key))
    config["plugins"] = plugins
    config.setdefault("management", {})["disable-auto-update-panel"] = True
    try:
        if initial_policy is not None:
            atomic_write(args.state_dir / "policy.json", (json.dumps(initial_policy, indent=2) + "\n").encode(),
                         0o600, account.pw_uid, account.pw_gid)
        atomic_write(management_key, key.encode(), 0o600, account.pw_uid, account.pw_gid)
        atomic_write(target, artifact, 0o755)
        meta = info["panel_stat"]
        atomic_write(args.panel, panel_after, meta["mode"], meta["uid"], meta["gid"])
        meta = info["config_stat"]
        atomic_write(args.config, yaml.safe_dump(config, allow_unicode=True, sort_keys=False).encode(),
                     meta["mode"], meta["uid"], meta["gid"])
        print("INSTALL files written; restarting CPA.", flush=True)
        restart(args.service)
        status = verify_registration(port, key)
        if status.get("build", {}).get("commit") != diagnostics["build"]["commit"] or status.get("build", {}).get("built_at") != diagnostics["build"]["built_at"]:
            raise RuntimeError("Registered build identity does not match the verified artifact")
        print("INSTALL service active; plugin API version verified.", flush=True)
    except Exception as error:
        print("INSTALL failed (" + type(error).__name__ + "); restoring backup " + str(backup), flush=True)
        try:
            recover(backup, info, key)
        except Exception as recovery_error:
            raise RuntimeError("Installation failed and recovery could not be verified. Backup: " + str(backup) +
                               "; recovery: " + str(recovery_error)) from error
        raise RuntimeError("Installation failed; original service/API restored. Backup: " + str(backup)) from error
    actual = status.get("policy", {})
    print(json.dumps({"installed": VERSION, "backup": str(backup),
                      "quota_mode": actual.get("mode", policy["quota_mode"]),
                      "auto_reset_enabled": actual.get("auto_reset", {}).get("enabled", policy["auto_reset_enabled"]),
                      "include_new_accounts": actual.get("auto_reset", {}).get("include_new_accounts", policy["include_new_accounts"]),
                      "policy_and_journal": "existing files preserved; no refresh or consume test requested"},
                     indent=2, ensure_ascii=False))


if __name__ == "__main__":
    main()
