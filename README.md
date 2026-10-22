# Credential Distribution Lab

[![Credential research](https://github.com/dio/credential-distribution-lab/actions/workflows/verify.yml/badge.svg)](https://github.com/dio/credential-distribution-lab/actions/workflows/verify.yml)

One Go authd implementation, a SQLite `:memory:` credential store, and Envoy
upstream extproc filters shared by LLM HTTP and real EAIGW MCP proxy egress.
This is an Apache-2.0 research reference, not a production authentication service.

The central question: **what may a stale local replica authorize?** Start with the
[research questions and experiments](RESEARCH.md), [protocol spec](SPEC.md), and
[verification results](RESEARCH-RESULTS.md). CI reports the model, runtime security,
contention, expected limitation controls, and upstream placement separately.

```sh
./ci/run.sh model          # bounded exploration and deliberate unsafe mutations
./ci/run.sh security       # real HTTPS/gRPC mutual TLS and private SQLite stores
```

See [third-party provenance](THIRD_PARTY.md) and [security scope](SECURITY.md).

See [security review and mitigation plan](SECURITY-REVIEW.md) before reusing the
pattern. `TestSecurityReview*` tests deliberately reproduce known harness gaps;
their passing status does not mean those gaps are fixed.

Run from this directory:

```sh
./run.sh
```

Requires `envoy` on PATH, Go (the pinned MCP source requires a recent Go toolchain),
a C compiler for go-sqlite3, curl and tar. First run downloads and compiles the MCP
source and dependencies. No Kubernetes or cloud service is used. The script stages
MCP source in a temporary directory; it never modifies a production checkout.

`run.sh` builds a thin launcher around the real `internal/mcpproxy` implementation
at commit `994579660ae2a9f2feef3cb77766e155746858b4`, then runs the tests with the
Go race detector. The MCP launcher itself is not race-instrumented. The harness
starts and stops Envoy, gRPC authd, a loopback credential-update API, fake upstream,
and the MCP fixture. Credentials are randomly generated at runtime and stay in
memory. Non-secret Envoy configuration and test logs use a temporary directory.
`results.jsonl` contains machine-readable Go test events without credential values.

For store-only tests:

```sh
go test -race -run TestStoreConcurrencyAndMemory .
```

The integration test intentionally fails if `MCP_PROXY_BIN` is absent: a missing
real MCP fixture must not silently turn a partial run into a passing full spike.

See [CI and public research repository design](CI-DESIGN.md) for implemented checks,
reproducible packaging, and publication boundaries. Hosted runs publish a separate summary and evidence artifact for each check.

## Research questions and executable model

[Research design and experiments](RESEARCH.md), with [local verification results](RESEARCH-RESULTS.md), explore multi-channel activation,
revocation boundaries, cold-start rollback, use binding, and contention.
Run `./ci/run.sh model` for bounded exploration or use the individual verification
lanes described there. These are model guarantees and explicit counterexamples,
not a proof that Envoy implements dispatch-time revalidation.

## Secured architecture reference

[Architecture spec](SPEC.md) and [verified security results](SECURITY-RESULTS.md) map identity, scoped delivery, fresh bootstrap,
atomic snapshots and leases to implementation functions and executable tests.

```sh
./run-security.sh
```

This runs real HTTPS/gRPC mutual TLS with in-memory test identity material and
separate per-subscriber databases. The secure entrypoint is `Subscriber`, not the
legacy unauthenticated `Auth` fixture. See the spec's evidence boundaries: Envoy's
file-free identity provisioning has not yet been integrated with this secured path.

## Ownership

- Static cluster configuration supplies a backend ID in gRPC `initial_metadata`.
  This is not an HTTP header forwarded from the client.
- Authd maps that backend ID to a scoped credential reference. Only authd writes
  outbound Authorization, using overwrite semantics.
- The update API commits ordered credential updates to one SQLite connection.
  Revocations are newer revisions; older deliveries cannot resurrect credentials.
- Extproc runs only on destination clusters, before upstream_codec, and not on its
  own gRPC transport cluster. Only request headers are processed; bodies pass through.
- MCP uses its real initialization, session, listing and tool-call code, then its
  normal backend listener. The fixture replaces metrics/tracing with no-ops and
  config delivery with a non-secret in-memory configuration.

## What the checks establish

No-filter control; header injection and replacement; unchanged LLM request body;
wrong-scope denial; rotation without route reload or credential-fetch calls;
missing/revoked/expired denial; rejection of delayed updates; real MCP tool call
and revocation; authd outage; restart with an empty store and subsequent rehydration.
Backend counters establish that failures do not dispatch an HTTP request.

SQLite checks confirm an empty database filename and `temp_store=MEMORY`. These
are not a syscall audit or proof of node swap/core-dump policy. No no-disk guarantee
beyond the configured in-memory database is claimed. No Turso delivery, signing,
OAuth refresh, retries or throughput benchmark is included. The placement fixture
uses plaintext local RPC; the separate security suite exercises mutual TLS.

See [the protocol spec](SPEC.md), [research design](RESEARCH.md), and RESULTS.md for observed
behavior and limits. Run `go vet ./...` for static analysis.
