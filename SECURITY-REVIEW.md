# Security review and mitigation plan

2026-10-22. Scope: the local spike and the proposed layered delivery design.
The harness proves placement and local enforcement, not a production trust boundary.
No deployed vulnerability is claimed. No production implementation was changed.

Follow-up: [the architecture spec and secured reference](SPEC.md) now implement
identity grants and fresh-bootstrap/lease enforcement in a separate Subscriber
entrypoint. The baseline harness gaps below remain as explicit control cases.

## Confirmed harness gaps

| Priority before production | Finding | Evidence |
|---|---|---|
| Critical if reachable by an untrusted process | Direct gRPC caller claims a backend ID and receives the credential in the mutation response | `auth.go:108`; `integration_test.go:41`; `TestSecurityReviewDirectGRPCDisclosure` |
| Critical if reachable by an untrusted process | Update API accepts credential replacement without publisher authentication or scope authorization | `auth.go:89`; `TestSecurityReviewUnauthenticatedDelivery` |
| High | Restart discards revision/revocation history; replayed old unexpired record becomes usable | `auth.go:36`; `TestSecurityReviewRestartReplay` |
| High availability risk | Denials and auth-service outages hold requests until the five-second route timeout on tested Envoy 1.33.2 | `RESULTS.md`, runtime observation; no load/DoS benchmark |

Executed `go test -race -count=1 -run TestSecurityReview -v .`: all three gap
characterization tests passed. A pass means the gap was reproduced, not mitigated.
Tests use synthetic values and never print credentials. Convert them into rejection
regressions when implementing the controls below.

## Target ownership

```text
Remote MP authority / credential publisher
  <authenticated, mutually authorized encrypted stream>
Proxy instance credential subscriber
  -> private SQLite :memory:
  -> local universal backend-auth enforcement
  -> authenticated destination
```

Each proxy instance owns its subscriber, DB, revision state and credential-use
handler. Do not share a SQLite file, expose arbitrary SQL, or offer a general
credential GET API. A compromise is limited to that instance's authorized working
set, not the entire fleet's credentials. Scope selection is enforced at the source;
subscriber-side filtering is defense in depth, not the primary isolation control.

With extproc, “embedded per proxy” means the DB is embedded in that proxy's dedicated
local auth process; it is not literally inside Envoy. Envoy receives header bytes
through ext_proc, so both processes are within the credential trust boundary. If we
later embed enforcement in Envoy, the RPC boundary disappears but the process blast
radius changes. Do not require that migration for this design.

## 1. Authenticate identities, then authorize each operation

Assign short-lived workload identities to the publisher and every subscriber using
a trusted issuer (for example SPIFFE-compatible workload identity). Bind identity to
the actual workload deployment and environment; a subscriber-supplied name is not
identity. Validate the exact expected publisher identity, trust domain and environment,
not merely a certificate chaining to any accepted CA. Publisher validates subscriber
identity and authorizes each requested subscription scope and credential family.

The MP owns the mapping from workload identity to allowed scopes. Certificate
rotation must preserve intended authority; CA rotation needs an explicit overlap
window and retirement. Authenticate relays as separate principals if introduced;
do not let a relay silently expand a subscriber's grants. If relays terminate the
trust boundary, use end-to-end signed publication envelopes bound to the authorized
subscription, or give the relay explicit publisher authority and accept that trust.

Use in-memory workload identity material delivered by a trusted identity agent or
platform mechanism; never a long-lived bootstrap secret written to a manifest.
Qualify bootstrap, renewal and outages independently of credential delivery.

Remove the harness PUT endpoint from the production runtime. The subscriber accepts
only its authorized publication stream. Delivery authority is separate from the
right to consume a credential: a proxy cannot publish or mint credentials.

## 2. Bind the local enforcement channel to the intended proxy

Static gRPC metadata is a selector, not proof of identity. Authenticate Envoy to its
local auth service and bind that peer to the permitted backend mappings. Use mTLS
with dedicated identities if retaining loopback TCP; disable unauthenticated RPC.
Loopback or Kubernetes network policy alone cannot separate containers sharing a
network namespace. Do not expose the auth listener through a Service/ingress.

For a no-mount implementation, an authenticated loopback stream is compatible;
filesystem socket mounts are not required. Test how the chosen Envoy build receives
and rotates its identity material without files. In-process enforcement is another
option, but not a shortcut that makes compromised-process access safe.

Any authorized ext_proc peer can receive the credentials it is permitted to inject.
This is inherent in the protocol. Authd must not accept an arbitrary backend ID from
an otherwise authorized peer; it checks that peer's exact binding allowlist.

## 3. Make credential use an authorized destination binding

Authenticate the external caller at ingress and authorize its selected backend or
profile before upstream credential use. A valid proxy identity alone does not prove
that the end user may spend or use the selected credential. Shared platform credentials
may use a backend-level grant; user credentials require the user's authorized binding.

Bind the configuration to `(scope, credential ID, mechanism, destination/audience,
policy revision)`. A request cannot supply its own URL, scope, credential ID or raw
trusted metadata. Envoy must prevent client headers from becoming trusted policy
context. Maintain exact authorized upstream destinations, TLS identity verification,
and an explicit policy for DNS/address changes. Redirects must not carry credentials
to a new origin. Retries/fallbacks must resolve the actual new destination's binding;
never reuse an injected header across incompatible destinations.

A route writer must not be able to retarget a privileged credential to an arbitrary
server. Validate binding changes against credential-use grants at publication and
again at the consumer. Signed configuration alone does not establish this grant.

## 4. Establish freshness again after every cold start

A local monotonically increasing revision only protects one process lifetime.
The no-persistent-local-state requirement is preserved by requiring an authenticated
fresh bootstrap before readiness:

1. Subscriber establishes a new authenticated session and sends a random challenge.
2. Publisher returns a complete authorized snapshot bound to that challenge/session,
   subscription ID, authority epoch, revision and bounded lease.
3. Consumer atomically installs and validates the snapshot, then becomes ready.
4. Deltas specify the expected previous revision. Gaps or epoch changes trigger
   a fresh snapshot; old-session messages are rejected.

A transport with equivalent authenticated session freshness can replace an explicit
signed challenge, but stale relay caches must not masquerade as fresh authority.
A signed old snapshot alone is insufficient: authentic does not mean current.
The authoritative publisher needs its own durable epoch/revision and failover rules.

On total volatile-state loss and MP unavailability, remain unready. Do not restore
from an unvalidated old message. This is the deliberate availability cost of having
no durable local credentials or freshness anchors.

## 5. Coordinate channels without merging their ownership

Use separate config, credential and policy channels, all bound to the same subscriber
identity and explicit scope. Activate a config generation only when its credential
bindings and authorization dependencies are usable together. Keep config revision,
credential revision, policy revision and lease state separately observable.

`current` means the latest authorized usable credential within a particular binding,
not any credential found under a matching ID. Never fall back across scopes/accounts
when a version is unavailable. Publish replacement authority grants before dependent
config and retire them deliberately. Readiness must cover new replicas as well as
updates on existing replicas. In-flight attempts retain their captured credential;
revocation prevents new resolutions, not already executed upstream effects.

## 6. Bound disconnected use and deny cheaply

Usability requires both credential validity and a still-valid subscription/authority
lease. Apply a locally configured maximum lease and clock-skew allowance; do not let
an update grant effectively infinite freshness. Anchor received lease duration to a
monotonic timer so wall-clock rollback does not extend a running lease. A partition
cannot provide immediate revocation: document the maximum exposure as the remaining
lease, and tighten it where required.

Fix/qualify prompt fail-closed responses on the deployment's Envoy pin. Bound gRPC
streams, lookup concurrency, queues, update batch sizes and memory use. Isolate update
processing from request starvation. Do not allow a configuration override to enable
fail-open, skip authentication or create an unfiltered destination cluster. A missing
filter is not protected by `failure_mode_allow: false` on another filter.

## 7. Treat memory custody honestly

SQLite :memory: removes DB-file persistence; it does not remove plaintext copies in
Go strings, SQLite/native allocations, gRPC messages and Envoy headers. Scope copies
minimally, avoid heap/core dumps and payload traces, protect admin/debug interfaces,
and configure no-swap/crash-dump handling at the node boundary. Garbage collection
is not guaranteed secret zeroization. TLS to the actual upstream is mandatory outside
the loopback test. A compromised authorized proxy can expose its permitted plaintext
credentials; use narrowly scoped and short-lived upstream credentials where available.

Validate credential envelope sizes and mechanism-specific input (including disallowed
header control characters) before activation. Parameterized SQL in the spike is a
positive control, but is not publisher authorization. The test-only fixed MCP session
key and absence of ingress authorization must never become production defaults.

## Small, dependency-ordered qualification plan

1. **Identity boundary:** unauthorized publisher and direct RPC caller are rejected;
   wrong issuer/environment/peer-scope grants rejected; legitimate rotation succeeds.
2. **Fresh bootstrap:** replay a pre-revocation snapshot after restart, through a stale
   relay and after publisher failover; readiness stays closed until fresh authority.
3. **Binding isolation:** two subscribers with disjoint scopes; malicious route edit,
   forged caller metadata, cross-origin redirect and fallback cannot export credentials.
4. **Channel skew:** config arrives before credential/policy, permission removed while
   delivery lags, and subscriber lease expires while disconnected; no unauthorized use.
5. **Failure containment:** authd loss, overload and invalid updates fail quickly with
   zero upstream HTTP requests; inspect effective Envoy config for bypasses.

Prioritize 1 and 2 next. They address reproduced gaps without requiring Turso or a
fleet-scale implementation. Production acceptance requires all applicable controls;
keeping one universal auth filter does not require one universal trust domain.

References: [Envoy ext_proc failure controls](https://www.envoyproxy.io/docs/envoy/v1.38.0/api-v3/extensions/filters/http/ext_proc/v3/ext_proc.proto),
[SQLite in-memory connection semantics](https://www.sqlite.org/c3ref/open.html),
[SQLite temporary storage](https://www.sqlite.org/tempfiles.html).
