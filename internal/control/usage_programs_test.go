package control

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"
)

/*
Busiest programs over time: each report's programs are kept with its
5-minute bucket at their busiest, a stretch answers each program's peaks
and how often it was named, a figure the agent did not measure stays
missing rather than 0, and only the fleet roles read it.
*/

func TestProgramsKeptWithHistory(t *testing.T) {
	a, s, h, _ := enrollAPI(t)
	operator, _ := newAccount(t, s, "operator")
	a.UserTokens["operator-token"] = operator
	asset, cred := enrolledAsset(t, s, h)

	now := time.Now().UTC()
	b0 := now.Truncate(historyBucket).Add(-time.Hour)
	beat := func(at time.Time, top string) {
		t.Helper()
		body := fmt.Sprintf(`{"hostname":%q,"version":"0.9.9","usage":{"at":%q,"cpu_busy":50,`+
			`"mem_total":1000,"mem_available":500,"swap_total":0,"swap_free":0,"disks":[],"top":[%s]}}`,
			asset.Hostname, at.Format(time.RFC3339), top)
		if w := do(t, h, http.MethodPost, "/api/v1/report/heartbeat", cred, body); w.Code != http.StatusOK {
			t.Fatalf("heartbeat: %d %s", w.Code, w.Body)
		}
	}
	// Two reports in one bucket: postgres busier in the second, by
	// processor but not memory; mysqld's disk refused; sshd from an agent
	// that names no disk; java only in the first.
	beat(b0.Add(30*time.Second), `{"name":"postgres","pid":10,"count":12,"cpu":20,"memory":900,"read_bps":100,"write_bps":5000},`+
		`{"name":"mysqld","pid":20,"cpu":1,"memory":400,"io_hidden":true},{"name":"java","pid":30,"cpu":60,"memory":2000,"read_bps":0,"write_bps":0}`)
	beat(b0.Add(90*time.Second), `{"name":"postgres","pid":11,"count":14,"cpu":45,"memory":800,"read_bps":300,"write_bps":1000},`+
		`{"name":"mysqld","pid":20,"cpu":2,"memory":410,"io_hidden":true},{"name":"sshd","pid":40,"cpu":0.5,"memory":10}`)
	// Another bucket, 20 minutes on: only postgres.
	beat(b0.Add(20*time.Minute), `{"name":"postgres","pid":11,"count":3,"cpu":5,"memory":700,"read_bps":0,"write_bps":0}`)

	get := func(token string, from, to time.Time) (int, AssetPrograms) {
		t.Helper()
		q := url.Values{"from": {from.Format(time.RFC3339)}, "to": {to.Format(time.RFC3339)}}
		w := do(t, h, http.MethodGet, "/api/v1/assets/"+asset.ID+"/programs?"+q.Encode(), token, "")
		var p AssetPrograms
		_ = json.Unmarshal(w.Body.Bytes(), &p)
		return w.Code, p
	}
	code, p := get("admin-token", b0, b0.Add(historyBucket))
	if code != http.StatusOK || p.Periods != 1 || len(p.Programs) != 4 {
		t.Fatalf("one bucket: %d %+v", code, p)
	}
	by := map[string]ProgramUse{}
	for _, x := range p.Programs {
		by[x.Name] = x
	}
	if p.Programs[0].Name != "java" {
		t.Errorf("first = %s, want the busiest by processor", p.Programs[0].Name)
	}
	pg := by["postgres"]
	if pg.CPU == nil || *pg.CPU != 45 || pg.Memory != 900 || pg.Count != 14 || *pg.ReadBps != 300 || *pg.WriteBps != 5000 || pg.Periods != 1 {
		t.Errorf("postgres = %+v, want each figure's peak across the bucket's reports", pg)
	}
	if my := by["mysqld"]; !my.Hidden || my.ReadBps != nil || my.Count != 1 {
		t.Errorf("mysqld = %+v, want disk hidden and no rate", my)
	}
	if ss := by["sshd"]; ss.Hidden || ss.ReadBps != nil {
		t.Errorf("sshd = %+v, want no rate from an agent that did not measure one, and not hidden", ss)
	}

	// The whole hour: postgres in both periods, the rest in one.
	_, p = get("admin-token", b0, b0.Add(time.Hour))
	if p.Periods != 2 {
		t.Errorf("periods = %d, want 2", p.Periods)
	}
	for _, x := range p.Programs {
		if x.Name == "postgres" && x.Periods != 2 {
			t.Errorf("postgres periods = %d, want 2", x.Periods)
		}
	}
	// A stretch with nothing reported answers no programs, not an error.
	if code, p := get("admin-token", b0.Add(-3*time.Hour), b0.Add(-2*time.Hour)); code != http.StatusOK || p.Periods != 0 || len(p.Programs) != 0 {
		t.Errorf("empty stretch = %d %+v", code, p)
	}
	// Refusals: an operator; a stretch backwards or past 30 days.
	if code, _ := get("operator-token", b0, b0.Add(time.Hour)); code != http.StatusForbidden {
		t.Errorf("operator = %d, want 403", code)
	}
	if code, _ := get("admin-token", b0.Add(time.Hour), b0); code != http.StatusBadRequest {
		t.Errorf("backwards = %d, want 400", code)
	}
	if code, _ := get("admin-token", now.Add(-40*24*time.Hour), now); code != http.StatusBadRequest {
		t.Errorf("40 days = %d, want 400", code)
	}

	// The System card's Now table carries the grouping too.
	sys := getSystem(t, h, "/api/v1/assets/"+asset.ID+"/system")
	if len(sys.Usage.Top) != 1 || sys.Usage.Top[0].Count != 3 || sys.Usage.Top[0].ReadBps == nil {
		t.Errorf("system top = %+v", sys.Usage.Top)
	}
}

// A report from an agent older than grouping still lands: one process
// each, no disk figures.
func TestBoundedTopKeepsOldAndNewAgents(t *testing.T) {
	cpu, bad, rd := 12.0, -1.0, 2e3
	got := boundedTop([]HostProcess{
		{Name: "old", PID: 1, CPU: &cpu, Memory: 10},
		{Name: "new", PID: 2, Count: 4, CPU: &cpu, Memory: 10, ReadBps: &rd, WriteBps: &rd},
		{Name: "half", PID: 3, Memory: 1, ReadBps: &rd},
		{Name: "broken", PID: 4, Count: 1 << 20, Memory: 1, ReadBps: &bad, WriteBps: &rd},
	})
	if len(got) != 4 || got[0].Count != 0 || got[0].ReadBps != nil {
		t.Fatalf("old = %+v", got)
	}
	if got[1].Count != 4 || got[1].ReadBps == nil || *got[1].WriteBps != 2e3 {
		t.Errorf("new = %+v", got[1])
	}
	if got[2].ReadBps != nil || got[2].WriteBps != nil {
		t.Errorf("a read rate with no write rate kept: %+v", got[2])
	}
	if got[3].Count != 0 || got[3].ReadBps != nil {
		t.Errorf("impossible figures kept: %+v", got[3])
	}
}
