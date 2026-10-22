# Research: what may a stale local replica authorize?

Status: executable bounded experiments, not a distributed-system correctness proof.
The TLS/SQLite reference and the abstract model are deliberately separate evidence.

## Questions worth preserving

1. **What does revocation actually guarantee?** A disconnected proxy cannot observe
   an immediate revocation. How long may it use previous knowledge, and does the
   decision occur at resolution or dispatch?
2. **When can independently delivered channels activate together?** Individually
   valid configuration and credential snapshots can form an invalid combination.
   What dependency contract permits activation without requiring global lockstep?
3. **What can an empty subscriber know after restart?** A fresh challenge proves
   response freshness, not that a trusted publisher retained authoritative history.
4. **Does identity authorize possession, use, or both?** A subscriber grant controls
   which bytes arrive. A use grant must additionally bind tenant, backend,
   destination, credential reference/version, and the lifetime of the decision.
5. **Can stronger isolation weaken revocation availability?** Private databases
   bound exposure, but queues, shared publisher locks and overloaded consumers can
   delay revocation. What still fails closed when update progress is impaired?

## Proposed contract

```text
receive -> validate -> stage -> satisfy dependencies -> activate
                                                   -> resolve -> recheck at dispatch
```

Receipt is not activation. `model` uses exact credential-version dependencies:
configuration v2 cannot activate against credential v1. A compatible previous
configuration can remain active, but every resolution checks whether its current
credential is usable. Removing/revoking/expiring a record makes it unusable even
if an old configuration remains stored as active. Invalid/old/equivocating updates
leave accepted state unchanged. This trades some availability during rotation for
an explicit, testable safety contract.

A proposed dispatch ticket binds the full configuration and subscriber session.
Dispatch checks the current active configuration, lease, credential expiry and
revocation, plus the exact requested use binding. This is an abstract local
linearization point. The existing ext_proc handler only controls resolution and
header mutation; it cannot retract bytes already returned to Envoy. The proposed
dispatch contract is NOT implemented in Envoy or established by the model tests.
Retries to a different destination require a new authorized resolution.

## Model and bounds

The value-type state machine in [model/model.go](model/model.go) has one publisher,
two subscriber states, two tenant scopes, one credential key per scope, two
credential/configuration versions, an authority epoch, a complete snapshot revision,
a logical clock, and a two-tick lease. Credentials expire at tick six. No real
credential bytes occur in the model. Subscriber identity and authenticated transport
are assumptions here, exercised separately by the TLS suite.

A response records session/challenge and fetch-start time. Its lease is anchored to
fetch start. Duplicate delivery cannot extend that deadline. Pending responses
survive restart in the simulated transport, so old-session replay can be attempted.
Session/challenge counters represent fresh unpredictable randomness; they are ghost
state, not durable storage proposed for the subscriber. Warm subscribers reject
publication rollback and same-revision content changes. Cold subscribers forget
history, but never reuse an old session's challenge.

`TestBoundedSchedules` explores all distinct states reachable within **six events**
from both a cold and a fully bootstrapped state, using exact state deduplication.
Actions include independent subscriber deliveries/configs, duplicate delivery,
resolve, disconnect/reconnect, restart, publication rotation/revocation and time.
Every reached state is checked for safe resolution and dispatch; revision monotonicity
is checked across transitions within the same subscriber lifetime. Failure prints
the action trace and full non-secret model state. Root state is cold or bootstrapped
(as reported by the test); reapply the trace from either root to reproduce it.

`TestChannelPermutations` explores all **720 permutations** of six deliveries,
including delayed old configurations, and requires eventual v2 usability at both
subscribers after compatible deliveries finish. This is bounded convergence under
complete delivery, not unconditional liveness under partition or infinite churn.
`TestLongSchedules` samples **256 schedules of 64 events**, seed **43197**, to cover
longer histories. It is deterministic sampling, not exhaustive exploration.

Logical time is monotonic and trusted. No clock skew, arbitrary publisher Byzantine
behavior, cryptographic attacks, identity revocation, multi-publisher fencing,
resource accounting or crash-during-transaction model is included. No claim extends
from this finite state space to all possible distributed executions.

## Experiments and falsifiable outcomes

| Question | Named experiment | Required observation |
|---|---|---|
| Compatible activation | `TestChannelPermutations`, `TestActivationCounterexample` | No mixed-version resolution; both subscribers converge when compatible deliveries complete |
| Delay/reorder/restart | `TestBoundedSchedules`, `TestLongSchedules`, `TestInvalidPublications` | No successful resolution/dispatch violates scope, dependency or validity; rejected updates are atomic |
| Revocation boundary | `TestRevocationAndDispatchBoundary` | Disconnected stale use remains possible before lease expiry; observed revocation or lease expiry blocks delayed dispatch |
| Cold authority rollback | `TestRestartAndPublisherRollbackLimit` | A live rolled-back publisher can bootstrap stale state with a new challenge: expected limitation |
| Real rollback boundary | `TestResearchTrustedPublisherRollback` | HTTPS/SQLite warm consumer rejects rollback; new consumer accepts stale authority: expected limitation |
| Use vs possession | `TestUseBindingAndRetry` | Tenant/backend/destination/key/version substitution, cross-subscriber tickets and pre-restart tickets are rejected |
| Reader contention | `TestResearchRevocationUnderReaders` | Real subscriber installs revocation with eight continuous readers within the five-second test deadline; 32 subsequent resolutions deny |
| Slow subscriber | `TestSecureSlowBodyDoesNotBlockPublication` | An incomplete authenticated body cannot retain the publisher state lock |
| Queued lookup | `TestSecureLeaseExpiresWhileLookupQueued` | A lookup queued across lease expiry returns no credential |
| Identity lifetime | `TestSecureExpiredSubscriberCannotRenew`, `TestSecureExpiredProxyCannotResolve`, `TestSecureExpiredPublisherCannotRenew` | Expired identities on reused connections cannot authorize new operations |

The model oracle independently inspects the record/binding/lease fields rather than
calling the implementation's `usable` predicate to decide whether success was safe.
Directed negative controls demonstrate why unconditional activation and unchecked
captured-byte dispatch are unsafe. Those controls characterize counterexamples;
the cold-publisher rollback control intentionally succeeds at reproducing a limit.
A passing control must never be presented as a mitigated vulnerability.

The model CI lane also deliberately removes dependency, lease and session guards in
three temporary copies. Each corresponding regression must fail behaviorally;
compiler errors and timeouts do not count. This checks that a green model suite
is sensitive to concrete unsafe changes, rather than merely exercising code.

The contention test establishes progress for that bounded load on the tested runtime,
not a latency SLO, starvation proof or throughput benchmark. Certificates are synthetic
and generated in memory; its request peer context is constructed directly, while
snapshot fetches use real mutually authenticated HTTPS. The existing security suite
separately verifies actual gRPC authentication.

## Mitigations to investigate next

- Durable publisher revision/epoch and fenced failover, with an explicit recovery
  procedure when no trustworthy checkpoint survives. Client freshness challenges
  alone cannot solve the cold-start rollback case.
- Coordinated activation across policy as a third channel. Start with explicit
  dependency revisions; compare availability with exact-version vs compatible-range
  dependencies and atomic generation manifests.
- A real dispatch enforcement point and per-attempt rebinding for retry/fallback.
  Document what happens to bytes already in Envoy before claiming dispatch-time
  revocation guarantees.
- Queue bounds, snapshot size limits and CPU/memory pressure experiments. Separate
  safety (deny expired authority) from timely installation of revocation updates.
- Model clock skew and identity/authority epochs independently. Host memory custody,
  swaps and dumps remain deployment concerns outside the protocol model.

## Run and inspect evidence

```sh
./ci/run.sh model
./ci/run.sh research
./ci/run.sh controls
./ci/run.sh security
./ci/run.sh spec-package
# Real Envoy/MCP placement, with Envoy on PATH:
./ci/run.sh placement
```

The manifest [ci/experiments.json](ci/experiments.json) requires named experiments.
[ci/check_results.py](ci/check_results.py) rejects failures, skips, missing experiments
and incomplete package/test completion. Its own negative tests run in spec-package.
Every lane records test JSON and an outcome summary under ignored `evidence/`.
Controls are labelled separately. A failed download is a failed bootstrap, not a
passed or skipped placement experiment.

The standalone workflow is in [.github/workflows/verify.yml](.github/workflows/verify.yml).
Hosted evidence is recorded separately from historical local results in
[RESEARCH-RESULTS.md](RESEARCH-RESULTS.md).
