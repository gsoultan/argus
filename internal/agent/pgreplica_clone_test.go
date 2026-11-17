package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gsoultan/argus/internal/tunnel"
)

func TestUnitFromCgroup(t *testing.T) {
	for cg, want := range map[string]string{
		"0::/system.slice/system-postgresql.slice/postgresql@16-main.service\n":               "postgresql@16-main.service",
		"12:pids:/system.slice/postgresql-16.service\n0::/system.slice/postgresql-16.service": "postgresql-16.service",
	} {
		if got, err := unitFromCgroup(cg); err != nil || got != want {
			t.Errorf("%q: %q %v", cg, got, err)
		}
	}
	if _, err := unitFromCgroup("0::/user.slice/user-1000.slice/session-3.scope\n"); err == nil {
		t.Error("a postmaster outside a service was given a unit")
	}
}

func TestReplicaRefusesAServerInUse(t *testing.T) {
	ok := pgLocal{DataDir: "/var/lib/postgresql/16/main", VersionNum: 160004}
	if why := ok.refuse(16); why != "" {
		t.Fatal(why)
	}
	for _, c := range []struct {
		l    pgLocal
		want string
	}{
		{pgLocal{DataDir: ok.DataDir, VersionNum: 150002}, "runs Postgres 15 and the primary runs 16"},
		{pgLocal{DataDir: ok.DataDir, VersionNum: 160004, InRecovery: true}, "a replica already"},
		{pgLocal{DataDir: ok.DataDir, VersionNum: 160004, Databases: []string{"shop"}}, "databases of its own (shop)"},
		{pgLocal{DataDir: ok.DataDir, VersionNum: 160004, UserTables: 3}, "3 tables of its own"},
		{pgLocal{DataDir: "/", VersionNum: 160004}, "not one Argus will replace"},
	} {
		if why := c.l.refuse(16); !strings.Contains(why, c.want) {
			t.Errorf("%+v: %q", c.l, why)
		}
	}
	bad := []PGCloneRequest{
		{DataDir: "relative/dir", Host: "10.0.0.1", Port: 5432, Role: "argus_repl_r1", Slot: "argus_replica_r1", Major: 16},
		{DataDir: "/var/lib/x/../y", Host: "10.0.0.1", Port: 5432, Role: "argus_repl_r1", Slot: "argus_replica_r1", Major: 16},
		{DataDir: "/d", Host: "-h evil", Port: 5432, Role: "argus_repl_r1", Slot: "argus_replica_r1", Major: 16},
		{DataDir: "/d", Host: "10.0.0.1", Port: 5432, Role: "postgres", Slot: "argus_replica_r1", Major: 16},
	}
	for _, c := range bad {
		if err := c.check(); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
}

/*
The whole clone on real clusters: ARGUS_TEST_PG_SUPER_DSN is the primary,
and ARGUS_TEST_PG_CLONE names a second, fresh Debian cluster on the same
server ("16/rtest:5434", made with pg_createcluster) with an argus superuser
whose password is the primary's. The test runs as root and stands in for
systemd with pg_ctlcluster. Afterwards the cluster is put back as it was.
*/
func TestPGReplicaCloneEndToEnd(t *testing.T) {
	primaryDSN, spec := os.Getenv("ARGUS_TEST_PG_SUPER_DSN"), os.Getenv("ARGUS_TEST_PG_CLONE")
	if primaryDSN == "" || spec == "" {
		t.Skip("ARGUS_TEST_PG_SUPER_DSN and ARGUS_TEST_PG_CLONE are not both set")
	}
	verCluster, port, _ := strings.Cut(spec, ":")
	ver, cluster, _ := strings.Cut(verCluster, "/")
	ctl := func(verb string) error {
		out, err := exec.Command("pg_ctlcluster", ver, cluster, verb).CombinedOutput()
		if err != nil {
			t.Logf("pg_ctlcluster %s: %s", verb, out)
		}
		return err
	}
	ctx := context.Background()
	pcfg, _ := pgconn.ParseConfig(primaryDSN)
	replicaDSN := strings.Replace(strings.Replace(primaryDSN, ":"+strconv.Itoa(int(pcfg.Port))+"/", ":"+port+"/", 1),
		"/"+pcfg.Database+"?", "/postgres?", 1)
	primary, err := pgconn.Connect(ctx, primaryDSN)
	if err != nil {
		t.Fatal(err)
	}
	// Closed after the cleanups below, which use it.
	t.Cleanup(func() { primary.Close(context.Background()) })

	rep := tunnel.PGReplica{Name: "t_c1", Address: "127.0.0.1", By: "ana@corp.example"}
	var dataDir string
	t.Cleanup(func() {
		_ = ctl("stop")
		if dataDir != "" {
			if _, err := os.Stat(dataDir + keptSuffix); err == nil {
				_ = os.RemoveAll(dataDir)
				_ = os.Rename(dataDir+keptSuffix, dataDir)
			}
		}
		_ = ctl("start")
		RunPGAdmin(ctx, primary, PGAdminRequest{Op: "replica-drop", Database: "postgres", Replica: &rep})
	})

	a, b := net.Pipe()
	go func() { _, _ = io.Copy(io.Discard, b) }()
	defer a.Close()
	p, _ := strconv.Atoi(port)
	j := &backupJob{
		cfg: BackupConfig{
			Admin: DSNAdmin(func(string) string { return replicaDSN }),
			Tool: func(ctx context.Context, req PGBackupRequest, stdin io.Reader, stdout, stderr io.Writer) error {
				return RunPGBackup(ctx, req, stdin, stdout, stderr)
			},
			Units: &pgUnits{
				unitOf: func(int) (string, error) { return "postgresql@" + ver + "-" + cluster + ".service", nil },
				run: func(_ context.Context, verb, _ string) error {
					if verb == "start" && dataDir != "" {
						// pg_basebackup ran as root here, not as postgres.
						_ = exec.Command("chown", "-R", "postgres:postgres", dataDir).Run()
					}
					return ctl(verb)
				},
			},
		},
		log: slog.Default(), stream: pipeStream{a}, started: time.Now(), goOn: make(chan struct{}, 1),
		b: tunnel.GrantBackup{Job: tunnel.BackupJobReplicaClone, Name: "postgres", Host: "127.0.0.1", Port: p,
			Replica: &tunnel.GrantReplica{PGReplica: rep, PrimaryHost: "127.0.0.1", PrimaryPort: int(pcfg.Port), PrimaryVersion: 16}},
	}
	l, err := j.local(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dataDir = l.DataDir

	done := make(chan tunnel.BackupResult, 1)
	go func() { done <- j.replicaClone(ctx) }()
	// Wait for the verifier, as the control plane would, and prepare the
	// primary with it.
	var verifier string
	for deadline := time.Now().Add(30 * time.Second); verifier == ""; {
		j.pmu.Lock()
		if j.stage == tunnel.ReplicaStageVerifier {
			verifier = j.det
		}
		j.pmu.Unlock()
		select {
		case res := <-done:
			t.Fatalf("stopped before the verifier: %+v", res)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("no verifier")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !scramPattern.MatchString(verifier) {
		t.Fatalf("not a verifier: %q", verifier)
	}
	rep.Verifier = verifier
	for _, op := range []string{"replica-prepare", "replica-allow"} {
		if r := RunPGAdmin(ctx, primary, PGAdminRequest{Op: op, Database: "postgres", Replica: &rep}); !r.OK {
			t.Fatalf("%s: %s", op, r.Error)
		}
	}
	j.goOn <- struct{}{}
	var res tunnel.BackupResult
	select {
	case res = <-done:
	case <-time.After(5 * time.Minute):
		t.Fatal("the clone did not finish")
	}
	if !res.OK {
		t.Fatalf("clone: %+v", res)
	}
	var cl tunnel.PGReplicaCloned
	_ = json.Unmarshal(res.Replica, &cl)
	if !cl.InRecovery || cl.Kept != dataDir+keptSuffix || cl.Version/10000 != 16 {
		t.Fatalf("cloned: %+v", cl)
	}
	// The primary sees it streaming through its slot.
	deadline := time.Now().Add(30 * time.Second)
	for {
		v, err := row(ctx, primary, `SELECT active FROM pg_replication_slots WHERE slot_name = 'argus_replica_t_c1'`)
		if err == nil && v != nil && string(v[0]) == "t" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the slot is not in use: %q %v", v, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	// Dropping while it streams is refused.
	if r := RunPGAdmin(ctx, primary, PGAdminRequest{Op: "replica-drop", Database: "postgres", Replica: &rep}); r.OK {
		t.Fatal("dropped the slot of a streaming replica")
	}
}
