package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const resoluteOS = "ID=ubuntu\nVERSION_ID=\"26.04\"\nVERSION_CODENAME=resolute\nPRETTY_NAME=\"Ubuntu 26.04.1 LTS\"\n"

// The server of 2026-11-24: PostgreSQL 17 runs, and an update brought
// psql 19 with the postgresql-client metapackage. psql said
// "psql (19.0, server 17.12)" and it read like a broken upgrade.
const lsclusters17 = "17  main    5432 online postgres /var/lib/postgresql/17/main /var/log/postgresql/postgresql-17-main.log\n"

func pgInstalledOn(names ...string) map[string]string {
	m := map[string]string{}
	for _, n := range names {
		m[n] = "1.pgdg24.04+1"
	}
	return m
}

func TestParsePgLsclusters(t *testing.T) {
	got := parsePgLsclusters([]byte(lsclusters17 +
		"19  main    5433 down   postgres /var/lib/postgresql/19/main /var/log/postgresql/postgresql-19-main.log\n" +
		"16  replica 5434 online,recovery postgres /srv/pg/16 /var/log/postgresql/postgresql-16-replica.log\n" +
		"garbage\n"))
	if len(got) != 3 || got[0] != (PgCluster{Version: "17", Name: "main", Port: 5432, Online: true,
		DataDir: "/var/lib/postgresql/17/main"}) || got[1].Online || !got[2].Online || got[2].Name != "replica" {
		t.Fatalf("clusters = %+v", got)
	}
}

func TestNewerClientIsSaidHarmless(t *testing.T) {
	cl := parsePgLsclusters([]byte(lsclusters17))
	nc := newerClient(cl, pgInstalledOn("postgresql-17", "postgresql-client-17", "postgresql-client-19", "postgresql-client"))
	if nc == nil || nc.Client != "19" || nc.Server != "17" || nc.ServerPsql != "/usr/lib/postgresql/17/bin/psql" {
		t.Fatalf("newer client = %+v", nc)
	}
	if nc := newerClient(cl, pgInstalledOn("postgresql-17", "postgresql-client-17")); nc != nil {
		t.Errorf("matching client called newer: %+v", nc)
	}
	// After the upgrade, 19 runs: nothing to say.
	both := parsePgLsclusters([]byte(lsclusters17 + "19 main 5433 online postgres /x /y\n"))
	if nc := newerClient(both, pgInstalledOn("postgresql-client-19")); nc != nil {
		t.Errorf("client of a running cluster called newer: %+v", nc)
	}
}

func TestOldClusterAfterAnUpgrade(t *testing.T) {
	cl := parsePgLsclusters([]byte("17 main 5433 down postgres /a /b\n19 main 5432 online postgres /c /d\n"))
	o := oldCluster(cl, pgInstalledOn("postgresql-17", "postgresql-17-postgis-3", "postgresql-17-postgis-3-dbgsym",
		"postgresql-client-17", "postgresql-19"))
	if o == nil || o.Expect() != "17/main" || o.Current != "19" || o.Port != 5433 ||
		strings.Join(o.Packages, ",") != "postgresql-17,postgresql-17-postgis-3" {
		t.Fatalf("old = %+v", o)
	}
	// Another 17 cluster still uses the packages: they stay.
	cl = append(cl, PgCluster{Version: "17", Name: "other", Port: 5434, Online: true})
	if o := oldCluster(cl, pgInstalledOn("postgresql-17")); o == nil || len(o.Packages) != 0 {
		t.Fatalf("old with a sibling = %+v", o)
	}
	// Both running is not "left behind".
	cl = parsePgLsclusters([]byte("17 main 5433 online postgres /a /b\n19 main 5432 online postgres /c /d\n"))
	if o := oldCluster(cl, nil); o != nil {
		t.Errorf("running cluster called old: %+v", o)
	}
}

// pgHost is a server with PostgreSQL 17 and apt offering 19.
func pgHost(t *testing.T, size string, extra map[string]func(io.Writer, []string) int) *scriptRun {
	h := newReleaseHost(t, resoluteOS)
	h.free = 50 << 30
	oldFree, oldDir := pgFree, pgBackupDir
	pgFree = func(string) int64 { return h.free }
	pgBackupDir = filepath.Join(h.dir, "pgbackups")
	t.Cleanup(func() { pgFree, pgBackupDir = oldFree, oldDir })
	on := map[string]func(io.Writer, []string) int{
		"pg_lsclusters --no-header": func(w io.Writer, _ []string) int { fmt.Fprint(w, lsclusters17); return 0 },
		"apt-cache pkgnames postgresql-": func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "postgresql-16\npostgresql-17\npostgresql-19\npostgresql-client-19\npostgresql-18")
			return 0
		},
		"apt-cache policy postgresql-19": func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "postgresql-19:\n  Installed: (none)\n  Candidate: 19.0-2.pgdg24.04+1")
			return 0
		},
		"apt-cache policy postgresql-19-postgis-3": func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "postgresql-19-postgis-3:\n  Installed: (none)\n  Candidate: 3.6.0+dfsg-1.pgdg24.04+1")
			return 0
		},
		"apt-cache policy postgresql-19-mystery": func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "postgresql-19-mystery:\n  Installed: (none)\n  Candidate: (none)")
			return 0
		},
		"runuser -u postgres -- psql -X -A -t -q -p 5432": func(w io.Writer, _ []string) int {
			fmt.Fprint(w, size+"|f|0\n")
			return 0
		},
	}
	for k, v := range extra {
		on[k] = v
	}
	return &scriptRun{on: on}
}

func pgChecker(sr *scriptRun) PackageChecker {
	return PackageChecker{Run: sr.run, Look: looks("apt-get", "dpkg-query", "pg_lsclusters")}
}

func TestPostgresUpgradeOffered(t *testing.T) {
	sr := pgHost(t, "1048576", nil)
	pu := pgChecker(sr).postgres(context.Background(), pgInstalledOn("postgresql-17", "postgresql-17-postgis-3",
		"postgresql-client-17", "postgresql-client-19"))
	if pu == nil || pu.NewerClient == nil || pu.Upgrade == nil {
		t.Fatalf("postgres = %+v", pu)
	}
	u := pu.Upgrade
	if u.Expect() != "17/main:19" || u.Method != "dump" || u.DropEmpty || len(u.Blockers) != 0 ||
		strings.Join(u.Packages, ",") != "postgresql-19,postgresql-19-postgis-3" {
		t.Fatalf("upgrade = %+v", u)
	}
}

func TestPostgresUpgradeBlockers(t *testing.T) {
	sr := pgHost(t, fmt.Sprint(int64(30)<<30), map[string]func(io.Writer, []string) int{
		"runuser -u postgres -- psql -X -A -t -q -p 5432": func(w io.Writer, _ []string) int {
			fmt.Fprint(w, fmt.Sprint(int64(30)<<30)+"|f|2\n")
			return 0
		},
	})
	if err := os.WriteFile(argusControlBinary, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	u := pgChecker(sr).postgres(context.Background(), pgInstalledOn("postgresql-17", "postgresql-17-mystery")).Upgrade
	if u == nil || u.Method != "upgrade" {
		t.Fatalf("upgrade = %+v", u)
	}
	all := strings.Join(u.Blockers, "\n")
	for _, want := range []string{"apt has no postgresql-19-mystery", "2 standby server(s)", "needs about 61.00 GB free",
		"argus-control runs on this server"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in %s", want, all)
		}
	}
}

// The package for 19 makes an empty 19 main on 5433: Argus drops it only
// when it is empty.
func TestPostgresUpgradeEmptyNewCluster(t *testing.T) {
	two := lsclusters17 + "19 main 5433 online postgres /var/lib/postgresql/19/main /l\n"
	for _, tc := range []struct {
		count string
		drop  bool
	}{{"0", true}, {"3", false}} {
		sr := pgHost(t, "1024", map[string]func(io.Writer, []string) int{
			"pg_lsclusters --no-header": func(w io.Writer, _ []string) int { fmt.Fprint(w, two); return 0 },
			"runuser -u postgres -- psql -X -A -t -q -p 5433": func(w io.Writer, _ []string) int {
				fmt.Fprintln(w, tc.count)
				return 0
			},
		})
		u := pgChecker(sr).postgres(context.Background(), pgInstalledOn("postgresql-17", "postgresql-19")).Upgrade
		if u == nil || u.DropEmpty != tc.drop || (len(u.Blockers) == 0) == !tc.drop {
			t.Errorf("count %s: upgrade = %+v", tc.count, u)
		}
	}
}

func pgApplier(t *testing.T, sr *scriptRun, mode, expect string) *PackageApplier {
	a, _ := releaseApplier(t, sr, looks("apt-get", "dpkg-query", "pg_lsclusters"), expect)
	a.Plan.Mode = mode
	return a
}

func TestApplyPostgresUpgrade(t *testing.T) {
	upgraded := false
	listing := func(w io.Writer, _ []string) int {
		if upgraded {
			fmt.Fprint(w, "17 main 5433 down postgres /a /b\n19 main 5432 online postgres /c /d\n")
		} else {
			fmt.Fprint(w, lsclusters17)
		}
		return 0
	}
	sr := pgHost(t, "1024", map[string]func(io.Writer, []string) int{
		"pg_lsclusters --no-header": listing,
		"dpkg-query -W -f ${Package}\t${db:Status-Abbrev}\t${Version}\n postgresql*": func(w io.Writer, _ []string) int {
			fmt.Fprint(w, "postgresql-17\tii \t17.12-1.pgdg24.04+1\npostgresql-client-19\tii \t19.0-2.pgdg24.04+1\n")
			return 0
		},
		"pg_upgradecluster": func(io.Writer, []string) int { upgraded = true; return 0 },
		"runuser -u postgres -- /usr/lib/postgresql/19/bin/psql": func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "PostgreSQL 19.0 (Ubuntu 19.0-2.pgdg24.04+1) on x86_64-pc-linux-gnu")
			return 0
		},
	})
	st := pgApplier(t, sr, PackagesPostgresUpgrade, "17/main:19").Apply(context.Background())
	if st.State != RunDone || !strings.Contains(st.Message, "PostgreSQL main is now 19 on port 5432 (PostgreSQL 19.0 "+
		"(Ubuntu 19.0-2.pgdg24.04+1))") || !strings.Contains(st.Message, "stopped, on port 5433") {
		t.Fatalf("run = %+v\n%q", st, sr.calls)
	}
	order := []string{"apt-get -o DPkg::Lock::Timeout=120 -q update", "install -d -m 0700 -o postgres",
		"runuser -u postgres -- /usr/lib/postgresql/17/bin/pg_dumpall -p 5432 -f " + pgBackupDir + "/argus-17-main-",
		"apt-get install -y", "pg_upgradecluster -v 19 -m dump 17 main",
		"runuser -u postgres -- /usr/lib/postgresql/19/bin/vacuumdb -p 5432 --all --analyze-in-stages"}
	i := 0
	for _, c := range sr.calls {
		if i < len(order) && strings.HasPrefix(c, order[i]) {
			i++
		}
	}
	if i != len(order) {
		t.Errorf("steps out of order or missing at %q: %q", order[min(i, len(order)-1)], sr.calls)
	}
	if sr.ran("pg_dropcluster") {
		t.Error("dropped a cluster that was not there")
	}
}

func TestApplyPostgresUpgradeRefusesAnotherVersion(t *testing.T) {
	sr := pgHost(t, "1024", nil)
	st := pgApplier(t, sr, PackagesPostgresUpgrade, "17/main:18").Apply(context.Background())
	if st.State != RunFailed || st.Outcome == nil || st.Outcome.Changed != ChangedNo ||
		sr.ran("runuser -u postgres -- /usr/lib/postgresql/17/bin/pg_dumpall") || sr.ran("pg_upgradecluster") {
		t.Fatalf("run = %+v\n%q", st, sr.calls)
	}
}

func TestApplyPostgresDropOld(t *testing.T) {
	sr := pgHost(t, "1024", map[string]func(io.Writer, []string) int{
		"pg_lsclusters --no-header": func(w io.Writer, _ []string) int {
			fmt.Fprint(w, "17 main 5433 down postgres /a /b\n19 main 5432 online postgres /c /d\n")
			return 0
		},
		"dpkg-query -W -f ${Package}\t${db:Status-Abbrev}\t${Version}\n postgresql*": func(w io.Writer, _ []string) int {
			fmt.Fprint(w, "postgresql-17\tii \t17.12\npostgresql-19\tii \t19.0\n")
			return 0
		},
		"apt-get -s remove": func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "Remv postgresql-17 [17.12]")
			return 0
		},
	})
	st := pgApplier(t, sr, PackagesPostgresDropOld, "17/main").Apply(context.Background())
	if st.State != RunDone || !sr.ran("pg_dropcluster 17 main") || !sr.ran("apt-get remove -y") ||
		!strings.Contains(st.Message, "Removed postgresql-17") {
		t.Fatalf("run = %+v\n%q", st, sr.calls)
	}
	// Asked for the running one: nothing happens.
	st = pgApplier(t, sr, PackagesPostgresDropOld, "19/main").Apply(context.Background())
	if st.State != RunFailed || st.Outcome.Changed != ChangedNo || sr.ran("pg_dropcluster 19") {
		t.Fatalf("run = %+v", st)
	}
}

/* ── apt's sources ───────────────────────────────────────────────────────── */

// What the server had after its upgrade to 26.04.
const pgdgSourcesOff = `# disabled on upgrade to resolute
Types: deb
URIs: https://apt.postgresql.org/pub/repos/apt
Suites: noble-pgdg
Components: main
Signed-By: /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc
Enabled: no
`

func writeSources(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(aptSourcesRoot, "sources.list.d")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestAptSourcesFix(t *testing.T) {
	newReleaseHost(t, resoluteOS)
	writeSources(t, map[string]string{
		"pgdg.sources": pgdgSourcesOff, "pgdg.list.migrate": "deb x", "pgdg.list.distUpgrade": "deb y",
		"pgdg.sources.distUpgrade": "z", "lonely.list.migrate": "deb q", "ubuntu.sources": "Types: deb\n",
		"thing.list.save": "s",
	})
	f := aptSourcesFix(osRelease(), true)
	if f == nil || f.PGDG == nil || !f.PGDG.Disabled || f.PGDG.Want != "resolute-pgdg" ||
		f.PGDG.File != "sources.list.d/pgdg.sources" || len(f.Fingerprint) != 64 {
		t.Fatalf("fix = %+v", f)
	}
	got := map[string]AptLeftover{}
	for _, l := range f.Leftovers {
		got[l.Name] = l
	}
	if !got["pgdg.list.migrate"].Movable || !got["pgdg.list.migrate"].Noticed || !got["pgdg.list.distUpgrade"].Movable ||
		got["pgdg.list.distUpgrade"].Noticed || got["lonely.list.migrate"].Movable || got["thing.list.save"].Movable ||
		len(got) != 5 {
		t.Fatalf("leftovers = %+v", f.Leftovers)
	}
	// Nothing came from PGDG: its source is left alone.
	if f := aptSourcesFix(osRelease(), false); f == nil || f.PGDG != nil {
		t.Errorf("fix without pgdg packages = %+v", f)
	}
}

func TestAptSourcesFixQuietWhenRight(t *testing.T) {
	newReleaseHost(t, resoluteOS)
	writeSources(t, map[string]string{
		"pgdg.sources": strings.ReplaceAll(strings.ReplaceAll(pgdgSourcesOff, "Enabled: no\n", ""), "noble", "resolute"),
		// Silent leftovers alone are not worth a card.
		"pgdg.sources.distUpgrade": "z",
	})
	if f := aptSourcesFix(osRelease(), true); f != nil {
		t.Errorf("fix = %+v", f)
	}
}

func TestFixPGDG(t *testing.T) {
	got := string(fixPGDGDeb822([]byte("Types: deb\nURIs: http://deb.debian.org/debian\nSuites: noble\n\n"+pgdgSourcesOff),
		"resolute"))
	if !strings.Contains(got, "Suites: resolute-pgdg\n") || strings.Contains(got, "Enabled") ||
		!strings.Contains(got, "Suites: noble\n") {
		t.Errorf("deb822 = %q", got)
	}
	list := "deb http://archive.ubuntu.com/ubuntu noble main\n" +
		"# deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt noble-pgdg main # disabled on upgrade to resolute\n" +
		disabledMark + "resolute: deb http://apt.postgresql.org/pub/repos/apt noble-pgdg-testing main 19\n"
	got = string(fixPGDGOneLine([]byte(list), "resolute"))
	want := "deb http://archive.ubuntu.com/ubuntu noble main\n" +
		"deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt resolute-pgdg main\n" +
		"deb http://apt.postgresql.org/pub/repos/apt resolute-pgdg-testing main 19\n"
	if got != want {
		t.Errorf("list =\n%s\nwant\n%s", got, want)
	}
}

func aptFixApplier(t *testing.T, sr *scriptRun, expect string) *PackageApplier {
	old := aptBackupDir
	aptBackupDir = filepath.Join(t.TempDir(), "kept")
	t.Cleanup(func() { aptBackupDir = old })
	sr.on["dpkg-query -W -f ${Package}\t${db:Status-Abbrev}\t${Version}\n postgresql*"] = func(w io.Writer, _ []string) int {
		fmt.Fprint(w, "postgresql-17\tii \t17.12-1.pgdg24.04+1\n")
		return 0
	}
	return pgApplier(t, sr, PackagesAptSourcesFix, expect)
}

func TestApplyAptSourcesFix(t *testing.T) {
	newReleaseHost(t, resoluteOS)
	dir := writeSources(t, map[string]string{"pgdg.sources": pgdgSourcesOff, "pgdg.list.migrate": "deb x"})
	sr := &scriptRun{on: map[string]func(io.Writer, []string) int{}}
	a := aptFixApplier(t, sr, "")
	f := aptSourcesFix(osRelease(), true)
	a.Plan.Expect = f.Fingerprint
	st := a.Apply(context.Background())
	if st.State != RunDone || !strings.Contains(st.Message, "Moved pgdg.list.migrate aside") ||
		!strings.Contains(st.Message, "resolute-pgdg") {
		t.Fatalf("run = %+v", st)
	}
	if _, err := os.Stat(filepath.Join(dir, "pgdg.list.migrate")); !os.IsNotExist(err) {
		t.Error("leftover still there")
	}
	b, _ := os.ReadFile(filepath.Join(dir, "pgdg.sources"))
	if !strings.Contains(string(b), "Suites: resolute-pgdg") || strings.Contains(string(b), "Enabled") {
		t.Errorf("pgdg.sources = %s", b)
	}
	if m, _ := filepath.Glob(filepath.Join(aptBackupDir, "*", "*")); len(m) != 2 {
		t.Errorf("kept = %v", m)
	}
}

// apt.postgresql.org does not serve the new release yet: the file goes back.
func TestApplyAptSourcesFixPutsPGDGBack(t *testing.T) {
	newReleaseHost(t, resoluteOS)
	dir := writeSources(t, map[string]string{"pgdg.sources": pgdgSourcesOff})
	sr := &scriptRun{on: map[string]func(io.Writer, []string) int{
		"apt-get -o DPkg::Lock::Timeout=120 -q update": func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "E: The repository 'https://apt.postgresql.org/pub/repos/apt resolute-pgdg Release' does not have a Release file.")
			return 100
		},
	}}
	a := aptFixApplier(t, sr, "")
	a.Plan.Expect = aptSourcesFix(osRelease(), true).Fingerprint
	st := a.Apply(context.Background())
	b, _ := os.ReadFile(filepath.Join(dir, "pgdg.sources"))
	if st.State != RunFailed || st.Outcome.Changed != ChangedNo || string(b) != pgdgSourcesOff {
		t.Fatalf("run = %+v\n%s", st, b)
	}
}

func TestApplyAptSourcesFixRefusesAStaleView(t *testing.T) {
	newReleaseHost(t, resoluteOS)
	dir := writeSources(t, map[string]string{"pgdg.sources": pgdgSourcesOff, "pgdg.list.migrate": "deb x"})
	sr := &scriptRun{on: map[string]func(io.Writer, []string) int{}}
	st := aptFixApplier(t, sr, strings.Repeat("0", 64)).Apply(context.Background())
	if _, err := os.Stat(filepath.Join(dir, "pgdg.list.migrate")); st.State != RunFailed || err != nil {
		t.Fatalf("run = %+v, leftover: %v", st, err)
	}
}

func TestValidPostgresExpect(t *testing.T) {
	for mode, ok := range map[string][]string{
		PackagesPostgresUpgrade: {"17/main:19", "9/my_cluster-1:19"},
		PackagesPostgresDropOld: {"17/main"},
		PackagesAptSourcesFix:   {strings.Repeat("a", 64)},
	} {
		for _, e := range ok {
			if !validPostgresExpect(mode, e) {
				t.Errorf("%s %q refused", mode, e)
			}
		}
	}
	for _, bad := range []string{"", "17/main", "17/main:19; rm -rf /", "17/../x:19", "x/main:19"} {
		if validPostgresExpect(PackagesPostgresUpgrade, bad) {
			t.Errorf("upgrade %q accepted", bad)
		}
	}
	if validPostgresExpect(PackagesPostgresDropOld, "17/main --stop") {
		t.Error("drop accepted options")
	}
}
