//go:build !windows

package agent

import (
	"database/sql"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
SQL Server replication through a DBA session, against a real server: an
availability group (one replica, no cluster: what a single container can
hold) that the database joins, is suspended in, resumed and leaves; and
transactional replication on this server as its own distributor, from a
publication to a subscriber database, rows arriving there. The admin
login's password is in no recording.

The availability group needs HADR enabled (MSSQL_ENABLE_HADR=1);
transactional replication needs SQL Server Agent running. Each part skips
when its server lacks them.
*/

func TestMSSQLReplication(t *testing.T) {
	m := newMSService(t)
	pw, port := msTest(t)
	root := msRoot(t, pw, port)
	t.Run("availability group", func(t *testing.T) { msAGTest(t, m, root) })
	t.Run("transactional", func(t *testing.T) { msPubTest(t, m, root, pw) })
	b, err := os.ReadFile(m.dir + "/" + msRecording(t, m.dir, "dba"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), pw) {
		t.Fatal("the admin password is in the recording")
	}
}

func msOps(t *testing.T, h *pgHarness) (ok func(tunnel.PGAdminOp) tunnel.PGAnswer, refused func(tunnel.PGAdminOp, string)) {
	id := uint32(100)
	ok = func(op tunnel.PGAdminOp) tunnel.PGAnswer {
		t.Helper()
		id++
		a := h.admin(id, op)
		if a.Type != "admin" {
			t.Fatalf("%s: %+v", op.Kind, a)
		}
		return a
	}
	refused = func(op tunnel.PGAdminOp, want string) {
		t.Helper()
		id++
		if a := h.admin(id, op); a.Type != "error" || !strings.Contains(a.Error, want) {
			t.Fatalf("%s: %+v, want an error with %q", op.Kind, a, want)
		}
	}
	return ok, refused
}

func msAGTest(t *testing.T, m msService, root *sql.DB) {
	var hadr int
	if err := root.QueryRow("SELECT CAST(ISNULL(SERVERPROPERTY('IsHadrEnabled'), 0) AS int)").Scan(&hadr); err != nil || hadr == 0 {
		t.Skip("HADR is not enabled on this server")
	}
	const db, ag = "argus_t_agdb", "argus_t_ag"
	var server string
	if err := root.QueryRow("SELECT @@SERVERNAME").Scan(&server); err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		_, _ = root.Exec("IF EXISTS (SELECT 1 FROM sys.availability_groups WHERE name = N'" + ag + "') DROP AVAILABILITY GROUP [" + ag + "]")
	}
	cleanup()
	msDB(t, root, db)
	t.Cleanup(cleanup)
	msExec(t, root,
		"IF NOT EXISTS (SELECT 1 FROM sys.symmetric_keys WHERE name = '##MS_DatabaseMasterKey##') CREATE MASTER KEY ENCRYPTION BY PASSWORD = 'Argus-Test-Key-1!'",
		"IF NOT EXISTS (SELECT 1 FROM sys.certificates WHERE name = 'argus_t_ag_cert') CREATE CERTIFICATE argus_t_ag_cert WITH SUBJECT = 'argus test'",
		"IF NOT EXISTS (SELECT 1 FROM sys.database_mirroring_endpoints) CREATE ENDPOINT argus_t_hadr STATE = STARTED AS TCP (LISTENER_PORT = 5022) "+
			"FOR DATABASE_MIRRORING (ROLE = ALL, AUTHENTICATION = CERTIFICATE argus_t_ag_cert, ENCRYPTION = REQUIRED ALGORITHM AES)",
		"CREATE AVAILABILITY GROUP ["+ag+"] WITH (CLUSTER_TYPE = NONE) FOR REPLICA ON N'"+server+"' WITH (ENDPOINT_URL = N'tcp://localhost:5022', "+
			"AVAILABILITY_MODE = ASYNCHRONOUS_COMMIT, FAILOVER_MODE = MANUAL, SEEDING_MODE = AUTOMATIC)",
		"ALTER DATABASE ["+db+"] SET RECOVERY SIMPLE")

	h := m.mustOpen(t, db, tunnel.LevelDBA)
	defer h.close()
	ok, refused := msOps(t, h)
	read := func() MSReplication {
		return msAdminData[MSReplication](t, ok(tunnel.PGAdminOp{Kind: tunnel.AdminMSReplication}))
	}
	r := read()
	if !r.HadrEnabled || r.AG != nil || !slices.ContainsFunc(r.Groups, func(g MSGroup) bool { return g.Name == ag && strings.EqualFold(g.ClusterType, "NONE") }) {
		t.Fatalf("before: %+v", r)
	}
	op := func(kind string) tunnel.PGAdminOp {
		return tunnel.PGAdminOp{Kind: kind, MSRepl: &tunnel.MSSQLReplSpec{Group: ag}}
	}
	// Simple recovery cannot join; full, with a full backup, can.
	refused(op(tunnel.AdminMSAGAdd), "full recovery model")
	msExec(t, root, "ALTER DATABASE ["+db+"] SET RECOVERY FULL",
		"BACKUP DATABASE ["+db+"] TO DISK = N'/var/opt/mssql/data/"+db+".bak' WITH INIT, FORMAT")
	ok(op(tunnel.AdminMSAGAdd))
	r = read()
	if r.AG == nil || r.AG.Group != ag || r.AG.LocalRole != "PRIMARY" || len(r.AG.Replicas) != 1 || !r.AG.Replicas[0].Local {
		t.Fatalf("in the group: %+v", r.AG)
	}
	refused(op(tunnel.AdminMSAGAdd), "in an availability group already")
	ok(op(tunnel.AdminMSAGSuspend))
	if r = read(); r.AG.Replicas[0].Suspended == "" {
		t.Fatalf("suspended: %+v", r.AG.Replicas[0])
	}
	ok(op(tunnel.AdminMSAGResume))
	if r = read(); r.AG.Replicas[0].Suspended != "" {
		t.Fatalf("resumed: %+v", r.AG.Replicas[0])
	}
	// A failover is run from the secondary that is to take over.
	refused(op(tunnel.AdminMSAGFailover), "not a secondary")
	ok(op(tunnel.AdminMSAGRemove))
	if r = read(); r.AG != nil {
		t.Fatalf("left the group: %+v", r.AG)
	}
	refused(op(tunnel.AdminMSAGRemove), "not in "+ag)
}

func msPubTest(t *testing.T, m msService, root *sql.DB, pw string) {
	var agent int
	_ = root.QueryRow("SELECT COUNT(*) FROM sys.dm_server_services WHERE servicename LIKE 'SQL Server Agent%' AND status_desc = 'Running'").Scan(&agent)
	if agent == 0 {
		t.Skip("SQL Server Agent is not running")
	}
	const pub, sub, p = "argus_t_pub", "argus_t_sub", "argus_t_orders"
	undo := func() {
		_, _ = root.Exec("IF DB_ID(N'" + pub + "') IS NOT NULL AND OBJECT_ID(N'" + pub + ".dbo.syspublications') IS NOT NULL " +
			"EXEC [" + pub + "].sys.sp_removedbreplication @dbname = N'" + pub + "', @type = N'both'")
		_, _ = root.Exec("IF EXISTS (SELECT 1 FROM sys.servers WHERE name = 'repl_distributor') EXEC master.sys.sp_dropdistributor @no_checks = 1, @ignore_distributor = 1")
	}
	undo()
	msDB(t, root, pub)
	msDB(t, root, sub)
	t.Cleanup(undo) // runs before the databases are dropped
	msExec(t, root, "CREATE TABLE ["+pub+"].dbo.orders (id INT PRIMARY KEY, note NVARCHAR(40))",
		"INSERT INTO ["+pub+"].dbo.orders VALUES (1, N'one'), (2, N'two')",
		"CREATE TABLE ["+pub+"].dbo.nokey (note NVARCHAR(40))")

	h := m.mustOpen(t, pub, tunnel.LevelDBA)
	defer h.close()
	ok, refused := msOps(t, h)
	yes, no := true, false
	repl := func(kind string, r tunnel.MSSQLReplSpec) tunnel.PGAdminOp {
		return tunnel.PGAdminOp{Kind: kind, MSRepl: &r}
	}
	read := func() MSReplication {
		return msAdminData[MSReplication](t, ok(tunnel.PGAdminOp{Kind: tunnel.AdminMSReplication}))
	}
	r := read()
	if r.Distributor || r.Published || r.ReplDir == "" || !slices.Contains(r.Databases, sub) ||
		!slices.ContainsFunc(r.Tables, func(x MSReplTable) bool { return x.Name == "orders" && x.PK }) ||
		!slices.ContainsFunc(r.Tables, func(x MSReplTable) bool { return x.Name == "nokey" && !x.PK }) {
		t.Fatalf("before: %+v", r)
	}
	refused(repl(tunnel.AdminMSReplPublish, tunnel.MSSQLReplSpec{Enabled: &yes}), "no distributor")
	ok(repl(tunnel.AdminMSReplDistributor, tunnel.MSSQLReplSpec{WorkingDir: r.ReplDir}))
	refused(repl(tunnel.AdminMSReplDistributor, tunnel.MSSQLReplSpec{WorkingDir: r.ReplDir}), "distributor already")
	refused(repl(tunnel.AdminMSReplPubAdd, tunnel.MSSQLReplSpec{Publication: p}), "publishing is off")
	ok(repl(tunnel.AdminMSReplPublish, tunnel.MSSQLReplSpec{Enabled: &yes}))
	ok(repl(tunnel.AdminMSReplPubAdd, tunnel.MSSQLReplSpec{Publication: p}))
	refused(repl(tunnel.AdminMSReplArticleAdd, tunnel.MSSQLReplSpec{Publication: p, Schema: "dbo", Table: "missing"}), "no table dbo.missing")
	ok(repl(tunnel.AdminMSReplArticleAdd, tunnel.MSSQLReplSpec{Publication: p, Schema: "dbo", Table: "orders"}))
	refused(repl(tunnel.AdminMSReplSubAdd, tunnel.MSSQLReplSpec{Publication: p, SubscriberDB: "argus_t_none"}), "create it first")
	ok(repl(tunnel.AdminMSReplSubAdd, tunnel.MSSQLReplSpec{Publication: p, SubscriberDB: sub}))
	ok(repl(tunnel.AdminMSReplSnapshot, tunnel.MSSQLReplSpec{Publication: p}))

	r = read()
	if !r.Distributor || !r.Published || r.Distribution != tunnel.MSDistributionDB || len(r.Publications) != 1 {
		t.Fatalf("published: %+v", r)
	}
	pb := r.Publications[0]
	if pb.Name != p || pb.Kind != "transactional" || len(pb.Articles) != 1 || pb.Articles[0].Table != "orders" ||
		len(pb.Subscriptions) != 1 || pb.Subscriptions[0].DB != sub {
		t.Fatalf("publication: %+v", pb)
	}

	// The snapshot, then a change, reach the subscriber.
	msExec(t, root, "INSERT INTO ["+pub+"].dbo.orders VALUES (3, N'three')")
	deadline := time.Now().Add(3 * time.Minute)
	for {
		var n int
		err := root.QueryRow("SELECT COUNT(*) FROM [" + sub + "].dbo.orders").Scan(&n)
		if err == nil && n == 3 {
			break
		}
		if time.Now().After(deadline) {
			r = read()
			t.Fatalf("rows at the subscriber: %d (%v); %+v %+v", n, err, r.Publications[0].Snapshot, r.Publications[0].Subscriptions[0].Agent)
		}
		time.Sleep(2 * time.Second)
	}
	r = read()
	if s := r.Publications[0].Subscriptions[0]; s.Agent == nil || s.Agent.Status == "" {
		t.Fatalf("agent: %+v", s)
	}

	ok(repl(tunnel.AdminMSReplSubDrop, tunnel.MSSQLReplSpec{Publication: p, SubscriberDB: sub}))
	ok(repl(tunnel.AdminMSReplPubDrop, tunnel.MSSQLReplSpec{Publication: p}))
	ok(repl(tunnel.AdminMSReplPublish, tunnel.MSSQLReplSpec{Enabled: &no}))
	if r = read(); r.Published || len(r.Publications) != 0 {
		t.Fatalf("after: %+v", r)
	}
	_ = pw
}
