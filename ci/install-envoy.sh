#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
source ci/versions.env
mkdir -p .tools/bin evidence
# The CI runner and all child processes share its loopback network namespace.
# Only extract the pinned binary; no privileged container or Docker socket enters tests.
container=$(docker create --platform linux/amd64 "$ENVOY_IMAGE")
trap 'docker rm "$container" >/dev/null' EXIT
docker cp "$container:/usr/local/bin/envoy" .tools/bin/envoy
chmod +x .tools/bin/envoy
.tools/bin/envoy --version | tee evidence/envoy-version.txt
printf '%s\n' "$ENVOY_IMAGE" > evidence/envoy-image.txt
sha256sum .tools/bin/envoy > evidence/envoy-binary-sha256.txt
if [[ -n ${GITHUB_PATH:-} ]]; then printf '%s/.tools/bin\n' "$PWD" >> "$GITHUB_PATH"; fi
