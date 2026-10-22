package spike

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	ep "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
)

func port(t *testing.T) int {
	t.Helper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
func check(t *testing.T, e error) {
	t.Helper()
	if e != nil {
		t.Fatal(e)
	}
}
func startAuth(t *testing.T, a *Auth, p int) *grpc.Server {
	t.Helper()
	l, e := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
	check(t, e)
	s := grpc.NewServer()
	ep.RegisterExternalProcessorServer(s, a)
	go s.Serve(l)
	return s
}
func request(t *testing.T, url, body, session string) (int, []byte, string) {
	t.Helper()
	r, e := http.NewRequest("POST", url, strings.NewReader(body))
	check(t, e)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("Authorization", "Bearer caller-spoof")
	r.Header.Set("x-spike-backend", "mcp")
	r.Header.Set("x-ai-eg-mcp-route", "spike")
	if session != "" {
		r.Header.Set("Mcp-Session-Id", session)
	}
	c := &http.Client{Timeout: 8 * time.Second}
	resp, e := c.Do(r)
	check(t, e)
	defer resp.Body.Close()
	b, e := io.ReadAll(resp.Body)
	check(t, e)
	return resp.StatusCode, b, resp.Header.Get("Mcp-Session-Id")
}
func waitPort(t *testing.T, p int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		c, e := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", p), 100*time.Millisecond)
		if e == nil {
			c.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("port %d did not start", p)
}
func TestLocalUpstream(t *testing.T) {
	envoy, e := exec.LookPath("envoy")
	check(t, e)
	version, e := exec.Command(envoy, "--version").CombinedOutput()
	check(t, e)
	t.Log(strings.TrimSpace(string(version)))
	bindings := map[string]Binding{"llm": {"spike", "llm-token"}, "mcp": {"spike", "mcp-token"}, "wrong": {"other", "llm-token"}}
	a, e := NewAuth(bindings)
	check(t, e)
	defer func() { a.Close() }()
	ap := port(t)
	g := startAuth(t, a, ap)
	defer func() { g.Stop() }()
	update := httptest.NewServer(a)
	defer update.Close()
	put := func(c Credential, want int) {
		t.Helper()
		b, e := json.Marshal(c)
		check(t, e)
		r, e := http.NewRequest("PUT", update.URL, bytes.NewReader(b))
		check(t, e)
		resp, e := http.DefaultClient.Do(r)
		check(t, e)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("update status %d want %d", resp.StatusCode, want)
		}
	}
	token1, token2, tokenMCP := rand.Text(), rand.Text(), rand.Text()
	now := time.Now().Unix()
	c := Credential{"spike", "llm-token", 1, token1, now + 300, false}
	m := Credential{"spike", "mcp-token", 1, tokenMCP, now + 300, false}
	var mu sync.Mutex
	count := 0
	seen := []string{}
	bodies := []string{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		count++
		bodies = append(bodies, string(raw))
		auth := r.Header.Values("Authorization")
		label := "invalid"
		if len(auth) == 1 {
			switch auth[0] {
			case "Bearer " + token1:
				label = "llm-v1"
			case "Bearer " + token2:
				label = "llm-v2"
			case "Bearer " + tokenMCP:
				label = "mcp-v1"
			}
		}
		seen = append(seen, label)
		mu.Unlock()
		if label == "invalid" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		json.Unmarshal(raw, &msg)
		if msg.Method == "" {
			w.Write([]byte(`{"ok":true}`))
			return
		}
		if len(msg.ID) == 0 {
			w.WriteHeader(202)
			return
		}
		var result any
		switch msg.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "fake-session")
			result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "fake", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "echo", "description": "echo", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "spike-ok"}}}
		default:
			result = map[string]any{}
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
	}))
	defer backend.Close()
	_, bpstr, _ := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	bp, _ := strconv.Atoi(bpstr)
	ingress, control, mcpEgress, wrong := port(t), port(t), port(t), port(t)
	cfg := envoyConfig(ap, bp, map[int]string{ingress: "llm", control: "control", mcpEgress: "mcp", wrong: "wrong"})
	dir := t.TempDir()
	config := filepath.Join(dir, "envoy.yaml")
	check(t, os.WriteFile(config, []byte(cfg), 0600))
	logfile, e := os.Create(filepath.Join(dir, "envoy.log"))
	check(t, e)
	defer logfile.Close()
	cmd := exec.Command(envoy, "-c", config, "--concurrency", "1", "--log-level", "error", "--disable-hot-restart")
	cmd.Stdout = logfile
	cmd.Stderr = logfile
	check(t, cmd.Start())
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
		if t.Failed() {
			b, _ := os.ReadFile(logfile.Name())
			t.Log(string(b))
		}
	}()
	waitPort(t, ingress)
	url := func(p int) string { return fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", p) }
	body := `{"model":"fake","messages":[{"role":"user","content":"hello"}]}`
	counts := func() int { mu.Lock(); defer mu.Unlock(); return count }
	deny := func(p int) {
		t.Helper()
		before := counts()
		status, _, _ := request(t, url(p), body, "")
		if status < 400 {
			t.Fatalf("denial status %d", status)
		}
		if counts() != before {
			t.Fatal("denied request reached backend")
		}
		t.Logf("denied status=%d backend_delta=0", status)
	}
	t.Run("control", func(t *testing.T) {
		before := a.Calls.Load()
		status, _, _ := request(t, url(control), body, "")
		if status != 401 || a.Calls.Load() != before {
			t.Fatal("control did not bypass auth")
		}
	})
	t.Run("missing", func(t *testing.T) { deny(ingress) })
	put(c, 204)
	put(m, 204)
	t.Run("inject_and_spoof", func(t *testing.T) {
		s, _, _ := request(t, url(ingress), body, "")
		if s != 200 {
			t.Fatalf("status %d", s)
		}
		mu.Lock()
		defer mu.Unlock()
		if seen[len(seen)-1] != "llm-v1" || bodies[len(bodies)-1] != body {
			t.Fatal("wrong credential or changed body")
		}
	})
	t.Run("scope", func(t *testing.T) { deny(wrong) })
	t.Run("rotation", func(t *testing.T) {
		c.Revision = 2
		c.Value = token2
		put(c, 204)
		for range 5 {
			s, _, _ := request(t, url(ingress), body, "")
			if s != 200 {
				t.Fatal(s)
			}
			mu.Lock()
			last := seen[len(seen)-1]
			mu.Unlock()
			if last != "llm-v2" {
				t.Fatal("stale version")
			}
		}
	})
	t.Run("revocation_and_old_update", func(t *testing.T) {
		c.Revision = 3
		c.Revoked = true
		put(c, 204)
		old := c
		old.Revision = 2
		old.Revoked = false
		put(old, 409)
		deny(ingress)
	})
	t.Run("expiry", func(t *testing.T) {
		c.Revision = 4
		c.Revoked = false
		c.ExpiresAt = now - 1
		put(c, 204)
		deny(ingress)
	})
	t.Run("real_mcp", func(t *testing.T) {
		bin := os.Getenv("MCP_PROXY_BIN")
		if bin == "" {
			t.Fatal("MCP_PROXY_BIN required: run ./run.sh")
		}
		mp := port(t)
		p := exec.Command(bin, fmt.Sprint(mp), fmt.Sprint(mcpEgress))
		p.Stdout = logfile
		p.Stderr = logfile
		check(t, p.Start())
		defer func() { p.Process.Kill(); p.Wait() }()
		waitPort(t, mp)
		u := fmt.Sprintf("http://127.0.0.1:%d/mcp", mp)
		s, b, session := request(t, u, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"spike","version":"1"}}}`, "")
		if s != 200 || !bytes.Contains(b, []byte("serverInfo")) {
			t.Fatalf("init %d %s", s, b)
		}
		request(t, u, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, session)
		s, b, _ = request(t, u, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`, session)
		if s != 200 || !bytes.Contains(b, []byte("echo")) {
			t.Fatalf("list %d %s", s, b)
		}
		s, b, _ = request(t, u, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"fake__echo","arguments":{}}}`, session)
		if s != 200 || !bytes.Contains(b, []byte("spike-ok")) {
			t.Fatalf("call %d %s", s, b)
		}
		mu.Lock()
		last := seen[len(seen)-1]
		mu.Unlock()
		if last != "mcp-v1" {
			t.Fatal("MCP wrong credential")
		}
		m.Revision = 2
		m.Revoked = true
		put(m, 204)
		before := counts()
		_, b, _ = request(t, u, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"fake__echo","arguments":{}}}`, session)
		if counts() != before || bytes.Contains(b, []byte("spike-ok")) {
			t.Fatal("revoked MCP reached backend")
		}
	})
	t.Run("outage_restart", func(t *testing.T) {
		g.Stop()
		deny(ingress)
		a.Close()
		a, e = NewAuth(bindings)
		check(t, e)
		g = startAuth(t, a, ap)
		time.Sleep(time.Second)
		deny(ingress)
		c.Revision = 1
		c.Revoked = false
		c.ExpiresAt = now + 300
		check(t, a.Update(context.Background(), c))
		s, _, _ := request(t, url(ingress), body, "")
		if s != 200 {
			t.Fatal(s)
		}
	})
}
func envoyConfig(ap, bp int, listeners map[int]string) string {
	s := "static_resources:\n  listeners:\n"
	for p, name := range listeners {
		s += fmt.Sprintf(`  - name: listener_%s
    address: {socket_address: {address: 127.0.0.1, port_value: %d}}
    filter_chains:
    - filters:
      - name: envoy.filters.network.http_connection_manager
        typed_config:
          '@type': type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
          stat_prefix: ingress_%s
          route_config:
            name: route_%s
            virtual_hosts:
            - name: all
              domains: ['*']
              routes:
              - match: {prefix: /}
                route: {cluster: %s, timeout: 5s}
          http_filters:
          - name: envoy.filters.http.router
            typed_config: {'@type': type.googleapis.com/envoy.extensions.filters.http.router.v3.Router}
`, name, p, name, name, name)
	}
	s += "  clusters:\n"
	for _, name := range []string{"llm", "mcp", "wrong", "control", "auth"} {
		p := bp
		if name == "auth" {
			p = ap
		}
		s += fmt.Sprintf(`  - name: %s
    connect_timeout: 1s
    type: STATIC
    load_assignment:
      cluster_name: %s
      endpoints:
      - lb_endpoints:
        - endpoint:
            address: {socket_address: {address: 127.0.0.1, port_value: %d}}
`, name, name, p)
		if name == "auth" {
			s += "    http2_protocol_options: {}\n"
			continue
		}
		if name == "control" {
			continue
		}
		s += fmt.Sprintf(`    typed_extension_protocol_options:
      envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
        '@type': type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
        explicit_http_config: {http_protocol_options: {}}
        http_filters:
        - name: envoy.filters.http.ext_proc
          typed_config:
            '@type': type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExternalProcessor
            grpc_service:
              envoy_grpc: {cluster_name: auth}
              initial_metadata: [{key: x-spike-backend, value: %s}]
            failure_mode_allow: false
            message_timeout: 1s
            processing_mode:
              request_header_mode: SEND
              response_header_mode: SKIP
              request_body_mode: NONE
              response_body_mode: NONE
              request_trailer_mode: SKIP
              response_trailer_mode: SKIP
        - name: envoy.filters.http.upstream_codec
          typed_config: {'@type': type.googleapis.com/envoy.extensions.filters.http.upstream_codec.v3.UpstreamCodec}
`, name)
	}
	return s
}
