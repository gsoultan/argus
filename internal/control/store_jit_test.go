package control

import (
	"context"
	"os"
	"strings"
	"testing"
)

// The control plane's connections run with JIT off, every organization's
// pool included, unless the database URL asks for it.
func TestConnectionsRunWithoutJIT(t *testing.T) {
	dsn := os.Getenv("ARGUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ARGUS_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	show := func(s *Store) string {
		t.Helper()
		var v string
		if err := s.pool.QueryRow(ctx, `SHOW jit`).Scan(&v); err != nil {
			t.Fatalf("SHOW jit: %v", err)
		}
		return v
	}

	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v := show(s); v != "off" {
		t.Fatalf("jit = %q on the control plane's own connections, want off", v)
	}
	if s.pool.Config().AfterConnect == nil {
		t.Fatal("a pool copied from this one, as each organization's is, would not turn JIT off")
	}

	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	asked, err := Open(ctx, dsn+sep+"jit=on")
	if err != nil {
		t.Fatal(err)
	}
	defer asked.Close()
	if v := show(asked); v != "on" {
		t.Fatalf("jit = %q with jit=on in the database URL, want on", v)
	}
}
