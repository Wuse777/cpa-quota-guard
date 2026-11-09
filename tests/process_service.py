"""Tiny service controller used only by isolated installer acceptance tests.

It can manage exactly one fixture process, verifies its command before signaling,
and cannot touch systemd units or arbitrary PIDs. It exercises real CPA restart
and API readiness on CI workers where systemd is not PID 1.
"""
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time


def alive(pid):
    try:
        state = Path(f'/proc/{pid}/stat').read_text().split(') ', 1)[1][0]
        return state != 'Z'
    except (OSError, IndexError):
        return False


def control(argv):
    descriptor = Path(os.environ['QUOTA_GUARD_TEST_SERVICE']).resolve()
    info = json.loads(descriptor.read_text())
    operation = argv[0]
    service = argv[-1]
    if service != info['name'] or not service.startswith('quota-guard-install-acceptance-'):
        raise SystemExit('This controller only accepts its synthetic fixture service')
    pid_file = descriptor.parent / 'service.pid'
    pid = int(pid_file.read_text()) if pid_file.exists() else None
    if operation == 'is-active':
        return 0 if pid and alive(pid) else 3
    if operation not in ('start', 'restart', 'stop'):
        raise SystemExit('Unsupported fixture service operation')
    if pid and alive(pid):
        command = Path(f'/proc/{pid}/cmdline').read_bytes().split(b'\0')
        if os.fsencode(info['cpa']) not in command or os.fsencode(info['config']) not in command:
            raise SystemExit('Refusing to signal a process outside this fixture')
        os.kill(pid, signal.SIGTERM)
        for _ in range(100):
            if not alive(pid):
                break
            time.sleep(.05)
        else:
            os.kill(pid, signal.SIGKILL)
    pid_file.unlink(missing_ok=True)
    if operation != 'stop':
        environment = dict(os.environ, MANAGEMENT_STATIC_PATH=info['static'])
        with (descriptor.parent / 'service.log').open('ab') as log:
            process = subprocess.Popen([info['cpa'], '--local-model', '--config', info['config']],
                cwd=descriptor.parent, env=environment, stdin=subprocess.DEVNULL,
                stdout=log, stderr=log, start_new_session=True)
        pid_file.write_text(str(process.pid))
    return 0


if __name__ == '__main__':
    raise SystemExit(control(sys.argv[1:]))
