# Local research verification — 2026-10-22

Executed on macOS arm64, Go 1.27.1. Envoy placement used local Envoy 1.33.2,
build `2135e1a42f002a939d60581096291acb6abce695`. This is local evidence, not a
GitHub Actions run or Linux qualification. No live provider credentials were used.

| Lane | Observed result |
|---|---|
| `model` | Passed under race detector, 20 tests/examples including subtests; 8.216 seconds |
| `security` | Passed under race detector, 38 tests including subtests; 12.129 seconds |
| `research` | Passed under race detector; eight-reader revocation progress and 32 post-install denials; 2.542 seconds |
| `controls` | Four expected vulnerabilities/limitations reproduced; 1.975 seconds |
| `placement` | Full root suite passed under race detector, 53 tests including subtests; 50.317 seconds; actual Envoy and pinned MCP process |
| `spec-package` | Formatting, module verification, vet, result-verifier negative tests, documentation/reference checks and actionlint passed |

Times are package elapsed values from Go test JSON, not performance benchmarks.
The model explored **70,925 distinct states and 365,180 transitions** at depth six
from cold and bootstrapped roots. All **720 channel-delivery permutations** converged
when compatible deliveries completed. It also checked **256 deterministic sampled
schedules of 64 events**, seed **43197**. This does not establish unbounded correctness.

All three temporary guard-removal controls produced their required behavioral failures:

- Dependency guard removal: `TestActivationCounterexample` failed.
- Lease guard removal: `TestRevocationAndDispatchBoundary` failed.
- Session guard removal: `TestRestartAndPublisherRollbackLimit` failed.

Compiler failures/timeouts do not count as successful mutation detection. The source
reference remained unchanged by these temporary-copy controls.

The real HTTPS/SQLite rollback control matched the model: a warm subscriber refused
a lower publication revision, while a cold subscriber accepted stale content from a
trusted authority answering a fresh challenge. This is a demonstrated limitation;
it requires durable/fenced publisher authority or additional trusted history.

The previous MCP source-download blockage was overcome. The archive was acquired,
SHA-256 checked against `ci/versions.env`, and the fixture built from the pinned
source. The successful rerun reused that verified local archive through
`MCP_SOURCE_ARCHIVE`; CI normally downloads it with bounded retries/timeouts.
The real MCP fixture is not race-instrumented; the test/auth process is.

## Evidence artifacts and remaining qualification

Each `ci/run.sh` lane writes test events, a completion summary, environment and source
hashes under ignored `evidence/`. Source hashes identify this uncommitted reference
independently of the parent notes repository's HEAD. Placement also records fixture
source/archive/launcher/binary provenance and Envoy version. CI uploads only this
allowlisted directory, with no databases, credentials, keys or request captures.

The workflow was checked with actionlint 1.7.12 and shell syntax validation. It pins
actions by full SHA, Go 1.27.1, an official Envoy image digest, and the MCP source
archive checksum. It supports both the current notes layout and standalone extraction.

The historical local run above did not qualify the hosted Linux environment; see
the hosted evidence below for that separate tier. Remaining architectural gaps:
secured Envoy identity provisioning/rotation; actual dispatch-time ticket
revalidation; arbitrary publisher failure/clock skew; host swap/dump guarantees.
Envoy's historical delayed 504 denial behavior remains a known placement observation.
The research is now published as `dio/credential-distribution-lab` under Apache-2.0.


## Hosted runs and evidence

The [first hosted execution](https://github.com/dio/credential-distribution-lab/actions/runs/39719228988)
completed successfully: all six checks passed. It tests source commit
`83587722697944eab4840b53dbc169fd367b6480` on Ubuntu 24.04,
Linux amd64, with Go 1.27.1 and the pinned Envoy image/MCP archive. Each lane uploads
its outcome, test JSON, environment and source hashes. The model and security lanes
exercise the same experiments as the local reference. Placement independently
qualifies the actual Linux Envoy/MCP path.

Consult the linked run for each lane's terminal conclusion; an in-progress, skipped
or failed lane is not a completed verification. The
[workflow history](https://github.com/dio/credential-distribution-lab/actions/workflows/verify.yml)
identifies subsequent tested commits, including the migration to SHA-pinned official
Node 24 actions. The README badge reports current workflow status and does not expand
the architectural guarantees documented in SPEC.md and RESEARCH.md.

Expected-limit controls remain labelled as such in hosted summaries. Passing them
means the known baseline flaws and cold-authority rollback limit were reproduced,
not mitigated. The real MCP fixture is still not race-instrumented.
