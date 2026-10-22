# Secured reference results

Executed locally on 2026-10-22 with Go 1.27.1, macOS arm64, using the module pins
recorded in go.mod/go.sum. All test certificates/private keys and credential values
were generated and held in memory. No production identity or credential was used.

- `./run-security.sh`: passed under the race detector (2.284 seconds); vet passed.
  Four top-level tests, including the store test, and 29 security subtests passed.
- `./run.sh`: full suite passed under the race detector (40.854 seconds), including
  the original real Envoy/MCP path and the deliberate insecure-fixture controls.
- Machine-readable output: `security-results.jsonl` and `results.jsonl`, regenerated
  by the respective scripts and git-ignored.

## Verified over real HTTPS/gRPC TLS connections

- Anonymous clients cannot complete publisher/auth-service mutual TLS.
- An untrusted issuer is rejected; a same-CA identity from another environment or
  proxy cannot resolve credentials.
- Subscribers reject a same-CA server whose URI is not the expected publisher.
- Publisher grants restrict two subscribers to separate credential scopes.
- A subscriber cannot request the other subscription; an ungranted workload cannot
  subscribe. Authenticated proxy peers cannot select ungranted backend IDs.
- An authorized peer successfully receives the expected bearer mutation.
- A newly issued certificate with the same authorized URI works on a new connection.
  This does not prove hot certificate reload or CA rollover.
- Before fresh bootstrap the secured handler denies use.
- A captured old-session snapshot and a pre-revocation snapshot are rejected by a
  newly created subscriber; fresh authoritative bootstrap retains revocation.
- Lower publication revision and changed content at the same revision are rejected.
- Foreign-scope content is rejected without partially changing the installed DB.
- Unknown epoch is rejected, requiring explicit future authority migration.
- Lease expiry stops credential resolution despite an unexpired credential.
- Cold bootstrap during publisher outage remains unready.

## Direct validation tests

Oversized/zero leases, wrong subscription, duplicate keys, and credential header
control characters are rejected. Rejected content does not advance the revision.
Slow bootstrap cannot extend the lease from response arrival. A valid full snapshot
removes omitted credentials. These use the install function directly to isolate
validation, not to claim additional network integration coverage.

## Boundaries

The new identity tests use actual TLS gRPC clients, not an Envoy client. The original
Envoy/MCP tests still use the original plaintext local fixture. End-to-end secured
Envoy + file-free identity provisioning/rotation is NOT established by combining
these two suites. Envoy 1.33.2's observed delayed 504 failure behavior is unchanged.

The secure entrypoint is `Subscriber`. `Auth` and its unauthenticated PUT endpoint
remain baseline test controls; `TestSecurityReview*` passes mean those controls
still reproduce their known vulnerabilities. Do not deploy the baseline fixture.

Publisher durability, attestation, relay trust, cross-channel activation, user-use
permission, destination binding, overload protection and host memory custody remain
explicit obligations in SPEC.md. The mock publisher is authoritative but volatile;
a malicious/rolled-back trusted publisher reissuing stale content under a fresh
challenge is outside the demonstrated replay protection.

## Review fixes (2026-10-22)

Four regression cases failed before the fixes: expired subscriber renewal, expired
proxy resolution, slow request body blocking publication, and a queued lookup
returning after lease expiry. All now pass. An additional regression rejects an
expired publisher on a reused HTTPS connection.

`./run-security.sh` passed with the race detector (12.593 seconds); `go vet ./...`
passed. The reference now rechecks verified-chain time validity per operation,
caps leases by identity validity, removes network I/O from publisher state locks,
and bounds/rechecks lookup lifetime. See `secure_regression_test.go` and SPEC.md.
Certificate revocation and CA reload remain outside this reference.

The full Envoy/MCP rerun was attempted but did not reach tests: the pinned source
archive download stalled, and a bounded retry timed out after 15 seconds. The
40.854-second full-suite result above is prior evidence, not a post-fix run.


## Subsequent research/CI verification

The [research verification record](RESEARCH-RESULTS.md) supersedes the previous
blocked full-rerun status: the complete Envoy/MCP suite now passes locally after
archive verification, including the security fixes. It also records bounded model
exploration, expected rollback limitations, and the new concurrent-reader test.
This still does not establish hosted Linux CI or secured Envoy integration.
