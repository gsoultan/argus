package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

// schedEnv is a manageable Production account with one instance, tagged
// onto a schedule named name that runs 08:00 to 19:00 UTC every day.
type schedEnv struct {
	changeEnv
	name string
}

const region = "ap-southeast-3"

func scheduleAPI(t *testing.T) schedEnv {
	t.Helper()
	e := changeAPI(t)
	name := strings.ReplaceAll(unique("office"), "_", "-")
	e.fake.Resources[region][0].Tags[cloud.ScheduleTag] = name
	e.fake.Resources[region][0].Addresses = []string{"10.9." + e.account[9:10] + "." + e.account[10:12]}
	if !e.a.SyncCloudAccount(context.Background(), e.acct) {
		t.Fatal("sync is busy")
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = e.s.pool.Exec(ctx, `DELETE FROM cloud_schedules WHERE name = $1`, name)
		_, _ = e.s.pool.Exec(ctx, `DELETE FROM cloud_keep_running WHERE account_id::text = $1`, e.acct.ID)
	})
	return schedEnv{changeEnv: e, name: name}
}

func (e schedEnv) body(extra string) string {
	return `{"name":"` + e.name + `","timezone":"UTC","windows":[{"days":[1,2,3,4,5,6,7],"start":"08:00","stop":"19:00"}]` + extra + `}`
}

// make creates the schedule as admin and approves it as the approver.
func (e schedEnv) make(t *testing.T, extra string) CloudSchedule {
	t.Helper()
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/schedules", "admin-token", e.body(extra))
	if w.Code != http.StatusAccepted {
		t.Fatalf("a Production schedule must wait for approval: %d %s", w.Code, w.Body)
	}
	var s CloudSchedule
	_ = json.Unmarshal(w.Body.Bytes(), &s)
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/schedules/"+s.ID+"/approve", "approver-token", ""); w.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", w.Code, w.Body)
	}
	got, err := e.s.CloudSchedule(context.Background(), s.ID)
	if err != nil || got.State != "active" {
		t.Fatalf("approved schedule is not active: %+v %v", got, err)
	}
	return got
}

// at is a time on Monday 2026-11-09, UTC.
func at(hhmm string) time.Time {
	t, _ := time.Parse("2006-01-02 15:04", "2026-11-09 "+hhmm)
	return t
}

func (e schedEnv) checkedAt(t *testing.T, s CloudSchedule, when time.Time) {
	t.Helper()
	if _, err := e.s.pool.Exec(context.Background(), `UPDATE cloud_schedules SET checked_at = $2 WHERE id::text = $1`, s.ID, when); err != nil {
		t.Fatal(err)
	}
}

func (e schedEnv) calls(prefix string) int {
	n := 0
	for _, c := range e.fake.CallLog() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (e schedEnv) runs(t *testing.T, s CloudSchedule) []CloudScheduleRun {
	t.Helper()
	runs, err := e.s.CloudScheduleRuns(context.Background(), s.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

func (e schedEnv) state() string { return e.fake.Resources[region][0].State }

func TestCloudScheduleOnProductionNeedsSomeoneElse(t *testing.T) {
	e := scheduleAPI(t)
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/schedules", "auditor-token", e.body("")); w.Code != http.StatusForbidden {
		t.Fatalf("an auditor made a schedule: %d", w.Code)
	}
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/schedules", "admin-token", e.body(""))
	if w.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d %s", w.Code, w.Body)
	}
	var s CloudSchedule
	_ = json.Unmarshal(w.Body.Bytes(), &s)
	if s.State != "awaiting_approval" || s.Owner != e.admin {
		t.Fatalf("schedule: %+v", s)
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/schedules/"+s.ID+"/approve", "admin-token", ""); w.Code != http.StatusForbidden {
		t.Fatalf("the person who asked approved their own schedule: %d", w.Code)
	}
	// While it waits, the loop leaves it alone.
	e.TickAt(at("19:00").Add(30 * time.Second))
	if e.calls("stop") != 0 {
		t.Fatal("a schedule waiting for approval ran")
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/schedules/"+s.ID+"/approve", "approver-token", ""); w.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", w.Code, w.Body)
	}
	target := "schedule " + e.name
	if auditCount(t, e.s, "cloud.schedule.requested", target) != 1 || auditCount(t, e.s, "cloud.schedule.approved", target) != 1 ||
		auditCount(t, e.s, "cloud.schedule.applied", target) != 1 {
		t.Fatal("a schedule must be audited as requested, approved and applied")
	}

	// An edit to it waits too, and the version in effect stays.
	w = do(t, e.h, http.MethodPut, "/api/v1/cloud/schedules/"+s.ID, "admin-token",
		strings.Replace(e.body(""), `"19:00"`, `"20:00"`, 1))
	if w.Code != http.StatusAccepted {
		t.Fatalf("an edit to a Production schedule must wait: %d %s", w.Code, w.Body)
	}
	got, _ := e.s.CloudSchedule(context.Background(), s.ID)
	if got.Windows[0].Stop != "19:00" || got.Pending == nil || got.Pending.Windows[0].Stop != "20:00" {
		t.Fatalf("the edit replaced the version in effect before approval: %+v", got)
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/schedules/"+s.ID+"/decline", "second-token", ""); w.Code != http.StatusOK {
		t.Fatalf("decline: %d %s", w.Code, w.Body)
	}
	got, _ = e.s.CloudSchedule(context.Background(), s.ID)
	if got.Pending != nil || got.Windows[0].Stop != "19:00" || got.State != "active" {
		t.Fatalf("a declined edit must leave the schedule as it was: %+v", got)
	}
}

// TickAt runs one minute of the loop at now.
func (e schedEnv) TickAt(now time.Time) { e.a.TickCloudSchedules(context.Background(), now) }

func TestCloudScheduleStopsAndStartsAsTheOwner(t *testing.T) {
	e := scheduleAPI(t)
	s := e.make(t, "")
	e.checkedAt(t, s, at("18:59"))
	e.TickAt(at("19:00").Add(30 * time.Second))
	if e.state() != "stopped" {
		t.Fatalf("the instance was not stopped at 19:00: %s", e.state())
	}
	if e.calls("stop-many "+e.instance()+" "+e.admin) != 1 {
		t.Fatalf("the stop must run once, batched, as the owner: %v", e.fake.CallLog())
	}
	runs := e.runs(t, s)
	if len(runs) != 1 || runs[0].State != "done" || runs[0].Action != cloud.ActionStop || len(runs[0].RequestIDs) != 1 {
		t.Fatalf("runs: %+v", runs)
	}
	if auditCount(t, e.s, "cloud.schedule.run", "schedule "+e.name) != 1 {
		t.Fatal("a run must be audited")
	}
	// A second control host ticking the same minute claims nothing.
	e.checkedAt(t, s, at("18:59"))
	e.TickAt(at("19:00").Add(40 * time.Second))
	if e.calls("stop-many") != 1 || len(e.runs(t, s)) != 1 {
		t.Fatal("a transition ran twice")
	}

	// The next morning it starts, once the inventory shows it stopped.
	e.a.SyncCloudAccount(context.Background(), e.acct)
	e.checkedAt(t, s, at("07:59").AddDate(0, 0, 1))
	e.TickAt(at("08:00").AddDate(0, 0, 1).Add(10 * time.Second))
	if e.state() != "running" || e.calls("start-many "+e.instance()+" "+e.admin) != 1 {
		t.Fatalf("the instance was not started as the owner: %s %v", e.state(), e.fake.CallLog())
	}
}

func TestCloudScheduleSkips(t *testing.T) {
	e := scheduleAPI(t)
	s := e.make(t, "")
	ctx := context.Background()
	if err := e.s.SetCloudKeepRunning(ctx, CloudKeepRunning{AccountID: e.acct.ID, InstanceID: e.instance(),
		Until: at("21:00"), Reason: "release night", SetBy: e.second}); err != nil {
		t.Fatal(err)
	}
	e.checkedAt(t, s, at("18:59"))
	e.TickAt(at("19:00").Add(30 * time.Second))
	if e.state() != "running" || e.calls("stop") != 0 {
		t.Fatal("an instance kept running was stopped")
	}
	runs := e.runs(t, s)
	if len(runs) != 1 || runs[0].Targets[0].Status != "skipped" || !strings.Contains(runs[0].Targets[0].Reason, "release night") &&
		!strings.Contains(runs[0].Targets[0].Reason, e.second) {
		t.Fatalf("the skip must say why: %+v", runs)
	}

	// A spot instance is never stopped.
	if _, _, err := e.s.ClearCloudKeepRunning(ctx, e.acct.ID, e.instance()); err != nil {
		t.Fatal(err)
	}
	e.fake.Resources[region][0].Details = map[string]string{"lifecycle": "spot"}
	e.a.SyncCloudAccount(ctx, e.acct)
	e.checkedAt(t, s, at("18:59").AddDate(0, 0, 1))
	e.TickAt(at("19:00").AddDate(0, 0, 1).Add(5 * time.Second))
	if e.calls("stop") != 0 {
		t.Fatal("a spot instance was stopped")
	}
}

func TestCloudScheduleWaitsForASession(t *testing.T) {
	e := scheduleAPI(t)
	s := e.make(t, "")
	ctx := context.Background()
	host := strings.ToLower(e.name)
	addr := e.fake.Resources[region][0].Addresses[0]
	if _, err := e.s.pool.Exec(ctx, `INSERT INTO assets (hostname, address, protocol, agent_state) VALUES ($1, $2, 'ssh', 'absent')`, host, addr); err != nil {
		t.Fatal(err)
	}
	var assetID string
	_ = e.s.pool.QueryRow(ctx, `SELECT id::text FROM assets WHERE hostname = $1`, host).Scan(&assetID)
	sid := newID(t)
	if _, err := e.s.pool.Exec(ctx, `INSERT INTO sessions (id, user_email, asset_id, asset_hostname, principal, state, started_at, last_reported_at)
		VALUES ($1::uuid, $2, $3::uuid, $4, 'ops', 'active', now(), now())`, sid, e.second, assetID, host); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.s.pool.Exec(context.Background(), `DELETE FROM sessions WHERE id = $1::uuid`, sid)
		_, _ = e.s.pool.Exec(context.Background(), `DELETE FROM assets WHERE hostname = $1`, host)
	})

	e.checkedAt(t, s, at("18:59"))
	e.TickAt(at("19:00").Add(30 * time.Second))
	if e.calls("stop") != 0 {
		t.Fatal("stopped with a session open")
	}
	runs := e.runs(t, s)
	if runs[0].State != "running" || runs[0].Targets[0].Status != "waiting" || runs[0].NextAt == nil {
		t.Fatalf("the run must wait for the session: %+v", runs[0])
	}
	// Forty-five minutes in, the person in the session is told.
	e.TickAt(at("19:46"))
	var notices int
	_ = e.s.pool.QueryRow(ctx, `SELECT count(*) FROM console_alerts WHERE recipient = $1 AND title LIKE '%stops at%'`,
		strings.ToLower(e.second)).Scan(&notices)
	if notices != 1 || e.calls("stop") != 0 {
		t.Logf("run: %+v", e.runs(t, s)[0])
		t.Fatalf("want one notice and no stop yet, got %d notices", notices)
	}
	// An hour in, it stops anyway.
	e.TickAt(at("20:01"))
	if e.state() != "stopped" {
		t.Fatalf("not stopped after the hour: %+v", e.runs(t, s)[0])
	}
	if r := e.runs(t, s)[0]; r.State != "done" || r.NextAt != nil {
		t.Fatalf("run not settled: %+v", r)
	}
}

func TestCloudScheduleMissedRunIsNotMadeUp(t *testing.T) {
	e := scheduleAPI(t)
	s := e.make(t, "")
	// The control plane was down from 18:00 to 22:00.
	e.checkedAt(t, s, at("18:00"))
	e.TickAt(at("22:00"))
	if e.calls("stop") != 0 {
		t.Fatal("a transition missed by more than two hours was made up")
	}
	runs := e.runs(t, s)
	if len(runs) != 1 || runs[0].State != "skipped" || !strings.Contains(runs[0].Detail, "Missed") {
		t.Fatalf("the missed stop must be recorded: %+v", runs)
	}
	// Within two hours, it is made up.
	e2 := scheduleAPI(t)
	s2 := e2.make(t, "")
	e2.checkedAt(t, s2, at("18:30"))
	e2.TickAt(at("20:30"))
	if e2.state() != "stopped" {
		t.Fatal("a transition missed by 90 minutes must still run")
	}
}

func TestCloudScheduleEnforce(t *testing.T) {
	e := scheduleAPI(t)
	s := e.make(t, `,"mode":"enforce"`)
	// Someone started it by hand at night: at the next quarter hour it is
	// put back.
	e.checkedAt(t, s, at("22:14"))
	e.TickAt(at("22:15").Add(10 * time.Second))
	if e.state() != "stopped" {
		t.Fatal("enforce mode left a running instance outside its window")
	}
	// A transitions-mode schedule leaves it.
	e2 := scheduleAPI(t)
	s2 := e2.make(t, "")
	e2.checkedAt(t, s2, at("22:14"))
	e2.TickAt(at("22:15").Add(10 * time.Second))
	if e2.state() != "running" {
		t.Fatal("transitions mode acted between transitions")
	}
}

func TestCloudSchedulePausesWhenTheOwnerLosesAdmin(t *testing.T) {
	e := scheduleAPI(t)
	s := e.make(t, "")
	if _, err := e.s.pool.Exec(context.Background(), `UPDATE users SET role = 'operator' WHERE lower(email) = lower($1)`, e.admin); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.s.pool.Exec(context.Background(), `UPDATE users SET role = 'admin' WHERE lower(email) = lower($1)`, e.admin)
	})
	e.checkedAt(t, s, at("18:59"))
	e.TickAt(at("19:00").Add(30 * time.Second))
	got, _ := e.s.CloudSchedule(context.Background(), s.ID)
	if got.State != "paused" || !strings.Contains(got.PausedReason, e.admin) || e.calls("stop") != 0 {
		t.Fatalf("the schedule must pause, not run: %+v", got)
	}
	if auditCount(t, e.s, "cloud.schedule.paused", "schedule "+e.name) != 1 {
		t.Fatal("pausing must be audited")
	}
	// Resuming makes the person who resumes the owner.
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/schedules/"+s.ID+"/resume", "second-token", ""); w.Code != http.StatusOK {
		t.Fatalf("resume: %d %s", w.Code, w.Body)
	}
	got, _ = e.s.CloudSchedule(context.Background(), s.ID)
	if got.State != "active" || got.Owner != e.second {
		t.Fatalf("resumed: %+v", got)
	}
}

func TestCloudScheduleRetriesAFailureOnce(t *testing.T) {
	e := scheduleAPI(t)
	s := e.make(t, "")
	e.fake.PowerErr = map[string]error{e.instance(): errors.New("IncorrectInstanceState: busy")}
	e.checkedAt(t, s, at("18:59"))
	e.TickAt(at("19:00").Add(30 * time.Second))
	r := e.runs(t, s)[0]
	if r.State != "running" || r.Targets[0].Status != "failed" || r.NextAt == nil {
		t.Fatalf("a failure must be tried again: %+v", r)
	}
	e.fake.PowerErr = nil
	e.TickAt(at("19:03"))
	r = e.runs(t, s)[0]
	if r.State != "done" || e.state() != "stopped" {
		t.Fatalf("the retry did not stop it: %+v", r)
	}
	// Failing twice is final.
	e.a.SyncCloudAccount(context.Background(), e.acct)
	e.fake.PowerErr = map[string]error{e.instance(): errors.New("UnauthorizedOperation")}
	e.checkedAt(t, s, at("07:59").AddDate(0, 0, 1))
	e.TickAt(at("08:00").AddDate(0, 0, 1))
	e.TickAt(at("08:03").AddDate(0, 0, 1))
	r = e.runs(t, s)[0]
	if r.State != "failed" || r.NextAt != nil {
		t.Fatalf("two failures must settle as failed: %+v", r)
	}
}

func TestCloudKeepRunningIsCappedAndAudited(t *testing.T) {
	e := scheduleAPI(t)
	path := "/api/v1/cloud/instances/" + e.acct.ID + "/" + region + "/" + e.instance() + "/keep-running"
	far := time.Now().Add(8 * 24 * time.Hour).UTC().Format(time.RFC3339)
	if w := do(t, e.h, http.MethodPut, path, "admin-token", `{"until":"`+far+`","reason":"x"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("more than 7 days was accepted: %d", w.Code)
	}
	soon := time.Now().Add(3 * time.Hour).UTC().Format(time.RFC3339)
	if w := do(t, e.h, http.MethodPut, path, "auditor-token", `{"until":"`+soon+`","reason":"x"}`); w.Code != http.StatusForbidden {
		t.Fatalf("an auditor kept an instance running: %d", w.Code)
	}
	if w := do(t, e.h, http.MethodPut, path, "admin-token", `{"until":"`+soon+`","reason":"load test"}`); w.Code != http.StatusOK {
		t.Fatalf("keep running: %d %s", w.Code, w.Body)
	}
	if w := do(t, e.h, http.MethodDelete, path, "admin-token", ""); w.Code != http.StatusNoContent {
		t.Fatalf("clear: %d %s", w.Code, w.Body)
	}
	var n int
	_ = e.s.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE action LIKE 'cloud.keep_running.%' AND target LIKE '%'||$1||'%'`,
		e.instance()).Scan(&n)
	if n != 2 {
		t.Fatalf("want set and cleared audited, got %d", n)
	}
}

func TestCloudScheduleListShowsWhatItMatches(t *testing.T) {
	e := scheduleAPI(t)
	e.make(t, "")
	w := do(t, e.h, http.MethodGet, "/api/v1/cloud/schedules", "auditor-token", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	var out struct {
		Schedules []CloudScheduleView `json:"schedules"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	for _, v := range out.Schedules {
		if v.Name != e.name {
			continue
		}
		// m6i.xlarge at 0.192/h, off 13 of 24 hours a day.
		if v.Matched != 1 || v.HoursPerWeek != 77 || v.Saving.Priced != 1 || v.Saving.Monthly < 75 || v.Saving.Monthly > 77 || len(v.Next) == 0 {
			t.Fatalf("view: %+v", v)
		}
		return
	}
	t.Fatal("the schedule is not listed")
}

func TestScheduleTagIsAChange(t *testing.T) {
	e := scheduleAPI(t)
	c := e.ask(t, "admin-token", e.changeEnv.body("tags", `,"spec":{"set":{"ArgusSchedule":"nights"}}`), http.StatusAccepted)
	if c.Level != cloud.LevelChange || c.State != "awaiting_approval" {
		t.Fatalf("tagging onto a schedule on Production must wait: %+v", c)
	}
}
