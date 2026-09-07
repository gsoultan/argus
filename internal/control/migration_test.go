package control

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

/*
Upgrades, against a database that has something in it.

Every migration here had only ever run against an empty schema, which is the
one situation no customer is ever in. An ALTER that needs a default, a UNIQUE
index that existing rows violate, a NOT NULL added to a populated column --
none of those can fail on an empty table, and all of them fail on a real one.

These tests take the upgrade the way a customer takes it: stop at version k,
write data through the schema as it existed then, and apply the rest.
*/

// adminDSN points at the maintenance database so scratch databases can be
// created and dropped. Derived from the test DSN rather than configured
// separately, so there is only one thing to set.
func adminDSN(t *testing.T) (string, string) {
	t.Helper()
	dsn := os.Getenv("ARGUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ARGUS_TEST_DATABASE_URL is not set; skipping database tests")
	}
	i := strings.LastIndex(dsn, "/")
	j := strings.Index(dsn[i:], "?")
	if i < 0 || j < 0 {
		t.Skipf("cannot derive an admin DSN from %q", dsn)
	}
	return dsn[:i+1] + "postgres" + dsn[i+j:], dsn[:i+1]
}

// scratchDB creates an empty database and drops it when the test ends.
func scratchDB(t *testing.T, name string) string {
	t.Helper()
	admin, prefix := adminDSN(t)
	ctx := context.Background()

	// Each admin statement gets its own connection rather than sharing a pool.
	//
	// The pooled version closed the pool when this function returned and then
	// used it again from t.Cleanup, where the error was discarded -- so every
	// scratch database survived the run. Sixty-nine of them accumulated before
	// CREATE DATABASE started blocking long enough to time tests out.
	onAdmin := func(sql string, args ...any) error {
		conn, err := pgx.Connect(ctx, admin)
		if err != nil {
			return err
		}
		defer conn.Close(ctx)
		_, err = conn.Exec(ctx, sql, args...)
		return err
	}
	if err := onAdmin("SELECT 1"); err != nil {
		t.Skipf("cannot reach the maintenance database: %v", err)
	}

	drop := func() {
		// Terminate stragglers first: a connection that has not finished
		// closing keeps the database alive and turns cleanup into a flake.
		_ = onAdmin(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		             WHERE datname = $1 AND pid <> pg_backend_pid()`, name)
		if err := onAdmin("DROP DATABASE IF EXISTS " + name); err != nil {
			t.Errorf("could not drop scratch database %s: %v", name, err)
		}
	}
	drop()
	if err := onAdmin("CREATE DATABASE " + name); err != nil {
		// Not skipped. These tests are the only cover the upgrade path has,
		// and a run that quietly declines to check it reads as a pass.
		t.Fatalf("create scratch database %s: %v\n"+
			"the test role needs CREATEDB to exercise the upgrade path", name, err)
	}
	t.Cleanup(drop)

	rest := os.Getenv("ARGUS_TEST_DATABASE_URL")
	return prefix + name + rest[strings.Index(rest[strings.LastIndex(rest, "/"):], "?")+strings.LastIndex(rest, "/"):]
}

// openAt connects and applies exactly the first n migrations.
func openAt(t *testing.T, dsn string, n int) *Store {
	t.Helper()
	ctx := context.Background()
	names, err := migrationNames()
	if err != nil {
		t.Fatal(err)
	}
	if n > len(names) {
		n = len(names)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	s := &Store{pool: pool}
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	if err := s.applyMigrations(ctx, names[:n]); err != nil {
		t.Fatalf("apply first %d migrations: %v", n, err)
	}
	return s
}

// seed writes a row into every table that exists at migration 001, using only
// the columns that existed then. Later migrations add columns with defaults,
// so the same statements stay valid at every version -- which is exactly the
// property being tested.
func seed(t *testing.T, s *Store, tag string) {
	t.Helper()
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := s.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed %s: %v", tag, err)
		}
	}
	exec(`INSERT INTO assets (hostname, address) VALUES ($1, '10.0.0.9')`, "host-"+tag)
	exec(`INSERT INTO users (email, display_name) VALUES ($1, 'Seeded User')`, "seed-"+tag+"@corp.example")
	// sessions.id has no default: the gateway mints it, so the seed must too.
	exec(`INSERT INTO sessions (id, user_email, asset_hostname, principal, started_at)
	      VALUES (gen_random_uuid(), $1, $2, 'ops', now())`,
		"seed-"+tag+"@corp.example", "host-"+tag)
	exec(`INSERT INTO access_requests (requester_email, principal, justification, duration_minutes)
	      VALUES ($1, 'root', 'seeded for the upgrade test', 30)`, "seed-"+tag+"@corp.example")
	exec(`INSERT INTO agents (hostname) VALUES ($1)`, "agent-"+tag)

	// Through the real API, so the audit chain is genuine and can be verified
	// after the upgrade rather than merely counted.
	if _, err := s.AppendAudit(ctx, AuditEvent{
		Action: "test.seeded", Severity: "info", ActorEmail: "seed-" + tag + "@corp.example",
		Target: "host-" + tag, Detail: "written before the remaining migrations ran",
	}); err != nil {
		t.Fatalf("seed audit %s: %v", tag, err)
	}
}

func counts(t *testing.T, s *Store) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, table := range []string{"assets", "users", "sessions", "access_requests", "agents", "audit_events"} {
		var n int
		if err := s.pool.QueryRow(context.Background(),
			"SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		out[table] = n
	}
	return out
}

// The upgrade path, stopped at every version along the way.
func TestMigrationsApplyToAPopulatedDatabase(t *testing.T) {
	names, err := migrationNames()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) < 2 {
		t.Skip("nothing to upgrade through")
	}

	for k := 1; k < len(names); k++ {
		t.Run(fmt.Sprintf("from_%02d_%s", k, strings.TrimSuffix(names[k-1], ".sql")), func(t *testing.T) {
			dsn := scratchDB(t, fmt.Sprintf("argus_mig_%d_%d", k, time.Now().UnixNano()%100000))

			s := openAt(t, dsn, k)
			seed(t, s, fmt.Sprintf("k%d", k))
			before := counts(t, s)
			before9, err := s.VerifyAuditChain(context.Background())
			if err != nil {
				t.Fatalf("verify chain before upgrade: %v", err)
			}
			if !before9.OK {
				t.Fatalf("the seeded chain was already broken: %s", before9.Detail)
			}
			s.Close()

			// The rest of the upgrade, exactly as a restart would run it.
			s2, err := Open(context.Background(), dsn)
			if err != nil {
				t.Fatalf("upgrade from %d to %d: %v", k, len(names), err)
			}
			defer s2.Close()

			after := counts(t, s2)
			for table, n := range before {
				if after[table] != n {
					t.Errorf("%s: %d rows before the upgrade, %d after", table, n, after[table])
				}
			}
			// The chain is the artefact an auditor relies on. An upgrade that
			// rewrites a timestamp, a column type or a default would leave the
			// rows intact and the evidence worthless.
			after9, err := s2.VerifyAuditChain(context.Background())
			if err != nil {
				t.Fatalf("verify chain after upgrade: %v", err)
			}
			if !after9.OK {
				t.Errorf("the upgrade broke the audit chain at %d: %s", after9.BrokenAt, after9.Detail)
			}
			if after9.Head != before9.Head {
				t.Errorf("the upgrade moved the audit chain head:\n  before %s\n  after  %s",
					before9.Head, after9.Head)
			}

			// An upgrade has to survive being run twice: a crash between the
			// migration and its bookkeeping row, or two replicas starting at
			// once, both land here.
			if err := s2.migrate(context.Background()); err != nil {
				t.Errorf("re-running migrations is not a no-op: %v", err)
			}
		})
	}
}

// A fresh install must reach the same schema as an upgraded one, or the two
// diverge quietly and only one of them is ever tested again.
func TestFreshInstallMatchesAnUpgrade(t *testing.T) {
	names, err := migrationNames()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	fresh := scratchDB(t, fmt.Sprintf("argus_fresh_%d", time.Now().UnixNano()%100000))
	f, err := Open(ctx, fresh)
	if err != nil {
		t.Fatalf("fresh install: %v", err)
	}
	defer f.Close()

	upgraded := scratchDB(t, fmt.Sprintf("argus_upg_%d", time.Now().UnixNano()%100000))
	u := openAt(t, upgraded, 1)
	seed(t, u, "upg")
	u.Close()
	u2, err := Open(ctx, upgraded)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer u2.Close()

	if a, b := schemaOf(t, f), schemaOf(t, u2); a != b {
		t.Errorf("a fresh install and an upgraded database have different schemas\n"+
			"--- fresh ---\n%s\n--- upgraded ---\n%s", a, b)
	}
	if len(names) == 0 {
		t.Fatal("no migrations found")
	}
}

// schemaOf renders every column and index, so a difference shows as a diff
// rather than as a mysterious failure three releases later.
func schemaOf(t *testing.T, s *Store) string {
	t.Helper()
	ctx := context.Background()
	var b strings.Builder

	rows, err := s.pool.Query(ctx, `
		SELECT table_name, column_name, data_type, is_nullable, coalesce(column_default,'')
		  FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name <> 'schema_migrations'
		 ORDER BY table_name, column_name`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var tbl, col, typ, null, def string
		if err := rows.Scan(&tbl, &col, &typ, &null, &def); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s.%s %s null=%s default=%s\n", tbl, col, typ, null, def)
	}
	rows.Close()

	idx, err := s.pool.Query(ctx, `
		SELECT tablename, indexdef FROM pg_indexes
		 WHERE schemaname = 'public' AND tablename <> 'schema_migrations'
		 ORDER BY tablename, indexdef`)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	for idx.Next() {
		var tbl, def string
		if err := idx.Scan(&tbl, &def); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "index %s: %s\n", tbl, def)
	}
	return b.String()
}
