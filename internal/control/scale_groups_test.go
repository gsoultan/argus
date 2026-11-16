package control

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

// scaleEnv is a production account with pay-api-01 behind front through
// the target group web, and its metrics.
type scaleEnv struct {
	changeEnv
	t *testing.T
}

func (e scaleEnv) usage(host string, from, to time.Time, cpu, memUsed float64) {
	e.t.Helper()
	ctx := context.Background()
	_, _ = e.s.pool.Exec(ctx, `DELETE FROM usage_history WHERE hostname = $1`, host)
	for b := from.Truncate(5 * time.Minute); !b.After(to); b = b.Add(5 * time.Minute) {
		if _, err := e.s.pool.Exec(ctx, `INSERT INTO usage_history (hostname, bucket, samples, cpu_sum, cpu_n, mem_pct)
			VALUES ($1, $2, 5, $3, 5, $4)`, host, b, cpu*5, memUsed); err != nil {
			e.t.Fatal(err)
		}
	}
}

// setFake changes the fake's resource by name.
func (e scaleEnv) setFake(kind cloud.Kind, name string, f func(*cloud.Resource)) string {
	e.t.Helper()
	res := e.fake.Resources[storageRegion]
	for i := range res {
		if res[i].Kind == kind && (res[i].Name == name || res[i].ID == name) {
			f(&res[i])
			return res[i].ID
		}
	}
	e.t.Fatalf("no %s %s in the fake", kind, name)
	return ""
}

func (e scaleEnv) group(id string) scaleGroupDetail {
	e.t.Helper()
	w := do(e.t, e.h, http.MethodGet, "/api/v1/cloud/scale-groups/"+id, "admin-token", "")
	if w.Code != http.StatusOK {
		e.t.Fatalf("group: %d %s", w.Code, w.Body)
	}
	var d scaleGroupDetail
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	return d
}

func (e scaleEnv) member(d scaleGroupDetail, host string) ScaleMember {
	for _, m := range d.Members {
		if m.Hostname == host {
			return m
		}
	}
	e.t.Fatalf("%s is not in %s: %+v", host, d.Name, d.Members)
	return ScaleMember{}
}

func (e scaleEnv) hasActivity(d scaleGroupDetail, kind, contains string) {
	e.t.Helper()
	for _, a := range d.Activity {
		if a.Kind == kind && strings.Contains(a.Detail, contains) {
			return
		}
	}
	e.t.Fatalf("no %s activity with %q: %+v", kind, contains, d.Activity)
}

// A group made from pay-api-01 asks to add a server when it is busy, lapses
// a question nobody answers, adds one when approved, puts it in the target
// group once it runs; turned automatic it adds by itself up to the maximum,
// and takes the newest away when quiet, draining it first.
func TestScaleGroups(t *testing.T) {
	e := scaleEnv{changeEnv: networkAPI(t), t: t}
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = e.s.pool.Exec(ctx, `DELETE FROM scale_groups WHERE name = 'pay-api'`)
		_, _ = e.s.pool.Exec(ctx, `DELETE FROM usage_history WHERE hostname LIKE 'pay-api-%'`)
	})
	src := e.instance()
	e.setFake(cloud.KindInstance, src, func(r *cloud.Resource) {
		r.Network = "vpc-1"
		r.Links = append(r.Links, cloud.Ref{Kind: cloud.KindSubnet, ID: e.account + "-subnet-1"})
	})
	e.resync(t)

	// The wizard's answers from the server.
	w := do(t, e.h, http.MethodGet, "/api/v1/cloud/scale-groups/draft?accountId="+e.acct.ID+"&region="+storageRegion+"&from="+src, "admin-token", "")
	var d scaleDraft
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if w.Code != http.StatusOK || d.Name != "pay-api" || d.TargetGroup != "arn:tg/web" || d.Problem != "" || len(d.Subnets) != 1 {
		t.Fatalf("draft: %d %+v", w.Code, d)
	}

	body := func(action, resource, spec string) string {
		return `{"accountId":"` + e.acct.ID + `","region":"` + storageRegion + `","action":"` + action + `","resourceId":"` + resource +
			`","spec":` + spec + `}`
	}
	create := `{"name":"pay-api","from":"` + src + `","targetGroup":"arn:tg/web","limits":{"min":1,"max":3,"costCap":900,"addWait":10,"removeWait":30},` +
		`"rules":{"add":[{"signal":"cpu","over":"average","op":"above","value":75,"minutes":10}],` +
		`"remove":[{"signal":"cpu","over":"average","op":"below","value":25,"minutes":30}]},"mode":"ask"}`
	e.ask(t, "admin-token", body("scale-group-create", "", strings.Replace(create, `"pay-api"`, `"Pay API"`, 1)), http.StatusBadRequest)
	noCap := strings.Replace(strings.Replace(create, `"ask"`, `"auto"`, 1), `"costCap":900`, `"costCap":0`, 1)
	e.ask(t, "admin-token", body("scale-group-create", "", noCap), http.StatusBadRequest)
	c := e.ask(t, "admin-token", body("scale-group-create", "", create), http.StatusAccepted)
	if !strings.HasPrefix(c.Summary, "Make scale group pay-api from pay-api-01: a new image of pay-api-01 (no reboot), m6i.xlarge in 1 zone, in web behind front, 1 to 3 servers (about $") ||
		!strings.HasSuffix(c.Summary, "a month, cap $900.00), ask each time") {
		t.Fatalf("create: %q", c.Summary)
	}
	c = e.decide(t, "approver-token", c.ID, true)
	if c.State != "applied" {
		t.Fatalf("create applied: %+v", c)
	}
	e.called(t, "image-create "+src+" reboot=false "+e.admin)
	var made cloud.Result
	_ = json.Unmarshal(c.Result, &made)
	gid := made.ResourceID
	if gid == "" || !strings.Contains(made.Detail, "Made image") {
		t.Fatalf("result: %s", c.Result)
	}
	e.resync(t)
	g := e.group(gid)
	if g.Mode != "ask" || len(g.Members) != 1 || g.Members[0].State != "member" || g.Template.Size != "m6i.xlarge" {
		t.Fatalf("group: %+v", g.ScaleGroup)
	}
	e.hasActivity(g, "created", "approved by "+e.approve)

	// Busy, but the image is still being made.
	now := time.Now().UTC()
	e.usage("pay-api-01", now.Add(-time.Hour), now.Add(3*time.Hour), 90, 50)
	e.a.TickScaleGroups(ctx, now)
	if g = e.group(gid); !strings.Contains(g.Note, "image") {
		t.Fatalf("image pending: %q", g.Note)
	}
	e.setFake(cloud.KindImage, g.Template.Image, func(r *cloud.Resource) { r.State = "available" })
	e.resync(t)

	// Ask each time: a question in Approvals, which lapses unanswered.
	e.a.TickScaleGroups(ctx, now)
	g = e.group(gid)
	if g.Open == nil || g.Open.State != "awaiting_approval" || g.Open.RequestedBy != "argus-scale" ||
		!strings.HasPrefix(g.Open.Summary, "Add pay-api-02 to pay-api: launch it from image of pay-api-01 (m6i.xlarge, ap-southeast-3a), register it in web when it runs") {
		t.Fatalf("asked: %+v", g.Open)
	}
	e.hasActivity(g, "rule", "group average processor above 75% for 10 min (90%): pay-api-01 90%")
	e.a.TickScaleGroups(ctx, now.Add(31*time.Minute))
	g = e.group(gid)
	e.hasActivity(g, "lapsed", "nobody answered in 30 minutes")
	if lapsed, _ := e.s.CloudChange(ctx, g.Activity[0].ChangeID); lapsed.State != "lapsed" || g.Open != nil ||
		!strings.Contains(g.Note, "asks again after 10 minutes") {
		t.Fatalf("lapsed: %+v %q", lapsed.State, g.Note)
	}
	e.a.TickScaleGroups(ctx, now.Add(35*time.Minute))
	if g = e.group(gid); g.Open != nil {
		t.Fatalf("asked again inside the wait: %+v", g.Open)
	}

	// Asked again and approved: Argus launches pay-api-02 unprotected,
	// tagged with the group, with its agent.
	e.a.TickScaleGroups(ctx, now.Add(42*time.Minute))
	g = e.group(gid)
	if g.Open == nil {
		t.Fatalf("not asked again: %q", g.Note)
	}
	add := e.decide(t, "approver-token", g.Open.ID, true)
	if add.State != "applied" {
		t.Fatalf("add: %+v", add)
	}
	e.called(t, "launch pay-api-02 argus-scale")
	stamp := func(at time.Time) {
		if _, err := e.s.pool.Exec(ctx, `UPDATE scale_groups SET last_add_at = $2 WHERE id::text = $1`, gid, at); err != nil {
			t.Fatal(err)
		}
	}
	stamp(now.Add(42 * time.Minute))
	if l := e.fake.LastLaunch; l.Protect || l.Tags["argus:scale-group"] != "pay-api" || l.Subnet != e.account+"-subnet-1" ||
		!strings.Contains(e.fake.LastUserData, "pay-api-02") {
		t.Fatalf("launch: %+v", l)
	}
	g = e.group(gid)
	if m := e.member(g, "pay-api-02"); m.State != "joining" {
		t.Fatalf("joining: %+v", m)
	}
	// Running: into the target group; healthy: a member.
	newID := e.setFake(cloud.KindInstance, "pay-api-02", func(r *cloud.Resource) { r.State = "running" })
	e.resync(t)
	e.a.TickScaleGroups(ctx, now.Add(43*time.Minute))
	e.called(t, "target-register arn:tg/web "+newID+" argus-scale")
	e.setFake(cloud.KindTargetGroup, "web", func(r *cloud.Resource) {
		for i := range r.Links {
			if r.Links[i].ID == newID {
				r.Links[i].Name = "healthy"
			}
		}
	})
	e.resync(t)
	e.a.TickScaleGroups(ctx, now.Add(44*time.Minute))
	g = e.group(gid)
	if m := e.member(g, "pay-api-02"); m.State != "member" {
		t.Fatalf("member: %+v %q", m, g.Note)
	}

	// Automatic needs a cost cap, and on Production waits for approval.
	e.ask(t, "admin-token", body("scale-group-change", gid, `{"mode":"auto","limits":{"min":1,"max":3,"costCap":0,"addWait":10,"removeWait":30}}`), http.StatusBadRequest)
	auto := e.ask(t, "admin-token", body("scale-group-change", gid, `{"mode":"auto"}`), http.StatusAccepted)
	if auto.Summary != "Change scale group pay-api: Argus adds and takes away by itself within the limits" {
		t.Fatalf("auto: %q", auto.Summary)
	}
	e.decide(t, "approver-token", auto.ID, true)

	// Still busy: a third server by itself, as argus-scale, naming who
	// turned automatic on; then the maximum holds.
	e.usage("pay-api-02", now.Add(-time.Hour), now.Add(3*time.Hour), 90, 50)
	e.a.TickScaleGroups(ctx, now.Add(50*time.Minute))
	if g = e.group(gid); !strings.Contains(g.Note, "waits 10 minutes") {
		t.Fatalf("cooldown: %q", g.Note)
	}
	e.a.TickScaleGroups(ctx, now.Add(53*time.Minute))
	g = e.group(gid)
	e.hasActivity(g, "added", "pay-api-03")
	e.hasActivity(g, "added", "automatic, turned on by "+e.admin)
	e.called(t, "launch pay-api-03 argus-scale")
	stamp(now.Add(53 * time.Minute))
	third := e.setFake(cloud.KindInstance, "pay-api-03", func(r *cloud.Resource) { r.State = "running" })
	e.resync(t)
	e.a.TickScaleGroups(ctx, now.Add(54*time.Minute))
	e.called(t, "target-register arn:tg/web "+third+" argus-scale")
	e.setFake(cloud.KindTargetGroup, "web", func(r *cloud.Resource) {
		for i := range r.Links {
			if r.Links[i].ID == third {
				r.Links[i].Name = "healthy"
			}
		}
	})
	e.resync(t)
	e.a.TickScaleGroups(ctx, now.Add(55*time.Minute))
	e.usage("pay-api-03", now.Add(-time.Hour), now.Add(3*time.Hour), 90, 50)
	e.a.TickScaleGroups(ctx, now.Add(65*time.Minute))
	if g = e.group(gid); !strings.Contains(g.Note, "at its maximum of 3") {
		t.Fatalf("maximum: %q", g.Note)
	}

	// Quiet: the newest goes, drained first, never the server it was made
	// from.
	for _, h := range []string{"pay-api-01", "pay-api-02", "pay-api-03"} {
		e.usage(h, now.Add(30*time.Minute), now.Add(4*time.Hour), 10, 40)
	}
	e.a.TickScaleGroups(ctx, now.Add(100*time.Minute))
	g = e.group(gid)
	if m := e.member(g, "pay-api-03"); m.State != "draining" {
		t.Fatalf("draining: %+v %q", m, g.Note)
	}
	e.called(t, "target-deregister arn:tg/web "+third+" argus-scale")
	e.a.TickScaleGroups(ctx, now.Add(106*time.Minute))
	e.called(t, "remove "+third+" snapshots=false argus-scale")
	g = e.group(gid)
	if m := e.member(g, "pay-api-03"); m.State != "gone" {
		t.Fatalf("gone: %+v", m)
	}
	e.hasActivity(g, "terminated", "Drained pay-api-03 for 300 s")

	// The list shows the group; pausing stops it at once, on Production too.
	w = do(t, e.h, http.MethodGet, "/api/v1/cloud/scale-groups", "admin-token", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"name":"pay-api"`) {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	p := e.ask(t, "admin-token", body("scale-group-change", gid, `{"pause":true}`), http.StatusOK)
	if p.State != "applied" {
		t.Fatalf("pause: %+v", p)
	}
	if g = e.group(gid); g.State != "paused" {
		t.Fatalf("paused: %+v", g.ScaleGroup)
	}
}
