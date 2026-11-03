package control

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestErrorLogFoldsRepeatsAndKeepsABoundedList(t *testing.T) {
	var l errorLog
	now := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	l.record(now, "sessions", "timeout")
	l.record(now.Add(time.Second), "assets", "boom")
	l.record(now.Add(2*time.Second), "sessions", "timeout")

	c := l.counts(now.Add(3 * time.Second))
	if c.SinceStart != 3 || c.LastHour != 3 {
		t.Fatalf("counts = %+v, want 3 since start and 3 in the hour", c)
	}
	if len(c.Recent) != 2 || c.Recent[0].Op != "sessions" || c.Recent[0].Count != 2 {
		t.Fatalf("recent = %+v, want the repeated failure first with a count of 2", c.Recent)
	}

	for i := 0; i < recentErrorsKept*3; i++ {
		l.record(now, "op", strings.Repeat("x", i))
	}
	if got := len(l.counts(now).Recent); got != recentErrorsKept {
		t.Errorf("kept %d failures, want at most %d", got, recentErrorsKept)
	}
}

func TestErrorLogForgetsTheHourButNotTheTotal(t *testing.T) {
	var l errorLog
	start := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	l.record(start, "a", "x")
	l.record(start.Add(30*time.Minute), "a", "x")
	c := l.counts(start.Add(61 * time.Minute))
	if c.LastHour != 1 || c.SinceStart != 2 {
		t.Errorf("an hour later = %+v, want 1 in the last hour and 2 in all", c)
	}
	// The same ring slot reused a full hour on is a new minute, not an add.
	l.record(start.Add(60*time.Minute), "a", "x")
	if c := l.counts(start.Add(60 * time.Minute)); c.LastHour != 2 {
		t.Errorf("after reusing a slot = %d in the hour, want 2", c.LastHour)
	}
}

func TestClipMessageDoesNotSplitARune(t *testing.T) {
	msg := strings.Repeat("é", errorMessageMax)
	got := clipMessage(msg)
	if !utf8.ValidString(got) || len(got) > errorMessageMax+len("…") {
		t.Errorf("clipped to %d bytes, valid=%v", len(got), utf8.ValidString(got))
	}
}

func TestBusyBetween(t *testing.T) {
	if v := busyBetween(cpuTimes{100, 50}, cpuTimes{200, 75}); v == nil || *v != 75 {
		t.Errorf("busy = %v, want 75", v)
	}
	if v := busyBetween(cpuTimes{200, 75}, cpuTimes{100, 50}); v != nil {
		t.Errorf("a counter that went backwards gave %v, want nothing", *v)
	}
}

func TestTopTablesAddUpToTheDatabase(t *testing.T) {
	got := topTables([]TableSize{{"a", 10}, {"b", 50}, {"c", 30}, {"d", 5}}, 100, 2)
	if len(got) != 3 || got[0].Name != "b" || got[1].Name != "c" || got[2].Name != "" || got[2].Bytes != 20 {
		t.Errorf("top tables = %+v", got)
	}
}

func TestStatusProblems(t *testing.T) {
	healthy := ControlStatus{
		Database: DatabaseStatus{OK: true, LatencyMs: 4, InUse: 2, Max: 20},
		Machine: MachineStatus{MemTotal: 100, MemAvailable: 60,
			Disks: []DiskStatus{{Path: "/tmp", Total: 100, Avail: 60}}},
		Certificate: &CertificateStatus{DaysLeft: 100},
	}
	if ps := statusProblems(healthy); len(ps) != 0 {
		t.Fatalf("a healthy control plane has problems: %+v", ps)
	}

	sick := healthy
	sick.Database = DatabaseStatus{OK: true, LatencyMs: 310, InUse: 19, Max: 20}
	sick.Machine.Disks = []DiskStatus{{Path: "/tmp", Total: 100, Avail: 6}}
	sick.Certificate = &CertificateStatus{DaysLeft: 10}
	sick.Errors.LastHour = 27
	failed := time.Now()
	sick.Recordings.LastFailure = &failed
	sick.Recordings.LastUploadError = "no space left on device"

	ps := statusProblems(sick)
	areas := map[string]int{}
	for _, p := range ps {
		areas[p.Area]++
	}
	for _, want := range []string{"database", "disk", "certificate", "errors", "recordings"} {
		if areas[want] == 0 {
			t.Errorf("no %s problem in %+v", want, ps)
		}
	}
	if areas["database"] != 2 {
		t.Errorf("slow and nearly out of connections should be two problems, got %d", areas["database"])
	}
	if ps[0].Severity != "critical" || ps[0].Area != "disk" {
		t.Errorf("the full disk should come first: %+v", ps[0])
	}

	// A managed certificate renews itself; 10 days out is only a problem
	// once renewal has clearly failed.
	managed := healthy
	managed.Certificate = &CertificateStatus{DaysLeft: 10, Managed: true}
	if ps := statusProblems(managed); len(ps) != 0 {
		t.Errorf("a managed certificate 10 days out was reported: %+v", ps)
	}
	managed.Certificate.DaysLeft = 3
	if ps := statusProblems(managed); len(ps) != 1 {
		t.Errorf("a managed certificate 3 days out was not reported: %+v", ps)
	}

	down := healthy
	down.Database = DatabaseStatus{Error: "connection refused"}
	if ps := statusProblems(down); len(ps) != 1 || ps[0].Severity != "critical" {
		t.Errorf("an unreachable database = %+v, want one critical problem", ps)
	}
}

func TestRecordingUploadFailureClearsOnTheNextSuccess(t *testing.T) {
	var a API
	a.uploads.failed(time.Now().Add(-time.Minute), errors.New("bucket refused"))
	a.uploads.ok(time.Now())
	st := ControlStatus{Database: DatabaseStatus{OK: true}}
	ok, fail, msg := a.uploads.snapshot()
	st.Recordings.LastUpload, st.Recordings.LastFailure, st.Recordings.LastUploadError = &ok, &fail, msg
	if ps := statusProblems(st); len(ps) != 0 {
		t.Errorf("a failure followed by a success is still reported: %+v", ps)
	}
}

// The page describes the host and every organization, so it is the
// platform's managers alone (an owner or admin of the default organization):
// an organization's own owner, and anyone else in the default one, are told it
// does not exist.
func TestControlStatusIsForThePlatformOwnerOnly(t *testing.T) {
	h := newTenantHarness(t)
	tn, orgOwner, orgPw := newTenant(t, h.store)
	ownerEmail, ownerPw := newAccount(t, h.store, "owner")
	opEmail, opPw := newAccount(t, h.store, "operator")

	if code, _ := h.call(t, &http.Client{}, "", http.MethodGet, "/api/v1/platform/status", ""); code != http.StatusUnauthorized {
		t.Errorf("signed out = %d, want 401", code)
	}
	inOrg := h.signIn(t, tn.Slug, orgOwner, orgPw)
	if code, _ := h.call(t, inOrg, "", http.MethodGet, "/api/v1/platform/status", ""); code != http.StatusNotFound {
		t.Errorf("an organization's owner = %d, want 404", code)
	}
	inOp := h.signIn(t, "", opEmail, opPw)
	if code, _ := h.call(t, inOp, "", http.MethodGet, "/api/v1/platform/status", ""); code != http.StatusNotFound {
		t.Errorf("an operator = %d, want 404", code)
	}

	in := h.signIn(t, "", ownerEmail, ownerPw)
	code, body := h.call(t, in, "", http.MethodGet, "/api/v1/platform/status", "")
	if code != http.StatusOK {
		t.Fatalf("platform owner = %d: %s", code, body)
	}
	var st ControlStatus
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	if !st.Database.OK || st.Database.SizeBytes == nil || *st.Database.SizeBytes <= 0 {
		t.Errorf("database = %+v, want it answering with a size", st.Database)
	}
	if st.Fleet == nil || st.Fleet.Organizations < 2 {
		t.Errorf("fleet = %+v, want both organizations counted", st.Fleet)
	}
	if st.Problems == nil || st.Errors.Recent == nil {
		t.Error("empty lists must be [] so the console need not guess")
	}
}
