#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
./ci/run.sh security
cp evidence/security.jsonl security-results.jsonl
go vet ./...
