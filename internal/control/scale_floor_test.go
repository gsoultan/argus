package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

// A group with a minimum of 2 gets an Auto Scaling group that keeps one
// copy besides the server it was made from, once its image is ready. The
// copy AWS starts joins the group, and its agent enrolls with the group's
// key, which takes only that server, once. Handed to AWS, the Auto Scaling
// group grows to the maximum with a policy that a pause turns off.
func TestScaleFloor(t *testing.T) {
	e := scaleEnv{changeEnv: networkAPI(t), t: t}
	ctx := context.Background()
	clean := func() {
		_, _ = e.s.pool.Exec(ctx, `DELETE FROM agent_credentials WHERE hostname LIKE 'pay-api-i-%'`)
		_, _ = e.s.pool.Exec(ctx, `DELETE FROM agent_enrollment_tokens t WHERE scale_group_id IS NOT NULL
			AND NOT EXISTS (SELECT 1 FROM agent_credentials c WHERE c.token_id = t.id)`)
	}
	clean()
	t.Cleanup(func() {
		clean()
		_, _ = e.s.pool.Exec(ctx, `DELETE FROM scale_groups WHERE name = 'pay-api'`)
		_, _ = e.s.pool.Exec(ctx, `DELETE FROM usage_history WHERE hostname LIKE 'pay-api-%'`)
	})
	src := e.instance()
	e.setFake(cloud.KindInstance, src, func(r *cloud.Resource) {
		r.Network = "vpc-1"
		r.Links = append(r.Links, cloud.Ref{Kind: cloud.KindSubnet, ID: e.account + "-subnet-1"})
	})
	e.resync(t)
	body := func(action, resource, spec string) string {
		return `{"accountId":"` + e.acct.ID + `","region":"` + storageRegion + `","action":"` + action + `","resourceId":"` + resource +
			`","spec":` + spec + `}`
	}
	create := `{"name":"pay-api","from":"` + src + `","targetGroup":"arn:tg/web","limits":{"min":2,"max":4,"costCap":900,"addWait":10,"removeWait":30},` +
		`"rules":{"add":[{"signal":"cpu","over":"average","op":"above","value":75,"minutes":10}],` +
		`"remove":[{"signal":"cpu","over":"average","op":"below","value":25,"minutes":30}]},"mode":"ask"}`
	// AWS cannot keep to a cost cap, so its maximum must.
	tight := strings.Replace(strings.Replace(create, `"ask"`, `"aws"`, 1), `"costCap":900`, `"costCap":200`, 1)
	e.ask(t, "admin-token", body("scale-group-create", "", tight), http.StatusBadRequest)
	c := e.decide(t, "approver-token", e.ask(t, "admin-token", body("scale-group-create", "", create), http.StatusAccepted).ID, true)
	var made cloud.Result
	_ = json.Unmarshal(c.Result, &made)
	gid := made.ResourceID
	e.resync(t)
	g := e.group(gid)

	// Nothing on the AWS side until the image is ready.
	now := time.Now().UTC()
	e.a.TickScaleGroups(ctx, now)
	if g = e.group(gid); g.AWSGroup != "" || e.fake.Floors["argus-pay-api"] != nil {
		t.Fatalf("made before the image: %+v", g.ScaleGroup)
	}
	e.setFake(cloud.KindImage, g.Template.Image, func(r *cloud.Resource) { r.State = "available" })
	e.resync(t)
	e.a.TickScaleGroups(ctx, now.Add(time.Minute))
	e.called(t, "floor-create argus-pay-api argus-scale")
	fl := e.fake.Floors["argus-pay-api"]
	if fl == nil || fl.Spec.Count != (cloud.GroupSize{Min: 1, Max: 1, Desired: 1}) || fl.Spec.TargetGroup != "arn:tg/web" ||
		fl.Spec.Tags["argus:scale-group"] != "pay-api" || !strings.HasPrefix(fl.Spec.Template, "argus-pay-api-") ||
		!strings.Contains(fl.Spec.UserData, `h='pay-api-'"$id"`) {
		t.Fatalf("floor: %+v", fl)
	}
	g = e.group(gid)
	if g.AWSGroup != "argus-pay-api" || g.AWSTemplate == "" || !strings.Contains(g.Note, "AWS is starting 1 in argus-pay-api") ||
		g.AWS.Words != "AWS keeps 1 copy besides pay-api-01" {
		t.Fatalf("group: %+v %q %+v", g.ScaleGroup, g.Note, g.AWS)
	}
	e.hasActivity(g, "aws", "AWS keeps 1 copy besides pay-api-01 even when Argus is down")
	var audited int
	if err := e.s.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action = 'cloud.scale.floor'
		AND actor_email = 'argus-scale' AND target = 'scale group pay-api' AND detail LIKE '%argus-pay-api%'`).Scan(&audited); err != nil || audited == 0 {
		t.Fatalf("no audit entry for the Auto Scaling group: %v %d", err, audited)
	}
	if g.Open != nil {
		t.Fatalf("Argus launched below the minimum itself: %+v", g.Open)
	}

	// AWS starts the copy; it joins as AWS's.
	id := e.fake.ScaleOut(storageRegion, "argus-pay-api")
	e.resync(t)
	e.a.TickScaleGroups(ctx, now.Add(2*time.Minute))
	g = e.group(gid)
	host := "pay-api-" + id
	if m := e.member(g, host); m.State != "joining" || !m.InAWS || m.AddedBy != "aws" {
		t.Fatalf("aws member: %+v", m)
	}
	e.hasActivity(g, "added", "AWS started "+host+" in argus-pay-api to keep the minimum")

	// Its agent enrolls with the group's key, as itself, once.
	tok := regexp.MustCompile(`token=([^&']+)`).FindStringSubmatch(fl.Spec.UserData)
	if tok == nil {
		t.Fatalf("no key in %s", fl.Spec.UserData)
	}
	key, _ := url.QueryUnescape(tok[1])
	enroll := func(hostname string) int {
		return do(t, e.h, http.MethodPost, "/api/v1/agents/enroll", "", `{"token":"`+key+`","hostname":"`+hostname+`"}`).Code
	}
	if code := enroll("pay-api-01"); code != http.StatusUnauthorized {
		t.Fatalf("another name: %d", code)
	}
	if code := enroll("pay-api-i-0123456789abcdef0"); code != http.StatusUnauthorized {
		t.Fatalf("a server AWS did not start: %d", code)
	}
	if code := enroll(host); code != http.StatusOK {
		t.Fatalf("enroll: %d", code)
	}
	if code := enroll(host); code != http.StatusConflict {
		t.Fatalf("enrolled twice: %d", code)
	}
	// A second server AWS starts uses the same key.
	id2 := e.fake.ScaleOut(storageRegion, "argus-pay-api")
	if code := enroll("pay-api-" + id2); code != http.StatusOK {
		t.Fatalf("second: %d", code)
	}

	// Argus never takes AWS's copy away.
	e.ask(t, "admin-token", body("scale-remove", gid, `{}`), http.StatusConflict)

	// Handed to AWS: the group grows to the maximum less the source, with
	// the policy on; Argus's own add is refused.
	toAWS := e.ask(t, "admin-token", body("scale-group-change", gid, `{"mode":"aws","track":{"metric":"cpu","target":55}}`), http.StatusAccepted)
	if !strings.Contains(toAWS.Summary, "AWS Auto Scaling decides, even when Argus is down") ||
		!strings.Contains(toAWS.Summary, "AWS keeps the group's processor near 55%") {
		t.Fatalf("to aws: %q", toAWS.Summary)
	}
	e.decide(t, "approver-token", toAWS.ID, true)
	e.called(t, "group-size argus-pay-api "+e.admin)
	e.called(t, "floor-policy argus-pay-api "+e.admin)
	if p := fl.Policy; p == nil || !p.On || p.Track != (cloud.AWSTrack{Metric: "cpu", Target: 55}) {
		t.Fatalf("policy: %+v", p)
	}
	e.resync(t)
	if g = e.group(gid); g.Mode != "aws" || g.AWS.Min == nil || *g.AWS.Min != 1 || *g.AWS.Max != 3 {
		t.Fatalf("aws mode: %+v %+v", g.ScaleGroup, g.AWS)
	}
	e.ask(t, "admin-token", body("scale-add", gid, `{}`), http.StatusConflict)
	e.a.TickScaleGroups(ctx, now.Add(3*time.Minute))
	if g = e.group(gid); !strings.HasPrefix(g.Note, "AWS decides, keeping the group's processor near 55%") {
		t.Fatalf("note: %q", g.Note)
	}

	// Paused, AWS stops too.
	if p := e.ask(t, "admin-token", body("scale-group-change", gid, `{"pause":true}`), http.StatusOK); !strings.Contains(p.Summary, "AWS stops") {
		t.Fatalf("pause: %q", p.Summary)
	}
	if fl.Policy.On {
		t.Fatal("the policy is still on after a pause")
	}
}
