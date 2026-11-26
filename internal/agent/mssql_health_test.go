//go:build !windows

package agent

import (
	"context"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

// ms.health against a real SQL Server (see mssql_test.go): every source is
// read, nothing is missing on SQL Server 2016 SP2 and later, and the numbers
// are the server's own.
func TestMSSQLHealthOnARealServer(t *testing.T) {
	pw, port := msTest(t)
	root := msRoot(t, pw, port)
	msDB(t, root, "argus_t_health")
	// root holds one connection: set up before taking it.
	msExec(t, root, "ALTER DATABASE argus_t_health SET QUERY_STORE = ON")
	ctx := context.Background()
	c, err := root.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.ExecContext(ctx, "USE "+tunnel.MSSQLIdent("argus_t_health")); err != nil {
		t.Fatal(err)
	}
	h, err := msHealth(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Missing) > 0 {
		t.Fatalf("missing: %v", h.Missing)
	}
	if h.CPUs < 1 || h.MemoryMB < 1 || h.UptimeS < 1 || h.Version == "" || h.Edition == "" {
		t.Fatalf("server facts: %+v", h)
	}
	if h.Batches < 1 || h.PageLife < 1 {
		t.Fatalf("counters not read: batches %d, page life %d", h.Batches, h.PageLife)
	}
	if h.TempdbFiles < 1 || len(h.TempdbSizes) < 1 || h.VLFs < 1 {
		t.Fatalf("tempdb %d files %v, %d VLFs", h.TempdbFiles, h.TempdbSizes, h.VLFs)
	}
	if h.QueryStore != "READ_WRITE" {
		t.Fatalf("Query Store is %q", h.QueryStore)
	}
	t.Logf("%+v", h)
}
