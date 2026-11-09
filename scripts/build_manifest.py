"""Write the exact provenance consumed by installer preflight and releases."""
import hashlib
import json
import os
from pathlib import Path
import re

root = Path(__file__).resolve().parents[1]
version = re.search(r'^const version = "([0-9.]+)"$', (root / 'types.go').read_text(), re.M).group(1)
raw = (root / 'dist/quota-guard.so').read_bytes()
value = {'version': version, 'target': 'linux-amd64-glibc', 'abi_version': 1, 'schema_version': 6,
         'sha256': hashlib.sha256(raw).hexdigest(), 'commit': os.environ['QUOTA_GUARD_BUILD_COMMIT'],
         'built_at': os.environ['QUOTA_GUARD_BUILD_TIME']}
(root / 'dist/BUILD.json').write_text(json.dumps(value, indent=2) + '\n')
