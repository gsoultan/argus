//go:build !windows

package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

func TestMySQLListOK(t *testing.T) {
	for v, want := range map[string]bool{
		"8.0.12": false, "8.0.13": true, "8.4.2": true, "5.7.44-log": false, "9.0.1": true,
	} {
		if mysqlListOK(v, false) != want {
			t.Errorf("MySQL %s: %v", v, !want)
		}
	}
	for v, want := range map[string]bool{"10.6.18-MariaDB-0ubuntu0.22.04.1": false, "10.11.14-MariaDB": true, "11.4.2-MariaDB": true} {
		if mysqlListOK(v, true) != want {
			t.Errorf("MariaDB %s: %v", v, !want)
		}
	}
	op := tunnel.PGAdminOp{Reach: tunnel.ReachSelected, Addresses: []string{"10.0.0.183"}, Sources: []string{"10.0.0.61"}, Restart: true}
	want := "SET bind_address = '127.0.0.1,10.0.0.183' (from the next start)\nFIREWALL ALLOW 10.0.0.61\nFIREWALL DENY OTHERS\nRESTART MYSQL"
	if got := tunnel.NetworkText(tunnel.EngineMySQL, op); got != want {
		t.Fatalf("text\n%s\nwant\n%s", got, want)
	}
}

// Against a real MariaDB or MySQL (see mysql_test.go): selected machines
// keep bind_address for the next start and narrow the firewall first, a
// user with no password stops anywhere, and the change is recorded.
func TestMySQLNetworkOnARealServer(t *testing.T) {
	sock, port := myTest(t)
	root := myRoot(t, sock)
	oldAddrs, oldSvc := hostAddresses, serviceOf
	hostAddresses = func() []RedisAddress {
		return []RedisAddress{{IP: "10.0.0.183", Iface: "eth0", Prefix: "10.0.0.0/24", Private: true}}
	}
	serviceOf = func(int, ...string) string { return "mariadb.service" }
	t.Cleanup(func() { hostAddresses, serviceOf = oldAddrs, oldSvc })

	conf := t.TempDir()
	if err := os.MkdirAll(filepath.Join(conf, "etc/mysql/mariadb.conf.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	var log []string
	fw := &fakeRedisFirewall{active: true}
	dir := t.TempDir()
	svc := MySQL(MySQLConfig{StoreConfig: StoreConfig{Verifier: &tunnel.GrantVerifier{Host: "db-01"}, RecordDir: dir,
		Firewall: func(ctx context.Context, r tunnel.FirewallRequest) tunnel.FirewallResult {
			if !r.Check {
				log = append(log, "firewall "+r.Reach)
			}
			return fw.do(ctx, r)
		},
		Restart: func(_ context.Context, unit string) error { log = append(log, "restart "+unit); return nil },
	}, Sockets: []string{sock}, ConfRoot: conf})
	m := myService{svc: svc, dir: dir, sock: sock, port: port}
	myDB(t, root, "argus_t_net")
	h, err := m.open(t, "ana_sari", "argus_t_net", tunnel.LevelDBA, nil)
	if err != nil {
		t.Fatal(err)
	}

	a := h.admin(1, tunnel.PGAdminOp{Kind: tunnel.AdminNetwork})
	if a.Type != "admin" {
		t.Fatalf("read: %+v", a)
	}
	var n MySQLNetwork
	_ = json.Unmarshal(a.Admin, &n)
	if n.Port != port || n.Reach == "" || n.Version == "" {
		t.Fatalf("view %+v", n)
	}

	a = h.admin(2, tunnel.PGAdminOp{Kind: tunnel.AdminNetworkSet, Reach: tunnel.ReachSelected,
		Addresses: []string{"10.0.0.183"}, Sources: []string{"10.0.0.61"}, Restart: true})
	if !n.ListOK {
		if a.Type != "error" || !strings.Contains(a.Error, "one address or on every one") {
			t.Fatalf("an old server took a list: %+v", a)
		}
		return
	}
	if a.Type != "admin" {
		t.Fatalf("set: %+v", a)
	}
	var res MySQLNetworkResult
	_ = json.Unmarshal(a.Admin, &res)
	if !slices.Equal(log, []string{"firewall selected", "restart mariadb.service"}) || !res.Restarted {
		t.Fatalf("ran %v, result %+v", log, res)
	}
	if n.MariaDB {
		b, err := os.ReadFile(filepath.Join(conf, "etc/mysql/mariadb.conf.d", mysqlArgusCnf))
		if err != nil || !strings.Contains(string(b), "bind-address = 127.0.0.1,10.0.0.183") {
			t.Fatalf("option file %q %v", b, err)
		}
	}
	if res.Network.Next != "127.0.0.1,10.0.0.183" || !res.Network.PendingRestart {
		t.Fatalf("next start %+v", res.Network)
	}

	// A user who may sign in from anywhere with no password stops anywhere.
	myExec(t, root, "DROP USER IF EXISTS 'argus_t_open'@'%'", "CREATE USER 'argus_t_open'@'%'")
	t.Cleanup(func() { _, _ = root.Exec("DROP USER IF EXISTS 'argus_t_open'@'%'") })
	op := tunnel.PGAdminOp{Kind: tunnel.AdminNetworkSet, Reach: tunnel.ReachAny, Confirm: "DB-01"}
	h.send(tunnel.PGRequest{ID: 3, Op: "admin", Admin: &op, Approval: "ap-1"})
	if a := h.answer(); a.Type != "error" || !strings.Contains(a.Error, "'argus_t_open'@'%' can sign in from another machine with no password") {
		t.Fatalf("anywhere with an open user: %+v", a)
	}
	if !mysqlRecorded(t, dir, "SET bind_address = '127.0.0.1,10.0.0.183'") {
		t.Fatal("the change is not in the recording")
	}
}

func mysqlRecorded(t *testing.T, dir, text string) bool {
	t.Helper()
	found := false
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), text) {
				found = true
			}
		}
		return nil
	})
	return found
}
