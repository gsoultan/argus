package agent

import (
	"archive/zip"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeService stands in for the Windows service manager. Starting it "runs"
// whatever binary is at exe: start reads the file and hands its contents to
// boot, which plays that version coming up (or not).
type fakeService struct {
	t    *testing.T
	exe  string
	boot func(binary string)

	mu      sync.Mutex
	pid     uint32
	stops   int
	starts  int
	stopErr error
}

func (f *fakeService) Stop(time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	if f.stopErr != nil {
		return f.stopErr
	}
	f.pid = 0
	return nil
}

func (f *fakeService) Start() error {
	b, err := os.ReadFile(f.exe)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.starts++
	f.pid = uint32(1000 + f.starts)
	f.mu.Unlock()
	go f.boot(string(b))
	return nil
}

func (f *fakeService) Process() uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pid
}

func (f *fakeService) stopCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stops
}

func (f *fakeService) setPID(p uint32) {
	f.mu.Lock()
	f.pid = p
	f.mu.Unlock()
}

// serviceFixture is an installed agent at version 1.0.0, a downloaded
// 1.1.0 next to it, and a copy of the running binary, as prepare leaves
// them.
func serviceFixture(t *testing.T, needTunnel bool) (UpdatePlan, string) {
	t.Helper()
	root := t.TempDir()
	state := filepath.Join(root, "state")
	upd := filepath.Join(state, updateDir)
	if err := os.MkdirAll(upd, 0o700); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(root, "Program Files", "Argus", "argus-agent.exe")
	_ = os.MkdirAll(filepath.Dir(exe), 0o755)
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(exe, "1.0.0")
	write(filepath.Join(upd, "argus-agent.exe"), "1.1.0")
	write(filepath.Join(upd, "argus-agent.previous.exe"), "1.0.0")
	return UpdatePlan{
		ID: "u1", From: "1.0.0", Target: "1.1.0", Format: "zip",
		Package:    filepath.Join(upd, "argus-agent.exe"),
		Previous:   filepath.Join(upd, "argus-agent.previous.exe"),
		Executable: exe, Unit: "argus-agent", StateDir: state,
		Marker: filepath.Join(state, UpdateMarkerName), NeedTunnel: needTunnel,
	}, exe
}

// comesUp is a binary that says it is serving, as the agent does.
func comesUp(state string) func(string) {
	return func(version string) {
		time.Sleep(20 * time.Millisecond)
		WriteReady(state, version, true, true)
	}
}

func TestServiceUpdateReplacesTheBinaryAndRestarts(t *testing.T) {
	plan, exe := serviceFixture(t, true)
	svc := &fakeService{t: t, exe: exe, boot: comesUp(plan.StateDir)}
	a := &ServiceApplier{Plan: plan, Service: svc, ReadyWait: 3 * time.Second, Poll: 10 * time.Millisecond}

	if got := a.Apply(); got != UpdateDone {
		st, _ := ReadUpdateStatus(plan.StateDir)
		t.Fatalf("Apply = %s (%s), want done", got, st.Message)
	}
	if b, _ := os.ReadFile(exe); string(b) != "1.1.0" {
		t.Fatalf("the service's binary is %q, want 1.1.0", b)
	}
	if svc.stops != 1 || svc.starts != 1 {
		t.Fatalf("stopped %d and started %d times, want once each", svc.stops, svc.starts)
	}
	st, _ := ReadUpdateStatus(plan.StateDir)
	if st.State != UpdateDone || !strings.Contains(st.Message, "updated from 1.0.0 to 1.1.0") {
		t.Fatalf("update.json says %s: %s", st.State, st.Message)
	}
	for _, f := range []string{exe + ".old", exe + ".new", plan.Package, plan.Previous, plan.Marker} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("%s was left behind", filepath.Base(f))
		}
	}
}

// A version that comes up but never takes logins is replaced by the one
// that was running, and the server ends up where it started.
func TestServiceUpdateGoesBackWhenTheNewVersionDoesNotServe(t *testing.T) {
	plan, exe := serviceFixture(t, true)
	svc := &fakeService{t: t, exe: exe}
	svc.boot = func(v string) {
		if v == "1.1.0" {
			WriteReady(plan.StateDir, v, false, false) // up, not serving
			return
		}
		comesUp(plan.StateDir)(v)
	}
	a := &ServiceApplier{Plan: plan, Service: svc, ReadyWait: 1500 * time.Millisecond, Poll: 10 * time.Millisecond}

	if got := a.Apply(); got != UpdateRolledBack {
		st, _ := ReadUpdateStatus(plan.StateDir)
		t.Fatalf("Apply = %s (%s), want rolled_back", got, st.Message)
	}
	if b, _ := os.ReadFile(exe); string(b) != "1.0.0" {
		t.Fatalf("after going back the service's binary is %q, want 1.0.0", b)
	}
	st, _ := ReadUpdateStatus(plan.StateDir)
	if !strings.Contains(st.Message, "did not take logins") || !strings.Contains(st.Message, "went back to 1.0.0") {
		t.Fatalf("update.json says %q", st.Message)
	}
}

// A version that keeps exiting is given up on long before readyWait: the
// service manager restarting it shows as a new process each time.
func TestServiceUpdateGivesUpOnACrashLoop(t *testing.T) {
	plan, exe := serviceFixture(t, false)
	svc := &fakeService{t: t, exe: exe}
	svc.boot = func(v string) {
		if v != "1.1.0" {
			comesUp(plan.StateDir)(v)
			return
		}
		// Exited and restarted by the recovery actions, slower than the
		// helper looks, as the service manager's are.
		// It ends when the helper stops the service to go back.
		for i := 0; i < 6 && svc.stopCount() < 2; i++ {
			time.Sleep(600 * time.Millisecond)
			svc.setPID(uint32(5000 + i))
		}
	}
	a := &ServiceApplier{Plan: plan, Service: svc, ReadyWait: time.Minute, Poll: 5 * time.Millisecond}
	start := time.Now()
	if got := a.Apply(); got != UpdateRolledBack {
		t.Fatalf("Apply = %s, want rolled_back", got)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("took %s to give up on a crashing version", took)
	}
	st, _ := ReadUpdateStatus(plan.StateDir)
	if !strings.Contains(st.Message, "exited") {
		t.Fatalf("update.json says %q", st.Message)
	}
}

// When the new binary cannot be put in place nothing is stopped: the agent
// that asked is still serving, on the binary it had.
func TestServiceUpdateThatCannotPlaceTheBinaryStopsNothing(t *testing.T) {
	plan, exe := serviceFixture(t, false)
	plan.Package = filepath.Join(plan.StateDir, "missing.exe")
	svc := &fakeService{t: t, exe: exe, boot: comesUp(plan.StateDir)}
	a := &ServiceApplier{Plan: plan, Service: svc, ReadyWait: time.Second}

	if got := a.Apply(); got != UpdateFailed {
		t.Fatalf("Apply = %s, want failed", got)
	}
	if svc.stops != 0 || svc.starts != 0 {
		t.Fatalf("stopped %d and started %d times; nothing should have", svc.stops, svc.starts)
	}
	if b, _ := os.ReadFile(exe); string(b) != "1.0.0" {
		t.Fatalf("the service's binary is %q, want 1.0.0 untouched", b)
	}
	st, _ := ReadUpdateStatus(plan.StateDir)
	if !strings.Contains(st.Message, "nothing was changed") {
		t.Fatalf("update.json says %q", st.Message)
	}
}

func TestServiceRestartComesBackOnTheSameBinary(t *testing.T) {
	plan, exe := serviceFixture(t, true)
	plan.Restart, plan.Target, plan.Package, plan.Previous = true, "1.0.0", "", ""
	svc := &fakeService{t: t, exe: exe, boot: comesUp(plan.StateDir)}
	a := &ServiceApplier{Plan: plan, Service: svc, ReadyWait: 3 * time.Second, Poll: 10 * time.Millisecond}
	if got := a.Apply(); got != UpdateDone {
		t.Fatalf("Apply = %s, want done", got)
	}
	if svc.stops != 1 || svc.starts != 1 {
		t.Fatalf("stopped %d and started %d times, want once each", svc.stops, svc.starts)
	}

	// One that will not stop is a failure that says where to look.
	svc = &fakeService{t: t, exe: exe, boot: comesUp(plan.StateDir), stopErr: errors.New("access is denied")}
	a = &ServiceApplier{Plan: plan, Service: svc, ReadyWait: time.Second, HowToLook: "look here"}
	if got := a.Apply(); got != UpdateFailed {
		t.Fatalf("Apply = %s, want failed", got)
	}
	st, _ := ReadUpdateStatus(plan.StateDir)
	if !strings.Contains(st.Message, "access is denied") || !strings.Contains(st.Message, "look here") {
		t.Fatalf("update.json says %q", st.Message)
	}
}

func TestUnpackFromZipFindsTheAgent(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "argus_1.1.0_windows_amd64.zip")
	f, _ := os.Create(archive)
	zw := zip.NewWriter(f)
	for name, body := range map[string]string{
		"README.md": "readme", "argus-control.exe": "control", "argus-agent.exe": "agent 1.1.0",
	} {
		w, _ := zw.Create(name)
		_, _ = w.Write([]byte(body))
	}
	_ = zw.Close()
	_ = f.Close()

	out := filepath.Join(dir, "out")
	_ = os.MkdirAll(out, 0o700)
	got, err := unpackFromZip(archive, out, "argus-agent.exe")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(got); string(b) != "agent 1.1.0" {
		t.Fatalf("unpacked %q", b)
	}
	if _, err := unpackFromZip(archive, out, "argus-verify.exe"); err == nil ||
		!strings.Contains(err.Error(), "holds no") {
		t.Fatalf("an archive without the agent: %v", err)
	}
}
