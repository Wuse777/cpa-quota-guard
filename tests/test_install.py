"""Preflight and transactional recovery against temporary files, with no CPA calls."""
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

import yaml

spec = importlib.util.spec_from_file_location('quota_guard_install', Path(__file__).parents[1] / 'install.py')
install = importlib.util.module_from_spec(spec)
spec.loader.exec_module(install)


def build_fixture(path):
    raw = b'\x7fELF\x02\x01' + bytes(12) + b'\x3e\x00' + b'fixture-only'
    path.write_bytes(raw)
    manifest = {'version': install.VERSION, 'target': 'linux-amd64-glibc', 'abi_version': 1,
                'schema_version': 6, 'sha256': install.sha256(raw), 'commit': 'fixture',
                'built_at': '2026-11-09T00:00:00Z'}
    (path.parent / 'BUILD.json').write_text(json.dumps(manifest))
    return manifest


class PreflightTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)

    def test_safe_first_install_and_explicit_accounts(self):
        summary, value = install.policy_summary(self.root)
        self.assertEqual(summary['quota_mode'], 'observe')
        self.assertFalse(summary['auto_reset_enabled'])
        self.assertFalse(value['auto_reset']['include_new_accounts'])
        with self.assertRaisesRegex(ValueError, 'explicit'):
            install.policy_summary(self.root, 'on')
        summary, value = install.policy_summary(self.root, 'on', ['a.json'])
        self.assertTrue(summary['auto_reset_enabled'])
        self.assertEqual(value['auto_reset']['accounts'], {'a.json': True})
        self.assertFalse(value['auto_reset']['include_new_accounts'])

    def test_legacy_policy_is_summarized_without_rewriting(self):
        path = self.root / 'policy.json'
        original = b'{"revision":10,"mode":"enforce","rules":[]}'
        path.write_bytes(original)
        summary, value = install.policy_summary(self.root)
        self.assertIsNone(value)
        self.assertTrue(summary['auto_reset_enabled'])
        self.assertTrue(summary['include_new_accounts'])
        self.assertEqual(summary['quota_mode'], 'enforce')
        self.assertEqual(path.read_bytes(), original)
        with self.assertRaisesRegex(ValueError, 'preserved'):
            install.policy_summary(self.root, 'off')

    def test_binary_architecture_manifest_and_hash_fail_before_mutation(self):
        binary = self.root / 'quota-guard.so'
        build_fixture(binary)
        install.validate_artifact(binary)
        binary.write_bytes(binary.read_bytes() + b'tampered')
        with self.assertRaisesRegex(ValueError, 'SHA256'):
            install.validate_artifact(binary)
        raw = bytearray(binary.read_bytes())
        raw[18:20] = b'\xb7\x00'
        binary.write_bytes(raw)
        with self.assertRaisesRegex(ValueError, 'amd64'):
            install.validate_artifact(binary)

    def test_host_scheduler_conflicts_and_non_scheduler_can_coexist(self):
        config = {'plugins': {'configs': {'keeper': {'enabled': True}, 'quota-guard': {'enabled': True}}}}
        for scheduler in (False, True):
            host = {'plugins': [{'id': 'keeper', 'registered': True, 'supports_scheduler': scheduler}]}
            item, = install.plugin_preflight(config, self.root, host)
            self.assertEqual(item['compatible'], not scheduler)
        host = {'plugins': [{'id': 'keeper', 'registered': True, 'capabilities': {'management_api': True}}]}
        self.assertTrue(install.plugin_preflight(config, self.root, host)[0]['compatible'])
        host['plugins'][0]['registered'] = False
        self.assertIsNone(install.plugin_preflight(config, self.root, host)[0]['scheduler'])

    def test_old_host_needs_exact_binary_bound_capabilities(self):
        binary = self.root / 'keeper.so'
        binary.write_bytes(b'keeper fixture')
        config = {'plugins': {'configs': {'keeper': {'enabled': True}}}}
        item, = install.plugin_preflight(config, self.root)
        self.assertIsNone(item['scheduler'])
        declarations = {'plugins': {'keeper': {'sha256': install.sha256(binary.read_bytes()), 'scheduler': False}}}
        self.assertTrue(install.plugin_preflight(config, self.root, declarations=declarations)[0]['compatible'])
        binary.write_bytes(b'changed fixture')
        self.assertFalse(install.plugin_preflight(config, self.root, declarations=declarations)[0]['compatible'])

    def test_doctor_without_host_files_or_build_is_read_only(self):
        output = io.StringIO()
        with patch.object(sys, 'argv', ['install.py', '--doctor', '--config', str(self.root / 'missing.yaml'),
                                      '--panel', str(self.root / 'missing.html'), '--binary', str(self.root / 'missing.so')]), \
                contextlib.redirect_stdout(output):
            install.main()
        report = json.loads(output.getvalue())
        self.assertFalse(report['config_found'])
        self.assertFalse(report['panel']['supported'])
        self.assertEqual(list(self.root.iterdir()), [])


@unittest.skipUnless(sys.platform.startswith('linux'), 'real POSIX permission/ownership transaction runs on Linux CI')
class InstallerTests(unittest.TestCase):
    def setUp(self):
        import pwd
        self.tmp = tempfile.TemporaryDirectory(prefix='quota-guard-installer-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.config = self.root / 'config.yaml'
        self.plugin_dir = self.root / 'plugins'
        self.plugin_dir.mkdir()
        self.target = self.plugin_dir / 'quota-guard.so'
        self.binary = self.root / 'build.so'
        build_fixture(self.binary)
        self.state = self.root / 'state'
        self.backups = self.root / 'backups'
        self.original = yaml.safe_dump({'server': {'port': 18317}, 'management': {'secret-key': '$2a$hashed'},
            'requests': {'proxy-url': 'http://127.0.0.1:7890'},
            'plugins': {'enabled': False, 'dir': str(self.plugin_dir)}}).encode()
        self.config.write_bytes(self.original)
        self.key = self.root / 'management.key'
        self.key.write_text('fixture-key-only')
        self.panel = self.root / 'management.html'
        self.original_panel = b'<html><script type="module">fixture</script></html>'
        self.panel.write_bytes(self.original_panel)
        self.args = ['install.py', '--config', str(self.config), '--binary', str(self.binary),
                     '--panel', str(self.panel), '--state-dir', str(self.state), '--backup-dir', str(self.backups),
                     '--service', 'test-cpa', '--service-user', pwd.getpwuid(os.getuid()).pw_name]
        self.baseline = {'port': 18317, 'plugin_present': False, 'cpa_version': '8.0.35'}

    def run_main(self, extra=(), verify_error=None, recovery_error=None):
        output = io.StringIO()
        ready = {'version': install.VERSION, 'build': {'commit': 'fixture', 'built_at': '2026-11-09T00:00:00Z'},
                 'policy': {'mode': 'observe', 'auto_reset': {'enabled': False, 'include_new_accounts': False}}}
        errors = [verify_error or ready, recovery_error] if verify_error else None
        with patch.object(sys, 'argv', self.args + list(extra)), \
                patch.object(install.subprocess, 'run') as systemctl, \
                patch.object(install, 'snapshot_runtime', return_value=self.baseline), \
                patch.object(install, 'management_get', return_value=({'plugins': []}, {'x-cpa-version': '8.0.35'})), \
                patch.object(install, 'verify_registration', side_effect=errors, return_value=ready) as verify, \
                patch.object(install, 'panel_diagnostics', return_value={'supported': True}), \
                patch.object(install, 'inject_session_bridge', return_value=b'patched-fixture'), \
                contextlib.redirect_stdout(output):
            self.last_output = output
            install.main()
            return systemctl, verify

    def apply(self, **kwargs):
        return self.run_main(['--apply', '--management-key-file', str(self.key)], **kwargs)

    def test_plan_is_read_only_and_shows_both_switches(self):
        systemctl, verify = self.run_main()
        self.assertEqual(self.config.read_bytes(), self.original)
        self.assertFalse(self.target.exists())
        self.assertFalse(self.state.exists())
        self.assertFalse(self.backups.exists())
        self.assertIn('"auto_reset_enabled": false', self.last_output.getvalue())
        systemctl.assert_not_called()
        verify.assert_not_called()

    def test_hashed_key_requires_actual_key_before_mutation(self):
        with self.assertRaises(SystemExit):
            self.run_main(['--apply'])
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

    def test_install_and_verified_rollback_preserve_policy_and_journal_bytes(self):
        self.state.mkdir()
        policy = self.state / 'policy.json'
        journal = self.state / 'reset-journal.json'
        saved = b'{"revision":10,"mode":"enforce","auto_reset":{"enabled":true,"before_hours":6,"accounts":{}},"rules":[]}'
        policy.write_bytes(saved)
        journal.write_bytes(b'fixture-journal-never-delete')
        _, verify = self.apply()
        self.assertEqual(self.target.read_bytes(), self.binary.read_bytes())
        current = yaml.safe_load(self.config.read_bytes())
        entry = current['plugins']['configs']['quota-guard']
        self.assertEqual(entry['proxy_url'], 'http://127.0.0.1:7890')
        self.assertEqual(entry['management_url'], 'http://127.0.0.1:18317')
        saved_key = self.state / 'cpa-management.key'
        self.assertEqual(saved_key.read_text(), 'fixture-key-only')
        self.assertEqual(saved_key.stat().st_mode & 0o777, 0o600)
        self.assertNotIn('fixture-key-only', self.config.read_text())
        self.assertEqual(policy.read_bytes(), saved)
        self.assertEqual(self.state.stat().st_mode & 0o777, 0o700)
        verify.assert_called_once_with(18317, 'fixture-key-only')
        backup, = self.backups.iterdir()
        self.assertEqual(backup.stat().st_mode & 0o777, 0o700)
        calls, verify = self.run_main(['--rollback', str(backup), '--management-key-file', str(self.key)])
        self.assertEqual(self.config.read_bytes(), self.original)
        self.assertEqual(self.panel.read_bytes(), self.original_panel)
        self.assertFalse(self.target.exists())
        self.assertEqual(policy.read_bytes(), saved)
        self.assertEqual(journal.read_bytes(), b'fixture-journal-never-delete')
        self.assertFalse(saved_key.exists())
        verify.assert_called_once_with(18317, 'fixture-key-only', None, False)
        self.assertEqual(calls.call_args_list[0].args[0], ['systemctl', 'restart', 'test-cpa'])

    def test_first_install_creates_safe_policy(self):
        self.apply()
        value = json.loads((self.state / 'policy.json').read_text())
        self.assertEqual(value['mode'], 'observe')
        self.assertFalse(value['auto_reset']['enabled'])
        self.assertFalse(value['auto_reset']['include_new_accounts'])
        self.assertLess(self.last_output.getvalue().index('BACKUP '), self.last_output.getvalue().index('INSTALL files'))

    def test_upgrade_rollback_restores_old_library_mode_and_api_version(self):
        self.target.write_bytes(b'old-plugin')
        self.target.chmod(0o750)
        self.baseline.update(plugin_present=True, version='0.2.2')
        self.apply()
        backup, = self.backups.iterdir()
        _, verify = self.run_main(['--rollback', str(backup), '--management-key-file', str(self.key)])
        self.assertEqual(self.target.read_bytes(), b'old-plugin')
        self.assertEqual(self.target.stat().st_mode & 0o777, 0o750)
        verify.assert_called_once_with(18317, 'fixture-key-only', '0.2.2', True)

    def test_failed_registration_restores_previous_files_and_verifies_old_service(self):
        self.target.write_bytes(b'old-plugin')
        with self.assertRaisesRegex(RuntimeError, 'original service/API restored'):
            self.apply(verify_error=RuntimeError('fixture-registration-failed'))
        self.assertEqual(self.config.read_bytes(), self.original)
        self.assertEqual(self.target.read_bytes(), b'old-plugin')
        self.assertEqual(self.panel.read_bytes(), self.original_panel)
        self.assertIn('original API version verified', self.last_output.getvalue())

    def test_failed_recovery_is_not_reported_as_success(self):
        with self.assertRaisesRegex(RuntimeError, 'recovery could not be verified'):
            self.apply(verify_error=RuntimeError('new not ready'), recovery_error=RuntimeError('old not ready'))
        self.assertNotIn('original API version verified', self.last_output.getvalue())
        self.assertIn('BACKUP ', self.last_output.getvalue())

    def test_key_rotation_is_reverted_on_failed_upgrade(self):
        self.state.mkdir()
        saved_key = self.state / 'cpa-management.key'
        saved_key.write_text('previous-key')
        saved_key.chmod(0o600)
        with self.assertRaises(RuntimeError):
            self.apply(verify_error=RuntimeError('fixture-registration-failed'))
        self.assertEqual(saved_key.read_text(), 'previous-key')
        self.assertEqual(saved_key.stat().st_mode & 0o777, 0o600)

    def test_management_key_symlink_rejected_before_mutation(self):
        self.state.mkdir()
        target = self.root / 'external-key'
        target.write_text('untouched')
        (self.state / 'cpa-management.key').symlink_to(target)
        with self.assertRaises(SystemExit):
            self.apply()
        self.assertEqual(target.read_text(), 'untouched')
        self.assertEqual(self.config.read_bytes(), self.original)

    def test_unknown_plugin_capabilities_block_before_backup(self):
        config = yaml.safe_load(self.config.read_text())
        config['plugins']['configs'] = {'keeper': {'enabled': True}}
        self.config.write_text(yaml.safe_dump(config))
        with self.assertRaisesRegex(SystemExit, 'unknown capabilities'):
            self.apply()
        self.assertFalse(self.backups.exists())


if __name__ == '__main__':
    unittest.main(verbosity=2)
