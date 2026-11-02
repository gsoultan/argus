package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeUpdates stands in for the host: it names files, and records the plan
// it was asked to launch instead of restarting anything.
type fakeUpdates struct {
	reason   string
	mu       sync.Mutex
	launched []string
}

func (f *fakeUpdates) check() string  { return f.reason }
func (f *fakeUpdates) format() string { return "deb" }
func (f *fakeUpdates) fileName(v string) string {
	return "argus-agent_" + v + "_linux_amd64.deb"
}
func (f *fakeUpdates) launch(_ context.Context, plan string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launched = append(f.launched, plan)
	return nil
}
func (f *fakeUpdates) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.launched)
}

// releases serves packages and their checksums the way a release does.
func releases(t *testing.T, packages map[string]string, tamper bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	byVersion := map[string][]string{}
	for path := range packages {
		v, _, _ := strings.Cut(path, "/")
		byVersion[v] = append(byVersion[v], path)
	}
	for v, paths := range byVersion {
		var sums strings.Builder
		for _, p := range paths {
			sum := sha256.Sum256([]byte(packages[p]))
			_, name, _ := strings.Cut(p, "/")
			fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(sum[:]), name)
		}
		body := sums.String()
		mux.HandleFunc("/"+v+"/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(body))
		})
	}
	for p, content := range packages {
		content := content
		if tamper {
			content += "x"
		}
		mux.HandleFunc("/"+p, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(content))
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func testUpdater(t *testing.T, platform updatePlatform, busy *atomic.Int64) *Updater {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "argus-agent")
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	u := NewUpdater(dir, "1.0.0", exe, nil)
	u.platform = platform
	u.Marker = filepath.Join(dir, UpdateMarkerName)
	u.Busy = func() int { return int(busy.Load()) }
	return u
}

func waitState(t *testing.T, u *Updater, state string) UpdateStatus {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s := u.Status(); s.State == state {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("never reached %s; status is %+v", state, u.Status())
	return UpdateStatus{}
}

// The update waits for what is open on the server to end before it restarts,
// and holds new sessions only once it is about to.
func TestAnUpdateWaitsUntilNothingIsOpen(t *testing.T) {
	srv := releases(t, map[string]string{
		"v1.1.0/argus-agent_1.1.0_linux_amd64.deb": "new package",
		"v1.0.0/argus-agent_1.0.0_linux_amd64.deb": "old package",
	}, false)
	var busy atomic.Int64
	busy.Store(2)
	plat := &fakeUpdates{}
	u := testUpdater(t, plat, &busy)
	var held atomic.Bool
	u.Hold = func(on bool) { held.Store(on) }

	u.Handle(context.Background(), &UpdateRequest{ID: "r1", Version: "1.1.0", URL: srv.URL})
	s := waitState(t, u, UpdateWaiting)
	if s.Sessions != 2 || !strings.Contains(s.Message, "waiting for 2 open session(s)") {
		t.Errorf("waiting status = %+v", s)
	}
	time.Sleep(3 * busyPoll / 2)
	if plat.count() != 0 || held.Load() {
		t.Fatal("the agent restarted, or held new sessions, while two were open")
	}

	busy.Store(0)
	waitState(t, u, UpdateRestarting)
	if plat.count() != 1 {
		t.Fatalf("launched %d times", plat.count())
	}
	if !held.Load() {
		t.Error("new sessions were not held for the restart")
	}
	if _, err := os.Stat(u.Marker); err != nil {
		t.Error("no marker telling logins to wait for the restart")
	}
	plan, err := os.ReadFile(plat.launched[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"target": "1.1.0"`, `"from": "1.0.0"`, `argus-agent_1.1.0_linux_amd64.deb`,
		`argus-agent_1.0.0_linux_amd64.deb`, `argus-agent.previous`} {
		if !strings.Contains(string(plan), want) {
			t.Errorf("plan lacks %s:\n%s", want, plan)
		}
	}

	// The status survives the restart: a new agent reads it from disk.
	again := NewUpdater(u.StateDir, "1.1.0", u.Executable, nil)
	again.platform = plat
	if s := again.Status(); s.State != UpdateRestarting || s.ID != "r1" {
		t.Errorf("after a restart the status is %+v", s)
	}
	// And the same request does not start a second update.
	again.Handle(context.Background(), &UpdateRequest{ID: "r1", Version: "1.1.0", URL: srv.URL})
	time.Sleep(100 * time.Millisecond)
	if plat.count() != 1 {
		t.Error("the request was carried out twice")
	}
}

// "Now" does not wait.
func TestAnUpdateAskedNowDoesNotWait(t *testing.T) {
	srv := releases(t, map[string]string{"v1.1.0/argus-agent_1.1.0_linux_amd64.deb": "new"}, false)
	var busy atomic.Int64
	busy.Store(3)
	plat := &fakeUpdates{}
	u := testUpdater(t, plat, &busy)
	u.Handle(context.Background(), &UpdateRequest{ID: "r1", Version: "1.1.0", URL: srv.URL})
	waitState(t, u, UpdateWaiting)
	u.Handle(context.Background(), &UpdateRequest{ID: "r1", Version: "1.1.0", URL: srv.URL, Now: true})
	s := waitState(t, u, UpdateRestarting)
	if !strings.Contains(s.Message, "ending 3 open session(s)") {
		t.Errorf("a forced update should say what it ends: %s", s.Message)
	}
}

// A package that does not match its checksum changes nothing.
func TestAnUpdateWithABadChecksumChangesNothing(t *testing.T) {
	srv := releases(t, map[string]string{"v1.1.0/argus-agent_1.1.0_linux_amd64.deb": "new"}, true)
	var busy atomic.Int64
	plat := &fakeUpdates{}
	u := testUpdater(t, plat, &busy)
	u.Handle(context.Background(), &UpdateRequest{ID: "r1", Version: "1.1.0", URL: srv.URL})
	s := waitState(t, u, UpdateFailed)
	if !strings.Contains(s.Message, "does not match its checksum") || plat.count() != 0 {
		t.Errorf("status = %+v, launched %d", s, plat.count())
	}
}

// Taken back while waiting, it stops; a request for the running version is
// answered at once; a host that cannot update says why.
func TestAnUpdateCanBeTakenBackAndSaysWhyNot(t *testing.T) {
	srv := releases(t, map[string]string{"v1.1.0/argus-agent_1.1.0_linux_amd64.deb": "new"}, false)
	var busy atomic.Int64
	busy.Store(1)
	plat := &fakeUpdates{}
	u := testUpdater(t, plat, &busy)
	u.Handle(context.Background(), &UpdateRequest{ID: "r1", Version: "1.1.0", URL: srv.URL})
	waitState(t, u, UpdateWaiting)
	u.Handle(context.Background(), nil)
	waitState(t, u, UpdateCancelled)
	if plat.count() != 0 {
		t.Error("a cancelled update restarted the agent")
	}

	u.Handle(context.Background(), &UpdateRequest{ID: "r2", Version: "1.0.0", URL: srv.URL})
	if s := u.Status(); s.State != UpdateDone || s.ID != "r2" {
		t.Errorf("the running version should be done at once: %+v", s)
	}

	plat.reason = "this server does not run systemd"
	if s := u.Status(); s.Supported || s.Reason != plat.reason {
		t.Errorf("an unsupported host should say why: %+v", s)
	}
	u.Handle(context.Background(), &UpdateRequest{ID: "r3", Version: "1.1.0", URL: srv.URL})
	if s := u.Status(); s.State != UpdateFailed || s.Message != plat.reason {
		t.Errorf("status = %+v", s)
	}
}

// A collector holding for a restart tells a login to wait, and the shim's
// openRecording waits for the agent that replaces it rather than refusing.
func TestALoginDuringAnUpdateRestartWaitsForTheNewAgent(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "argus-upd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "c.sock")

	old := NewCollector(filepath.Join(dir, "rec-old"), nil)
	go func() { _ = old.Listen(sock) }()
	waitSocket(t, sock)
	old.Hold(true)

	start := SessionStart{Type: "start", Principal: "ops", Hostname: "h", StartedAt: time.Now()}
	got := make(chan *recording, 1)
	go func() {
		rc, _ := openRecording(sock, start, restartWait, false)
		got <- rc
	}()

	// The old agent goes away after a moment, as a restart does, and a new
	// one comes up well after the five seconds a stopped agent gets.
	time.Sleep(300 * time.Millisecond)
	_ = old.CloseWithin(time.Second)
	time.Sleep(restartWait + time.Second)
	fresh := NewCollector(filepath.Join(dir, "rec-new"), nil)
	go func() { _ = fresh.Listen(sock) }()
	t.Cleanup(func() { _ = fresh.CloseWithin(time.Second) })

	select {
	case rc := <-got:
		if rc == nil {
			t.Fatal("the login was refused during the update instead of waiting")
		}
		if !rc.d.record() || rc.d.sessionID == "" {
			t.Errorf("the new agent should record it: %+v", rc.d)
		}
		_ = rc.conn.Close()
	case <-time.After(30 * time.Second):
		t.Fatal("the login never got through")
	}
}

// With no word of an update, a collector that is not there is reported down
// after a short wait, so break-glass is not held up by a stopped agent.
func TestALoginToAStoppedAgentIsNotHeldLong(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "argus-upd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	began := time.Now()
	rc, why := openRecording(filepath.Join(dir, "none.sock"), SessionStart{Type: "start"}, restartWait, false)
	if rc != nil || why == "" {
		t.Fatal("a missing agent recorded a login")
	}
	if took := time.Since(began); took > restartWait+2*time.Second {
		t.Errorf("a stopped agent held the login %s", took)
	}
}

func waitSocket(t *testing.T, path string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("collector never listened")
}
