package spike

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"strings"
	"sync"
	"time"

	ep "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

// PeerIdentity requires a normally verified chain and exactly one URI identity.
func PeerIdentity(state tls.ConnectionState) (string, error) {
	if len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 || len(state.PeerCertificates[0].URIs) != 1 {
		return "", errors.New("unverified workload identity")
	}
	if _, err := peerExpiry(state); err != nil {
		return "", err
	}
	return state.PeerCertificates[0].URIs[0].String(), nil
}

// Revalidate time on every operation, including reused TLS connections. Any
// previously verified chain that remains valid is sufficient.
func peerExpiry(state tls.ConnectionState) (time.Time, error) {
	now := time.Now()
	var expiry time.Time
	for _, chain := range state.VerifiedChains {
		var end time.Time
		valid := len(chain) > 0
		for _, cert := range chain {
			if now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) {
				valid = false
				break
			}
			if end.IsZero() || cert.NotAfter.Before(end) {
				end = cert.NotAfter
			}
		}
		if valid && end.After(expiry) {
			expiry = end
		}
	}
	if expiry.IsZero() {
		return time.Time{}, errors.New("workload identity expired or not yet valid")
	}
	return expiry, nil
}

func WorkloadServerTLS(cert tls.Certificate, roots *x509.CertPool) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert}
}

// Normal certificate/hostname verification stays enabled; URI authorization is additional.
func WorkloadClientTLS(cert tls.Certificate, roots *x509.CertPool, expected string) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, RootCAs: roots, ServerName: "localhost", VerifyConnection: func(s tls.ConnectionState) error {
		id, e := PeerIdentity(s)
		if e != nil || id != expected {
			return errors.New("unexpected server identity")
		}
		return nil
	}}
}

type SnapshotRequest struct {
	Challenge    string
	Subscription string
}
type Snapshot struct {
	Challenge    string
	Subscription string
	Epoch        string
	Revision     int64
	LeaseMillis  int64
	Credentials  []Credential
}
type DeliveryGrant struct {
	Subscription string
	Bindings     []Binding
}

// Publisher is the mock MP. All state, including its test credentials, is in memory.
// A production MP must persist authority epoch/revision and guard failover separately.
type Publisher struct {
	mu       sync.RWMutex
	epoch    string
	revision int64
	records  []Credential
	grants   map[string]DeliveryGrant
	lease    time.Duration
}

func NewPublisher(epoch string, lease time.Duration, grants map[string]DeliveryGrant) *Publisher {
	copyGrants := make(map[string]DeliveryGrant, len(grants))
	for id, g := range grants {
		g.Bindings = append([]Binding(nil), g.Bindings...)
		copyGrants[id] = g
	}
	return &Publisher{epoch: epoch, lease: lease, grants: copyGrants}
}
func (p *Publisher) Publish(records []Credential) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.revision++
	p.records = append([]Credential(nil), records...)
}
func (p *Publisher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" || r.URL.Path != "/snapshot" {
		http.Error(w, "not found", 404)
		return
	}
	if r.TLS == nil {
		http.Error(w, "identity required", 403)
		return
	}
	id, e := PeerIdentity(*r.TLS)
	if e != nil {
		http.Error(w, "identity required", 403)
		return
	}
	// Bound slow readers/writers without holding publication state locks.
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(3 * time.Second))
	_ = controller.SetWriteDeadline(time.Now().Add(3 * time.Second))
	p.mu.RLock()
	g, ok := p.grants[id]
	p.mu.RUnlock()
	if !ok {
		http.Error(w, "subscription denied", 403)
		return
	}
	var q SnapshotRequest
	if decodeOne(http.MaxBytesReader(w, r.Body, 4096), &q) != nil || len(q.Challenge) < 32 || len(q.Challenge) > 128 || q.Subscription != g.Subscription {
		http.Error(w, "subscription denied", 403)
		return
	}
	expiry, err := peerExpiry(*r.TLS)
	if err != nil {
		http.Error(w, "identity expired", 403)
		return
	}
	p.mu.RLock()
	records := []Credential{}
	for _, c := range p.records {
		for _, b := range g.Bindings {
			if c.Scope == b.Scope && c.ID == b.ID {
				records = append(records, c)
				break
			}
		}
	}
	lease := min(p.lease, time.Until(expiry))
	snapshot := Snapshot{q.Challenge, q.Subscription, p.epoch, p.revision, lease.Milliseconds(), records}
	p.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(snapshot)
}
func decodeOne(r io.Reader, out any) error {
	d := json.NewDecoder(r)
	d.DisallowUnknownFields()
	if e := d.Decode(out); e != nil {
		return e
	}
	var extra any
	if e := d.Decode(&extra); e != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}

type Subscriber struct {
	ep.UnimplementedExternalProcessorServer
	auth                          *Auth
	mu                            sync.RWMutex
	fetchMu                       sync.Mutex
	client                        *http.Client
	endpoint, subscription, epoch string
	proxyGrants                   map[string]map[string]bool
	allowed                       map[Binding]bool
	revision                      int64
	digest                        [32]byte
	deadline                      time.Time
	maxLease                      time.Duration
}

func NewSubscriber(bindings map[string]Binding, proxyGrants map[string][]string, endpoint, subscription, epoch string, tlsConfig *tls.Config, maxLease time.Duration) (*Subscriber, error) {
	if maxLease <= 0 || epoch == "" || subscription == "" || !strings.HasPrefix(endpoint, "https://") || tlsConfig == nil || tlsConfig.InsecureSkipVerify || tlsConfig.VerifyConnection == nil {
		return nil, errors.New("invalid subscriber trust configuration")
	}
	a, e := NewAuth(maps.Clone(bindings))
	if e != nil {
		return nil, e
	}
	s := &Subscriber{auth: a, endpoint: endpoint, subscription: subscription, epoch: epoch, maxLease: maxLease, allowed: map[Binding]bool{}, proxyGrants: map[string]map[string]bool{}}
	for _, b := range bindings {
		s.allowed[b] = true
	}
	for id, backends := range proxyGrants {
		s.proxyGrants[id] = map[string]bool{}
		for _, b := range backends {
			if _, ok := bindings[b]; !ok {
				a.Close()
				return nil, errors.New("unknown backend grant")
			}
			s.proxyGrants[id][b] = true
		}
	}
	s.client = &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig.Clone()}, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect denied") }}
	a.resolveRequest = s.resolve
	return s, nil
}
func (s *Subscriber) Close() error { s.client.CloseIdleConnections(); return s.auth.Close() }
func (s *Subscriber) resolve(ctx context.Context, backend string) (string, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return "", errors.New("peer required")
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return "", errors.New("TLS required")
	}
	id, e := PeerIdentity(info.State)
	if e != nil {
		return "", e
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.proxyGrants[id][backend] || s.deadline.IsZero() || !time.Now().Before(s.deadline) {
		return "", errors.New("binding or lease denied")
	}
	identityDeadline, e := peerExpiry(info.State)
	if e != nil {
		return "", e
	}
	deadline := s.deadline
	if identityDeadline.Before(deadline) {
		deadline = identityDeadline
	}
	lookupCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	value, e := s.auth.Resolve(lookupCtx, backend)
	if e != nil {
		return "", e
	}
	if lookupCtx.Err() != nil || !time.Now().Before(deadline) {
		return "", errors.New("resolution lifetime expired")
	}
	if _, e = PeerIdentity(info.State); e != nil {
		return "", e
	}
	return value, nil
}
func (s *Subscriber) Ready() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.deadline.IsZero() && time.Now().Before(s.deadline)
}
func (s *Subscriber) Fetch(ctx context.Context) error {
	s.fetchMu.Lock()
	defer s.fetchMu.Unlock()
	started := time.Now()
	q := SnapshotRequest{rand.Text() + rand.Text(), s.subscription}
	raw, _ := json.Marshal(q)
	req, e := http.NewRequestWithContext(ctx, "POST", s.endpoint+"/snapshot", bytes.NewReader(raw))
	if e != nil {
		return e
	}
	req.Header.Set("Content-Type", "application/json")
	resp, e := s.client.Do(req)
	if e != nil {
		return errors.New("publisher unavailable or untrusted")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return errors.New("publication denied")
	}
	limited := io.LimitReader(resp.Body, 1<<20+1)
	payload, e := io.ReadAll(limited)
	if e != nil || len(payload) > 1<<20 {
		return errors.New("snapshot too large")
	}
	var snapshot Snapshot
	if decodeOne(bytes.NewReader(payload), &snapshot) != nil {
		return errors.New("invalid snapshot")
	}
	if snapshot.LeaseMillis <= 0 || snapshot.LeaseMillis > s.maxLease.Milliseconds() {
		return errors.New("snapshot lease outside policy")
	}
	// VerifyConnection runs at handshake, not on each keepalive response.
	if resp.TLS == nil {
		return errors.New("publisher TLS required")
	}
	transport := s.client.Transport.(*http.Transport)
	if e = transport.TLSClientConfig.VerifyConnection(*resp.TLS); e != nil {
		return e
	}
	expiry, e := peerExpiry(*resp.TLS)
	if e != nil {
		return e
	}
	snapshot.LeaseMillis = min(snapshot.LeaseMillis, expiry.Sub(started).Milliseconds())
	return s.install(ctx, q, snapshot, started)
}
func (s *Subscriber) install(ctx context.Context, q SnapshotRequest, snap Snapshot, started time.Time) error {
	if snap.Challenge != q.Challenge || snap.Subscription != s.subscription || snap.Epoch != s.epoch || snap.Revision < 1 || snap.LeaseMillis <= 0 || snap.LeaseMillis > s.maxLease.Milliseconds() {
		return errors.New("snapshot authority mismatch")
	}
	deadline := started.Add(time.Duration(snap.LeaseMillis) * time.Millisecond)
	if !time.Now().Before(deadline) {
		return errors.New("bootstrap lease expired")
	}
	seen := map[Binding]bool{}
	for _, c := range snap.Credentials {
		b := Binding{c.Scope, c.ID}
		if !s.allowed[b] || seen[b] || c.Revision < 1 || c.ExpiresAt <= 0 || len(c.Value) > 8192 || strings.ContainsAny(c.Value, "\r\n\x00") {
			return errors.New("invalid credential binding")
		}
		seen[b] = true
	}
	raw, _ := json.Marshal(snap.Credentials)
	digest := sha256.Sum256(raw)
	s.mu.Lock()
	defer s.mu.Unlock()
	if snap.Revision < s.revision || (snap.Revision == s.revision && digest != s.digest) {
		return errors.New("publication rollback or equivocation")
	}
	tx, e := s.auth.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, "DELETE FROM credentials"); e != nil {
		return e
	}
	for _, c := range snap.Credentials {
		if _, e = tx.ExecContext(ctx, "INSERT INTO credentials VALUES(?,?,?,?,?,?)", c.Scope, c.ID, c.Revision, c.Value, c.ExpiresAt, c.Revoked); e != nil {
			return e
		}
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	s.revision = snap.Revision
	s.digest = digest
	s.deadline = deadline
	return nil
}

func (s *Subscriber) Process(stream ep.ExternalProcessor_ProcessServer) error {
	return s.auth.Process(stream)
}
