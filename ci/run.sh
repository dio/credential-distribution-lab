#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
lane=${1:?usage: ci/run.sh model|security|research|controls|placement|spec-package}
case "$lane" in
  model|security|research|controls|placement|spec-package) ;;
  *) echo 'unknown verification lane' >&2; exit 2 ;;
esac
mkdir -p evidence
# Overwrite, never append to a previous run's evidence.
{
  go version
  uname -sm
  git rev-parse HEAD
  git diff --quiet && echo 'tracked working tree clean' || echo 'tracked working tree modified'
} > "evidence/$lane-environment.txt"
python3 ci/source_hashes.py > "evidence/$lane-sources.txt"
printf 'RUNNING %s\n' "$lane" > "evidence/$lane-summary.txt"
trap 'status=$?; if ((status != 0)); then printf "FAILED %s exit=%s (inspect logs; incomplete tests are not a pass)\n" "$lane" "$status" >> "evidence/$lane-summary.txt"; fi' EXIT
case "$lane" in
  model) package=./model; selector='.' ;;
  security) package=.; selector='^(TestSecure|TestStoreConcurrencyAndMemory)' ;;
  research) package=.; selector='^TestResearchRevocationUnderReaders$' ;;
  controls) package=.; selector='^(TestSecurityReview|TestResearchTrustedPublisherRollback)' ;;
  placement)
    if ./run.sh; then :; else
      status=$?
      if [[ -f results.jsonl ]]; then cp results.jsonl evidence/placement.jsonl; fi
      exit "$status"
    fi
    cp results.jsonl evidence/placement.jsonl
    python3 ci/check_results.py placement evidence/placement.jsonl | tee evidence/placement-summary.txt
    exit 0 ;;
  spec-package)
    test -z "$(gofmt -l .)"
    go mod verify
    go vet ./...
    python3 -m unittest discover -s ci -p 'test_*.py'
    python3 ci/check_docs.py
    if command -v actionlint >/dev/null; then
      actionlint .github/workflows/verify.yml
    else
      go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12 .github/workflows/verify.yml
    fi
    printf 'PASS spec-package\n' | tee evidence/spec-package-summary.txt
    exit 0 ;;
esac
go test -race -count=1 -timeout=3m -run "$selector" -v -json "$package" | tee "evidence/$lane.jsonl"
if [[ "$lane" == model ]]; then python3 ci/check_mutations.py | tee evidence/model-mutations.txt; fi
python3 ci/check_results.py "$lane" "evidence/$lane.jsonl" | tee "evidence/$lane-summary.txt"
