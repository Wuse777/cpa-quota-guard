#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
export CGO_ENABLED=1
if [ "$(go env GOOS)" != linux ] || [ "$(go env GOARCH)" != amd64 ]; then
  printf 'Only the verified Linux amd64/glibc target is supported.\n' >&2
  exit 1
fi
export QUOTA_GUARD_BUILD_COMMIT="${QUOTA_GUARD_BUILD_COMMIT:-$(git rev-parse HEAD 2>/dev/null || printf source-export)}"
export QUOTA_GUARD_BUILD_TIME="${QUOTA_GUARD_BUILD_TIME:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
go mod download
go test -race -count=1 ./...
go vet ./...
mkdir -p dist
go build -trimpath -buildvcs=false -ldflags "-X main.buildCommit=$QUOTA_GUARD_BUILD_COMMIT -X main.buildTime=$QUOTA_GUARD_BUILD_TIME" -buildmode=c-shared -o dist/quota-guard.so .
python3 scripts/build_manifest.py
sha256sum dist/quota-guard.so dist/quota-guard.h dist/BUILD.json > dist/SHA256SUMS
printf 'Built %s\n' "$PWD/dist/quota-guard.so"
