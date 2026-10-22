package spike

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	ep "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// These characterize known harness gaps, not security guarantees. Change them to
// rejection assertions when authenticated delivery and peer authorization exist.
func TestSecurityReviewUnauthenticatedDelivery(t *testing.T) {
	a, err := NewAuth(map[string]Binding{"llm": {"spike", "token"}})
	check(t, err)
	defer a.Close()
	server := httptest.NewServer(a)
	defer server.Close()
	c := Credential{"spike", "token", 1, rand.Text(), time.Now().Unix() + 300, false}
	body, err := json.Marshal(c)
	check(t, err)
	req, err := http.NewRequest("PUT", server.URL, bytes.NewReader(body))
	check(t, err)
	resp, err := http.DefaultClient.Do(req)
	check(t, err)
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("gap changed: unauthenticated update status %d", resp.StatusCode)
	}
	value, err := a.Resolve(context.Background(), "llm")
	check(t, err)
	if value != c.Value {
		t.Fatal("update did not install supplied credential")
	}
	t.Log("CONFIRMED GAP: reachable update endpoint accepts unauthenticated credential replacement")
}
func TestSecurityReviewDirectGRPCDisclosure(t *testing.T) {
	a, err := NewAuth(map[string]Binding{"llm": {"spike", "token"}})
	check(t, err)
	defer a.Close()
	c := Credential{"spike", "token", 1, rand.Text(), time.Now().Unix() + 300, false}
	check(t, a.Update(context.Background(), c))
	p := port(t)
	server := startAuth(t, a, p)
	defer server.Stop()
	conn, err := grpc.NewClient(fmt.Sprintf("127.0.0.1:%d", p), grpc.WithTransportCredentials(insecure.NewCredentials()))
	check(t, err)
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs("x-spike-backend", "llm"))
	stream, err := ep.NewExternalProcessorClient(conn).Process(ctx)
	check(t, err)
	check(t, stream.Send(&ep.ProcessingRequest{Request: &ep.ProcessingRequest_RequestHeaders{RequestHeaders: &ep.HttpHeaders{}}}))
	resp, err := stream.Recv()
	check(t, err)
	headers := resp.GetRequestHeaders().GetResponse().GetHeaderMutation().GetSetHeaders()
	if len(headers) != 1 || string(headers[0].GetHeader().GetRawValue()) != "Bearer "+c.Value {
		t.Fatal("gap changed: direct caller did not receive credential")
	}
	check(t, stream.CloseSend())
	t.Log("CONFIRMED GAP: unauthenticated gRPC caller obtains credential by claiming backend ID")
}
func TestSecurityReviewRestartReplay(t *testing.T) {
	bindings := map[string]Binding{"llm": {"spike", "token"}}
	a, err := NewAuth(bindings)
	check(t, err)
	old := Credential{"spike", "token", 1, rand.Text(), time.Now().Unix() + 300, false}
	check(t, a.Update(context.Background(), old))
	revoked := old
	revoked.Revision = 2
	revoked.Revoked = true
	check(t, a.Update(context.Background(), revoked))
	if a.Update(context.Background(), old) == nil {
		t.Fatal("live rollback guard broken")
	}
	check(t, a.Close())
	a, err = NewAuth(bindings)
	check(t, err)
	defer a.Close()
	check(t, a.Update(context.Background(), old))
	_, err = a.Resolve(context.Background(), "llm")
	check(t, err)
	t.Log("CONFIRMED GAP: fresh store accepts old unexpired record after revocation history is lost")
}
