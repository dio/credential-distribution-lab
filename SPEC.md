# Authenticated per-proxy credential distribution

Version: local reference 0.2, 2026-10-22. This spec defines the implemented trust
protocol and identifies architecture obligations that remain outside the reference.
Normative statements apply to this protocol unless explicitly marked future work.

## Objective and threat boundary

Remote MP retains durable authority. Each proxy instance holds only its authorized
credential working set in a private in-memory database. A universal upstream auth
handler resolves credentials locally. Routing/configuration contains references,
not credential bytes. Credential delivery never runs on the request path.

Protect against unauthenticated delivery, an unauthenticated local RPC caller,
a trusted-CA peer with the wrong identity, subscription/scope escalation, old-session
replay, rollback within a running subscriber, and disconnected use after lease expiry.

An authorized publisher can issue current credentials; this protocol cannot protect
against compromise of that authority or its issuer. A compromised authorized proxy
can access the credentials it may inject. Static grants in the reference stand in
for an authoritative policy service, not caller authorization.

## Actors and private state

```text
MP publisher
  -- TLS 1.3 mutual authentication / complete scoped snapshots -->
subscriber for proxy A [private SQLite :memory:]
  -- local lookup --> auth enforcement for A
  <-- mutually authenticated ext_proc RPC -- proxy A

MP publisher
  -- separate authorized subscription -->
subscriber for proxy B [different private SQLite :memory:]
```

The subscriber and auth handler share a Go process. Envoy remains a separate
credential-bearing process: ext_proc returns header bytes to its authorized peer.
There is no shared SQLite file, mounted credential volume, SQL service or credential
GET endpoint. Identity keys and certificates are also generated/held in memory by
the test harness. Actual identity issuance/attestation is future integration work.

## Identity and authority rules

**I1.** Every transport peer MUST present a certificate chaining to the configured
trust bundle. TLS 1.3 is the minimum. Normal server hostname verification remains
active; the local reference uses `localhost`. Certificate-chain time validity MUST
be rechecked per publication, accepted response, and credential resolution, including
reused connections. Expired connections may remain open but MUST NOT authorize new
operations. This does not implement certificate revocation or trust-bundle reload.

**I2.** A peer certificate MUST contain exactly one URI identity. Subscribers MUST
match the exact expected publisher URI. The publisher MUST map the authenticated
subscriber URI to a subscription and exact `(scope, credential ID)` allowlist.
Sharing a CA or providing a claimed subscription name is insufficient.

**I3.** Auth enforcement MUST map the authenticated proxy URI to an exact allowed
backend set. gRPC `x-spike-backend` metadata selects a binding within that grant;
it does not establish identity. Anonymous peers, unknown backends and peers outside
the binding grant MUST receive no credential mutation.

The local identity examples use `spiffe://spike.local/...` URI SANs. This is not a
SPIRE deployment, SPIFFE conformance test or workload-attestation implementation.
Trusted code constructs grants and transport configuration; untrusted requests
cannot change either. The legacy `Auth`/PUT fixture is not the secure entrypoint.

## Fresh complete-snapshot protocol

`Subscriber.Fetch` sends HTTPS `POST /snapshot` over mutual TLS:

```text
request  = challenge, subscription
response = challenge, subscription, epoch, publication revision,
           lease milliseconds, complete authorized credential records
```

**F1.** Each fetch MUST generate a new cryptographically random challenge. The
publisher MUST echo it in a fresh response for the authenticated subscription.
The response is authenticated by the end-to-end TLS connection, not a standalone
signature. HTTP redirects are rejected. Relays are not included in this reference.

**F2.** The subscriber MUST validate the challenge, subscription, configured epoch,
positive revision and bounded positive lease before accepting any records. Epoch
changes fail closed; authorized epoch migration is not implemented.

**F3.** Within one subscriber lifetime, publication revisions MUST not decrease.
An equal revision MUST contain the same credential sequence/content as previously
accepted; a changed payload at that revision is rejected. Equal-content responses
may renew the lease if they carry the current fetch challenge.

**F4.** On cold start no local revision history survives. Readiness MUST remain false
until a fresh authenticated response is accepted. A previously captured response
has the wrong challenge and MUST not bootstrap the new instance. During an MP outage,
a cold instance stays unready. The MP must supply current authoritative state; this
protocol cannot detect an authoritative server deliberately reissuing stale data
under a new challenge after the consumer has lost all history.

**F5.** The reference uses complete snapshots only. Install MUST replace the local
working set transactionally; omitted records are removed. Duplicate keys, foreign
bindings, invalid revision/expiry fields, oversized values and header control
characters are rejected before installation. Failed installation MUST not advance
the applied revision or renew the lease.

The mock publisher's state is volatile for testing. A production MP MUST maintain
durable authority epoch/revision and prevent stale failover. Deltas, content-addressed
objects, relays and multi-publisher ordering require additional protocol work.

## Local usability and lifetime

**L1.** An authenticated proxy request may resolve a credential only while the
subscription lease is valid AND the credential exists, is unrevoked, has a nonempty
value and has not expired. Unknown bindings fail closed. Resolution returns one
captured value for the attempt; no acquisition/refresh is performed in this path.

**L2.** Lease duration MUST be capped by local policy. Its monotonic deadline is
anchored to fetch start, not receipt, so a delayed response cannot create a longer
lease. The publisher caps the offered lease by subscriber chain validity; the
subscriber additionally caps it by publisher chain validity. Credentials also carry absolute expiry. Clock discipline and expiry safety
margin are deployment requirements, not qualified here.

**L3.** Rejected updates leave the last accepted state usable only for its remaining
lease and credential validity. Lease expiry denies new resolutions even if no new
traffic/update event has changed the database. Already dispatched attempts are not
retroactively cancelled.

**L4.** `Ready()` in this reference means the credential channel has a valid accepted
snapshot/lease. It does NOT mean every route dependency is present. A full-system
readiness gate must additionally validate required bindings, policy and configuration
versions; a valid empty snapshot can have a live channel but no usable credentials.

**L5.** Lookup and full-snapshot replacement are serialized with the subscriber lock;
the database has one retained connection. No lookup observes a partially installed
snapshot. Lookups MUST use a deadline bounded by the lease and proxy certificate
validity, and recheck validity after the database returns, before releasing a value.
Publisher state locks MUST NOT span request-body reads or response writes; slow
subscribers must not block publication of revocations. The reference bounds HTTP
reads/writes to three seconds using the standard server response controller.
SQLite is opened as `:memory:` with in-memory temporary storage.

## Invariant → implementation → evidence

| Invariant | Reference implementation | Executable evidence |
|---|---|---|
| Verified exact workload identities | `PeerIdentity`, `WorkloadClientTLS`, `WorkloadServerTLS` in [secure.go](secure.go) | `TestSecureIdentityAndIsolation`: no certificate, unknown issuer/environment, wrong publisher/proxy |
| Scoped source delivery | `Publisher.ServeHTTP` | `scoped_delivery`, `subscription_escalation_rejected`, `wrong_subscriber_rejected` |
| Authorized local credential use | `Subscriber.resolve`, `Subscriber.Process` | `legitimate_proxy`, `forged_backend_rejected`, `direct_plaintext_rejected` |
| Private per-instance DB | `NewSubscriber` → `NewAuth` | Two subscribers with disjoint working sets; `TestStoreConcurrencyAndMemory` |
| Fresh restart bootstrap | `Subscriber.Fetch`, `Subscriber.install` | `old_session_replay`, `revocation_then_restart`, `publisher_outage` |
| Rollback/equivocation rejection | `Subscriber.install` | `publisher_rollback`, `publisher_equivocation` |
| Atomic bounded snapshot | `Subscriber.install` | `invalid_snapshot_is_atomic`, `TestSecureSnapshotValidation` |
| Lease-enforced use | `Subscriber.resolve`, `Ready`, `install` | `lease_expiry`, `slow_bootstrap_does_not_extend_lease` |
| Expiry on reused connections | `PeerIdentity`, `peerExpiry`, `Fetch`, `resolve` | `TestSecureExpiredSubscriberCannotRenew`, `TestSecureExpiredProxyCannotResolve`, `TestSecureExpiredPublisherCannotRenew` |
| Publication independent of slow body | `Publisher.ServeHTTP` | `TestSecureSlowBodyDoesNotBlockPublication` |
| Lease expiry during queued lookup | `Subscriber.resolve` | `TestSecureLeaseExpiresWhileLookupQueued` |
| Certificate replacement with same grant | TLS identity extraction | `same_identity_cert_rotation`: new certificate, same URI, new connection |
| Universal upstream bearer placement | [auth.go](auth.go), [integration_test.go](integration_test.go) | Original local Envoy + real MCP lifecycle/tool-call checks |

## Running the reference

```sh
./run-security.sh  # TLS identity + replay/lease + store tests; race detector and vet
./run.sh           # additionally builds real MCP fixture and runs Envoy integration
```

No cloud, Kubernetes, real credentials or mounts are required. The security-only
suite needs Go and a C compiler for the SQLite driver; it does not need Envoy.
Tests generate private keys and credential values in memory and do not print them.

The original `TestSecurityReview*` tests remain deliberate insecure-fixture controls.
They reproduce the baseline gaps. `TestSecure*` tests verify the secured Subscriber
entrypoint rejects those cases; the insecure fixture has not become a production API.

## What is and is not proven

The security suite uses real HTTPS and gRPC mutual-TLS connections, including
successful authorized injection and adversarial requests. The existing Envoy/MCP
suite proves filter placement using the original local plaintext fixture. These are
two evidence tiers, NOT an end-to-end proof that Envoy has received/rotated file-free
identity material and used the secured Subscriber. That integration is outstanding.

Production work also includes: durable MP/failover, workload attestation and renewal,
CA rollover, end-user authorization and destination binding, configuration/policy
channel coordination, prompt failure handling on the deployment's Envoy version,
capacity/overload limits, retries/signing, and host memory/dump protections.
Turso and SDS are not dependencies of this reference.

## Executable multi-channel research

[RESEARCH.md](RESEARCH.md) records five open architectural questions and maps them
to bounded state exploration, counterexamples, and runtime tests. The separate
`model` package proposes dependency-gated activation and dispatch revalidation.
Those proposals do not extend the implemented Subscriber/Envoy contract above.
`TestResearchTrustedPublisherRollback` demonstrates the documented cold-bootstrap
limit against the real HTTPS/SQLite reference; `TestResearchRevocationUnderReaders`
checks bounded update progress under concurrent local resolution.
