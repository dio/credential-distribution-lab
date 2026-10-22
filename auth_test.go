package spike

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestStoreConcurrencyAndMemory(t *testing.T) {
	a, e := NewAuth(map[string]Binding{"backend": {"scope", "token"}})
	check(t, e)
	defer a.Close()
	ctx := context.Background()
	expiry := time.Now().Unix() + 300
	check(t, a.Update(ctx, Credential{"scope", "token", 1, "v1", expiry, false}))
	var seq int
	var name, path string
	check(t, a.db.QueryRow("PRAGMA database_list").Scan(&seq, &name, &path))
	if path != "" {
		t.Fatal("database has filesystem path")
	}
	var temp int
	check(t, a.db.QueryRow("PRAGMA temp_store").Scan(&temp))
	if temp != 2 {
		t.Fatal("temp store not memory")
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 100 {
				v, e := a.Resolve(ctx, "backend")
				if e != nil || len(v) == 0 {
					t.Error("lookup failed during update")
				}
			}
		})
	}
	for i := int64(2); i <= 50; i++ {
		check(t, a.Update(ctx, Credential{"scope", "token", i, fmt.Sprintf("v%d", i), expiry, false}))
	}
	wg.Wait()
	c := Credential{"scope", "token", 51, "", expiry, true}
	check(t, a.Update(ctx, c))
	check(t, a.Update(ctx, c))
	c.Value = "different"
	if a.Update(ctx, c) == nil {
		t.Fatal("conflicting replay accepted")
	}
	if _, e = a.Resolve(ctx, "backend"); e == nil {
		t.Fatal("revocation ignored")
	}
	if _, e = a.Resolve(ctx, "unknown"); e == nil {
		t.Fatal("unknown binding accepted")
	}
}
