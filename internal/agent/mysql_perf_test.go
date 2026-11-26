//go:build !windows

package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

// Against a real MariaDB or MySQL (see mysql_test.go): mysql.perf answers
// this schema's statements, tables and indexes, read as the administrator,
// and is refused below DBA against the signed grant.
func TestMySQLPerfOnARealServer(t *testing.T) {
	sock, port := myTest(t)
	root := myRoot(t, sock)
	dir := t.TempDir()
	svc := MySQL(MySQLConfig{StoreConfig: StoreConfig{Verifier: &tunnel.GrantVerifier{Host: "db-01"}, RecordDir: dir}, Sockets: []string{sock}})
	m := myService{svc: svc, dir: dir, sock: sock, port: port}
	myDB(t, root, "argus_t_perf")
	myExec(t, root,
		"CREATE TABLE argus_t_perf.orders (id INT PRIMARY KEY, customer INT, status VARCHAR(10), KEY st (status), KEY st2 (status))",
		"CREATE TABLE argus_t_perf.nokey (a INT, b INT)",
		"INSERT INTO argus_t_perf.orders VALUES (1, 7, 'open'), (2, 8, 'done')",
		"SELECT COUNT(*) FROM argus_t_perf.orders WHERE customer = 7")

	h, err := m.open(t, "ana_sari", "argus_t_perf", tunnel.LevelDBA, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := h.admin(1, tunnel.PGAdminOp{Kind: tunnel.AdminMySQLPerf})
	if a.Type != "admin" {
		t.Fatalf("perf: %+v", a)
	}
	var p tunnel.MySQLPerf
	if err := json.Unmarshal(a.Admin, &p); err != nil {
		t.Fatal(err)
	}
	if p.Version == "" || p.Status["uptime"] == "" || p.Variables["max_connections"] == "" {
		t.Fatalf("server facts: version %q status %v variables %v missing %v", p.Version, len(p.Status), len(p.Variables), p.Missing)
	}
	names := func(tb *tunnel.PoolerTable, col int) string {
		if tb == nil {
			return ""
		}
		var out []string
		for _, r := range tb.Rows {
			out = append(out, r[col])
		}
		return strings.Join(out, ",")
	}
	if got := names(p.Tables, 0); !strings.Contains(got, "orders") || !strings.Contains(got, "nokey") {
		t.Fatalf("tables %q", got)
	}
	if got := names(p.Indexes, 1); !strings.Contains(got, "st2") {
		t.Fatalf("indexes %q", got)
	}
	if p.Variables["performance_schema"] == "ON" || p.Variables["performance_schema"] == "1" {
		if !strings.Contains(strings.ToLower(names(p.Digests, 0)), "customer") {
			t.Logf("digests %q (missing %v)", names(p.Digests, 0), p.Missing)
		}
	}

	r, err := m.open(t, "ana_sari", "argus_t_perf", tunnel.LevelRead, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a := r.admin(1, tunnel.PGAdminOp{Kind: tunnel.AdminMySQLPerf}); a.Type != "error" || a.Code != "42501" {
		t.Fatalf("read level got %+v", a)
	}
}
