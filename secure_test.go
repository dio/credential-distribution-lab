package spike

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	ep "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

const publisherID = "spiffe://spike.local/mp/publisher"
const authID = "spiffe://spike.local/proxy/a/auth"
const subscriberA = "spiffe://spike.local/proxy/a/subscriber"
const subscriberB = "spiffe://spike.local/proxy/b/subscriber"
const proxyA = "spiffe://spike.local/proxy/a/envoy"
const proxyB = "spiffe://spike.local/proxy/b/envoy"

type testCA struct {
	cert  *x509.Certificate
	key   *ecdsa.PrivateKey
	roots *x509.CertPool
}

func newCA(t *testing.T) *testCA {
	t.Helper()
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	check(t, e)
	serial, e := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	check(t, e)
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "ephemeral spike CA"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, e := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	check(t, e)
	cert, e := x509.ParseCertificate(der)
	check(t, e)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{cert, key, pool}
}
func (c *testCA) leaf(t *testing.T, id string) tls.Certificate {
	t.Helper()
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	check(t, e)
	serial, e := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	check(t, e)
	uri, e := url.Parse(id)
	check(t, e)
	tmpl := &x509.Certificate{SerialNumber: serial, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), URIs: []*url.URL{uri}, DNSNames: []string{"localhost"}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
	der, e := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	check(t, e)
	private, e := x509.MarshalPKCS8PrivateKey(key)
	check(t, e)
	cert, e := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}))
	check(t, e)
	return cert
}
func tlsPublisher(t *testing.T, ca *testCA, h http.Handler, id string) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(h)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = WorkloadServerTLS(ca.leaf(t, id), ca.roots)
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}
func secureSubscriber(t *testing.T, ca *testCA, endpoint, id, sub, scope, cred string) *Subscriber {
	t.Helper()
	s, e := NewSubscriber(map[string]Binding{"backend": {scope, cred}}, map[string][]string{proxyA: {"backend"}}, endpoint, sub, "epoch-1", WorkloadClientTLS(ca.leaf(t, id), ca.roots, publisherID), time.Second)
	check(t, e)
	t.Cleanup(func() { s.Close() })
	return s
}
func grpcSubscriber(t *testing.T, ca *testCA, s *Subscriber) string {
	t.Helper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	check(t, e)
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(WorkloadServerTLS(ca.leaf(t, authID), ca.roots))))
	ep.RegisterExternalProcessorServer(server, s)
	go server.Serve(l)
	t.Cleanup(server.Stop)
	return l.Addr().String()
}
func callSecure(t *testing.T, address string, transport credentials.TransportCredentials, backend string) (string, error) {
	t.Helper()
	conn, e := grpc.NewClient(address, grpc.WithTransportCredentials(transport))
	if e != nil {
		return "", e
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs("x-spike-backend", backend))
	stream, e := ep.NewExternalProcessorClient(conn).Process(ctx)
	if e != nil {
		return "", e
	}
	if e = stream.Send(&ep.ProcessingRequest{Request: &ep.ProcessingRequest_RequestHeaders{RequestHeaders: &ep.HttpHeaders{}}}); e != nil {
		return "", e
	}
	r, e := stream.Recv()
	if e != nil {
		return "", e
	}
	defer stream.CloseSend()
	if r.GetImmediateResponse() != nil {
		return "", fmt.Errorf("credential denied")
	}
	headers := r.GetRequestHeaders().GetResponse().GetHeaderMutation().GetSetHeaders()
	if len(headers) != 1 {
		return "", fmt.Errorf("missing mutation")
	}
	return string(headers[0].GetHeader().GetRawValue()), nil
}
func TestSecureIdentityAndIsolation(t *testing.T) {
	ca := newCA(t)
	grants := map[string]DeliveryGrant{subscriberA: {"sub-a", []Binding{{"scope-a", "token"}}}, subscriberB: {"sub-b", []Binding{{"scope-b", "token"}}}}
	pub := NewPublisher("epoch-1", time.Second, grants)
	tokenA, tokenB := rand.Text(), rand.Text()
	pub.Publish([]Credential{{"scope-a", "token", 1, tokenA, time.Now().Unix() + 60, false}, {"scope-b", "token", 1, tokenB, time.Now().Unix() + 60, false}})
	server := tlsPublisher(t, ca, pub, publisherID)
	a := secureSubscriber(t, ca, server.URL, subscriberA, "sub-a", "scope-a", "token")
	b := secureSubscriber(t, ca, server.URL, subscriberB, "sub-b", "scope-b", "token")
	address := grpcSubscriber(t, ca, a)
	proxyTLS := func(id string) credentials.TransportCredentials {
		return credentials.NewTLS(WorkloadClientTLS(ca.leaf(t, id), ca.roots, authID))
	}
	t.Run("not_ready_before_bootstrap", func(t *testing.T) {
		if a.Ready() {
			t.Fatal("ready before bootstrap")
		}
		if _, e := callSecure(t, address, proxyTLS(proxyA), "backend"); e == nil {
			t.Fatal("accepted before bootstrap")
		}
	})
	t.Run("scoped_delivery", func(t *testing.T) {
		check(t, a.Fetch(context.Background()))
		check(t, b.Fetch(context.Background()))
		for _, s := range []*Subscriber{a, b} {
			var n int
			check(t, s.auth.db.QueryRow("SELECT count(*) FROM credentials").Scan(&n))
			if n != 1 {
				t.Fatal("unscoped delivery")
			}
		}
		v, e := a.auth.Resolve(context.Background(), "backend")
		check(t, e)
		if v != tokenA {
			t.Fatal("wrong scope A")
		}
		v, e = b.auth.Resolve(context.Background(), "backend")
		check(t, e)
		if v != tokenB {
			t.Fatal("wrong scope B")
		}
	})
	t.Run("legitimate_proxy", func(t *testing.T) {
		check(t, a.Fetch(context.Background()))
		v, e := callSecure(t, address, proxyTLS(proxyA), "backend")
		check(t, e)
		if v != "Bearer "+tokenA {
			t.Fatal("wrong credential")
		}
	})
	t.Run("direct_plaintext_rejected", func(t *testing.T) {
		if _, e := callSecure(t, address, insecure.NewCredentials(), "backend"); e == nil {
			t.Fatal("plaintext caller accepted")
		}
	})
	t.Run("no_client_certificate_rejected", func(t *testing.T) {
		cfg := WorkloadClientTLS(tls.Certificate{}, ca.roots, authID)
		cfg.Certificates = nil
		if _, e := callSecure(t, address, credentials.NewTLS(cfg), "backend"); e == nil {
			t.Fatal("anonymous TLS peer accepted")
		}
	})
	t.Run("anonymous_publisher_request_rejected", func(t *testing.T) {
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: ca.roots, ServerName: "localhost", MinVersion: tls.VersionTLS13}}, Timeout: time.Second}
		defer client.CloseIdleConnections()
		if r, e := client.Post(server.URL+"/snapshot", "application/json", nil); e == nil {
			r.Body.Close()
			t.Fatal("anonymous subscriber completed handshake")
		}
	})
	t.Run("wrong_proxy_rejected", func(t *testing.T) {
		check(t, a.Fetch(context.Background()))
		if _, e := callSecure(t, address, proxyTLS(proxyB), "backend"); e == nil {
			t.Fatal("wrong peer accepted")
		}
	})
	t.Run("forged_backend_rejected", func(t *testing.T) {
		if _, e := callSecure(t, address, proxyTLS(proxyA), "other-backend"); e == nil {
			t.Fatal("forged backend accepted")
		}
	})
	t.Run("wrong_environment_rejected", func(t *testing.T) {
		if _, e := callSecure(t, address, proxyTLS("spiffe://other.local/proxy/a/envoy"), "backend"); e == nil {
			t.Fatal("wrong environment accepted")
		}
	})
	t.Run("unknown_issuer_rejected", func(t *testing.T) {
		other := newCA(t)
		cfg := WorkloadClientTLS(other.leaf(t, proxyA), ca.roots, authID)
		if _, e := callSecure(t, address, credentials.NewTLS(cfg), "backend"); e == nil {
			t.Fatal("untrusted issuer accepted")
		}
	})
	t.Run("wrong_subscriber_rejected", func(t *testing.T) {
		s := secureSubscriber(t, ca, server.URL, proxyA, "sub-a", "scope-a", "token")
		if s.Fetch(context.Background()) == nil {
			t.Fatal("ungranted subscriber accepted")
		}
	})
	t.Run("subscription_escalation_rejected", func(t *testing.T) {
		s := secureSubscriber(t, ca, server.URL, subscriberA, "sub-b", "scope-b", "token")
		if s.Fetch(context.Background()) == nil {
			t.Fatal("subscriber selected foreign scope")
		}
	})
	t.Run("wrong_publisher_rejected", func(t *testing.T) {
		evil := tlsPublisher(t, ca, pub, "spiffe://spike.local/mp/other")
		s := secureSubscriber(t, ca, evil.URL, subscriberA, "sub-a", "scope-a", "token")
		if s.Fetch(context.Background()) == nil {
			t.Fatal("wrong publisher accepted")
		}
	})
	t.Run("same_identity_cert_rotation", func(t *testing.T) {
		check(t, a.Fetch(context.Background()))
		v, e := callSecure(t, address, proxyTLS(proxyA), "backend")
		check(t, e)
		if v != "Bearer "+tokenA {
			t.Fatal("new cert wrong result")
		}
	})
}

func TestSecureBootstrapReplayAndLease(t *testing.T) {
	ca := newCA(t)
	var mu sync.Mutex
	var captured Snapshot
	replay := false
	badScope := false
	badEpoch := false
	token := rand.Text()
	records := []Credential{{"scope-a", "token", 1, token, time.Now().Unix() + 60, false}}
	revision := int64(1)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, e := PeerIdentity(*r.TLS)
		if e != nil || id != subscriberA {
			http.Error(w, "denied", 403)
			return
		}
		var q SnapshotRequest
		if decodeOne(r.Body, &q) != nil {
			http.Error(w, "invalid", 400)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		snap := Snapshot{q.Challenge, q.Subscription, "epoch-1", revision, 200, append([]Credential(nil), records...)}
		if replay {
			snap = captured
		} else {
			if badScope {
				snap.Credentials = append(snap.Credentials, Credential{"foreign", "token", 1, rand.Text(), time.Now().Unix() + 60, false})
			}
			if badEpoch {
				snap.Epoch = "epoch-2"
			}
			captured = snap
		}
		json.NewEncoder(w).Encode(snap)
	})
	server := tlsPublisher(t, ca, h, publisherID)
	a := secureSubscriber(t, ca, server.URL, subscriberA, "sub-a", "scope-a", "token")
	address := grpcSubscriber(t, ca, a)
	transport := credentials.NewTLS(WorkloadClientTLS(ca.leaf(t, proxyA), ca.roots, authID))
	check(t, a.Fetch(context.Background()))
	mu.Lock()
	old := captured
	mu.Unlock()
	t.Run("old_session_replay", func(t *testing.T) {
		mu.Lock()
		replay = true
		mu.Unlock()
		fresh := secureSubscriber(t, ca, server.URL, subscriberA, "sub-a", "scope-a", "token")
		if fresh.Fetch(context.Background()) == nil || fresh.Ready() {
			t.Fatal("old snapshot accepted on restart")
		}
		mu.Lock()
		replay = false
		mu.Unlock()
	})
	t.Run("revocation_then_restart", func(t *testing.T) {
		mu.Lock()
		revision = 2
		records[0].Revision = 2
		records[0].Revoked = true
		mu.Unlock()
		check(t, a.Fetch(context.Background()))
		if _, e := callSecure(t, address, transport, "backend"); e == nil {
			t.Fatal("revocation ignored")
		}
		mu.Lock()
		captured = old
		replay = true
		mu.Unlock()
		fresh := secureSubscriber(t, ca, server.URL, subscriberA, "sub-a", "scope-a", "token")
		if fresh.Fetch(context.Background()) == nil {
			t.Fatal("pre-revocation snapshot restored")
		}
		mu.Lock()
		replay = false
		mu.Unlock()
		check(t, fresh.Fetch(context.Background()))
		if _, e := fresh.auth.Resolve(context.Background(), "backend"); e == nil {
			t.Fatal("fresh bootstrap resurrected revoked credential")
		}
	})
	t.Run("publisher_rollback", func(t *testing.T) {
		mu.Lock()
		revision = 1
		mu.Unlock()
		if a.Fetch(context.Background()) == nil {
			t.Fatal("lower publication revision accepted")
		}
		mu.Lock()
		revision = 2
		mu.Unlock()
	})
	t.Run("publisher_equivocation", func(t *testing.T) {
		mu.Lock()
		records[0].Revoked = false
		mu.Unlock()
		if a.Fetch(context.Background()) == nil {
			t.Fatal("same revision changed content accepted")
		}
		mu.Lock()
		records[0].Revoked = true
		mu.Unlock()
	})
	t.Run("invalid_snapshot_is_atomic", func(t *testing.T) {
		mu.Lock()
		revision = 3
		badScope = true
		mu.Unlock()
		if a.Fetch(context.Background()) == nil {
			t.Fatal("foreign scope accepted")
		}
		var n int
		check(t, a.auth.db.QueryRow("SELECT count(*) FROM credentials").Scan(&n))
		if n != 1 {
			t.Fatal("partial snapshot installed")
		}
		mu.Lock()
		badScope = false
		mu.Unlock()
	})
	t.Run("epoch_change_requires_new_authority", func(t *testing.T) {
		mu.Lock()
		badEpoch = true
		mu.Unlock()
		if a.Fetch(context.Background()) == nil {
			t.Fatal("epoch silently changed")
		}
		mu.Lock()
		badEpoch = false
		mu.Unlock()
	})
	t.Run("lease_expiry", func(t *testing.T) {
		mu.Lock()
		records[0].Revoked = false
		records[0].Revision = 3
		mu.Unlock()
		check(t, a.Fetch(context.Background()))
		v, e := callSecure(t, address, transport, "backend")
		check(t, e)
		if v != "Bearer "+token {
			t.Fatal("unexpected credential")
		}
		time.Sleep(250 * time.Millisecond)
		if a.Ready() {
			t.Fatal("expired lease still ready")
		}
		if _, e = callSecure(t, address, transport, "backend"); e == nil {
			t.Fatal("expired lease allowed resolution")
		}
	})
	t.Run("publisher_outage", func(t *testing.T) {
		server.Close()
		if a.Fetch(context.Background()) == nil {
			t.Fatal("offline fetch succeeded")
		}
		fresh := secureSubscriber(t, ca, server.URL, subscriberA, "sub-a", "scope-a", "token")
		if fresh.Fetch(context.Background()) == nil || fresh.Ready() {
			t.Fatal("ready without authority")
		}
	})
}

func TestSecureSnapshotValidation(t *testing.T) {
	ca := newCA(t)
	s := secureSubscriber(t, ca, "https://localhost:1", subscriberA, "sub-a", "scope-a", "token")
	q := SnapshotRequest{rand.Text() + rand.Text(), "sub-a"}
	base := Snapshot{q.Challenge, "sub-a", "epoch-1", 1, 500, []Credential{{"scope-a", "token", 1, rand.Text(), time.Now().Unix() + 60, false}}}
	check(t, s.install(context.Background(), q, base, time.Now()))
	cases := map[string]func(*Snapshot){
		"wrong_subscription":       func(x *Snapshot) { x.Subscription = "sub-b" },
		"oversized_lease":          func(x *Snapshot) { x.LeaseMillis = 1001 },
		"zero_lease":               func(x *Snapshot) { x.LeaseMillis = 0 },
		"header_control_character": func(x *Snapshot) { x.Credentials[0].Value = "bad\r\nvalue" },
		"duplicate_key":            func(x *Snapshot) { x.Credentials = append(x.Credentials, x.Credentials[0]) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			x := base
			x.Revision = 2
			x.Credentials = append([]Credential(nil), base.Credentials...)
			mutate(&x)
			if s.install(context.Background(), q, x, time.Now()) == nil {
				t.Fatal("invalid snapshot accepted")
			}
			if s.revision != 1 {
				t.Fatal("rejected snapshot advanced cursor")
			}
		})
	}
	t.Run("slow_bootstrap_does_not_extend_lease", func(t *testing.T) {
		x := base
		x.Revision = 2
		if s.install(context.Background(), q, x, time.Now().Add(-time.Second)) == nil {
			t.Fatal("expired fetch granted new lease")
		}
	})
	t.Run("complete_snapshot_removes_omitted_credentials", func(t *testing.T) {
		x := base
		x.Revision = 2
		x.Credentials = nil
		check(t, s.install(context.Background(), q, x, time.Now()))
		if _, e := s.auth.Resolve(context.Background(), "backend"); e == nil {
			t.Fatal("omitted credential retained")
		}
	})
}
