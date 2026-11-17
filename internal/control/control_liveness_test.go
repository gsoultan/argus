package control

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

/*
When Argus itself stops: the outage record, the outside check-in and the
"If Argus goes down" page (control_liveness.go, control_readiness.go).

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5432/argus_test?sslmode=disable \
	  go test ./internal/control/ -run 'Outage|Outside|Readiness'

The control_instances table is platform-wide, so each test starts it empty.
*/

// livenessStore is a database of its own: the outage rule reads the whole
// audit log, which the shared test database fills from every other test.
func livenessStore(t *testing.T) *Store {
	t.Helper()
	dsn := scratchDB(t, fmt.Sprintf("argus_liveness_%d", time.Now().UnixNano()))
	s, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// livenessAPI is enrollAPI on a database of its own.
func livenessAPI(t *testing.T) (*API, *Store, http.Handler, string) {
	t.Helper()
	s := livenessStore(t)
	a := NewAPI(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.ReporterToken = testReporterToken
	admin, _ := newAccount(t, s, "admin")
	a.UserTokens = map[string]string{"admin-token": admin}
	return a, s, a.Handler(), admin
}

func resetLiveness(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), `DELETE FROM argus_platform.control_instances;
		DELETE FROM argus_platform.control_outages; DELETE FROM argus_platform.control_readiness`); err != nil {
		t.Fatal(err)
	}
}

func setSeen(t *testing.T, s *Store, host string, ago time.Duration) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), `INSERT INTO argus_platform.control_instances (host, seen_at)
		VALUES ($1, now() - $2::interval)
		ON CONFLICT (host) DO UPDATE SET seen_at = EXCLUDED.seen_at`,
		host, ago.String()); err != nil {
		t.Fatal(err)
	}
}

// A first start, and a restart quicker than outageAfter, are not outages.
func TestNoOutageOnAFirstStartOrAQuickRestart(t *testing.T) {
	s := livenessStore(t)
	resetLiveness(t, s)
	ctx := context.Background()
	if o, err := s.NoteControlAlive(ctx, "ctl-a", "1.0.0"); err != nil || o != nil {
		t.Fatalf("first start: outage %+v, err %v", o, err)
	}
	setSeen(t, s, "ctl-a", 90*time.Second)
	if o, err := s.NoteControlAlive(ctx, "ctl-a", "1.0.1"); err != nil || o != nil {
		t.Fatalf("a restart in 90 s: outage %+v, err %v", o, err)
	}
}

// Nothing heard from for ten minutes is an outage, found by the first
// server back, and only once even when a second one comes back too.
func TestAnOutageIsFoundOnceByTheFirstServerBack(t *testing.T) {
	s := livenessStore(t)
	resetLiveness(t, s)
	ctx := context.Background()
	setSeen(t, s, "ctl-a", 10*time.Minute)
	setSeen(t, s, "ctl-b", 11*time.Minute)

	o, err := s.NoteControlAlive(ctx, "ctl-a", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if o == nil || o.FoundBy != "ctl-a" || o.Minutes() != 10 {
		t.Fatalf("outage: %+v", o)
	}
	again, err := s.NoteControlAlive(ctx, "ctl-b", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if again != nil {
		t.Fatalf("the second server back reported it again: %+v", again)
	}
	last, err := s.LastControlOutage(ctx)
	if err != nil || last == nil || last.FoundBy != "ctl-a" {
		t.Fatalf("recorded outage: %+v, %v", last, err)
	}
}

// One replica down while another serves is not an outage.
func TestOneReplicaDownWhileAnotherServesIsNotAnOutage(t *testing.T) {
	s := livenessStore(t)
	resetLiveness(t, s)
	ctx := context.Background()
	setSeen(t, s, "ctl-a", 20*time.Minute)
	setSeen(t, s, "ctl-b", 30*time.Second)
	if o, err := s.NoteControlAlive(ctx, "ctl-a", "1.0.0"); err != nil || o != nil {
		t.Fatalf("outage %+v, err %v", o, err)
	}
}

// The tick that finds an outage audits it and puts it in the bell.
func TestAnOutageIsAuditedAndRaised(t *testing.T) {
	a, s, _, _ := livenessAPI(t)
	resetLiveness(t, s)
	a.InstanceName = "ctl-audit"
	setSeen(t, s, "ctl-other", 33*time.Minute)

	a.livenessTick(context.Background(), time.Now())

	var action, detail string
	if err := s.pool.QueryRow(context.Background(), `SELECT action, detail FROM audit_events
		WHERE action = 'control.outage' ORDER BY seq DESC LIMIT 1`).Scan(&action, &detail); err != nil {
		t.Fatalf("no audit entry for the outage: %v", err)
	}
	if !strings.Contains(detail, "Argus was down for 33 minutes") {
		t.Errorf("audit detail: %s", detail)
	}
	var title string
	if err := s.pool.QueryRow(context.Background(), `SELECT title FROM console_alerts
		WHERE title LIKE 'Argus was down%' ORDER BY seq DESC LIMIT 1`).Scan(&title); err != nil {
		t.Fatalf("no alert for the outage: %v", err)
	}
}

// The check-in is called while the database answers, and a failure is
// recorded without the address, which is the service's secret.
func TestTheOutsideCheckInIsCalledAndItsSecretKept(t *testing.T) {
	a, s, _, _ := livenessAPI(t)
	resetLiveness(t, s)
	a.InstanceName = "ctl-check"
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if strings.HasSuffix(r.URL.Path, "/bad") {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	a.OutsideCheck = OutsideCheck{URL: srv.URL + "/secret-uuid", Every: time.Minute}
	now := time.Now()
	a.livenessTick(context.Background(), now)
	a.livenessTick(context.Background(), now.Add(20*time.Second)) // not due yet
	if hits.Load() != 1 {
		t.Fatalf("check-ins: %d, want 1", hits.Load())
	}
	list, _ := s.ControlInstances(context.Background())
	if len(list) != 1 || list[0].CheckOK == nil || !*list[0].CheckOK {
		t.Fatalf("recorded check-in: %+v", list)
	}

	a.OutsideCheck.URL = srv.URL + "/secret-uuid/bad"
	a.livenessTick(context.Background(), now.Add(2*time.Minute))
	list, _ = s.ControlInstances(context.Background())
	if list[0].CheckOK == nil || *list[0].CheckOK {
		t.Fatalf("a 404 should be a failed check-in: %+v", list[0])
	}
	if strings.Contains(list[0].CheckDetail, "secret-uuid") {
		t.Errorf("the detail shows the secret address: %s", list[0].CheckDetail)
	}
}

// The page is the platform owner's, confirmations are audited and lapse,
// and only known items can be confirmed.
func TestReadinessIsForPlatformOwnersAndConfirmationsAreAudited(t *testing.T) {
	a, s, h, admin := livenessAPI(t)
	resetLiveness(t, s)
	a.InstanceName = "ctl-ready"
	a.livenessTick(context.Background(), time.Now())

	w := do(t, h, http.MethodGet, "/api/v1/platform/readiness", "admin-token", "")
	if w.Code != http.StatusOK {
		t.Fatalf("readiness: %d %s", w.Code, w.Body)
	}
	var v ReadinessView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Running != 1 || v.OutsideCheck.Configured || v.ConfirmDays != 90 {
		t.Fatalf("view: %+v", v)
	}
	for _, item := range ReadinessItems {
		if c, ok := v.Confirmations[item]; !ok || c != nil {
			t.Errorf("%s should be listed and unconfirmed: %+v", item, c)
		}
	}

	if w := do(t, h, http.MethodPost, "/api/v1/platform/readiness/nonsense/confirm", "admin-token", ""); w.Code != http.StatusNotFound {
		t.Errorf("unknown item: %d", w.Code)
	}
	if w := do(t, h, http.MethodPost, "/api/v1/platform/readiness/backups_off_host/confirm", "admin-token", ""); w.Code != http.StatusOK {
		t.Fatalf("confirm: %d %s", w.Code, w.Body)
	}
	if e := lastAuditOn(t, s, "argus-control"); e.Action != "control.readiness_confirmed" || e.ActorEmail != admin {
		t.Errorf("audit: %+v", e)
	}

	// Older than 90 days, it lapses.
	if _, err := s.pool.Exec(context.Background(), `UPDATE argus_platform.control_readiness
		SET confirmed_at = now() - interval '91 days'`); err != nil {
		t.Fatal(err)
	}
	w = do(t, h, http.MethodGet, "/api/v1/platform/readiness", "admin-token", "")
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	if c := v.Confirmations[ReadyBackupsOffHost]; c == nil || !c.Lapsed {
		t.Errorf("a 91-day-old confirmation should have lapsed: %+v", c)
	}
}

func TestOutsideCheckAddressesAreChecked(t *testing.T) {
	for _, bad := range []string{"ftp://x", "https://", "https://u:p@hc-ping.com/x", "::"} {
		if ValidOutsideCheckURL(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, good := range []string{"", "https://hc-ping.com/abc", "http://kuma.lan:3001/api/push/x"} {
		if err := ValidOutsideCheckURL(good); err != nil {
			t.Errorf("%q refused: %v", good, err)
		}
	}
}

// Going back to a version that writes no row, and forward again, leaves a
// gap that was not an outage: the older version wrote to the audit log
// while it served.
func TestAGapAnOlderVersionServedIsNotAnOutage(t *testing.T) {
	s := livenessStore(t)
	resetLiveness(t, s)
	ctx := context.Background()
	setSeen(t, s, "ctl-a", 3*time.Hour)
	if _, err := s.pool.Exec(ctx, `INSERT INTO audit_events (at, action, actor_email, target, prev_hash, hash)
		VALUES (now() - interval '1 hour', 'auth.login', 'someone@example.com', 'test-gap', '', '')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.pool.Exec(ctx, `DELETE FROM audit_events WHERE target = 'test-gap'`) })
	if o, err := s.NoteControlAlive(ctx, "ctl-a", "1.0.0"); err != nil || o != nil {
		t.Fatalf("outage %+v, err %v", o, err)
	}
}

// A command on the host during an outage does not hide it.
func TestAHostCommandDuringAnOutageDoesNotHideIt(t *testing.T) {
	s := livenessStore(t)
	resetLiveness(t, s)
	ctx := context.Background()
	setSeen(t, s, "ctl-a", time.Hour)
	if _, err := s.pool.Exec(ctx, `INSERT INTO audit_events (at, action, actor_email, target, prev_hash, hash)
		VALUES (now() - interval '30 minutes', 'user.password_set', 'root@host', 'test-gap-host', '', '')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.pool.Exec(ctx, `DELETE FROM audit_events WHERE target = 'test-gap-host'`) })
	if o, err := s.NoteControlAlive(ctx, "ctl-a", "1.0.0"); err != nil || o == nil {
		t.Fatalf("outage %+v, err %v", o, err)
	}
}
