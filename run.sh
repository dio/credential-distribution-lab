#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
: > results.jsonl
command -v envoy >/dev/null
command -v go >/dev/null
source ci/versions.env
spike_tmp=$(mktemp -d)
trap 'rm -rf "$spike_tmp"' EXIT
mkdir -p evidence
printf 'bootstrap: acquiring pinned MCP source\n' > evidence/placement-bootstrap.txt
if [[ -n ${MCP_SOURCE_ARCHIVE:-} ]]; then
  cp "$MCP_SOURCE_ARCHIVE" "$spike_tmp/source.tgz"
else
  curl --connect-timeout 10 --max-time 120 --retry 2 --retry-max-time 240 -fsSL \
    "https://codeload.github.com/envoyproxy/ai-gateway/tar.gz/$MCP_COMMIT" -o "$spike_tmp/source.tgz"
fi
python3 - "$spike_tmp/source.tgz" "$MCP_ARCHIVE_SHA256" <<'PY'
import hashlib, sys
from pathlib import Path
if hashlib.sha256(Path(sys.argv[1]).read_bytes()).hexdigest() != sys.argv[2]:
    raise SystemExit('MCP source checksum mismatch')
PY
printf 'source commit=%s archive sha256=%s verified\n' "$MCP_COMMIT" "$MCP_ARCHIVE_SHA256" >> evidence/placement-bootstrap.txt
tar -xzf "$spike_tmp/source.tgz" -C "$spike_tmp"
source_dir=$(find "$spike_tmp" -maxdepth 1 -type d -name "*-$MCP_COMMIT" -print)
test -f "$source_dir/go.mod"
mkdir -p "$source_dir/cmd/credential-spike-mcp"
cp fixture/mcp-main.go.txt "$source_dir/cmd/credential-spike-mcp/main.go"
printf 'bootstrap: building real MCP fixture (not race-instrumented)\n' >> evidence/placement-bootstrap.txt
(cd "$source_dir" && go mod download && go mod verify && go build -o "$spike_tmp/mcp-proxy" ./cmd/credential-spike-mcp)
python3 - fixture/mcp-main.go.txt "$spike_tmp/mcp-proxy" >> evidence/placement-bootstrap.txt <<'PY'
import hashlib, sys
from pathlib import Path
for name in sys.argv[1:]:
    p = Path(name)
    print(p.name, 'sha256='+hashlib.sha256(p.read_bytes()).hexdigest())
PY
envoy --version > evidence/envoy-version.txt
printf 'bootstrap complete; executing tests\n' >> evidence/placement-bootstrap.txt
MCP_PROXY_BIN="$spike_tmp/mcp-proxy" go test -race -count=1 -timeout=5m -v -json . | tee results.jsonl
