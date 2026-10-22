package spike

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

func researchPeer(t *testing.T, ca *testCA) context.Context {
	t.Helper()
	cert, err := x509.ParseCertificate(ca.leaf(t, proxyA).Certificate[0])
	check(t, err)
	return peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{
		VerifiedChains: [][]*x509.Certificate{{cert, ca.cert}}, PeerCertificates: []*x509.Certificate{cert},
	}}})
}

// This is an expected limitation control, not a security guarantee: a fresh
// challenge does not replace durable authority state at the trusted publisher.
func TestResearchTrustedPublisherRollback(t *testing.T) {
	ca := newCA(t)
	grant := map[string]DeliveryGrant{subscriberA: {"sub-a", []Binding{{"scope-a", "token"}}}}
	old := Credential{"scope-a", "token", 1, "synthetic-research-value", time.Now().Unix() + 60, false}
	pub := NewPublisher("epoch-1", time.Second, grant)
	pub.Publish([]Credential{old})
	var current atomic.Pointer[Publisher]
	current.Store(pub)
	server := tlsPublisher(t, ca, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { current.Load().ServeHTTP(w, r) }), publisherID)
	s := secureSubscriber(t, ca, server.URL, subscriberA, "sub-a", "scope-a", "token")
	check(t, s.Fetch(context.Background()))
	revoked := old
	revoked.Revision++
	revoked.Revoked = true
	pub.Publish([]Credential{revoked})
	check(t, s.Fetch(context.Background()))
	ctx := researchPeer(t, ca)
	if _, err := s.resolve(ctx, "backend"); err == nil {
		t.Fatal("revocation not observed")
	}
	rolledBack := NewPublisher("epoch-1", time.Second, grant)
	rolledBack.Publish([]Credential{old})
	current.Store(rolledBack)
	if s.Fetch(context.Background()) == nil {
		t.Fatal("warm subscriber accepted rollback")
	}
	cold := secureSubscriber(t, ca, server.URL, subscriberA, "sub-a", "scope-a", "token")
	check(t, cold.Fetch(context.Background()))
	if _, err := cold.resolve(ctx, "backend"); err != nil {
		t.Fatal("expected cold-start limitation was not reproduced")
	}
	t.Log("EXPECTED LIMITATION: warm rollback rejected; cold bootstrap trusts stale authority answering a fresh challenge")
}

func TestResearchRevocationUnderReaders(t *testing.T) {
	ca := newCA(t)
	pub := NewPublisher("epoch-1", time.Second, map[string]DeliveryGrant{subscriberA: {"sub-a", []Binding{{"scope-a", "token"}}}})
	c := Credential{"scope-a", "token", 1, "synthetic-research-value", time.Now().Unix() + 60, false}
	pub.Publish([]Credential{c})
	server := tlsPublisher(t, ca, pub, publisherID)
	s := secureSubscriber(t, ca, server.URL, subscriberA, "sub-a", "scope-a", "token")
	check(t, s.Fetch(context.Background()))
	ctx, cancel := context.WithTimeout(researchPeer(t, ca), 5*time.Second)
	var readers sync.WaitGroup
	defer func() { cancel(); readers.Wait() }()
	started := make(chan struct{}, 8)
	for range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			_, _ = s.resolve(ctx, "backend")
			started <- struct{}{}
			for ctx.Err() == nil {
				_, _ = s.resolve(ctx, "backend")
			}
		}()
	}
	for range 8 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("readers did not start")
		}
	}
	c.Revision++
	c.Revoked = true
	pub.Publish([]Credential{c})
	check(t, s.Fetch(ctx))
	for range 32 {
		if value, err := s.resolve(ctx, "backend"); err == nil || value != "" {
			t.Fatal("credential released after revocation installation")
		}
	}
	t.Log("8 continuous readers; revocation installed within 5s test deadline; 32 subsequent resolutions denied")
}
