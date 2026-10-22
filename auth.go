package spike

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	ep "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typ "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	_ "github.com/mattn/go-sqlite3"
	"google.golang.org/grpc/metadata"
)

type Credential struct {
	Scope     string
	ID        string
	Revision  int64
	Value     string
	ExpiresAt int64
	Revoked   bool
}
type Binding struct{ Scope, ID string }
type Auth struct {
	ep.UnimplementedExternalProcessorServer
	db             *sql.DB
	bindings       map[string]Binding
	Calls          atomic.Int64
	resolveRequest func(context.Context, string) (string, error)
}

func NewAuth(bindings map[string]Binding) (*Auth, error) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	_, err = db.Exec(`PRAGMA temp_store=MEMORY; CREATE TABLE credentials(scope TEXT,id TEXT,revision INTEGER,value TEXT,expires INTEGER,revoked INTEGER,PRIMARY KEY(scope,id));`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Auth{db: db, bindings: bindings}, nil
}
func (a *Auth) Close() error { return a.db.Close() }
func (a *Auth) Update(ctx context.Context, c Credential) error {
	if c.Scope == "" || c.ID == "" || c.Revision < 1 || c.ExpiresAt <= 0 {
		return errors.New("invalid update")
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var old Credential
	err = tx.QueryRowContext(ctx, "SELECT revision,value,expires,revoked FROM credentials WHERE scope=? AND id=?", c.Scope, c.ID).Scan(&old.Revision, &old.Value, &old.ExpiresAt, &old.Revoked)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && c.Revision <= old.Revision {
		if c.Revision == old.Revision && c.Value == old.Value && c.ExpiresAt == old.ExpiresAt && c.Revoked == old.Revoked {
			return tx.Commit()
		}
		return errors.New("revision conflict")
	}
	_, err = tx.ExecContext(ctx, "INSERT OR REPLACE INTO credentials VALUES(?,?,?,?,?,?)", c.Scope, c.ID, c.Revision, c.Value, c.ExpiresAt, c.Revoked)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (a *Auth) Resolve(ctx context.Context, backend string) (string, error) {
	b, ok := a.bindings[backend]
	if !ok {
		return "", errors.New("binding denied")
	}
	var c Credential
	err := a.db.QueryRowContext(ctx, "SELECT value,expires,revoked FROM credentials WHERE scope=? AND id=?", b.Scope, b.ID).Scan(&c.Value, &c.ExpiresAt, &c.Revoked)
	if err != nil || c.Revoked || c.ExpiresAt <= time.Now().Unix() || c.Value == "" {
		return "", errors.New("credential unavailable")
	}
	return c.Value, nil
}
func (a *Auth) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "PUT" {
		w.WriteHeader(405)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	var c Credential
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil {
		http.Error(w, "invalid update", 400)
		return
	}
	if err := a.Update(r.Context(), c); err != nil {
		http.Error(w, "update rejected", 409)
		return
	}
	w.WriteHeader(204)
}
func (a *Auth) Process(s ep.ExternalProcessor_ProcessServer) error {
	md, _ := metadata.FromIncomingContext(s.Context())
	ids := md.Get("x-spike-backend")
	for {
		r, err := s.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if r.GetRequestHeaders() == nil {
			return errors.New("unexpected processing phase")
		}
		a.Calls.Add(1)
		backend := ""
		if len(ids) == 1 {
			backend = ids[0]
		}
		resolve := a.Resolve
		if a.resolveRequest != nil {
			resolve = a.resolveRequest
		}
		value, err := resolve(s.Context(), backend)
		if err != nil {
			return s.Send(&ep.ProcessingResponse{Response: &ep.ProcessingResponse_ImmediateResponse{ImmediateResponse: &ep.ImmediateResponse{Status: &typ.HttpStatus{Code: typ.StatusCode_ServiceUnavailable}, Details: "credential_unavailable"}}})
		}
		resp := &ep.ProcessingResponse{Response: &ep.ProcessingResponse_RequestHeaders{RequestHeaders: &ep.HeadersResponse{Response: &ep.CommonResponse{HeaderMutation: &ep.HeaderMutation{SetHeaders: []*core.HeaderValueOption{{Header: &core.HeaderValue{Key: "authorization", RawValue: []byte("Bearer " + value)}, AppendAction: core.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD}}}}}}}
		if err = s.Send(resp); err != nil {
			return err
		}
	}
}
