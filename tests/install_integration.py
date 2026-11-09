"""Real CPA installation, coexistence and verified rollback without real accounts.

Use --runtime process on CI without systemd PID 1; --runtime systemd verifies an
exclusive temporary unit. Both modes load the real shared library and exercise
actual process restarts/API checks. Production services are never referenced.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import pwd
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
import uuid

import yaml


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument('--plugin', required=True, type=Path)
    ap.add_argument('--cpa', required=True, type=Path)
    ap.add_argument('--panel', required=True, type=Path)
    ap.add_argument('--runtime', choices=('process', 'systemd'), default='process')
    ap.add_argument('--report', type=Path, default=Path('test-results/install-integration-report.json'))
    args = ap.parse_args()
    if args.runtime == 'systemd' and os.geteuid() != 0:
        raise SystemExit('systemd fixture mode requires root')
    source = Path(__file__).resolve().parents[1]
    work = Path(tempfile.mkdtemp(prefix='quota-guard-install-acceptance-'))
    unit_name = 'quota-guard-install-acceptance-' + uuid.uuid4().hex[:12]
    unit_file = Path('/run/systemd/system') / (unit_name + '.service')
    config = work / 'config.yaml'
    (work / 'auths').mkdir()
    (work / 'static').mkdir()
    (work / 'plugins').mkdir()
    shutil.copy2(args.panel, work / 'static/management.html')
    key = 'isolated-install-acceptance-only'
    (work / 'key').write_text(key)
    (work / 'key').chmod(0o600)
    companion = work / 'plugins/keeper-fixture.so'
    subprocess.run(['cc', '-shared', '-fPIC', '-o', str(companion), str(source / 'tests/fixtures/management_plugin.c')], check=True)
    capabilities = work / 'plugin-capabilities.json'
    capabilities.write_text(json.dumps({'schema_version': 1, 'plugins': {'keeper-fixture': {
        'sha256': hashlib.sha256(companion.read_bytes()).hexdigest(), 'scheduler': False}}}))
    config.write_text(yaml.safe_dump({'config-version': 8, 'server': {'host': '127.0.0.1', 'port': 18417},
        'management': {'secret-key': key, 'disable-auto-update-panel': True, 'allow-remote': False},
        'oauth': {'auth-dir': str(work / 'auths')},
        'plugins': {'enabled': True, 'dir': str(work / 'plugins'),
                    'configs': {'keeper-fixture': {'enabled': True, 'priority': 1}}}}))
    environment = dict(os.environ)
    if args.runtime == 'process':
        descriptor = work / 'service.json'
        descriptor.write_text(json.dumps({'name': unit_name, 'cpa': str(args.cpa.resolve()),
                                         'config': str(config), 'static': str(work / 'static')}))
        control_dir = work / 'control'
        control_dir.mkdir()
        wrapper = control_dir / 'systemctl'
        wrapper.write_text('#!' + sys.executable + '\nimport runpy\nrunpy.run_path(' + repr(str(source / 'tests/process_service.py')) + ',run_name="__main__")\n')
        wrapper.chmod(0o755)
        environment.update(QUOTA_GUARD_TEST_SERVICE=str(descriptor), PATH=str(control_dir) + os.pathsep + environment['PATH'])
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    checks = []

    def get(path):
        request = urllib.request.Request('http://127.0.0.1:18417' + path, headers={'Authorization': 'Bearer ' + key})
        with opener.open(request, timeout=3) as response:
            return json.load(response)

    def ready():
        for _ in range(100):
            try:
                get('/v0/management/config')
                return
            except OSError:
                time.sleep(.1)
        raise RuntimeError('Isolated CPA did not become ready; see ' + str(work / 'service.log'))

    def run(*command):
        completed = subprocess.run(command, env=environment, capture_output=True, text=True)
        if completed.returncode:
            raise RuntimeError('Fixture command failed:\n' + completed.stdout + '\n' + completed.stderr)
        return completed.stdout

    created = False
    try:
        if args.runtime == 'systemd':
            with unit_file.open('x') as file:
                file.write('[Unit]\nDescription=Isolated quota guard installer acceptance\n[Service]\nType=simple\n'
                    'WorkingDirectory=' + str(work) + '\nEnvironment=MANAGEMENT_STATIC_PATH=' + str(work / 'static') + '\n'
                    'ExecStart=' + str(args.cpa.resolve()) + ' --local-model --config ' + str(config) + '\n')
            created = True
            run('systemctl', 'daemon-reload')
        run('systemctl', 'start', unit_name)
        created = True
        ready()
        before = config.read_bytes()
        assert yaml.safe_load(before)['management']['secret-key'].startswith('$2')
        assert any(item['id'] == 'keeper-fixture' and item['registered'] for item in get('/v0/management/plugins')['plugins'])
        checks.append('baseline real CPA with management-only companion loaded')
        command = [sys.executable, str(source / 'install.py'), '--config', str(config),
            '--binary', str(args.plugin.resolve()), '--panel', str(work / 'static/management.html'),
            '--state-dir', str(work / 'state'), '--backup-dir', str(work / 'backups'),
            '--service', unit_name, '--service-user', pwd.getpwuid(os.getuid()).pw_name,
            '--management-key-file', str(work / 'key'), '--plugin-capabilities', str(capabilities)]
        output = run(*command, '--apply')
        assert 'INSTALL service active' in output and 'BACKUP ' in output
        status = get('/v0/management/plugins/quota-guard/status')
        assert status['version'] == '0.3.0' and status['policy']['mode'] == 'observe'
        assert not status['policy']['auto_reset']['enabled']
        assert not status['policy']['auto_reset']['include_new_accounts']
        assert status['accounts'] == []
        assert any(item['id'] == 'keeper-fixture' and item['registered'] for item in get('/v0/management/plugins')['plugins'])
        checks.append('installer loads real library alongside management plugin; safe reset defaults; API verified')
        policy = work / 'state/policy.json'
        saved = policy.read_bytes()
        journal = work / 'state/reset-journal.json'
        saved_journal = journal.read_bytes() if journal.exists() else None
        backup, = (work / 'backups').iterdir()
        output = run(*command, '--rollback', str(backup))
        assert 'original API version verified' in output
        ready()
        assert config.read_bytes() == before
        assert not (work / 'plugins/quota-guard.so').exists()
        assert companion.exists()
        assert policy.read_bytes() == saved
        if saved_journal is not None:
            assert journal.read_bytes() == saved_journal
        try:
            get('/v0/management/plugins/quota-guard/status')
            raise AssertionError('plugin route remained registered')
        except urllib.error.HTTPError as error:
            assert error.code == 404
        checks.append('verified rollback restores config, retains companion/policy/journal and original API baseline')
        report = {'checks': checks, 'count': len(checks), 'service_runtime': args.runtime,
                  'plugin_sha256': hashlib.sha256(args.plugin.read_bytes()).hexdigest(), 'production_unit_touched': False}
        args.report.parent.mkdir(parents=True, exist_ok=True)
        args.report.write_text(json.dumps(report, indent=2) + '\n')
        for check in checks:
            print('PASS', check)
        print('INSTALL_INTEGRATION_OK', len(checks), args.runtime)
    finally:
        if created:
            subprocess.run(['systemctl', 'stop', unit_name], env=environment, check=False, capture_output=True)
        if args.runtime == 'systemd' and unit_file.exists():
            unit_file.unlink()
            subprocess.run(['systemctl', 'daemon-reload'], check=False, capture_output=True)


if __name__ == '__main__':
    main()
