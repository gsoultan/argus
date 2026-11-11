//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// logTree builds a fixture server under a temp dir: files with sizes and
// ages in days.
type logFixture struct {
	path string
	size int
	age  float64 // days
}

func logTree(t *testing.T, now time.Time, files []logFixture) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range files {
		p := root + f.path
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		line := []byte("a log line that says nothing\n")
		buf := make([]byte, 0, f.size)
		for len(buf) < f.size {
			buf = append(buf, line...)
		}
		if err := os.WriteFile(p, buf[:f.size], 0o644); err != nil {
			t.Fatal(err)
		}
		m := now.Add(-time.Duration(f.age * 24 * float64(time.Hour)))
		if err := os.Chtimes(p, m, m); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func scanFixture(root string, now time.Time, req LogScanRequest) LogReport {
	s := linuxLogScan{req: req, root: root, now: func() time.Time { return now },
		disks: func() []Disk { return []Disk{{Mount: "/", Total: 100 << 30, Used: 93 << 30}} }}
	return s.scan(context.Background())
}

func itemByKey(r LogReport, key string) *LogItem {
	for i := range r.Items {
		if r.Items[i].Key == key {
			return &r.Items[i]
		}
	}
	return nil
}

func TestLogScanFindsWhatCanBeCleanedAndWhatCannot(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	root := logTree(t, now, []logFixture{
		{"/var/log/syslog", 2000, 0},
		{"/var/log/syslog.1", 3000, 2},
		{"/var/log/syslog.2.gz", 4000, 20},
		{"/var/log/nginx/access.log", 9000, 0},
		{"/var/log/nginx/access.log.3.gz", 5000, 30},
		{"/var/log/auth.log", 700, 0},
		{"/var/log/auth.log.2.gz", 800, 30},
		{"/var/log/wtmp", 600, 1},
		{"/var/log/audit/audit.log.1", 900, 40},
		{"/var/log/journal/abc/system.journal", 6000, 1},
		{"/var/log/journal/abc/system@1.journal~", 6000, 50},
		{"/var/log/pgbackrest/archive.log.1", 1000, 30},
		{"/srv/billing/logs/app.log", 1500, 30},
		{"/srv/billing/logs/app.log.1", 1500, 40},
		{"/var/lib/docker/containers/0123456789abcdef/0123456789abcdef-json.log", minContainerLog + 1, 0},
		{"/var/lib/argus/recordings/x.cast", 1234, 0},
	})
	_ = os.WriteFile(root+"/var/lib/docker/containers/0123456789abcdef/config.v2.json", []byte(`{"Name":"/api"}`), 0o644)
	r := scanFixture(root, now, LogScanRequest{StateDir: "/var/lib/argus", ActiveOver: 8000,
		Paths: []string{"/srv/*/logs/*"}, Protect: []string{"/var/log/pgbackrest/*"}})

	rot := itemByKey(r, "rotated")
	if rot == nil || rot.Count != 3 || rot.Bytes != 3000+4000+5000 {
		t.Fatalf("rotated = %+v", rot)
	}
	if rot.Files[0].Path != "/var/log/nginx/access.log.3.gz" {
		t.Errorf("rotated files should be oldest first: %v", rot.Files)
	}
	for _, f := range rot.Files {
		if strings.Contains(f.Path, "auth.log") || strings.Contains(f.Path, "audit") || strings.Contains(f.Path, "pgbackrest") {
			t.Errorf("a protected file was offered: %s", f.Path)
		}
	}
	if a := itemByKey(r, "active:/var/log/nginx/access.log"); a == nil || a.Bytes != 9000 {
		t.Errorf("the big live log was not reported: %+v", a)
	}
	if itemByKey(r, "active:/var/log/syslog") != nil {
		t.Error("a small live log is not worth a line")
	}
	if j := itemByKey(r, "journal"); j == nil || j.Bytes != 12000 || j.Count != 2 {
		t.Errorf("journal = %+v", j)
	}
	if c := itemByKey(r, "container:0123456789abcdef"); c == nil || c.Title != "Container api" {
		t.Errorf("container = %+v", c)
	}
	if p := itemByKey(r, "path:/srv/*/logs/*"); p == nil || p.Count != 2 {
		t.Errorf("rule path = %+v", p)
	}
	got := map[string]int64{}
	for _, p := range r.Protected {
		got[p.Title] = p.Bytes
	}
	if got["Login records"] != 700+800+600 || got["Audit log"] != 900 || got["Argus"] != 1234 {
		t.Errorf("protected = %v", got)
	}
	if rot.Mount != "/" {
		t.Errorf("mount = %q", rot.Mount)
	}
}

func cleanerFor(t *testing.T, root string, now time.Time, req LogCleanRequest) *LogCleaner {
	t.Helper()
	state := t.TempDir()
	return &LogCleaner{Plan: LogPlan{LogCleanRequest: req, StateDir: state, Unit: "argus-logclean-test"},
		Now: func() time.Time { return now }, ops: linuxLogOps{stateDir: "/var/lib/argus", root: root}}
}

func TestLogCleanDeletesOnlyWhatIsStillAsScanned(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	root := logTree(t, now, []logFixture{
		{"/var/log/syslog.2.gz", 4000, 20},
		{"/var/log/syslog.3.gz", 4000, 25},
		{"/var/log/kern.log.1", 4000, 21},
		{"/var/log/recent.log.1", 4000, 2},
		{"/var/log/auth.log.2.gz", 800, 30},
		{"/etc/shadow", 100, 400},
		{"/srv/elsewhere/real.log.1", 100, 400},
	})
	scan := scanFixture(root, now, LogScanRequest{StateDir: "/var/lib/argus"})
	files := itemByKey(scan, "rotated").Files
	// What the console would send, plus what it must never get away with.
	files = append(files,
		LogFile{Path: "/var/log/auth.log.2.gz", Bytes: 800, Modified: now.Add(-30 * 24 * time.Hour)},
		LogFile{Path: "/etc/shadow", Bytes: 100, Modified: now.Add(-400 * 24 * time.Hour)},
		LogFile{Path: "/var/log/../../etc/shadow", Bytes: 100, Modified: now.Add(-400 * 24 * time.Hour)},
	)
	// kern.log.1 grew after the scan.
	if err := os.WriteFile(root+"/var/log/kern.log.1", make([]byte, 9000), 0o644); err != nil {
		t.Fatal(err)
	}
	// A symlinked folder inside /var/log that points somewhere else.
	if err := os.Symlink(root+"/srv/elsewhere", root+"/var/log/linked"); err != nil {
		t.Fatal(err)
	}
	files = append(files, LogFile{Path: "/var/log/linked/real.log.1", Bytes: 100, Modified: now.Add(-400 * 24 * time.Hour)})

	c := cleanerFor(t, root, now, LogCleanRequest{ID: "run-00000001", Actions: []LogAction{
		{Kind: LogActDelete, Item: "rotated", Files: files, KeepDays: 14},
	}})
	st := c.Clean(context.Background())
	if st.State != RunDone {
		t.Fatalf("state = %s: %s", st.State, st.Message)
	}
	deleted := map[string]bool{}
	for _, r := range st.Removed {
		deleted[r.Path] = true
	}
	if !deleted["/var/log/syslog.2.gz"] || !deleted["/var/log/syslog.3.gz"] || len(deleted) != 2 {
		t.Errorf("deleted = %v", deleted)
	}
	if st.Freed != 8000 {
		t.Errorf("freed = %d", st.Freed)
	}
	for _, keep := range []string{"/var/log/kern.log.1", "/var/log/recent.log.1", "/var/log/auth.log.2.gz",
		"/etc/shadow", "/srv/elsewhere/real.log.1"} {
		if _, err := os.Stat(root + keep); err != nil {
			t.Errorf("%s should still be there: %v", keep, err)
		}
	}
	why := map[string]string{}
	for _, s := range st.Skipped {
		why[s.Path] = s.Why
	}
	for p, want := range map[string]string{
		"/var/log/kern.log.1":        "changed since the scan",
		"/var/log/recent.log.1":      "newer than the rule keeps",
		"/var/log/auth.log.2.gz":     "evidence",
		"/etc/shadow":                "not where",
		"/var/log/../../etc/shadow":  "not a plain path",
		"/var/log/linked/real.log.1": "symlink",
	} {
		if !strings.Contains(why[p], want) {
			t.Errorf("%s: skipped because %q, want %q", p, why[p], want)
		}
	}
	// The state file the daemon follows says the same.
	var disk LogRunStatus
	b, _ := os.ReadFile(filepath.Join(c.Plan.StateDir, logsRunFile))
	if json.Unmarshal(b, &disk) != nil || disk.State != RunDone || disk.Freed != 8000 {
		t.Errorf("state file = %+v", disk)
	}
}

func TestLogCleanEmptiesALiveLogKeepingItsEnd(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	root := logTree(t, now, []logFixture{{"/var/log/nginx/access.log", 0, 0}})
	var big strings.Builder
	for i := 0; i < 2000; i++ {
		big.WriteString("GET /index.html 200 line number ")
		big.WriteString(strings.Repeat("x", i%7))
		big.WriteString("\n")
	}
	p := root + "/var/log/nginx/access.log"
	if err := os.WriteFile(p, []byte(big.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	size := int64(big.Len())
	c := cleanerFor(t, root, now, LogCleanRequest{ID: "run-00000002", Actions: []LogAction{
		{Kind: LogActTruncate, Item: "active:/var/log/nginx/access.log", Path: "/var/log/nginx/access.log",
			Bytes: size, KeepBytes: 1000},
		{Kind: LogActTruncate, Item: "active:/var/log/auth.log", Path: "/var/log/auth.log", Bytes: 10},
	}})
	st := c.Clean(context.Background())
	if st.State != RunDone || len(st.Removed) != 1 {
		t.Fatalf("st = %+v", st)
	}
	b, _ := os.ReadFile(p)
	if len(b) == 0 || len(b) > 1000 || !strings.HasPrefix(string(b), "GET ") || !strings.HasSuffix(string(b), "\n") {
		t.Errorf("kept %d bytes starting %q", len(b), string(b[:min(len(b), 20)]))
	}
	if st.Removed[0].After != int64(len(b)) || st.Freed != size-int64(len(b)) {
		t.Errorf("manifest = %+v freed %d", st.Removed[0], st.Freed)
	}
	if len(st.Skipped) != 1 || !strings.Contains(st.Skipped[0].Why, "evidence") {
		t.Errorf("skipped = %+v", st.Skipped)
	}
}

func TestLogCleanRulePathStaysInsideItsGlob(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	root := logTree(t, now, []logFixture{
		{"/srv/billing/logs/app.log.1", 1500, 40},
		{"/srv/billing/data/ledger.db", 1500, 40},
	})
	glob := "/srv/*/logs/*"
	files := []LogFile{
		{Path: "/srv/billing/logs/app.log.1", Bytes: 1500, Modified: now.Add(-40 * 24 * time.Hour)},
		{Path: "/srv/billing/data/ledger.db", Bytes: 1500, Modified: now.Add(-40 * 24 * time.Hour)},
	}
	c := cleanerFor(t, root, now, LogCleanRequest{ID: "run-00000003", Paths: []string{glob},
		Actions: []LogAction{{Kind: LogActDelete, Item: "path:" + glob, Files: files, KeepDays: 14}}})
	st := c.Clean(context.Background())
	if len(st.Removed) != 1 || st.Removed[0].Path != "/srv/billing/logs/app.log.1" {
		t.Errorf("removed = %+v", st.Removed)
	}
	if _, err := os.Stat(root + "/srv/billing/data/ledger.db"); err != nil {
		t.Error("a file outside the rule's path was deleted")
	}
	// A path no longer in the rule is not cleaned at all.
	c = cleanerFor(t, root, now, LogCleanRequest{ID: "run-00000004",
		Actions: []LogAction{{Kind: LogActDelete, Item: "path:/srv/*/data/*", Files: files[1:], KeepDays: 0}}})
	st = c.Clean(context.Background())
	if len(st.Removed) != 0 {
		t.Errorf("removed without a rule: %+v", st.Removed)
	}
}

func TestProtectedLinux(t *testing.T) {
	for p, want := range map[string]bool{
		"/var/log/auth.log":        true,
		"/var/log/auth.log.4.gz":   true,
		"/var/log/secure-20261101": true,
		"/var/log/wtmp.1":          true,
		"/var/log/audit/audit.log": true,
		"/var/log/sudo-io/00/01":   true,
		"/var/lib/argus/x":         true,
		// argus-control's own server: its data and backups are the evidence.
		"/var/lib/argus-control/recordings/a.log": true,
		"/var/backups/argus/argus-2026.log":       true,
		"/opt/argus-state/a":                      true,
		"/var/log/syslog.1":                       false,
		"/var/log/authority.log":                  false,
	} {
		got := protectedLinux(p, "/opt/argus-state", nil) != ""
		if p == "/var/log/authority.log" {
			// auth.log is a prefix of neither: authority.log does not start with it.
			got = protectedLinux(p, "", nil) != ""
		}
		if got != want {
			t.Errorf("%s: protected=%v, want %v", p, got, want)
		}
	}
	if protectedLinux("/var/log/pgbackrest/a.log", "", []string{"/var/log/pgbackrest"}) == "" {
		t.Error("an added folder protects what is in it")
	}
}

func TestUnitOf(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cgroup")
	_ = os.WriteFile(p, []byte("0::/system.slice/tomcat.service\n"), 0o644)
	if u := unitOf(p); u != "tomcat.service" {
		t.Errorf("unit = %q", u)
	}
	_ = os.WriteFile(p, []byte("0::/user.slice/user-1000.slice/session-3.scope\n"), 0o644)
	if u := unitOf(p); u != "" {
		t.Errorf("unit = %q", u)
	}
}
