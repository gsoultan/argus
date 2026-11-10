package controlupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHost stands in for dpkg and systemd: installing a package makes the
// "binary" that version, and restarting starts whatever is installed, which
// says it is ready unless it is one of the versions told to fail.
type fakeHost struct {
	t        *testing.T
	dir      string
	mu       sync.Mutex
	version  string
	broken   map[string]string // version -> how it fails: "crash" or "silent"
	restarts int
	crashing bool // systemd keeps restarting it
	running  bool
	calls    []string
	noBackup bool
	backupKO bool
	// backupLog is what the failed backup run printed, as the journal has it.
	backupLog string
}

func (f *fakeHost) run(name string, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	switch {
	case name == "dpkg":
		pkg := args[len(args)-1]
		b, err := os.ReadFile(pkg)
		if err != nil {
			return err
		}
		f.version = strings.TrimPrefix(string(b), "package ")
	case name == "restore":
		f.version = "restored"
	case call == "systemctl cat argus-backup.service":
		if f.noBackup {
			return errors.New("no such unit")
		}
	case call == "systemctl start argus-backup.service":
		if f.backupKO {
			return errors.New("exit status 1")
		}
	case len(args) > 0 && args[0] == "restart":
		f.running, f.crashing = true, false
		switch f.broken[f.version] {
		case "crash":
			f.crashing = true
			f.running = false
		case "silent":
		default:
			_ = WriteReady(f.dir, Ready{Version: f.version, At: time.Now().UTC(), Listening: true, Database: true})
		}
	case len(args) > 0 && args[0] == "is-active":
		if !f.running {
			return errors.New("inactive")
		}
	}
	return nil
}

func (f *fakeHost) output(name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasSuffix(name, "argus-control"):
		return []byte("argus-control " + f.version + "\n"), nil
	case name == "dpkg-query":
		return []byte("install ok installed"), nil
	case name == "systemctl" && len(args) > 0 && args[0] == "show" && f.backupLog != "":
		return []byte("3f2a9c\n"), nil
	case name == "journalctl" && len(args) > 0 && args[0] == "_SYSTEMD_INVOCATION_ID=3f2a9c":
		return []byte(f.backupLog), nil
	}
	return nil, errors.New("not here")
}

func (f *fakeHost) did(call string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == call {
			return true
		}
	}
	return false
}

// releases serves packages whose content names their version.
func releases(t *testing.T, versions ...string) *httptest.Server {
	files := map[string][]byte{}
	sums := map[string]string{}
	for _, v := range versions {
		name := FileName(v, "amd64", "deb")
		body := []byte("package " + v)
		s := sha256.Sum256(body)
		files["/v"+v+"/"+name] = body
		sums[v] += hex.EncodeToString(s[:]) + "  " + name + "\n"
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) == 3 && parts[2] == "checksums.txt" {
			if s, ok := sums[strings.TrimPrefix(parts[1], "v")]; ok {
				fmt.Fprint(w, s)
				return
			}
		}
		if b, ok := files[r.URL.Path]; ok {
			_, _ = w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newHelper(t *testing.T, host *fakeHost, url string) *Helper {
	dir := t.TempDir()
	host.dir = dir
	exe := filepath.Join(t.TempDir(), "argus-control")
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := &Helper{
		Dir: dir, Work: filepath.Join(t.TempDir(), "work"), ReleasesURL: url, Executable: exe,
		Run: host.run, Output: host.output, Arch: "amd64",
		LookPath: func(string) (string, error) { return "/usr/bin/pg_dump", nil },
		Restarts: func() int {
			host.mu.Lock()
			defer host.mu.Unlock()
			if host.crashing {
				host.restarts++
			}
			return host.restarts
		},
		Active:    func() bool { host.mu.Lock(); defer host.mu.Unlock(); return host.running },
		ReadyWait: 2 * time.Second, SteadyFor: 600 * time.Millisecond,
	}
	if err := WriteRequest(dir, Request{ID: "u1", Version: "0.9.0", RequestedBy: "ops@acme.co",
		At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestUpdateInstallsAndReportsDone(t *testing.T) {
	host := &fakeHost{t: t, version: "0.8.2"}
	h := newHelper(t, host, releases(t, "0.8.2", "0.9.0").URL)
	if got := h.Apply(context.Background()); got != StateDone {
		st, _ := ReadStatus(h.Dir)
		t.Fatalf("state %q: %s", got, st.Message)
	}
	st, _ := ReadStatus(h.Dir)
	if st.From != "0.8.2" || st.Target != "0.9.0" || st.RequestedBy != "ops@acme.co" {
		t.Fatalf("status %+v", st)
	}
	if !host.did("systemctl start argus-backup.service") {
		t.Fatal("no backup was taken before installing")
	}
	var states []string
	for _, s := range st.Steps {
		states = append(states, s.State)
	}
	want := "downloading downloading backing_up backing_up installing restarting done"
	if strings.Join(states, " ") != want {
		t.Fatalf("steps %v, want %s", states, want)
	}
	if _, err := os.Stat(h.Work); !os.IsNotExist(err) {
		t.Fatal("the packages were left behind")
	}
}

func TestUpdateRunsOnce(t *testing.T) {
	host := &fakeHost{t: t, version: "0.8.2"}
	h := newHelper(t, host, releases(t, "0.8.2", "0.9.0").URL)
	h.Apply(context.Background())
	host.calls = nil
	if got := h.Apply(context.Background()); got != StateDone {
		t.Fatalf("second run: %q", got)
	}
	if host.did("dpkg -i --force-confold " + filepath.Join(h.Work, "new", FileName("0.9.0", "amd64", "deb"))) {
		t.Fatal("the same request was installed twice")
	}
}

func TestVersionThatCrashesIsRolledBack(t *testing.T) {
	host := &fakeHost{t: t, version: "0.8.2", broken: map[string]string{"0.9.0": "crash"}}
	h := newHelper(t, host, releases(t, "0.8.2", "0.9.0").URL)
	if got := h.Apply(context.Background()); got != StateRolledBack {
		st, _ := ReadStatus(h.Dir)
		t.Fatalf("state %q: %s", got, st.Message)
	}
	if host.version != "0.8.2" {
		t.Fatalf("running %s after the rollback", host.version)
	}
	st, _ := ReadStatus(h.Dir)
	if !strings.Contains(st.Message, "exited 3 times") || !strings.Contains(st.Message, "went back to 0.8.2") {
		t.Fatalf("message %q", st.Message)
	}
}

func TestVersionThatNeverAnswersIsRolledBack(t *testing.T) {
	host := &fakeHost{t: t, version: "0.8.2", broken: map[string]string{"0.9.0": "silent"}}
	h := newHelper(t, host, releases(t, "0.8.2", "0.9.0").URL)
	if got := h.Apply(context.Background()); got != StateRolledBack {
		t.Fatalf("state %q", got)
	}
}

// Going back to a version from before ready.json existed: it never writes
// one, so staying up counts.
func TestRollbackToAnOlderVersionThatWritesNoReady(t *testing.T) {
	host := &fakeHost{t: t, version: "0.8.2", broken: map[string]string{"0.9.0": "crash", "0.8.2": "silent"}}
	h := newHelper(t, host, releases(t, "0.8.2", "0.9.0").URL)
	if got := h.Apply(context.Background()); got != StateRolledBack {
		st, _ := ReadStatus(h.Dir)
		t.Fatalf("state %q: %s", got, st.Message)
	}
}

func TestNoRollbackPackageRestoresTheBinary(t *testing.T) {
	host := &fakeHost{t: t, version: "0.8.2", broken: map[string]string{"0.9.0": "crash", "restored": "silent"}}
	h := newHelper(t, host, releases(t, "0.9.0").URL)
	if got := h.Apply(context.Background()); got != StateRolledBack {
		st, _ := ReadStatus(h.Dir)
		t.Fatalf("state %q: %s", got, st.Message)
	}
	if !host.did("restore " + filepath.Join(h.Work, "argus-control.previous") + " " + h.Executable) {
		t.Fatalf("binary not restored: %v", host.calls)
	}
}

func TestBadChecksumChangesNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "checksums.txt") {
			fmt.Fprintf(w, "%s  %s\n", strings.Repeat("ab", 32), FileName("0.9.0", "amd64", "deb"))
			return
		}
		fmt.Fprint(w, "package 0.9.0")
	}))
	defer srv.Close()
	host := &fakeHost{t: t, version: "0.8.2"}
	h := newHelper(t, host, srv.URL)
	if got := h.Apply(context.Background()); got != StateFailed {
		t.Fatalf("state %q", got)
	}
	st, _ := ReadStatus(h.Dir)
	if !strings.Contains(st.Message, "does not match its checksum") || !strings.Contains(st.Message, "nothing was changed") {
		t.Fatalf("message %q", st.Message)
	}
	for _, c := range host.calls {
		if strings.HasPrefix(c, "dpkg") || strings.Contains(c, "restart") {
			t.Fatalf("changed something: %s", c)
		}
	}
}

func TestFailedBackupChangesNothing(t *testing.T) {
	host := &fakeHost{t: t, version: "0.8.2", backupKO: true}
	h := newHelper(t, host, releases(t, "0.8.2", "0.9.0").URL)
	if got := h.Apply(context.Background()); got != StateFailed {
		t.Fatalf("state %q", got)
	}
	for _, c := range host.calls {
		if strings.HasPrefix(c, "dpkg") {
			t.Fatalf("installed after a failed backup: %s", c)
		}
	}
}

// The console shows why the backup failed, not systemd's "see journalctl".
func TestFailedBackupSaysWhy(t *testing.T) {
	host := &fakeHost{t: t, version: "0.8.2", backupKO: true, backupLog: "" +
		"pg_dump: error: aborting because of server version mismatch\n" +
		"argus-control: pg_dump: exit status 1\n"}
	h := newHelper(t, host, releases(t, "0.8.2", "0.9.0").URL)
	h.Apply(context.Background())
	st, _ := ReadStatus(h.Dir)
	if !strings.Contains(st.Message, "nothing was changed: pg_dump: exit status 1") {
		t.Fatalf("message %q", st.Message)
	}

	// Without a journal to read, systemctl's own error is all there is.
	host = &fakeHost{t: t, version: "0.8.2", backupKO: true}
	h = newHelper(t, host, releases(t, "0.8.2", "0.9.0").URL)
	h.Apply(context.Background())
	if st, _ := ReadStatus(h.Dir); !strings.Contains(st.Message, "exit status 1") {
		t.Fatalf("message %q", st.Message)
	}
}

func TestLastError(t *testing.T) {
	for log, want := range map[string]string{
		"backing up to /x\nargus-control: pg_dump is not installed\n": "pg_dump is not installed",
		"pg_dump: error: connection refused\n\n":                      "pg_dump: error: connection refused",
		"":                                                            "",
		"argus-control: verify audit chain: x\nsomething after\n": "verify audit chain: x",
	} {
		if got := lastError(log); got != want {
			t.Errorf("lastError(%q) = %q, want %q", log, got, want)
		}
	}
}

// A host without pg_dump gets the PostgreSQL client before the backup runs,
// instead of an update that stops until someone installs it by hand.
func TestMissingPgDumpIsInstalled(t *testing.T) {
	host := &fakeHost{t: t, version: "0.8.2"}
	h := newHelper(t, host, releases(t, "0.8.2", "0.9.0").URL)
	h.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	if got := h.Apply(context.Background()); got != StateDone {
		st, _ := ReadStatus(h.Dir)
		t.Fatalf("state %q: %s", got, st.Message)
	}
	install, backup := -1, -1
	for i, c := range host.calls {
		switch c {
		case "apt-get install -y --no-install-recommends postgresql-client":
			install = i
		case "systemctl start argus-backup.service":
			backup = i
		}
	}
	if install < 0 || backup < install {
		t.Fatalf("client not installed before the backup: %v", host.calls)
	}

	// With pg_dump already there, nothing is installed.
	host = &fakeHost{t: t, version: "0.8.2"}
	h = newHelper(t, host, releases(t, "0.8.2", "0.9.0").URL)
	h.LookPath = func(string) (string, error) { return "/usr/bin/pg_dump", nil }
	h.Apply(context.Background())
	for _, c := range host.calls {
		if strings.HasPrefix(c, "apt-get") {
			t.Fatalf("installed with pg_dump present: %s", c)
		}
	}
}

func TestEnabledUnitStaysEnabled(t *testing.T) {
	host := &fakeHost{t: t, version: "0.8.2"}
	h := newHelper(t, host, releases(t, "0.8.2", "0.9.0").URL)
	h.Apply(context.Background())
	if !host.did("systemctl enable argus-control") {
		t.Fatal("an enabled control plane was not left enabled")
	}
}

func TestRequestIsUntrusted(t *testing.T) {
	dir := t.TempDir()
	for _, v := range []string{"../../etc", "0.9.0/../../x", "0.9.0 ", "latest", ""} {
		if err := WriteRequest(dir, Request{ID: "x", Version: v}); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadRequest(dir); err == nil {
			t.Fatalf("accepted version %q", v)
		}
	}
	// A link where the request should be is not followed.
	secret := filepath.Join(t.TempDir(), "secret")
	_ = os.WriteFile(secret, []byte(`{"id":"x","version":"1.0.0"}`), 0o600)
	_ = os.Remove(filepath.Join(dir, RequestFile))
	if err := os.Symlink(secret, filepath.Join(dir, RequestFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRequest(dir); err == nil {
		t.Fatal("followed a link to the request")
	}
	// Nor is one where the status goes: the link is replaced, not written through.
	target := filepath.Join(t.TempDir(), "target")
	_ = os.WriteFile(target, []byte("keep"), 0o600)
	if err := os.Symlink(target, filepath.Join(dir, StatusFile)); err != nil {
		t.Fatal(err)
	}
	if err := WriteStatus(dir, Status{ID: "x", State: StateDone}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Fatalf("wrote through a link: %q", b)
	}
}
