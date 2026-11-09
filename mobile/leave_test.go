package mobile

import (
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/agent"
)

/*
Taking a device off Argus, from the app's Remove button or from the console:
the link is revoked and deleted, the evidence is not, and the app is told so
it can switch off the rest of what it set up.
*/

type removedApp struct {
	fakeApp
	mu        sync.Mutex
	calls     int
	byConsole bool
	summary   string
}

func (r *removedApp) Removed(byConsole bool, summary string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.byConsole, r.summary = byConsole, summary
}

// controlPlane answers deregister with status, and records what it was told.
type controlPlane struct {
	*httptest.Server
	mu       sync.Mutex
	status   int
	told     map[string]string
	reported int
}

func newControlPlane(t *testing.T, status int) *controlPlane {
	cp := &controlPlane{status: status}
	cp.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cp.mu.Lock()
		defer cp.mu.Unlock()
		if cp.status != http.StatusOK {
			http.Error(w, "down", cp.status)
			return
		}
		switch r.URL.Path {
		case "/api/v1/report/deregister":
			_ = json.NewDecoder(r.Body).Decode(&cp.told)
			_, _ = io.WriteString(w, `{"state":"revoked"}`)
		default:
			cp.reported++
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	t.Cleanup(cp.Close)
	return cp
}

// linked writes an enrolment to cp into a fresh state directory, trusting
// its certificate, with a recording already shipped and a report waiting.
func linked(t *testing.T, cp *controlPlane) string {
	t.Helper()
	dir := t.TempDir()
	ca := filepath.Join(dir, "control-ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cp.Certificate().Raw})
	if err := os.WriteFile(ca, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	enr, _ := json.Marshal(map[string]string{
		"controlUrl": cp.URL, "hostname": "rina-pixel-8", "credential": "argus-agent-test",
		"enrolledAt": "2026-01-02T03:04:05Z", "caFile": ca,
	})
	for name, body := range map[string]string{
		agent.EnrollmentFile:  string(enr),
		agent.GrantKeyFile:    "key",
		"recordings/a.cast":   "rec",
		"report-spool.jsonl":  `{"path":"/api/v1/report/session","body":{"id":"s1"},"at":"2026-01-02T03:04:05Z"}` + "\n",
		"sessions-kept.jsonl": "{}\n",
	} {
		p := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLeavingRevokesTheLinkKeepsTheEvidenceAndTellsTheApp(t *testing.T) {
	cp := newControlPlane(t, http.StatusOK)
	dir := linked(t, cp)
	app := &removedApp{}
	a := NewAgent(dir, app, nil, "Android 14", "Pixel 8", "m1")
	a.SetRemoved(app)

	summary, err := a.Leave(false)
	if err != nil {
		t.Fatalf("Leave: %v", err)
	}
	cp.mu.Lock()
	told, reported := cp.told, cp.reported
	cp.mu.Unlock()
	if told["hostname"] != "rina-pixel-8" || !strings.Contains(told["ran_by"], "Argus app") {
		t.Fatalf("the control plane was told %v", told)
	}
	if reported == 0 {
		t.Error("the waiting report was not shipped before the link went")
	}
	for _, f := range []string{agent.EnrollmentFile, agent.GrantKeyFile, "control-ca.pem"} {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("%s survived leaving", f)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "recordings", "a.cast")); err != nil {
		t.Errorf("a recording was deleted: %v", err)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.calls != 1 || app.byConsole || !strings.Contains(summary, "revoked") {
		t.Errorf("app told %d time(s), byConsole=%v, summary %q", app.calls, app.byConsole, summary)
	}
	if st, _ := Enrolled(dir); st.Enrolled {
		t.Error("still enrolled after leaving")
	}
}

// A report that cannot be shipped stops the Remove button, and the link stays
// so it can be shipped later. Forced, the device leaves and says the console
// still lists it.
func TestLeavingWithEvidenceStrandedAsksFirst(t *testing.T) {
	cp := newControlPlane(t, http.StatusServiceUnavailable)
	dir := linked(t, cp)
	app := &removedApp{}
	a := NewAgent(dir, app, nil, "Android 14", "Pixel 8", "m1")
	a.SetRemoved(app)

	_, err := a.Leave(false)
	if err == nil || !strings.Contains(err.Error(), "1 report(s)") {
		t.Fatalf("Leave(false) with a report stranded: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, agent.EnrollmentFile)); err != nil {
		t.Fatalf("the link was deleted although nothing was shipped: %v", err)
	}
	if app.calls != 0 {
		t.Fatal("the app was told the device left when it did not")
	}

	summary, err := a.Leave(true)
	if err != nil {
		t.Fatalf("Leave(true): %v", err)
	}
	if !strings.Contains(summary, "could not be told") || !strings.Contains(summary, "rina-pixel-8") ||
		!strings.Contains(summary, "1 report(s)") {
		t.Errorf("summary %q does not say what is left to do", summary)
	}
	if _, err := os.Stat(filepath.Join(dir, agent.EnrollmentFile)); !os.IsNotExist(err) {
		t.Error("the link survived a forced leave")
	}
}

// Removed in the console: the device goes, and if the control plane cannot
// be told it keeps its credential to tell it on a later heartbeat.
func TestTheConsolesRemovalKeepsTheLinkUntilItCanBeTold(t *testing.T) {
	cp := newControlPlane(t, http.StatusServiceUnavailable)
	dir := linked(t, cp)
	app := &removedApp{}
	a := NewAgent(dir, app, nil, "Android 14", "Pixel 8", "m1")
	a.SetRemoved(app)

	a.leaveForConsole()
	if _, err := os.Stat(filepath.Join(dir, agent.EnrollmentFile)); err != nil {
		t.Fatalf("the credential went before the control plane was told: %v", err)
	}
	if app.calls != 0 {
		t.Fatal("the app was told the device left")
	}

	cp.mu.Lock()
	cp.status = http.StatusOK
	cp.mu.Unlock()
	a.lastLeave = a.lastLeave.Add(-11 * time.Minute)
	a.leaveForConsole()
	if _, err := os.Stat(filepath.Join(dir, agent.EnrollmentFile)); !os.IsNotExist(err) {
		t.Fatal("the credential survived a removal the control plane heard")
	}
	cp.mu.Lock()
	ranBy := cp.told["ran_by"]
	cp.mu.Unlock()
	if !strings.Contains(ranBy, "console") {
		t.Errorf("ran_by = %q", ranBy)
	}
	if app.calls != 1 || !app.byConsole || !strings.Contains(app.summary, "administrator") {
		t.Errorf("app told %d time(s), byConsole=%v, %q", app.calls, app.byConsole, app.summary)
	}
}
