package model

import (
	"fmt"
	"math/rand"
	"testing"
)

func ready() World {
	w := Initial()
	for i := range w.Subscribers {
		for _, kind := range []string{"config1", "fetch", "deliver", "resolve"} {
			w = w.Step(Action{kind, i})
		}
	}
	return w
}

// Independent assertion: do not call usable to decide whether a successful
// resolution/dispatch was safe. Failures include the replayable action trace.
func invariant(t *testing.T, w World, trace []Action) {
	t.Helper()
	for i, s := range w.Subscribers {
		for owner := range 2 {
			for version := 1; version <= 2; version++ {
				request := Configuration(owner, version).Binding
				ticket, ok := w.Resolve(i, request)
				if ok {
					c := s.Applied.Records[i]
					if owner != i || request != s.Active.Binding || s.Active.Revision == 0 ||
						s.Applied.Epoch != 1 || s.Applied.Revision == 0 || w.Now >= s.Deadline ||
						c.Tenant != request.Tenant || c.Key != request.Key || c.Version != request.Version ||
						c.Revoked || w.Now >= c.Expires {
						t.Fatalf("unsafe resolution: trace=%v state=%+v", trace, w)
					}
					if !w.Dispatch(i, ticket, request) {
						t.Fatalf("fresh valid ticket rejected: %v", trace)
					}
				}
				if w.Dispatch(i, s.Ticket, request) && (!ok || s.Ticket != ticket) {
					t.Fatalf("stale or misbound dispatch: trace=%v state=%+v", trace, w)
				}
			}
		}
		if s.Applied.Records[1-i] != (Credential{}) {
			t.Fatalf("scope leaked: %v", trace)
		}
	}
}

func TestBoundedSchedules(t *testing.T) {
	// Explore every unique state reachable within six events, starting both cold
	// and bootstrapped. State deduplication is exact (all fields are comparable).
	const depth = 6
	actions := []Action{{"tick", 0}, {"rotate", 0}, {"revoke", 0}}
	for i := range 2 {
		for _, kind := range []string{"config1", "config2", "fetch", "deliver", "resolve", "restart", "disconnect", "reconnect"} {
			actions = append(actions, Action{kind, i})
		}
	}
	type node struct {
		state World
		trace []Action
	}
	queue := []node{{Initial(), nil}, {ready(), nil}}
	seen := map[World]bool{queue[0].state: true, queue[1].state: true}
	transitions := 0
	for head := 0; head < len(queue); head++ {
		n := queue[head]
		invariant(t, n.state, n.trace)
		if len(n.trace) == depth {
			continue
		}
		for _, a := range actions {
			next := n.state.Step(a)
			trace := append(append([]Action(nil), n.trace...), a)
			transitions++
			for i, before := range n.state.Subscribers {
				after := next.Subscribers[i]
				if before.Session == after.Session && (after.Applied.Revision < before.Applied.Revision || after.Active.Revision < before.Active.Revision) {
					t.Fatalf("rollback within lifetime: %v", trace)
				}
			}
			if !seen[next] {
				seen[next] = true
				queue = append(queue, node{next, trace})
			}
		}
	}
	if len(seen) < 1000 {
		t.Fatalf("exploration unexpectedly small: %d", len(seen))
	}
	t.Logf("depth=%d states=%d transitions=%d roots=cold,bootstrapped", depth, len(seen), transitions)
}

func TestChannelPermutations(t *testing.T) {
	// Exhaust all 6! schedules: config and credentials for each subscriber,
	// with duplicated/delayed v1 delivery after v2 has been prepared.
	actions := []Action{{"config2", 0}, {"deliver", 0}, {"config2", 1}, {"deliver", 1}, {"config1", 0}, {"config1", 1}}
	w := ready().Step(Action{"rotate", 0}).Step(Action{"fetch", 0}).Step(Action{"fetch", 1})
	count := 0
	var visit func(World, []Action, []Action)
	visit = func(state World, remaining, trace []Action) {
		invariant(t, state, trace)
		if len(remaining) == 0 {
			count++
			for i := range 2 {
				if _, ok := state.Resolve(i, Configuration(i, 2).Binding); !ok {
					t.Fatalf("compatible state did not converge: %v", trace)
				}
			}
			return
		}
		for j, a := range remaining {
			rest := append(append([]Action(nil), remaining[:j]...), remaining[j+1:]...)
			visit(state.Step(a), rest, append(append([]Action(nil), trace...), a))
		}
	}
	visit(w, actions, nil)
	if count != 720 {
		t.Fatal(count)
	}
	t.Logf("complete delivery schedules=%d", count)
}

func TestRevocationAndDispatchBoundary(t *testing.T) {
	w := ready()
	ticket := w.Subscribers[0].Ticket
	request := Configuration(0, 1).Binding
	w = w.Step(Action{"revoke", 0}).Step(Action{"disconnect", 0})
	if !w.Dispatch(0, ticket, request) {
		t.Fatal("model incorrectly promises immediate disconnected revocation")
	}
	for range Lease {
		w = w.Step(Action{"tick", 0})
	}
	if w.Dispatch(0, ticket, request) {
		t.Fatal("lease did not bound stale use")
	}
	w = ready().Step(Action{"revoke", 0}).Step(Action{"fetch", 0}).Step(Action{"deliver", 0})
	if w.Dispatch(0, ticket, request) {
		t.Fatal("observed revocation did not block delayed dispatch")
	}
	// Negative control: a design that sends a previously captured credential
	// without rechecking would still hold this valid-looking nonzero ticket.
	if ticket.Config.Revision == 0 {
		t.Fatal("control did not capture a credential decision")
	}
	t.Log("counterexample: resolve -> receive revocation -> dispatch captured bytes; revalidation rejects")
}

func TestRestartAndPublisherRollbackLimit(t *testing.T) {
	w := ready()
	old := w.Pending[0]
	w = w.Step(Action{"revoke", 0}).Step(Action{"fetch", 0}).Step(Action{"deliver", 0})
	w = w.Step(Action{"restart", 0}).Step(Action{"config1", 0}).Step(Action{"fetch", 0})
	if w.Receive(0, old) {
		t.Fatal("old-session replay accepted")
	}
	// A trusted publisher that lost durable state can answer a NEW challenge
	// with old content. Memory-only subscribers cannot distinguish this from
	// legitimate first bootstrap. This test records a limitation, not a fix.
	w.Publisher = Initial().Publisher
	w = w.Step(Action{"fetch", 0}).Step(Action{"deliver", 0})
	if _, ok := w.Resolve(0, Configuration(0, 1).Binding); !ok {
		t.Fatal("limitation control not reproduced")
	}
	t.Log("counterexample: revoke -> restart -> fresh challenge to rolled-back trusted publisher -> stale use")
}

func TestUseBindingAndRetry(t *testing.T) {
	w := ready()
	request := Configuration(0, 1).Binding
	ticket := w.Subscribers[0].Ticket
	for _, field := range []string{"tenant", "backend", "destination", "key", "version"} {
		t.Run(field, func(t *testing.T) {
			other := request
			switch field {
			case "tenant":
				other.Tenant++
			case "backend":
				other.Backend++
			case "destination":
				other.Destination++
			case "key":
				other.Key++
			case "version":
				other.Version++
			}
			if _, ok := w.Resolve(0, other); ok {
				t.Fatal("forged reference resolved")
			}
			if w.Dispatch(0, ticket, other) {
				t.Fatal("retry redirected captured credential")
			}
		})
	}
	if w.Dispatch(1, ticket, Configuration(1, 1).Binding) {
		t.Fatal("cross-subscriber ticket accepted")
	}
	w = w.Step(Action{"restart", 0}).Step(Action{"config1", 0}).Step(Action{"fetch", 0}).Step(Action{"deliver", 0})
	if w.Dispatch(0, ticket, request) {
		t.Fatal("ticket survived restart")
	}
}

func TestInvalidPublications(t *testing.T) {
	for _, fault := range []string{"epoch", "rollback", "equivocation", "foreign-scope", "late", "challenge"} {
		t.Run(fault, func(t *testing.T) {
			w := ready()
			r := w.Pending[0]
			switch fault {
			case "epoch":
				r.Publication.Epoch++
			case "rollback":
				r.Publication.Revision = 0
			case "equivocation":
				r.Publication.Records[0].Revoked = true
			case "foreign-scope":
				r.Publication.Revision++
				r.Publication.Records[1] = Initial().Publisher.Records[1]
			case "late":
				w.Now = Lease
			case "challenge":
				r.Challenge++
			}
			before := w
			if w.Receive(0, r) || w != before {
				t.Fatalf("invalid %s changed state", fault)
			}
		})
	}
}

func ExampleWorld() {
	w := ready()
	_, ok := w.Resolve(0, Configuration(0, 1).Binding)
	fmt.Println(ok)
	// Output: true
}

func TestLongSchedules(t *testing.T) {
	const seed = 43197
	rng := rand.New(rand.NewSource(seed))
	kinds := []string{"config1", "config2", "fetch", "deliver", "resolve", "restart", "disconnect", "reconnect", "tick", "rotate", "revoke"}
	for trial := range 256 {
		w := Initial()
		if trial%2 == 0 {
			w = ready()
		}
		trace := make([]Action, 0, 64)
		for range 64 {
			a := Action{kinds[rng.Intn(len(kinds))], rng.Intn(2)}
			trace = append(trace, a)
			w = w.Step(a)
			invariant(t, w, trace)
		}
	}
	t.Logf("seed=%d schedules=256 length=64 (sampled, not exhaustive)", seed)
}

func TestActivationCounterexample(t *testing.T) {
	w := ready()
	w = w.Step(Action{"config2", 0})
	if w.Subscribers[0].Active.Revision != 1 {
		t.Fatal("incompatible configuration activated")
	}
	// Negative control for receive-equals-activate: a nonempty local record
	// alone is insufficient to satisfy the new configuration dependency.
	staged := w.Subscribers[0].Staged
	record := w.Subscribers[0].Applied.Records[0]
	if record.Version == staged.Binding.Version {
		t.Fatal("incompatibility control absent")
	}
	if _, ok := w.Resolve(0, staged.Binding); ok {
		t.Fatal("mixed generations served")
	}
	t.Log("counterexample: config v2 arrives while credential v1 exists; receipt alone cannot authorize activation")
}
