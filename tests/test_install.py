"""Exercise install/rollback on temporary files; systemd and HTTP are mocked."""
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import pwd
import sys
import tempfile
import unittest
from unittest.mock import patch

import yaml

spec = importlib.util.spec_from_file_location("quota_guard_install", Path(__file__).parents[1] / "install.py")
install = importlib.util.module_from_spec(spec)
spec.loader.exec_module(install)


class InstallerTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="quota-guard-installer-")
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.config = self.root / "config.yaml"
        self.plugin_dir = self.root / "plugins"
        self.plugin_dir.mkdir()
        self.target = self.plugin_dir / "quota-guard.so"
        self.binary = self.root / "build.so"
        self.binary.write_bytes(b"\x7fELF" + b"fake-fixture-only")
        self.state = self.root / "state"
        self.backups = self.root / "backups"
        self.original = yaml.safe_dump({
            "server": {"port": 18317}, "management": {"secret-key": "$2a$hashed"},
            "requests": {"proxy-url": "http://127.0.0.1:7890"},
            "plugins": {"enabled": False, "dir": str(self.plugin_dir)}}).encode()
        self.config.write_bytes(self.original)
        self.key = self.root / "management.key"
        self.key.write_text("fixture-key-only")
        self.panel = self.root / "management.html"
        self.original_panel = b'<html><script type="module">fixture</script></html>'
        self.panel.write_bytes(self.original_panel)
        self.args = ["install.py", "--config", str(self.config), "--binary", str(self.binary),
                     "--panel", str(self.panel), "--state-dir", str(self.state), "--backup-dir", str(self.backups),
                     "--service", "test-cpa", "--service-user", pwd.getpwuid(os.getuid()).pw_name]

    def run_main(self, extra=(), verify_error=None):
        with patch.object(sys, "argv", self.args + list(extra)), \
                patch.object(install.subprocess, "run") as systemctl, \
                patch.object(install, "verify_registration", side_effect=verify_error) as verify, \
                patch.object(install, "inject_session_bridge", return_value=b"patched-fixture") as bridge, \
                contextlib.redirect_stdout(io.StringIO()):
            install.main()
            return systemctl, verify

    def apply(self, **kwargs):
        return self.run_main(["--apply", "--management-key-file", str(self.key)], **kwargs)

    def test_plan_is_read_only(self):
        systemctl, verify = self.run_main()
        self.assertEqual(self.config.read_bytes(), self.original)
        self.assertFalse(self.target.exists())
        self.assertFalse(self.state.exists())
        self.assertFalse(self.backups.exists())
        systemctl.assert_not_called()
        verify.assert_not_called()

    def test_hashed_key_requires_actual_key_before_mutation(self):
        with self.assertRaises(SystemExit):
            self.run_main(["--apply"])
        self.assertEqual(self.config.read_bytes(), self.original)
        self.assertFalse(self.target.exists())
        self.assertFalse(self.backups.exists())

    def test_new_plugin_directory_remains_readable_under_private_umask(self):
        self.plugin_dir.rmdir()
        previous = os.umask(0o077)
        try:
            self.apply()
        finally:
            os.umask(previous)
        self.assertEqual(self.plugin_dir.stat().st_mode & 0o777, 0o755)

    def test_install_preserves_state_and_rollback_restores_config(self):
        self.state.mkdir()
        policy = self.state / "policy.json"
        policy.write_text('{"fixture":"existing"}')
        systemctl, verify = self.apply()
        self.assertEqual(self.target.read_bytes(), self.binary.read_bytes())
        current = yaml.safe_load(self.config.read_bytes())
        self.assertTrue(current["plugins"]["enabled"])
        self.assertEqual(current["plugins"]["configs"]["quota-guard"]["proxy_url"], "http://127.0.0.1:7890")
        self.assertEqual(policy.read_text(), '{"fixture":"existing"}')
        self.assertEqual(self.panel.read_bytes(), b"patched-fixture")
        self.assertEqual(self.state.stat().st_mode & 0o777, 0o700)
        verify.assert_called_once_with(18317, "fixture-key-only")
        backup, = self.backups.iterdir()
        self.assertEqual(backup.stat().st_mode & 0o777, 0o700)
        rollback_calls, _ = self.run_main(["--rollback", str(backup)])
        self.assertEqual(self.config.read_bytes(), self.original)
        self.assertEqual(self.panel.read_bytes(), self.original_panel)
        self.assertFalse(self.target.exists())
        self.assertTrue(policy.exists())
        self.assertEqual(rollback_calls.call_args_list[0].args[0], ["systemctl", "restart", "test-cpa"])

    def test_upgrade_rollback_restores_old_library_and_mode(self):
        self.target.write_bytes(b"old-plugin")
        self.target.chmod(0o750)
        self.apply()
        backup, = self.backups.iterdir()
        self.run_main(["--rollback", str(backup)])
        self.assertEqual(self.target.read_bytes(), b"old-plugin")
        self.assertEqual(self.target.stat().st_mode & 0o777, 0o750)

    def test_failed_registration_automatically_restores_previous_files(self):
        self.target.write_bytes(b"old-plugin")
        with self.assertRaisesRegex(RuntimeError, "fixture-registration-failed"):
            self.apply(verify_error=RuntimeError("fixture-registration-failed"))
        self.assertEqual(self.config.read_bytes(), self.original)
        self.assertEqual(self.target.read_bytes(), b"old-plugin")
        self.assertEqual(self.panel.read_bytes(), self.original_panel)

    def test_failed_first_install_removes_unregistered_library(self):
        with self.assertRaises(RuntimeError):
            self.apply(verify_error=RuntimeError("fixture-registration-failed"))
        self.assertEqual(self.config.read_bytes(), self.original)
        self.assertFalse(self.target.exists())


if __name__ == "__main__":
    unittest.main(verbosity=2)
