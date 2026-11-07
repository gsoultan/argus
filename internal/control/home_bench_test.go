package control

import (
	"context"
	"os"
	"testing"
	"time"
)

/*
What one build of Home's live dashboard costs argus-control, on a fleet of a
thousand servers, each with an agent reporting usage, and ten thousand
sessions in the last day. The hub runs the fast half every 5 s and the slow
half every 30 s, once per organization while anyone watches.

	ARGUS_TEST_DATABASE_URL=postgres://… go test ./internal/control/ \
	  -run xxx -bench HomeSnapshot -benchmem
*/

func BenchmarkHomeSnapshot(b *testing.B) {
	dsn := os.Getenv("ARGUS_TEST_DATABASE_URL")
	if dsn == "" {
		b.Skip("ARGUS_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	s, err := Open(ctx, dsn)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()

	prefix := unique("home")
	usage := `{"at":"` + time.Now().UTC().Format(time.RFC3339) + `","load":[1,1,1],"cpu_busy":42.5,"mem_total":8000,` +
		`"mem_available":4000,"swap_total":0,"swap_free":0,"disks":[{"mount":"/","fs":"ext4","total":100,"used":40,"avail":60},` +
		`{"mount":"/var","fs":"ext4","total":100,"used":93,"avail":7}]}`
	for _, q := range []string{
		`INSERT INTO assets (hostname, address, port, protocol, source, health, host_key_state)
		 SELECT $1 || '-' || g || '.prod.example.com', '10.0.0.1', 22, 'ssh', 'console', 'reachable', 'unpinned'
		   FROM generate_series(1, 1000) g`,
		`INSERT INTO agents (hostname, version, last_seen_at, updated_at, usage, usage_at)
		 SELECT $1 || '-' || g || '.prod.example.com', 'test', now(), now(), '` + usage + `'::jsonb, now()
		   FROM generate_series(1, 1000) g`,
		`INSERT INTO sessions (id, user_email, asset_hostname, principal, state, started_at, last_reported_at, kind)
		 SELECT gen_random_uuid(), $1 || '@x.id', $1 || '-' || (g % 1000) || '.prod.example.com', 'deploy',
		        CASE WHEN g % 100 = 0 THEN 'active' ELSE 'ended' END,
		        now() - (g || ' seconds')::interval * 8, now(),
		        CASE g % 3 WHEN 0 THEN 'terminal' WHEN 1 THEN 'desktop' ELSE 'database' END
		   FROM generate_series(1, 10000) g`,
	} {
		if _, err := s.pool.Exec(ctx, q, prefix); err != nil {
			b.Fatal(err)
		}
	}
	b.Cleanup(func() {
		_, _ = s.pool.Exec(ctx, `DELETE FROM sessions WHERE user_email = $1 || '@x.id'`, prefix)
		_, _ = s.pool.Exec(ctx, `DELETE FROM agents WHERE hostname LIKE $1 || '-%'`, prefix)
		_, _ = s.pool.Exec(ctx, `DELETE FROM assets WHERE hostname LIKE $1 || '-%'`, prefix)
	})
	_, _ = s.pool.Exec(ctx, `ANALYZE assets; ANALYZE agents; ANALYZE sessions`)

	b.Run("fast", func(b *testing.B) {
		for b.Loop() {
			var h HomeSnapshot
			if err := s.homeFast(ctx, &h); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("slow", func(b *testing.B) {
		for b.Loop() {
			var h HomeSnapshot
			if err := s.homeSlow(ctx, &h, time.Now()); err != nil {
				b.Fatal(err)
			}
		}
	})
}
