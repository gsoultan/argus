package control

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

/*
What one agent heartbeat costs the database. A fleet of a thousand agents
beating every 30 seconds is 33 of these a second, for ever, so its write
volume is most of what the control plane's Postgres does while nobody is
using the console.

	ARGUS_TEST_DATABASE_URL=postgres://… go test ./internal/control/ \
	  -run xxx -bench Heartbeat -benchtime 2000x

Reports WAL bytes per heartbeat beside the time: an UPDATE that rewrites a
row it did not change still costs a new tuple, its index entries and the WAL
for both, and that is the disk an idle control plane fills.
*/

func BenchmarkHeartbeat(b *testing.B) {
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

	// A fleet: hosts in the inventory under their full names, agents that
	// know only the short one -- the match the heartbeat has to make.
	const fleet = 2000
	prefix := unique("hb")
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO assets (hostname, address, port, protocol, source, health, host_key_state)
		SELECT $1 || '-' || g || '.prod.example.com', '10.0.0.1', 22, 'ssh', 'console', 'reachable', 'unpinned'
		  FROM generate_series(1, $2) g`, prefix, fleet); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		_, _ = s.pool.Exec(ctx, `DELETE FROM agents WHERE hostname LIKE $1 || '-%'`, prefix)
		_, _ = s.pool.Exec(ctx, `DELETE FROM assets WHERE hostname LIKE $1 || '-%'`, prefix)
	})

	// Posture as the agent sends it from a host with a few dozen accounts.
	var accounts []map[string]any
	for i := 0; i < 40; i++ {
		accounts = append(accounts, map[string]any{
			"user": fmt.Sprintf("user%02d", i), "password": "locked", "keys": 2, "can_login": true})
	}
	var keys []any
	for i := 0; i < 6; i++ {
		keys = append(keys, map[string]any{"user": "deploy", "file": "/home/deploy/.ssh/authorized_keys",
			"line": i + 1, "fingerprint": fmt.Sprintf("SHA256:%043d", i), "comment": "ci@builder", "key_type": "ssh-ed25519"})
	}
	raw, _ := json.Marshal(map[string]any{
		"rating": "recorded", "unmanaged_keys": keys, "shim_installed": true, "drift": []string{},
		"login_accounts": accounts,
		"sshd_auth":      map[string]any{"passwords": false, "keys": true, "expose_auth_info": true},
	})
	var posture any
	_ = json.Unmarshal(raw, &posture)

	// Every agent once, so what is measured is the steady state.
	for i := 1; i <= fleet; i++ {
		if err := s.Heartbeat(ctx, fmt.Sprintf("%s-%d", prefix, i), "dev", 0, posture, true, "",
			nil, nil, nil); err != nil && err != ErrAgentUnmatched {
			b.Fatal(err)
		}
	}
	_, _ = s.pool.Exec(ctx, `VACUUM ANALYZE assets`)
	_, _ = s.pool.Exec(ctx, `VACUUM ANALYZE agents`)

	var start string
	_ = s.pool.QueryRow(ctx, `SELECT pg_current_wal_insert_lsn()::text`).Scan(&start)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		host := fmt.Sprintf("%s-%d", prefix, i%fleet+1)
		if err := s.Heartbeat(ctx, host, "dev", 0, posture, true, "", nil, nil, nil); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	var wal float64
	_ = s.pool.QueryRow(ctx, `SELECT pg_wal_lsn_diff(pg_current_wal_insert_lsn(), $1::pg_lsn)`, start).Scan(&wal)
	b.ReportMetric(wal/float64(b.N), "WAL-B/op")
}
