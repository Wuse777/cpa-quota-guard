#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
export CGO_ENABLED=1
go mod download
go test -race -count=1 ./...
go vet ./...
mkdir -p dist
go build -trimpath -buildvcs=false -buildmode=c-shared -o dist/quota-guard.so .
sha256sum dist/quota-guard.so > dist/SHA256SUMS
printf 'Built %s\n' "$PWD/dist/quota-guard.so"
