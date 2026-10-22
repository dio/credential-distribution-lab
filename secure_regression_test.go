package spike

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	ep "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

func shortLeaf(t *testing.T, ca *testCA, id string, expires time.Time) tls.Certificate {
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	check(t, e)
	serial, e := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	check(t, e)
	uri, e := url.Parse(id)
	check(t, e)
	tpl := &x509.Certificate{SerialNumber: serial, NotBefore: time.Now().Add(-time.Minute), NotAfter: expires, URIs: []*url.URL{uri}, DNSNames: []string{"localhost"}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
	der, e := x509.CreateCertificate(rand.Reader, tpl, ca.cert, &key.PublicKey, ca.key)
	check(t, e)
	pk, e := x509.MarshalPKCS8PrivateKey(key)
	check(t, e)
	cert, e := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk}))
	check(t, e)
	return cert
}
func TestSecureExpiredSubscriberCannotRenew(t *testing.T) {
	ca := newCA(t)
	pub := NewPublisher("epoch-1", time.Second, map[string]DeliveryGrant{subscriberA: {"sub-a", []Binding{{"scope-a", "token"}}}})
	pub.Publish([]Credential{{"scope-a", "token", 1, rand.Text(), time.Now().Unix() + 60, false}})
	server := tlsPublisher(t, ca, pub, publisherID)
	expiry := time.Now().Add(3 * time.Second).Truncate(time.Second)
	s, e := NewSubscriber(map[string]Binding{"backend": {"scope-a", "token"}}, map[string][]string{proxyA: {"backend"}}, server.URL, "sub-a", "epoch-1", WorkloadClientTLS(shortLeaf(t, ca, subscriberA, expiry), ca.roots, publisherID), time.Second)
	check(t, e)
	defer s.Close()
	check(t, s.Fetch(context.Background()))
	time.Sleep(time.Until(expiry.Add(100 * time.Millisecond)))
	e = s.Fetch(context.Background())
	if e == nil || s.Ready() {
		t.Fatal("expired subscriber renewed lease")
	}
	s.client.CloseIdleConnections()
	if e = s.Fetch(context.Background()); e == nil {
		t.Fatal("fresh TLS unexpectedly accepted expired cert")
	}
}
func TestSecureExpiredProxyCannotResolve(t *testing.T) {
	ca := newCA(t)
	pub := NewPublisher("epoch-1", time.Second, map[string]DeliveryGrant{subscriberA: {"sub-a", []Binding{{"scope-a", "token"}}}})
	pub.Publish([]Credential{{"scope-a", "token", 1, rand.Text(), time.Now().Unix() + 60, false}})
	server := tlsPublisher(t, ca, pub, publisherID)
	s := secureSubscriber(t, ca, server.URL, subscriberA, "sub-a", "scope-a", "token")
	check(t, s.Fetch(context.Background()))
	addr := grpcSubscriber(t, ca, s)
	expiry := time.Now().Add(3 * time.Second).Truncate(time.Second)
	conn, e := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(WorkloadClientTLS(shortLeaf(t, ca, proxyA, expiry), ca.roots, authID))))
	check(t, e)
	defer conn.Close()
	invoke := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs("x-spike-backend", "backend"))
		stream, e := ep.NewExternalProcessorClient(conn).Process(ctx)
		if e != nil {
			return e
		}
		defer stream.CloseSend()
		if e = stream.Send(&ep.ProcessingRequest{Request: &ep.ProcessingRequest_RequestHeaders{RequestHeaders: &ep.HttpHeaders{}}}); e != nil {
			return e
		}
		r, e := stream.Recv()
		if e != nil {
			return e
		}
		if r.GetImmediateResponse() != nil {
			return fmt.Errorf("denied")
		}
		return nil
	}
	check(t, invoke())
	time.Sleep(time.Until(expiry.Add(100 * time.Millisecond)))
	check(t, s.Fetch(context.Background()))
	if invoke() == nil {
		t.Fatal("expired proxy received credential")
	}
}
func TestSecureSlowBodyDoesNotBlockPublication(t *testing.T) {
	ca := newCA(t)
	pub := NewPublisher("epoch-1", time.Second, map[string]DeliveryGrant{subscriberA: {"sub-a", []Binding{{"scope-a", "token"}}}})
	pub.Publish(nil)
	reading := make(chan struct{})
	server := tlsPublisher(t, ca, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = &signaledBody{ReadCloser: r.Body, started: reading}
		pub.ServeHTTP(w, r)
	}), publisherID)
	u, e := url.Parse(server.URL)
	check(t, e)
	conn, e := tls.Dial("tcp", u.Host, WorkloadClientTLS(ca.leaf(t, subscriberA), ca.roots, publisherID))
	check(t, e)
	defer conn.Close()
	_, e = fmt.Fprintf(conn, "POST /snapshot HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n1\r\n{\r\n")
	check(t, e)
	// The incomplete body must not retain the state lock.
	select {
	case <-reading:
	case <-time.After(time.Second):
		t.Fatal("body read not reached")
	}
	done := make(chan struct{})
	go func() { pub.Publish(nil); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		conn.Close()
		<-done
		t.Fatal("slow subscriber blocked publication")
	}

}
func TestSecureLeaseExpiresWhileLookupQueued(t *testing.T) {
	ca := newCA(t)
	s := secureSubscriber(t, ca, "https://localhost:1", subscriberA, "sub-a", "scope-a", "token")
	q := SnapshotRequest{rand.Text() + rand.Text(), "sub-a"}
	snap := Snapshot{q.Challenge, q.Subscription, "epoch-1", 1, 100, []Credential{{"scope-a", "token", 1, rand.Text(), time.Now().Unix() + 60, false}}}
	check(t, s.install(context.Background(), q, snap, time.Now()))
	cert := ca.leaf(t, proxyA)
	parsed, e := x509.ParseCertificate(cert.Certificate[0])
	check(t, e)
	ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{parsed, ca.cert}}, PeerCertificates: []*x509.Certificate{parsed}}}})
	held, e := s.auth.db.Conn(context.Background())
	check(t, e)
	type result struct {
		v string
		e error
	}
	done := make(chan result, 1)
	go func() { v, e := s.resolve(ctx, "backend"); done <- result{v, e} }()
	deadline := time.Now().Add(time.Second)
	for s.auth.db.Stats().WaitCount == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
	check(t, held.Close())
	r := <-done
	if r.e == nil || r.v != "" {
		t.Fatal("credential returned after lease expiry")
	}
	if s.Ready() {
		t.Fatal("lease unexpectedly valid")
	}
}

type signaledBody struct {
	io.ReadCloser
	started chan struct{}
}

func (b *signaledBody) Read(p []byte) (int, error) {
	if b.started != nil {
		close(b.started)
		b.started = nil
	}
	return b.ReadCloser.Read(p)
}

func TestSecureExpiredPublisherCannotRenew(t *testing.T) {
	ca := newCA(t)
	expiry := time.Now().Add(3 * time.Second).Truncate(time.Second)
	pub := NewPublisher("epoch-1", time.Second, map[string]DeliveryGrant{subscriberA: {"sub-a", []Binding{{"scope-a", "token"}}}})
	pub.Publish(nil)
	server := httptest.NewUnstartedServer(pub)
	server.TLS = WorkloadServerTLS(shortLeaf(t, ca, publisherID, expiry), ca.roots)
	server.StartTLS()
	defer server.Close()
	s := secureSubscriber(t, ca, server.URL, subscriberA, "sub-a", "scope-a", "token")
	check(t, s.Fetch(context.Background()))
	time.Sleep(time.Until(expiry.Add(100 * time.Millisecond)))
	if s.Fetch(context.Background()) == nil || s.Ready() {
		t.Fatal("expired publisher renewed lease")
	}
}
