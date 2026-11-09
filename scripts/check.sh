#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
export CGO_ENABLED=1
go test -race -count=1 ./...
go vet ./...
node --test tests/session_bridge.test.mjs tests/ui-model.test.mjs
python3 -m unittest discover -s tests -p 'test_*.py' -v
