package control

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Changing a server's firewall and firewall policies: how a line someone
writes becomes what the agent holds, what a change may not do, the
approval, the heartbeat that carries it and how its end is kept, and a
policy's rollout.

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable \
	  go test ./internal/control/ -run 'FW|Firewall'
*/

/* ── Without a database ──────────────────────────────────────────────────── */

func TestFWLineValidate(t *testing.T) {
	l := FWLine{Protocol: "tcp", Ports: " 443, 8443 ", From: []FWSource{{Kind: FWFromAddress, Value: "10.0.0.5"},
		{Kind: FWFromAddress, Value: "10.0.0.5"}, {Kind: FWFromPrivate, Value: "junk"}}}
	if err := l.validate(); err != nil {
		t.Fatal(err)
	}
	if l.Direction != "in" || l.Action != "allow" || l.Ports != "443,8443" || len(l.From) != 2 || l.From[1].Value != "" {
		t.Errorf("normal form: %+v", l)
	}
	every := FWLine{Protocol: "any", From: []FWSource{{Kind: FWFromAddress, Value: "10.0.0.5"}, {Kind: FWFromEveryone}}}
	if err := every.validate(); err != nil || len(every.From) != 1 || !every.opensToEveryone() {
		t.Errorf("everyone swallows the rest: %+v %v", every, err)
	}
	for name, l := range map[string]FWLine{
		"nobody":         {Protocol: "tcp", Ports: "22"},
		"no ports":       {Protocol: "tcp", From: []FWSource{{Kind: FWFromPrivate}}},
		"ports on any":   {Protocol: "any", Ports: "22", From: []FWSource{{Kind: FWFromPrivate}}},
		"bad address":    {Protocol: "tcp", Ports: "22", From: []FWSource{{Kind: FWFromAddress, Value: "10.0.0.1; reboot"}}},
		"anywhere typed": {Protocol: "tcp", Ports: "22", From: []FWSource{{Kind: FWFromAddress, Value: "0.0.0.0/0"}}},
		"server by name": {Protocol: "tcp", Ports: "22", From: []FWSource{{Kind: FWFromServer, Value: "web-01"}}},
		"unknown kind":   {Protocol: "tcp", Ports: "22", From: []FWSource{{Kind: "world"}}},
	} {
		if err := l.validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestFWResolveNeverWidens(t *testing.T) {
	r := fwResolver{
		addrs:    map[string][]string{"a1": {"10.1.0.5"}, "a2": {"10.1.0.6", "fd00::6"}},
		tags:     map[string][]string{"web": {"a1", "a2"}},
		policies: map[string][]string{"p1": {"a2"}},
	}
	l := FWLine{Direction: "in", Action: "allow", Protocol: "tcp", Ports: "5432",
		From: []FWSource{{Kind: FWFromTag, Value: "Web"}, {Kind: FWFromAddress, Value: "192.0.2.9"}}}
	got, ok := r.resolve(l, "sabc")
	if !ok || !slices.Equal(got.Sources, []string{"10.1.0.5", "10.1.0.6", "192.0.2.9", "fd00::6"}) {
		t.Errorf("tag and address: %+v %v", got, ok)
	}
	// Servers with no address yet: the line is left off, not sent with no
	// sources, which every firewall reads as "from anywhere".
	none := FWLine{Direction: "in", Action: "allow", Protocol: "tcp", Ports: "22",
		From: []FWSource{{Kind: FWFromServer, Value: "a9"}, {Kind: FWFromPolicy, Value: "p9"}}}
	if _, ok := r.resolve(none, "sdef"); ok {
		t.Error("a line naming servers with no address was kept")
	}
	if got, ok := r.resolve(FWLine{Direction: "in", Action: "allow", Protocol: "any", From: []FWSource{{Kind: FWFromEveryone}}}, "s1"); !ok || len(got.Sources) != 0 {
		t.Errorf("everyone: %+v", got)
	}
	if got, _ := r.resolve(FWLine{Direction: "in", Action: "allow", Protocol: "any", From: []FWSource{{Kind: FWFromPolicy, Value: "p1"}}}, "s2"); !slices.Equal(got.Sources, []string{"10.1.0.6", "fd00::6"}) {
		t.Errorf("servers with a policy: %+v", got)
	}
}

func TestFWDeltaApply(t *testing.T) {
	past := time.Now().Add(-time.Minute)
	st := fwServerState{Own: []FWLine{{Key: "sa"}, {Key: "sb"}, {Key: "sc", Until: &past}}, Policies: map[string]int{"p1": 2}}
	got := FWDelta{Add: []FWLine{{Key: "sd", Direction: "in", Action: "allow"}}, Remove: []string{"sa"},
		Policies: map[string]int{"p1": 0, "p2": 1}}.apply(st, time.Now())
	keys := []string{}
	for _, l := range got.Own {
		keys = append(keys, l.Key)
	}
	if !slices.Equal(keys, []string{"sb", "sd"}) || got.Policies["p2"] != 1 || len(got.Policies) != 1 {
		t.Errorf("after: %v %v", keys, got.Policies)
	}
	if len(st.Own) != 3 || st.Policies["p1"] != 2 {
		t.Error("apply changed what it was given")
	}
}

func TestLineOfRuleMadeByHand(t *testing.T) {
	l, err := lineOfRule(tunnel.FirewallRule{Direction: "in", Action: "allow", Protocol: "tcp", Ports: "6000:6010",
		Sources: []string{"10.8.0.0/24"}})
	if err != nil || l.Ports != "6000-6010" || l.From[0].Kind != FWFromAddress || l.From[0].Value != "10.8.0.0/24" {
		t.Errorf("a range from a network: %+v %v", l, err)
	}
	if l, _ := lineOfRule(tunnel.FirewallRule{Direction: "in", Action: "allow", Protocol: "tcp", Ports: "80",
		Sources: []string{"Anywhere"}}); l.From[0].Kind != FWFromEveryone {
		t.Errorf("anywhere: %+v", l)
	}
	if _, err := lineOfRule(tunnel.FirewallRule{Direction: "in", Action: "allow", Protocol: "tcp", Service: "ssh"}); err == nil {
		t.Error("a firewalld service has no ports to keep")
	}
}

func TestFWPortOpen(t *testing.T) {
	fw := tunnel.HostFirewall{Rules: []tunnel.FirewallRule{{Direction: "in", Action: "allow", Protocol: "tcp", Ports: "22", Raw: "22/tcp ALLOW IN Anywhere"}}}
	if !fwPortOpen(fw, nil, nil, 22) {
		t.Error("a rule made by hand lets 22 in")
	}
	if fwPortOpen(fw, nil, []string{"22/tcp ALLOW IN Anywhere"}, 22) {
		t.Error("a rule being taken over does not count")
	}
	if !fwPortOpen(fw, []tunnel.FirewallLine{{Direction: "in", Action: "allow", Protocol: "tcp", Ports: "20-30"}}, []string{"22/tcp ALLOW IN Anywhere"}, 22) {
		t.Error("a line covering 22 counts")
	}
}

func TestFirewallProtectLines(t *testing.T) {
	got := FirewallProtectLines([]string{":8443", "", "127.0.0.1:443"}, "0.0.0.0:4433")
	if len(got) != 2 || got[0].Ports != "80,443,8443" || got[1].Protocol != "udp" || got[1].Ports != "4433" {
		t.Errorf("protect: %+v", got)
	}
	for _, l := range got {
		if err := l.Validate(); err != nil {
			t.Error(err)
		}
	}
	if k := policyLineKey("0a1b2c3d-0000-0000-0000-000000000000", "ssh"); k != "p0a1b2c3dssh" {
		t.Errorf("policy key: %s", k)
	}
}

/* ── With a database ─────────────────────────────────────────────────────── */

type fwEnv struct{ pkgEnv }

func firewallAPI(t *testing.T) fwEnv {
	t.Helper()
	return fwEnv{packagesAPI(t)}
}

// fwServer is an enrolled server whose agent read a ufw firewall that is
// on, blocks what comes in, and has SSH open to everyone by hand.
func (e fwEnv) server(t *testing.T, tags ...string) (Asset, string) {
	t.Helper()
	asset, cred := e.pkgEnv.server(t, tags...)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = e.s.pool.Exec(ctx, `DELETE FROM firewall_changes WHERE asset_id::text = $1`, asset.ID)
		_, _ = e.s.pool.Exec(ctx, `DELETE FROM firewall_servers WHERE asset_id::text = $1`, asset.ID)
	})
	e.beat(t, cred, asset.Hostname, nil) // the agent's row
	e.setFirewall(t, asset.Hostname, tunnel.HostFirewall{Kind: tunnel.FirewallUFW, Active: true, DefaultIn: "deny",
		DefaultOut: "allow", Manageable: true, ReadAt: time.Now().UTC(), Rules: []tunnel.FirewallRule{
			{Direction: "in", Action: "allow", Protocol: "tcp", Ports: "22", Raw: "22/tcp ALLOW IN Anywhere"},
		}})
	return asset, cred
}

func (e fwEnv) setFirewall(t *testing.T, host string, fw tunnel.HostFirewall) {
	t.Helper()
	raw, _ := json.Marshal(fw)
	if _, err := e.s.pool.Exec(context.Background(), `UPDATE agents SET firewall = $2::jsonb WHERE lower(hostname) = lower($1)`,
		host, raw); err != nil {
		t.Fatal(err)
	}
}

func (e fwEnv) beat(t *testing.T, cred, host string, b *tunnel.FirewallBeat) *tunnel.FirewallOrders {
	t.Helper()
	body := map[string]any{"hostname": host, "version": "1.2.0", "active_sessions": 0}
	if b != nil {
		body["firewall"] = b
	}
	raw, _ := json.Marshal(body)
	w := do(t, e.h, http.MethodPost, "/api/v1/report/heartbeat", cred, string(raw))
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", w.Code, w.Body)
	}
	var out map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	p, ok := out["firewall"]
	if !ok {
		return nil
	}
	var o tunnel.FirewallOrders
	if err := json.Unmarshal(p, &o); err != nil {
		t.Fatalf("firewall: %v", err)
	}
	return &o
}

func (e fwEnv) tab(t *testing.T, asset Asset, token string) ServerFirewall {
	t.Helper()
	w := do(t, e.h, http.MethodGet, "/api/v1/assets/"+asset.ID+"/firewall", token, "")
	if w.Code != http.StatusOK {
		t.Fatalf("tab: %d %s", w.Code, w.Body)
	}
	var v ServerFirewall
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	return v
}

func (e fwEnv) ask(t *testing.T, asset Asset, token, path, body string, want int) []byte {
	t.Helper()
	w := do(t, e.h, http.MethodPost, "/api/v1/assets/"+asset.ID+"/firewall/"+path, token, body)
	if w.Code != want {
		t.Fatalf("%s: %d %s, want %d", path, w.Code, w.Body, want)
	}
	return w.Body.Bytes()
}

func (e fwEnv) change(t *testing.T, asset Asset, token, body string, want int) FirewallChange {
	t.Helper()
	var c FirewallChange
	_ = json.Unmarshal(e.ask(t, asset, token, "changes", body, want), &c)
	return c
}

// run plays the agent: it takes the change on the reply, goes on trial,
// and ends as state, reading back after.
func (e fwEnv) run(t *testing.T, cred, host string, spec *tunnel.FirewallChangeSpec, state string, after *tunnel.HostFirewall) *tunnel.FirewallOrders {
	t.Helper()
	at := time.Now().UTC()
	e.beat(t, cred, host, &tunnel.FirewallBeat{Supported: true, Run: &tunnel.FirewallRun{ID: spec.ID, State: tunnel.FirewallRunTrial, At: at}})
	return e.beat(t, cred, host, &tunnel.FirewallBeat{Supported: true, Run: &tunnel.FirewallRun{ID: spec.ID, State: state, At: at,
		Message: "done", Ran: []tunnel.FirewallRan{{Command: "ufw allow", OK: state == tunnel.FirewallRunApplied}}, After: after}})
}

// A line let in from the Firewall tab, start to end: planned with the
// commands it will run, sent on the heartbeat until the agent has it, and
// what the server holds after it is the line. Then it comes off, and the
// first change is put back.
func TestFirewallLineByHand(t *testing.T) {
	e := firewallAPI(t)
	asset, cred := e.server(t)

	// An agent older than firewall changes: the tab says why not.
	if m := e.tab(t, asset, "admin-token").Manage; m == nil || m.Ready || !m.CanChange || !strings.Contains(m.Reason, "too old") {
		t.Fatalf("before the agent says it can: %+v", m)
	}
	if o := e.beat(t, cred, asset.Hostname, &tunnel.FirewallBeat{Supported: true}); o == nil || o.Change != nil {
		t.Fatalf("orders with nothing to do = %+v", o)
	}
	m := e.tab(t, asset, "admin-token").Manage
	if !m.Ready || m.RemotePort != 22 || len(m.Always) != 3 {
		t.Fatalf("ready: %+v", m)
	}

	// Auditors read; they do not change.
	e.ask(t, asset, "auditor-token", "plan", `{"add":[]}`, http.StatusForbidden)
	// ufw cannot let ping in line by line.
	e.ask(t, asset, "admin-token", "plan", `{"add":[{"protocol":"icmp","from":[{"kind":"private"}]}]}`, http.StatusBadRequest)
	// A block that covers the person asking.
	e.ask(t, asset, "admin-token", "plan", `{"add":[{"action":"deny","protocol":"any","from":[{"kind":"address","value":"192.0.2.0/24"}]}]}`,
		http.StatusBadRequest)

	add := `{"add":[{"protocol":"tcp","ports":"443","from":[{"kind":"private"}],"note":"web"}],"reason":"the new site"}`
	var plan FirewallPlan
	_ = json.Unmarshal(e.ask(t, asset, "admin-token", "plan", add, http.StatusOK), &plan)
	if plan.NeedsApproval || plan.NeedsConfirm != "" || len(plan.Lines) != 1 ||
		!slices.Contains(plan.Preview, "ufw allow in proto tcp from 10.0.0.0/8 to any port 443 comment 'argus fw-"+plan.Lines[0].Key+"'") {
		t.Fatalf("plan = %+v", plan)
	}
	c := e.change(t, asset, "admin-token", add, http.StatusAccepted)
	if c.State != FWQueued || c.Kind != "lines" || !strings.Contains(c.Summary, "Let in TCP 443 from private networks") {
		t.Fatalf("change = %+v", c)
	}
	if a := lastAuditOn(t, e.s, asset.Hostname); a.Action != "firewall.change_requested" || !strings.Contains(a.Detail, "the new site") {
		t.Errorf("audit = %s: %s", a.Action, a.Detail)
	}

	o := e.beat(t, cred, asset.Hostname, &tunnel.FirewallBeat{Supported: true})
	if o.Change == nil || o.Change.ID != c.ID || len(o.Change.Lines) != 1 || o.Change.Lines[0].Ports != "443" ||
		o.Change.Keep || o.Change.TurnOn {
		t.Fatalf("the reply carries the change: %+v", o.Change)
	}
	// Sent until the agent says it has it; then it cannot be taken back.
	if again := e.beat(t, cred, asset.Hostname, &tunnel.FirewallBeat{Supported: true}); again.Change == nil || again.Change.ID != c.ID {
		t.Fatal("the change is sent again until the agent has it")
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/firewall-changes/"+c.ID+"/cancel", "admin-token", ""); w.Code != http.StatusConflict {
		t.Errorf("a sent change was taken back: %d", w.Code)
	}
	key := o.Change.Lines[0].Key
	after := tunnel.HostFirewall{Kind: tunnel.FirewallUFW, Active: true, DefaultIn: "deny", Manageable: true, ReadAt: time.Now().UTC(),
		Rules: []tunnel.FirewallRule{
			{Direction: "in", Action: "allow", Protocol: "tcp", Ports: "22", Raw: "22/tcp ALLOW IN Anywhere"},
			{Direction: "in", Action: "allow", Protocol: "tcp", Ports: "443", Sources: []string{"10.0.0.0/8"}, Name: "argus fw-" + key},
		}}
	if o := e.run(t, cred, asset.Hostname, o.Change, tunnel.FirewallRunApplied, &after); o.Change != nil {
		t.Fatalf("nothing more to send: %+v", o.Change)
	}
	v := e.tab(t, asset, "admin-token")
	if len(v.Firewall.Rules) != 2 || len(v.Manage.Own) != 1 || v.Manage.Own[0].Key != key || v.Manage.Own[0].Note != "web" {
		t.Fatalf("after: %+v / %+v", v.Firewall.Rules, v.Manage.Own)
	}
	first := v.Manage.Changes[0]
	if first.State != FWApplied || !first.CanPutBack || len(first.Ran) != 1 {
		t.Fatalf("first change = %+v", first)
	}
	if a := lastAuditOn(t, e.s, asset.Hostname); a.Action != "firewall.change_applied" || !strings.Contains(a.Detail, "ufw allow (ok)") {
		t.Errorf("audit = %s: %s", a.Action, a.Detail)
	}

	// Take it off: the agent is sent no lines.
	c2 := e.change(t, asset, "admin-token", `{"remove":["`+key+`"]}`, http.StatusAccepted)
	o = e.beat(t, cred, asset.Hostname, &tunnel.FirewallBeat{Supported: true})
	if o.Change == nil || o.Change.ID != c2.ID || len(o.Change.Lines) != 0 {
		t.Fatalf("remove: %+v", o.Change)
	}
	e.run(t, cred, asset.Hostname, o.Change, tunnel.FirewallRunApplied, nil)
	if v := e.tab(t, asset, "admin-token"); len(v.Manage.Own) != 0 {
		t.Fatalf("removed: %+v", v.Manage.Own)
	}

	// Put back the first change: the agent puts back its saved copy, and
	// what Argus keeps is what it held before (nothing).
	c3 := e.change(t, asset, "admin-token", `{"putBack":"`+first.ID+`"}`, http.StatusAccepted)
	o = e.beat(t, cred, asset.Hostname, &tunnel.FirewallBeat{Supported: true})
	if o.Change == nil || o.Change.ID != c3.ID || o.Change.PutBack != first.ID {
		t.Fatalf("put back: %+v", o.Change)
	}
	e.run(t, cred, asset.Hostname, o.Change, tunnel.FirewallRunApplied, nil)
	if v := e.tab(t, asset, "admin-token"); len(v.Manage.Own) != 0 || v.Manage.Changes[0].State != FWApplied {
		t.Fatalf("after put back: %+v", v.Manage)
	}
}

// A change the agent undid (no heartbeat got through on trial) is kept as
// undone, raises the bell, and leaves what Argus keeps as it was.
func TestFirewallChangeUndone(t *testing.T) {
	e := firewallAPI(t)
	asset, cred := e.server(t)
	e.beat(t, cred, asset.Hostname, &tunnel.FirewallBeat{Supported: true})
	e.change(t, asset, "admin-token", `{"add":[{"protocol":"tcp","ports":"8080","from":[{"kind":"address","value":"10.9.0.1"}]}]}`, http.StatusAccepted)
	o := e.beat(t, cred, asset.Hostname, &tunnel.FirewallBeat{Supported: true})
	e.run(t, cred, asset.Hostname, o.Change, tunnel.FirewallRunRolledBack, nil)
	v := e.tab(t, asset, "admin-token")
	if v.Manage.Changes[0].State != FWRolledBack || len(v.Manage.Own) != 0 || v.Manage.Changes[0].CanPutBack {
		t.Fatalf("undone: %+v", v.Manage.Changes[0])
	}
	if a := lastAuditOn(t, e.s, asset.Hostname); a.Action != "firewall.change_undone" {
		t.Errorf("audit = %s", a.Action)
	}
}

// Letting everyone in waits for another admin; an operator changes only
// a server they manage, and always with approval; taking SSH's last rule
// over needs the server's name typed.
func TestFirewallApprovalAndWhoMayChange(t *testing.T) {
	e := firewallAPI(t)
	asset, cred := e.server(t)
	e.beat(t, cred, asset.Hostname, &tunnel.FirewallBeat{Supported: true})

	everyone := `{"add":[{"protocol":"tcp","ports":"80","from":[{"kind":"everyone"}]}]}`
	c := e.change(t, asset, "admin-token", everyone, http.StatusAccepted)
	if c.State != FWAwaitingApproval {
		t.Fatalf("opening to everyone waits: %+v", c)
	}
	if o := e.beat(t, cred, asset.Hostname, &tunnel.FirewallBeat{Supported: true}); o.Change != nil {
		t.Fatal("a change waiting for approval was sent")
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/firewall-changes/"+c.ID+"/approve", "admin-token", ""); w.Code == http.StatusOK {
		t.Fatal("the person who asked approved it")
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/firewall-changes/"+c.ID+"/approve", "second-token", ""); w.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", w.Code, w.Body)
	}
	if o := e.beat(t, cred, asset.Hostname, &tunnel.FirewallBeat{Supported: true}); o.Change == nil || o.Change.ID != c.ID {
		t.Fatal("an approved change is sent")
	}

	// An operator: not until made a manager.
	if w := do(t, e.h, http.MethodGet, "/api/v1/assets/"+asset.ID+"/firewall", "operator-token", ""); w.Code != http.StatusForbidden {
		t.Fatalf("operator reads: %d", w.Code)
	}
	if w := do(t, e.h, http.MethodPut, "/api/v1/assets/"+asset.ID+"/firewall/managers", "operator-token", `{"emails":["x@y"]}`); w.Code != http.StatusForbidden {
		t.Fatalf("operators choose no managers: %d", w.Code)
	}
	var operator string
	_ = e.s.pool.QueryRow(context.Background(), `SELECT email FROM users WHERE role = 'operator' ORDER BY created_at DESC LIMIT 1`).Scan(&operator)
	if w := do(t, e.h, http.MethodPut, "/api/v1/assets/"+asset.ID+"/firewall/managers", "admin-token", `{"emails":["`+strings.ToUpper(operator)+`"]}`); w.Code != http.StatusOK {
		t.Fatalf("managers: %d %s", w.Code, w.Body)
	}
	if m := e.tab(t, asset, "operator-token").Manage; !m.CanChange || len(m.Managers) != 1 {
		t.Fatalf("a manager: %+v", m)
	}
	var plan FirewallPlan
	_ = json.Unmarshal(e.ask(t, asset, "operator-token", "plan", `{"add":[{"protocol":"tcp","ports":"9100","from":[{"kind":"private"}]}]}`, http.StatusOK), &plan)
	if !plan.NeedsApproval {
		t.Error("an operator's change waits for an admin")
	}

	// Taking over the only rule that lets SSH in.
	_ = json.Unmarshal(e.ask(t, asset, "admin-token", "plan", `{"takeOver":["22/tcp ALLOW IN Anywhere"]}`, http.StatusOK), &plan)
	if plan.Kind != "take_over" || plan.NeedsConfirm != "" || len(plan.Lines) != 1 {
		t.Fatalf("taking over keeps SSH open as a line: %+v", plan)
	}
	if m := e.tab(t, asset, "admin-token").Manage; m.Open == nil {
		t.Fatal("the change in flight is shown")
	}
}

// A policy put on a server goes out as lines keyed by the policy, in a
// rollout that ends when the server holds it; changing it moves the server
// to the new version; one failure stops the rollout.
func TestFirewallPolicyRollout(t *testing.T) {
	e := firewallAPI(t)
	tag := unique("fwtag")
	a1, cred1 := e.server(t, tag)
	a2, cred2 := e.server(t, tag)
	for _, s := range []struct {
		a    Asset
		cred string
	}{{a1, cred1}, {a2, cred2}} {
		e.beat(t, s.cred, s.a.Hostname, &tunnel.FirewallBeat{Supported: true})
	}
	name := unique("Web servers")
	w := do(t, e.h, http.MethodPost, "/api/v1/firewall-policies", "admin-token",
		`{"name":"`+name+`","lines":[{"protocol":"tcp","ports":"443","from":[{"kind":"private"}]}]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var p FirewallPolicy
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = e.s.pool.Exec(ctx, `DELETE FROM firewall_rollouts WHERE policy_id::text = $1`, p.ID)
		_, _ = e.s.pool.Exec(ctx, `DELETE FROM firewall_policies WHERE id::text = $1`, p.ID)
	})
	if p.Version != 1 || len(p.Lines) != 1 || p.Lines[0].Key == "" {
		t.Fatalf("policy = %+v", p)
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/firewall-policies", "admin-token", `{"name":"`+strings.ToUpper(name)+`"}`); w.Code != http.StatusConflict {
		t.Errorf("same name: %d", w.Code)
	}

	w = do(t, e.h, http.MethodPost, "/api/v1/firewall-policies/"+p.ID+"/targets", "admin-token", `{"tag":"`+tag+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("target: %d %s", w.Code, w.Body)
	}
	var tr struct{ Rollout *FWRollout }
	_ = json.Unmarshal(w.Body.Bytes(), &tr)
	if tr.Rollout == nil || tr.Rollout.State != FWRolloutRunning || tr.Rollout.Total != 2 {
		t.Fatalf("rollout = %+v", tr.Rollout)
	}
	// One server first: the second waits for it.
	first, firstCred, second, secondCred := a1, cred1, a2, cred2
	if tr.Rollout.Servers[0].AssetID != a1.ID {
		first, firstCred, second, secondCred = a2, cred2, a1, cred1
	}
	if o := e.beat(t, secondCred, second.Hostname, &tunnel.FirewallBeat{Supported: true}); o.Change != nil {
		t.Fatal("the second wave went before the first finished")
	}
	o := e.beat(t, firstCred, first.Hostname, &tunnel.FirewallBeat{Supported: true})
	if o.Change == nil || len(o.Change.Lines) != 1 || o.Change.Lines[0].Key != policyLineKey(p.ID, p.Lines[0].Key) {
		t.Fatalf("policy lines: %+v", o.Change)
	}
	e.run(t, firstCred, first.Hostname, o.Change, tunnel.FirewallRunApplied, nil)
	o = e.beat(t, secondCred, second.Hostname, &tunnel.FirewallBeat{Supported: true})
	if o.Change == nil {
		t.Fatal("the next wave goes once the first is done")
	}
	e.run(t, secondCred, second.Hostname, o.Change, tunnel.FirewallRunApplied, nil)
	w = do(t, e.h, http.MethodGet, "/api/v1/firewall-policies/"+p.ID, "admin-token", "")
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	if p.Servers != 2 || p.Current != 2 || p.Rollout == nil || p.Rollout.State != FWRolloutDone {
		t.Fatalf("done: %+v %+v", p, p.Rollout)
	}
	if m := e.tab(t, first, "admin-token").Manage; len(m.Policies) != 1 || m.Policies[0].Name != name || m.Policies[0].Version != 1 {
		t.Fatalf("held: %+v", m.Policies)
	}
	if w := do(t, e.h, http.MethodDelete, "/api/v1/firewall-policies/"+p.ID, "admin-token", ""); w.Code != http.StatusConflict {
		t.Errorf("a policy in use was deleted: %d", w.Code)
	}

	// Version 2; the first server fails and the rollout stops before the
	// second.
	w = do(t, e.h, http.MethodPut, "/api/v1/firewall-policies/"+p.ID, "admin-token",
		`{"name":"`+name+`","lines":[{"key":"`+p.Lines[0].Key+`","protocol":"tcp","ports":"443,8443","from":[{"kind":"private"}]}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update: %d %s", w.Code, w.Body)
	}
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	if p.Version != 2 || p.Rollout == nil || p.Rollout.Version != 2 {
		t.Fatalf("v2: %+v", p)
	}
	firstOf := func() (Asset, string, Asset, string) {
		ro, _ := e.s.FWRolloutByID(context.Background(), p.Rollout.ID)
		if ro.Servers[0].AssetID == a1.ID {
			return a1, cred1, a2, cred2
		}
		return a2, cred2, a1, cred1
	}
	first, firstCred, second, secondCred = firstOf()
	o = e.beat(t, firstCred, first.Hostname, &tunnel.FirewallBeat{Supported: true})
	if o.Change == nil || o.Change.Lines[0].Ports != "443,8443" {
		t.Fatalf("v2 lines: %+v", o.Change)
	}
	e.run(t, firstCred, first.Hostname, o.Change, tunnel.FirewallRunFailed, nil)
	if o := e.beat(t, secondCred, second.Hostname, &tunnel.FirewallBeat{Supported: true}); o.Change != nil {
		t.Fatal("a stopped rollout went on")
	}
	ro, err := e.s.FWRolloutByID(context.Background(), p.Rollout.ID)
	if err != nil || ro.State != FWRolloutStopped || ro.Failed != 1 {
		t.Fatalf("stopped: %+v %v", ro, err)
	}
}
