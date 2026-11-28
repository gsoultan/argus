package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

const debianHBA = `# Database administrative login by Unix domain socket
local   all             postgres                                peer
local   all             all                                     peer
host    all             all             127.0.0.1/32            scram-sha-256
host    all             all             ::1/128                 scram-sha-256
host    reports         analyst         10.0.9.0 255.255.255.0  scram-sha-256
host    all             all             10.0.0.50/32            md5 # argus
`

func TestHBAReadsArgusLinesAndTheServersOwn(t *testing.T) {
	var st PGHBAState
	readHBA(debianHBA, &st)
	if len(st.Mine) != 1 || !strings.HasPrefix(st.Mine[0], "host all all 10.0.0.50/32 md5") {
		t.Fatalf("mine %v", st.Mine)
	}
	if len(st.Others) != 1 || st.OpenToAll || len(st.Trust) != 0 {
		t.Fatalf("others %v open %v trust %v", st.Others, st.OpenToAll, st.Trust)
	}
	if addr, method, _ := hbaLine("host reports analyst 10.0.9.0 255.255.255.0 scram-sha-256"); addr != "10.0.9.0/24" || method != "scram-sha-256" {
		t.Fatalf("mask form read as %s %s", addr, method)
	}
	readHBA("host all all 0.0.0.0/0 trust\n", &st)
	if !st.OpenToAll || len(st.Trust) != 1 {
		t.Fatalf("an everyone trust line went unseen: %+v", st)
	}
}

func TestHBARewriteReplacesOnlyArgusLines(t *testing.T) {
	out := rewriteHBA(debianHBA, tunnel.PGHBALines(tunnel.ReachSelected, []string{"10.0.0.61", "10.0.4.0/24"}, "scram-sha-256"))
	if strings.Contains(out, "10.0.0.50") {
		t.Fatalf("the old Argus line stayed:\n%s", out)
	}
	for _, want := range []string{
		"host    reports         analyst         10.0.9.0 255.255.255.0  scram-sha-256",
		"host all all 10.0.0.61/32 scram-sha-256 # argus",
		"host all all 10.0.4.0/24 scram-sha-256 # argus",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	// The server's own lines come first, so Argus's never override them.
	if strings.Index(out, "analyst") > strings.Index(out, "10.0.0.61") {
		t.Fatalf("Argus's lines are not last:\n%s", out)
	}
	back := rewriteHBA(out, nil)
	if strings.Contains(back, "argus") || !strings.Contains(back, "analyst") {
		t.Fatalf("closing again left\n%s", back)
	}
	if rewriteHBA(back, nil) != back {
		t.Fatal("rewriting with nothing to add is not stable")
	}
}

func TestPGReach(t *testing.T) {
	for _, c := range []struct {
		n    PGNetwork
		want string
	}{
		{PGNetwork{PGHBAState: PGHBAState{Listen: "localhost"}}, tunnel.ReachLocal},
		{PGNetwork{PGHBAState: PGHBAState{Listen: "localhost,10.0.0.183"}, FirewallActive: true, FirewallSources: []string{"10.0.0.61"}}, tunnel.ReachSelected},
		{PGNetwork{PGHBAState: PGHBAState{Listen: "localhost,10.0.0.183"}}, tunnel.ReachNetwork},
		{PGNetwork{PGHBAState: PGHBAState{Listen: "*", Mine: []string{"host all all 0.0.0.0/0 scram-sha-256 # argus"}}}, tunnel.ReachAny},
	} {
		if got := pgReach(c.n); got != c.want {
			t.Errorf("%+v: %s, want %s", c.n, got, c.want)
		}
	}
}

// pgNetFake is the helper's half in memory, and a log of what ran in order.
type pgNetFake struct {
	st  PGHBAState
	log *[]string
}

func (f *pgNetFake) admin(_ context.Context, req PGAdminRequest) PGAdminResult {
	op := req.Admin
	if op.Kind == tunnel.AdminNetworkSet {
		*f.log = append(*f.log, "pg "+tunnel.PGListenFor(*op))
		f.st.Listen = tunnel.PGListenFor(*op)
		f.st.PendingRestart = true
		f.st.Mine = tunnel.PGHBALines(op.Reach, op.Sources, "scram-sha-256")
	}
	b, _ := json.Marshal(f.st)
	return PGAdminResult{OK: true, Data: b}
}

type bufStream struct{ bytes.Buffer }

func (*bufStream) Close() error                  { return nil }
func (*bufStream) SetDeadline(time.Time) error   { return nil }
func (b *bufStream) Read(p []byte) (int, error)  { return b.Buffer.Read(p) }
func (b *bufStream) Write(p []byte) (int, error) { return b.Buffer.Write(p) }

func pgNetSession(t *testing.T, f *pgNetFake, fw func(context.Context, tunnel.FirewallRequest) tunnel.FirewallResult, restarted *[]string) (*pgSession, *bufStream) {
	t.Helper()
	rec, err := os.Create(filepath.Join(t.TempDir(), "rec"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	st := &bufStream{}
	cfg := PostgresConfig{Firewall: fw, Restart: func(_ context.Context, unit string) error {
		*restarted = append(*restarted, unit)
		return nil
	}}
	return &pgSession{cfg: cfg, admin: f.admin, g: tunnel.Grant{Host: "db-01", Email: "ana@corp.example"},
		db:  tunnel.GrantDB{Name: "app", Host: "/var/run/postgresql", Port: 5432, Level: tunnel.LevelDBA},
		rec: rec, stream: st, log: slog.New(slog.DiscardHandler)}, st
}

// answers are the session's answers, and the records it wrote.
func answers(t *testing.T, st *bufStream) (ans []tunnel.PGAnswer, recs []tunnel.PGRecord) {
	t.Helper()
	fr := tunnel.NewFrameReader(&st.Buffer)
	for {
		typ, p, err := fr.Next()
		if err != nil {
			return
		}
		switch typ {
		case tunnel.FramePGAnswer:
			var a tunnel.PGAnswer
			_ = json.Unmarshal(p, &a)
			ans = append(ans, a)
		case tunnel.FramePGRecord:
			var r tunnel.PGRecord
			_ = json.Unmarshal(p, &r)
			recs = append(recs, r)
		}
	}
}

func TestPGNetworkSelectedNarrowsTheFirewallFirstAndRestarts(t *testing.T) {
	oldAddrs, oldSvc := hostAddresses, serviceOf
	hostAddresses = func() []RedisAddress { return nil }
	serviceOf = func(int, ...string) string { return "postgresql@16-main.service" }
	t.Cleanup(func() { hostAddresses, serviceOf = oldAddrs, oldSvc })

	var log, restarted []string
	f := &pgNetFake{st: PGHBAState{Listen: "localhost", Port: 5432, PID: 42}, log: &log}
	fw := &fakeRedisFirewall{active: true}
	s, st := pgNetSession(t, f, func(ctx context.Context, r tunnel.FirewallRequest) tunnel.FirewallResult {
		if !r.Check {
			log = append(log, "firewall "+r.Reach)
		}
		return fw.do(ctx, r)
	}, &restarted)

	op := &tunnel.PGAdminOp{Kind: tunnel.AdminNetworkSet, Reach: tunnel.ReachSelected,
		Addresses: []string{"10.0.0.183"}, Sources: []string{"10.0.0.61"}, Restart: true}
	if err := s.network(context.Background(), tunnel.PGRequest{ID: 7, Op: "admin", Admin: op}); err != nil {
		t.Fatal(err)
	}
	ans, recs := answers(t, st)
	if len(ans) != 1 || ans[0].Type != "admin" {
		t.Fatalf("answers %+v", ans)
	}
	if !slices.Equal(log, []string{"firewall selected", "pg localhost,10.0.0.183"}) {
		t.Fatalf("ran %v", log)
	}
	if !slices.Equal(restarted, []string{"postgresql@16-main.service"}) {
		t.Fatalf("restarted %v", restarted)
	}
	var res PGNetworkResult
	_ = json.Unmarshal(ans[0].Admin, &res)
	if !res.Restarted || res.Network.Reach != tunnel.ReachSelected {
		t.Fatalf("result %+v", res)
	}
	if len(recs) != 2 || recs[0].Kind != "admin" || !strings.Contains(recs[0].SQL, "PG_HBA host all all 10.0.0.61/32 PASSWORD") ||
		!strings.Contains(recs[0].SQL, "RESTART POSTGRES") || recs[1].Kind != "done" {
		t.Fatalf("records %+v", recs)
	}

	// Closing stops Postgres listening before the firewall's rules go.
	log = nil
	op = &tunnel.PGAdminOp{Kind: tunnel.AdminNetworkSet, Reach: tunnel.ReachLocal, Restart: true}
	if err := s.network(context.Background(), tunnel.PGRequest{ID: 8, Op: "admin", Admin: op}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(log, []string{"pg localhost", "firewall local"}) {
		t.Fatalf("ran %v", log)
	}
}

func TestPGNetworkRefusesBeforeRecording(t *testing.T) {
	oldAddrs := hostAddresses
	hostAddresses = func() []RedisAddress { return nil }
	t.Cleanup(func() { hostAddresses = oldAddrs })
	var log, restarted []string
	for _, c := range []struct {
		name     string
		st       PGHBAState
		fw       fakeRedisFirewall
		op       tunnel.PGAdminOp
		approval string
		want     string
	}{
		{"no firewall", PGHBAState{Listen: "localhost", Port: 5432}, fakeRedisFirewall{},
			tunnel.PGAdminOp{Reach: tunnel.ReachSelected, Addresses: []string{"10.0.0.183"}, Sources: []string{"10.0.0.61"}}, "", "no firewall is on"},
		{"open rule", PGHBAState{Listen: "localhost", Port: 5432}, fakeRedisFirewall{active: true, others: []string{"5432/tcp ALLOW IN Anywhere"}},
			tunnel.PGAdminOp{Reach: tunnel.ReachSelected, Addresses: []string{"10.0.0.183"}, Sources: []string{"10.0.0.61"}}, "", "did not add"},
		{"anywhere unapproved", PGHBAState{Listen: "localhost", Port: 5432}, fakeRedisFirewall{active: true},
			tunnel.PGAdminOp{Reach: tunnel.ReachAny, Confirm: "db-01"}, "", "someone else approves"},
		{"anywhere misnamed", PGHBAState{Listen: "localhost", Port: 5432}, fakeRedisFirewall{active: true},
			tunnel.PGAdminOp{Reach: tunnel.ReachAny, Confirm: "db-02"}, "ap-1", "type the server's name"},
		{"anywhere with trust", PGHBAState{Listen: "localhost", Port: 5432, Trust: []string{"host all all 10.0.0.0/8 trust"}}, fakeRedisFirewall{active: true},
			tunnel.PGAdminOp{Reach: tunnel.ReachAny, Confirm: "DB-01"}, "ap-1", "no password"},
		{"public network", PGHBAState{Listen: "localhost", Port: 5432}, fakeRedisFirewall{active: true},
			tunnel.PGAdminOp{Reach: tunnel.ReachNetwork, Addresses: []string{"10.0.0.183"}, Sources: []string{"8.8.8.0/24"}}, "", "not a private network"},
	} {
		t.Run(c.name, func(t *testing.T) {
			log = nil
			f := &pgNetFake{st: c.st, log: &log}
			fw := c.fw
			s, st := pgNetSession(t, f, fw.do, &restarted)
			op := c.op
			op.Kind = tunnel.AdminNetworkSet
			if err := s.network(context.Background(), tunnel.PGRequest{ID: 1, Op: "admin", Admin: &op, Approval: c.approval}); err != nil {
				t.Fatal(err)
			}
			ans, recs := answers(t, st)
			if len(ans) != 1 || ans[0].Type != "error" || !strings.Contains(ans[0].Error, c.want) {
				t.Fatalf("answers %+v", ans)
			}
			if len(recs) != 0 || len(log) != 0 {
				t.Fatalf("recorded %v, ran %v", recs, log)
			}
		})
	}
}

func TestPGNetworkTextAndValidate(t *testing.T) {
	op := tunnel.PGAdminOp{Kind: tunnel.AdminNetworkSet, Reach: tunnel.ReachAny, Restart: true}
	if err := tunnel.ValidatePGNetwork(&op); err != nil {
		t.Fatal(err)
	}
	want := "ALTER SYSTEM SET listen_addresses = '*'\nPG_HBA host all all 0.0.0.0/0 PASSWORD\nPG_HBA host all all ::/0 PASSWORD\nFIREWALL ALLOW ANY\nRESTART POSTGRES"
	if got := tunnel.PGNetworkText(op); got != want {
		t.Fatalf("text\n%s\nwant\n%s", got, want)
	}
	if !tunnel.PGAlwaysApproval(&op) {
		t.Fatal("anywhere does not always wait")
	}
	for _, bad := range []tunnel.PGAdminOp{
		{Reach: "open"},
		{Reach: tunnel.ReachSelected, Addresses: []string{"10.0.0.1"}},
		{Reach: tunnel.ReachSelected, Sources: []string{"10.0.0.61"}},
		{Reach: tunnel.ReachSelected, Addresses: []string{"127.0.0.1"}, Sources: []string{"10.0.0.61"}},
		{Reach: tunnel.ReachSelected, Addresses: []string{"10.0.0.1'; DROP"}, Sources: []string{"10.0.0.61"}},
		{Reach: tunnel.ReachLocal, Sources: []string{"10.0.0.61"}},
	} {
		if tunnel.ValidatePGNetwork(&bad) == nil {
			t.Errorf("%+v was accepted", bad)
		}
	}
}

// Against a real Postgres whose pg_hba.conf this test can reach: the
// helper's change is loaded, a bad file is put back, and closing again
// leaves the file as it was. Skipped where Postgres runs in a container.
func TestPGNetworkHelperOnARealPostgres(t *testing.T) {
	dsn := os.Getenv("ARGUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ARGUS_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	conn, err := pgConnect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	hba, err := showText(ctx, conn, "SHOW hba_file")
	if err != nil {
		t.Skip("cannot read hba_file: " + err.Error())
	}
	before, err := os.ReadFile(hba)
	if err != nil {
		t.Skip("pg_hba.conf is not on this machine: " + err.Error())
	}
	listen, _ := showText(ctx, conn, "SHOW listen_addresses")
	t.Cleanup(func() {
		_ = os.WriteFile(hba, before, 0o600)
		_, _ = pgExec(ctx, conn, "ALTER SYSTEM SET listen_addresses = "+quoteLiteral(listen))
		_, _ = pgExec(ctx, conn, "SELECT pg_reload_conf()")
	})

	op := &tunnel.PGAdminOp{Kind: tunnel.AdminNetworkSet, Reach: tunnel.ReachSelected,
		Addresses: []string{"10.255.0.1"}, Sources: []string{"10.255.0.61"}}
	res := pgNetworkHelper(ctx, conn, PGAdminRequest{Admin: op})
	if !res.OK {
		t.Fatal(res.Error)
	}
	var st PGHBAState
	_ = json.Unmarshal(res.Data, &st)
	if len(st.Mine) != 1 || !strings.Contains(st.Mine[0], "10.255.0.61/32") {
		t.Fatalf("mine %v", st.Mine)
	}
	if !st.PendingRestart {
		t.Fatal("a new listen_addresses does not say it waits for a restart")
	}
	got, err := showText(ctx, conn, "SELECT count(*) FROM pg_hba_file_rules WHERE address = '10.255.0.61' AND netmask = '255.255.255.255' AND error IS NULL")
	if err != nil {
		t.Fatal(err)
	}
	if got != "1" {
		t.Fatalf("Postgres does not list the new line: %s", got)
	}

	res = pgNetworkHelper(ctx, conn, PGAdminRequest{Admin: &tunnel.PGAdminOp{Kind: tunnel.AdminNetworkSet, Reach: tunnel.ReachLocal}})
	if !res.OK {
		t.Fatal(res.Error)
	}
	after, _ := os.ReadFile(hba)
	if strings.TrimRight(string(after), "\n") != strings.TrimRight(string(before), "\n") {
		t.Fatalf("closing did not leave the file as it was:\n%s", after)
	}
}
