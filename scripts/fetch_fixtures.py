"""Fetch immutable-version public integration fixtures and verify pinned hashes.

No account, authorization file or production configuration is used. A changed
upstream asset is an error, never a reason to update the checksum automatically.
"""
import argparse
import hashlib
import json
from pathlib import Path
import tarfile
import tempfile
import urllib.request

FIXTURES = {
    'cpa_archive': {
        'url': 'https://github.com/router-for-me/CLIProxyAPI/releases/download/v8.0.35/CLIProxyAPI_8.0.35_linux_amd64.tar.gz',
        'sha256': '1d9edde6973e8f15694debcd186b5ab693e0741bd6a4c5e551aa89e0a6b57e38',
        'filename': 'CLIProxyAPI_8.0.35_linux_amd64.tar.gz',
    },
    'cpa_archive_8_0_77': {
        'url': 'https://github.com/router-for-me/CLIProxyAPI/releases/download/v8.0.77/CLIProxyAPI_8.0.77_linux_amd64.tar.gz',
        'sha256': '1a7ea3f968c414ed9839facfdb1c0efe34cf3038b75b2e118db143d04de1aeb3',
        'filename': 'CLIProxyAPI_8.0.77_linux_amd64.tar.gz',
    },
    'panel': {
        'url': 'https://github.com/router-for-me/Cli-Proxy-API-Management-Center/releases/download/v1.26.5/management.html',
        'sha256': '57558cd023475674634a114824b014d3e5ac161f6f3791e3d86b6adab962a700',
        'filename': 'management.html',
    },
    'panel_1_28_1': {
        'url': 'https://github.com/router-for-me/Cli-Proxy-API-Management-Center/releases/download/v1.28.1/management.html',
        'sha256': 'c00ac5577db8feb12d37de6ecb783cf919bb81536902308079f754337d7a7bc5',
        'filename': 'management-1.28.1.html',
    },
}
CPA_BINARIES = {
    'cli-proxy-api': ('cpa_archive', '7d2fff1780cf680704ed9df1204c4d7362d21f1c7ec7788de4f1a2394c025583'),
    'cli-proxy-api-8.0.77': ('cpa_archive_8_0_77', 'a653044124e5600fbb73208a2b541521d010c4bccafda676e128be75d43741ab'),
}


def digest(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def fetch(output):
    output = Path(output)
    output.mkdir(parents=True, exist_ok=True)
    for fixture in FIXTURES.values():
        target = output / fixture['filename']
        if not target.exists() or digest(target) != fixture['sha256']:
            request = urllib.request.Request(fixture['url'], headers={'User-Agent': 'cpa-quota-guard-test-fixtures'})
            with tempfile.NamedTemporaryFile(dir=output, delete=False) as temporary:
                temporary_path = Path(temporary.name)
                try:
                    with urllib.request.urlopen(request, timeout=60) as response:
                        total = 0
                        while chunk := response.read(1024 * 1024):
                            total += len(chunk)
                            if total > 150 * 1024 * 1024:
                                raise ValueError('Fixture exceeds 150 MiB')
                            temporary.write(chunk)
                except Exception:
                    temporary.close()
                    temporary_path.unlink(missing_ok=True)
                    raise
            if digest(temporary_path) != fixture['sha256']:
                temporary_path.unlink()
                raise ValueError('Pinned upstream SHA256 mismatch: ' + fixture['filename'])
            temporary_path.replace(target)
    for name, (archive_key, sha256) in CPA_BINARIES.items():
        binary = output / name
        with tarfile.open(output / FIXTURES[archive_key]['filename']) as archive:
            members = [item for item in archive.getmembers() if Path(item.name).name == 'cli-proxy-api']
            if len(members) != 1 or not members[0].isfile():
                raise ValueError('Expected one regular CPA binary in fixture archive')
            data = archive.extractfile(members[0]).read()
            if hashlib.sha256(data).hexdigest() != sha256:
                raise ValueError('Pinned CPA binary SHA256 mismatch: ' + name)
            binary.write_bytes(data)
            binary.chmod(0o755)
    (output / 'FIXTURES.json').write_text(json.dumps({'cpa_versions': ['8.0.35', '8.0.77'],
        'cpamc_versions': ['1.26.5', '1.28.1'],
        'cpa_binary_sha256': {name: sha for name, (_, sha) in CPA_BINARIES.items()},
        'sources': FIXTURES}, indent=2) + '\n')
    return (output / 'cli-proxy-api').resolve(), (output / 'management.html').resolve()


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output-dir', type=Path, default=Path('test-results/fixtures'))
    args = parser.parse_args()
    binary, panel = fetch(args.output_dir)
    print(json.dumps({'cpa': str(binary), 'panel': str(panel)}, indent=2))
