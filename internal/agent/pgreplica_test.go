package agent

import (
	"context"
	"encoding/json"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gsoultan/argus/internal/tunnel"
)

func TestHBAEditKeepsEverythingElse(t *testing.T) {
	in := "# comment\n\nlocal all postgres peer\nhost all all 0.0.0.0/0 reject\n"
	got := hbaWithReplica(in, "r1", "host replication argus_repl_r1 10.0.0.5/32 scram-sha-256")
	want := "# comment\n\nhost replication argus_repl_r1 10.0.0.5/32 scram-sha-256 # argus-replica:r1\nlocal all postgres peer\nhost all all 0.0.0.0/0 reject\n"
	if got != want {
		t.Fatalf("got\n%s", got)
	}
	// Again with a new address replaces the line rather than adding one.
	again := hbaWithReplica(got, "r1", "host replication argus_repl_r1 10.0.0.6/32 scram-sha-256")
	if strings.Count(again, "argus-replica:r1") != 1 || !strings.Contains(again, "10.0.0.6/32") {
		t.Fatalf("again\n%s", again)
	}
	// r1 is not r10.
	two := hbaWithReplica(again, "r10", "host replication argus_repl_r10 10.0.0.7/32 scram-sha-256")
	if back := hbaWithout(hbaWithout(two, "r1"), "r10"); back != in {
		t.Fatalf("removed\n%q", back)
	}
	if hbaWithout(two, "r1") == hbaWithout(two, "r10") {
		t.Fatal("removing r1 took r10 too")
	}
}

func TestHBAAddressMatching(t *testing.T) {
	a := netip.MustParseAddr("10.0.2.15")
	for _, c := range []struct {
		addr, mask string
		want       bool
	}{{"all", "", true}, {"10.0.0.0/16", "", true}, {"10.1.0.0/16", "", false}, {"10.0.2.15", "255.255.255.255", true},
		{"10.0.0.0", "255.255.0.0", true}, {"10.0.2.15/32", "", true}, {"samenet", "", false}, {"::1/128", "", false}} {
		if got := addrMatches(c.addr, c.mask, a); got != c.want {
			t.Errorf("%s %s: %v", c.addr, c.mask, got)
		}
	}
}

// On a real server: check, prepare, allow and drop, and an existing role or
// slot that is not Argus's left alone. allow edits the server's pg_hba.conf,
// so that part runs only where the test can read and write it.
func TestPGReplicaOnThePrimary(t *testing.T) {
	dsn := os.Getenv("ARGUS_TEST_PG_SUPER_DSN")
	if dsn == "" {
		t.Skip("ARGUS_TEST_PG_SUPER_DSN is not set")
	}
	ctx := context.Background()
	conn, err := pgconn.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	// Closed after the cleanups below, which use it.
	t.Cleanup(func() { conn.Close(context.Background()) })
	r := tunnel.PGReplica{Name: "t_r1", Address: "10.99.2.15", By: "ana@corp.example"}
	run := func(op string, r tunnel.PGReplica, into any) PGAdminResult {
		res := RunPGAdmin(ctx, conn, PGAdminRequest{Op: op, Database: "postgres", Replica: &r})
		if res.OK && into != nil {
			if err := json.Unmarshal(res.Data, into); err != nil {
				t.Fatal(err)
			}
		}
		return res
	}
	cleanup := func() {
		_ = conn.Exec(ctx, `SELECT pg_drop_replication_slot(slot_name) FROM pg_replication_slots WHERE slot_name LIKE 'argus_replica_t\_%';
			DROP ROLE IF EXISTS argus_repl_t_r1; DROP ROLE IF EXISTS argus_repl_t_r2`).Close()
	}
	cleanup()
	t.Cleanup(func() { run("replica-drop", r, nil); cleanup() })

	if res := run("replica-check", tunnel.PGReplica{Name: "Bad-Name"}, nil); res.OK {
		t.Fatal("a bad name was accepted")
	}
	var c tunnel.PGReplicaCheck
	if res := run("replica-check", r, &c); !res.OK {
		t.Fatal(res.Error)
	}
	if c.VersionNum < 100000 || c.WalLevel == "" || c.Role != "argus_repl_t_r1" || c.Slot != "argus_replica_t_r1" || c.RoleExists ||
		!strings.Contains(c.HBALine, "replication argus_repl_t_r1 10.99.2.15/32 scram-sha-256") || c.HBAOK {
		t.Fatalf("check: %+v", c)
	}
	if !strings.Contains(strings.Join(c.Missing, "\n"), "pg_hba.conf does not let 10.99.2.15") {
		t.Fatalf("missing: %v", c.Missing)
	}

	// Prepare: the role may only replicate, and the slot holds no WAL yet.
	if res := run("replica-prepare", r, nil); res.OK {
		t.Fatal("prepared without a verifier")
	}
	v, _ := scramVerifier("repl-secret")
	r.Verifier = v
	var p PGReplicaPrepared
	if res := run("replica-prepare", r, &p); !res.OK || !p.RoleCreated || !p.SlotCreated {
		t.Fatalf("prepare: %+v %+v", res, p)
	}
	if res := run("replica-prepare", r, &p); !res.OK || p.RoleCreated || p.SlotCreated {
		t.Fatalf("prepare again: %+v %+v", res, p)
	}
	got, err := row(ctx, conn, `SELECT rolreplication, rolcanlogin, rolsuper OR rolcreatedb OR rolcreaterole,
		(SELECT restart_lsn IS NULL FROM pg_replication_slots WHERE slot_name = 'argus_replica_t_r1')
		FROM pg_roles WHERE rolname = 'argus_repl_t_r1'`)
	if err != nil || string(got[0]) != "t" || string(got[1]) != "t" || string(got[2]) != "f" || string(got[3]) != "t" {
		t.Fatalf("role and slot: %q %v", got, err)
	}
	// Nothing connected yet: the slot is there, idle, and the replica is behind.
	var st tunnel.PGReplicaStatus
	if res := run("replica-status", r, &st); !res.OK || !st.SlotExists || st.SlotActive || st.Streaming || st.LagBytes != -1 || st.Lag() != -1 {
		t.Fatalf("status: %+v %+v", res, st)
	}

	// Someone else's role of that name is left alone.
	if err := conn.Exec(ctx, `CREATE ROLE argus_repl_t_r2 LOGIN`).Close(); err != nil {
		t.Fatal(err)
	}
	other := tunnel.PGReplica{Name: "t_r2", Verifier: v}
	if res := run("replica-prepare", other, nil); res.OK || !strings.Contains(res.Error, "Argus did not make it") {
		t.Fatalf("theirs: %+v", res)
	}
	var d PGReplicaDropped
	if res := run("replica-drop", other, &d); !res.OK || d.RoleDropped {
		t.Fatalf("dropped theirs: %+v %+v", res, d)
	}

	// Allow: one line, at the top, and the check now finds it.
	file, _ := row(ctx, conn, `SELECT current_setting('hba_file')`)
	before, rerr := os.ReadFile(string(file[0]))
	if rerr == nil && os.WriteFile(string(file[0]), before, 0o640) == nil {
		var a PGReplicaAllowed
		if res := run("replica-allow", r, &a); !res.OK || !a.Added {
			t.Fatalf("allow: %+v %+v", res, a)
		}
		if res := run("replica-check", r, &c); !res.OK || !c.HBAOK || !strings.Contains(c.HBA, "argus_repl_t_r1") {
			t.Fatalf("check after allow: %+v", c)
		}
		if res := run("replica-allow", r, &a); !res.OK || a.Added {
			t.Fatalf("allowed twice: %+v %+v", res, a)
		}
	} else {
		t.Log("pg_hba.conf is not writable here; replica-allow not run")
	}

	if res := run("replica-drop", r, &d); !res.OK || !d.RoleDropped || !d.SlotDropped || (rerr == nil && !d.HBARemoved) {
		t.Fatalf("drop: %+v %+v", res, d)
	}
	if rerr == nil {
		after, _ := os.ReadFile(string(file[0]))
		if string(after) != string(before) {
			t.Fatalf("pg_hba.conf not put back:\n%s", after)
		}
	}
}
