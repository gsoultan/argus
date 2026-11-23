package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/auth"
	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Changing a server's firewall from its Firewall tab, and firewall policies
(plans/server-firewall/design.md).

Who: admins and owners, and operators an admin named as the server's
firewall managers. Approval (Settings › Approvals, "Server firewalls")
applies to a change that lets everyone in, turns a firewall on, takes over
rules made by hand, blocks everything going out, or touches a production
server; and to every change an operator asks for.
*/

func (a *API) registerFirewallRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/assets/{id}/firewall/plan", a.postFirewallPlan)
	mux.HandleFunc("POST /api/v1/assets/{id}/firewall/changes", a.postFirewallChange)
	mux.HandleFunc("PUT /api/v1/assets/{id}/firewall/managers", a.putFirewallManagers)
	mux.HandleFunc("GET /api/v1/firewall-changes/{id}", a.getFirewallChange)
	mux.HandleFunc("POST /api/v1/firewall-changes/{id}/approve", a.postFirewallChangeDecision(true))
	mux.HandleFunc("POST /api/v1/firewall-changes/{id}/deny", a.postFirewallChangeDecision(false))
	mux.HandleFunc("POST /api/v1/firewall-changes/{id}/cancel", a.postFirewallChangeCancel)
	mux.HandleFunc("POST /api/v1/firewall-changes/{id}/keep", a.postFirewallChangeKeep)
	mux.HandleFunc("GET /api/v1/firewall-policies", a.getFirewallPolicies)
	mux.HandleFunc("GET /api/v1/firewall-policies/starters", a.getFirewallStarters)
	mux.HandleFunc("POST /api/v1/firewall-policies", a.postFirewallPolicy)
	mux.HandleFunc("GET /api/v1/firewall-policies/{id}", a.getFirewallPolicy)
	mux.HandleFunc("PUT /api/v1/firewall-policies/{id}", a.putFirewallPolicy)
	mux.HandleFunc("DELETE /api/v1/firewall-policies/{id}", a.deleteFirewallPolicy)
	mux.HandleFunc("POST /api/v1/firewall-policies/{id}/targets", a.postFirewallTarget)
	mux.HandleFunc("DELETE /api/v1/firewall-policies/{id}/targets/{target}", a.deleteFirewallTarget)
	mux.HandleFunc("POST /api/v1/firewall-policies/{id}/rollout", a.postFirewallRolloutStart)
	mux.HandleFunc("GET /api/v1/firewall-rollouts/{id}", a.getFirewallRollout)
	mux.HandleFunc("POST /api/v1/firewall-rollouts/{id}/approve", a.postFirewallRolloutDecision(true))
	mux.HandleFunc("POST /api/v1/firewall-rollouts/{id}/deny", a.postFirewallRolloutDecision(false))
	mux.HandleFunc("POST /api/v1/firewall-rollouts/{id}/stop", a.postFirewallRolloutStop)
}

/* ── The Firewall tab's half for changes ─────────────────────────────────── */

// FirewallManage is what the Firewall tab needs to change the firewall.
type FirewallManage struct {
	// CanChange says this person may ask for changes here; Ready that the
	// server can take one, and Reason why not.
	CanChange bool   `json:"canChange"`
	Ready     bool   `json:"ready"`
	Reason    string `json:"reason,omitempty"`
	// Install is the firewall Argus would install to turn one on, on a
	// Linux server that has none.
	Install string `json:"install,omitempty"`
	// Own are the server's own lines; Policies the policies it holds.
	Own      []FWLine `json:"own"`
	Policies []FWHeld `json:"policies"`
	// DefaultOut is what Argus last set for going out ("" when it never
	// set it).
	DefaultOut string `json:"defaultOut"`
	// Always is what always stays open going out when it is blocked.
	Always []string `json:"always"`
	// Changes are the latest changes, newest first; Open the one in
	// flight or waiting, if any.
	Changes  []FirewallChange `json:"changes"`
	Open     *FirewallChange  `json:"open,omitempty"`
	Managers []string         `json:"managers"`
	// RemotePort is how people reach the server (22 or 3389), which the
	// tab warns about before anything closes it.
	RemotePort int `json:"remotePort"`
}

// FWHeld is a policy on a server.
type FWHeld struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Version    int      `json:"version"`
	Latest     int      `json:"latest"`
	DefaultOut string   `json:"defaultOut"`
	Lines      []FWLine `json:"lines"`
	// Wanted says a target still puts it here; false while a rollout
	// takes it off.
	Wanted bool `json:"wanted"`
}

var fwAlwaysWords = []string{
	"This server's own connection to Argus, and agent updates",
	"Name lookups (DNS) to the resolvers the server uses",
	"System updates from the server's package sources (Windows Update on Windows)",
}

// fwCanChange is whether the person may change this server's firewall.
func fwCanChange(sess auth.Session, managers []string) bool {
	switch sess.Role {
	case "admin", "owner":
		return true
	case "operator":
		return slices.ContainsFunc(managers, func(m string) bool { return strings.EqualFold(m, sess.Email) })
	}
	return false
}

// fwReady says why t cannot take a change now, or "".
func fwReady(t fwTarget) (reason, install string) {
	switch {
	case t.asset.Kind != "server" && t.asset.Kind != "workstation":
		return "firewalls are changed on servers and workstations", ""
	case t.agent == "" || t.beat == nil:
		return "this server's agent is too old to change its firewall; update the agent from its Settings tab first", ""
	case !t.beat.Supported:
		return t.beat.Reason, ""
	case !t.reported:
		return "the firewall has not been read yet; it is read within ten minutes of the agent starting", ""
	case t.fw.Error != "":
		return "the firewall could not be read: " + t.fw.Error, ""
	case t.fw.Kind == tunnel.FirewallIptables:
		return "this server's firewall is iptables rules written by hand; Argus changes ufw, firewalld, nftables and Windows Firewall", ""
	case t.fw.Kind == "":
		if t.asset.Protocol == "rdp" {
			return "no firewall was found", ""
		}
		install := tunnel.FirewallUFW
		if strings.Contains(strings.ToLower(t.asset.OS), "rhel") || strings.Contains(strings.ToLower(t.asset.OS), "red hat") ||
			strings.Contains(strings.ToLower(t.asset.OS), "fedora") || strings.Contains(strings.ToLower(t.asset.OS), "centos") ||
			strings.Contains(strings.ToLower(t.asset.OS), "rocky") || strings.Contains(strings.ToLower(t.asset.OS), "alma") ||
			strings.Contains(strings.ToLower(t.asset.OS), "suse") {
			install = tunnel.FirewallFirewalld
		}
		return "", install
	}
	return "", ""
}

func (t fwTarget) remotePort() int {
	if t.asset.Protocol == "rdp" {
		if t.asset.Port > 0 {
			return t.asset.Port
		}
		return 3389
	}
	if t.asset.Port > 0 {
		return t.asset.Port
	}
	return 22
}

// fwManage fills the Firewall tab's half for changes.
func (a *API) fwManage(ctx context.Context, sess auth.Session, asset Asset) (*FirewallManage, error) {
	t, err := a.fwTargetFor(ctx, asset)
	if err != nil {
		return nil, err
	}
	m := &FirewallManage{CanChange: fwCanChange(sess, t.managers), Own: t.state.Own, Policies: []FWHeld{},
		DefaultOut: t.state.DefaultOut, Always: fwAlwaysWords, Managers: t.managers, RemotePort: t.remotePort()}
	m.Reason, m.Install = fwReady(t)
	m.Ready = m.Reason == ""
	if t.agent != "" {
		a.expireFWChanges(ctx, t.agent)
	}
	if m.Changes, err = a.store.FWChangesFor(ctx, asset.ID, 20); err != nil {
		return nil, err
	}
	for i := range m.Changes {
		if fwOpen(m.Changes[i].State) && (m.Open == nil || m.Changes[i].RequestedAt.Before(m.Open.RequestedAt)) {
			c := m.Changes[i]
			m.Open = &c
		}
	}
	wanted, err := a.store.fwPoliciesOn(ctx, asset.ID)
	if err != nil {
		return nil, err
	}
	names, err := a.store.fwPolicyNames(ctx)
	if err != nil {
		return nil, err
	}
	held := map[string]bool{}
	for id, v := range t.state.Policies {
		held[id] = true
		lines, out, err := a.store.fwPolicyVersion(ctx, a.store.db(ctx), id, v)
		if err != nil {
			continue
		}
		m.Policies = append(m.Policies, FWHeld{ID: id, Name: names[id], Version: v, Latest: wanted[id],
			DefaultOut: out, Lines: lines, Wanted: wanted[id] > 0})
	}
	for id, v := range wanted {
		if !held[id] {
			m.Policies = append(m.Policies, FWHeld{ID: id, Name: names[id], Latest: v, Lines: []FWLine{}, Wanted: true})
		}
	}
	slices.SortFunc(m.Policies, func(x, y FWHeld) int { return strings.Compare(strings.ToLower(x.Name), strings.ToLower(y.Name)) })
	if err := a.labelFWLines(ctx, names, m.Own); err != nil {
		return nil, err
	}
	for i := range m.Policies {
		if err := a.labelFWLines(ctx, names, m.Policies[i].Lines); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// labelFWLines names the servers and policies lines are for.
func (a *API) labelFWLines(ctx context.Context, policyNames map[string]string, lines []FWLine) error {
	var ids []string
	for _, l := range lines {
		for _, s := range l.From {
			if s.Kind == FWFromServer {
				ids = append(ids, s.Value)
			}
		}
	}
	hosts := map[string]string{}
	if len(ids) > 0 {
		rows, err := a.store.db(ctx).Query(ctx, `SELECT id::text, hostname FROM assets WHERE id::text = ANY($1)`, ids)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, h string
			if err := rows.Scan(&id, &h); err != nil {
				rows.Close()
				return err
			}
			hosts[id] = h
		}
		rows.Close()
	}
	for i := range lines {
		for j := range lines[i].From {
			s := &lines[i].From[j]
			switch s.Kind {
			case FWFromServer:
				s.Label = hosts[s.Value]
				if s.Label == "" {
					s.Label = "a server no longer in Argus"
				}
			case FWFromPolicy:
				s.Label = policyNames[s.Value]
				if s.Label == "" {
					s.Label = "a policy no longer in Argus"
				}
			}
		}
	}
	return nil
}

/* ── Planning and asking for a change ────────────────────────────────────── */

// fwAsk is a change asked for on the Firewall tab.
type fwAsk struct {
	// Add are new lines; Remove the keys of own lines to take off.
	Add    []FWLine `json:"add"`
	Remove []string `json:"remove"`
	// TakeOver are rules made by hand, by their text, that Argus takes
	// over: each becomes a line of the server's own.
	TakeOver []string `json:"takeOver"`
	// TurnOn turns the firewall on, installing Install first when the
	// server has none.
	TurnOn  bool   `json:"turnOn"`
	Install string `json:"install"`
	// PutBack is a change whose saved copy to put back.
	PutBack string `json:"putBack"`
	Reason  string `json:"reason"`
	// Confirm is the server's name, typed when nothing would let people
	// reach it any more.
	Confirm string `json:"confirm"`
}

// FirewallPlan is what a change would do, for the review screen.
type FirewallPlan struct {
	Kind    string   `json:"kind"`
	Summary string   `json:"summary"`
	Preview []string `json:"preview"`
	// Lines are every line the server would hold.
	Lines []tunnel.FirewallLine `json:"lines"`
	// NeedsApproval: the change waits for another admin.
	NeedsApproval bool `json:"needsApproval"`
	// NeedsConfirm is why the server's name must be typed, or "".
	NeedsConfirm string   `json:"needsConfirm,omitempty"`
	Warnings     []string `json:"warnings"`
	// Keep: the change waits for a Keep within ten minutes.
	Keep bool `json:"keep"`

	delta FWDelta
	dr    directRuns
}

type fwPlanError struct {
	status int
	msg    string
}

func (e fwPlanError) Error() string { return e.msg }

func badPlan(format string, args ...any) error {
	return fwPlanError{http.StatusBadRequest, fmt.Sprintf(format, args...)}
}

// planFW checks a change and works out what it would do.
func (a *API) planFW(ctx context.Context, sess auth.Session, client string, t fwTarget, in *fwAsk) (FirewallPlan, error) {
	p := FirewallPlan{Warnings: []string{}, dr: directRuns{}}
	reason, install := fwReady(t)
	if reason != "" {
		return p, fwPlanError{http.StatusConflict, reason}
	}
	now := time.Now().UTC()
	in.Reason = strings.TrimSpace(in.Reason)
	if len(in.Reason) > 500 {
		return p, badPlan("the reason is at most 500 characters")
	}
	kinds := 0
	for _, b := range []bool{in.PutBack != "", in.TurnOn, len(in.TakeOver) > 0, len(in.Add)+len(in.Remove) > 0} {
		if b {
			kinds++
		}
	}
	if kinds != 1 && !(in.TurnOn && len(in.Add) > 0 && in.PutBack == "" && len(in.TakeOver) == 0) {
		return p, badPlan("one change at a time: lines, taking over, turning on or putting back")
	}
	d := FWDelta{}
	var words []string
	switch {
	case in.PutBack != "":
		prev, err := a.store.FWChangeByID(ctx, in.PutBack)
		if err != nil || prev.AssetID != t.asset.ID {
			return p, fwPlanError{http.StatusNotFound, "no such change on this server"}
		}
		if !prev.CanPutBack {
			return p, fwPlanError{http.StatusConflict, "only an applied change can be put back"}
		}
		p.Kind, d.PutBack = "put_back", prev.ID
		words = append(words, "Put back the firewall as it was before: "+prev.Summary)
	case len(in.TakeOver) > 0:
		p.Kind = "take_over"
		if len(in.TakeOver) > 50 {
			return p, badPlan("at most 50 rules at a time")
		}
		for _, raw := range in.TakeOver {
			var rule *tunnel.FirewallRule
			for i := range t.fw.Rules {
				if t.fw.Rules[i].Raw == raw && !t.fw.Rules[i].Argus {
					rule = &t.fw.Rules[i]
					break
				}
			}
			if rule == nil {
				return p, fwPlanError{http.StatusConflict, "the rule " + raw + " is not on the server any more; read the firewall again"}
			}
			l, err := lineOfRule(*rule)
			if err != nil {
				return p, badPlan("%s: %v", raw, err)
			}
			l.Key, l.AddedBy, l.AddedAt = newLineKey("s", 8), sess.Email, &now
			l.Note = "Taken over from " + raw
			d.Add = append(d.Add, l)
			d.TakeOver = append(d.TakeOver, raw)
		}
		d.ExpectHash = t.fw.Hash()
		words = append(words, fmt.Sprintf("Take over %d rule(s) made by hand", len(in.TakeOver)))
	default:
		p.Kind = "lines"
		if in.TurnOn {
			p.Kind = "turn_on"
		}
		for _, k := range in.Remove {
			i := slices.IndexFunc(t.state.Own, func(l FWLine) bool { return l.Key == k })
			if i < 0 {
				return p, fwPlanError{http.StatusConflict, "that line is not on the server any more"}
			}
			d.Remove = append(d.Remove, k)
			words = append(words, "Remove: "+t.state.Own[i].words())
		}
		for i := range in.Add {
			l := in.Add[i]
			if err := l.validate(); err != nil {
				return p, badPlan("%v", err)
			}
			if l.Until != nil && !l.Until.After(now.Add(time.Minute)) {
				return p, badPlan("the end time has passed")
			}
			l.Key, l.AddedBy, l.AddedAt = newLineKey("s", 8), sess.Email, &now
			d.Add = append(d.Add, l)
			words = append(words, l.words())
		}
	}
	if in.TurnOn {
		if t.fw.Active {
			return p, fwPlanError{http.StatusConflict, "the firewall is on already"}
		}
		d.TurnOn = true
		if t.fw.Kind == "" {
			if in.Install == "" {
				in.Install = install
			}
			if in.Install != tunnel.FirewallUFW && in.Install != tunnel.FirewallFirewalld {
				return p, badPlan("Argus installs ufw or firewalld")
			}
			d.Install = in.Install
			words = append([]string{"Install " + in.Install + " and turn it on"}, words...)
		} else {
			words = append([]string{"Turn " + t.fw.Kind + " on, blocking what no line lets in"}, words...)
		}
		d.ExpectHash = t.fw.Hash()
	} else if t.fw.Kind == "" {
		return p, fwPlanError{http.StatusConflict, "this server has no firewall; turn one on first"}
	}
	kind := t.fwKind(d)
	for _, l := range d.Add {
		if kind == tunnel.FirewallUFW && l.Protocol == "icmp" {
			return p, badPlan("ufw cannot let ping in or out line by line; ufw lets ping in by default")
		}
	}
	if len(d.Apply(t.state, now).Own) > maxFWOwnLines {
		return p, badPlan("at most %d lines of the server's own; put shared lines in a policy", maxFWOwnLines)
	}
	built, err := a.buildFWChange(ctx, t, d, "00000000-0000-0000-0000-000000000000", now)
	if err != nil {
		return p, badPlan("%v", err)
	}
	// A block that keeps out the person asking would end their session.
	if ip, err := netip.ParseAddr(client); err == nil {
		for _, l := range built.lines {
			if l.Direction != "in" || l.Action != "deny" || !strings.HasPrefix(l.Key, "s") {
				continue
			}
			if len(l.Sources) == 0 && slices.ContainsFunc(d.Add, func(x FWLine) bool { return x.Key == l.Key }) {
				return p, badPlan("a block from everyone would keep you out too; let in only who needs it instead")
			}
			for _, src := range l.Sources {
				if pre, err := tunnel.ParseSource(src); err == nil && pre.Contains(ip.Unmap()) &&
					slices.ContainsFunc(d.Add, func(x FWLine) bool { return x.Key == l.Key }) {
					return p, badPlan("this block covers %s, the address you are connected from", ip)
				}
			}
		}
	}
	for _, s := range built.skipped {
		p.Warnings = append(p.Warnings, "Left off until the servers it names report an address: "+s)
	}
	if t.fw.Docker {
		p.Warnings = append(p.Warnings, "Docker publishes ports around the firewall on this server; lines here do not cover them")
	}
	if built.spec.DefaultOut == "deny" && t.state.DefaultOut != "deny" {
		p.Warnings = append(p.Warnings, "Everything going out is blocked but what the lines and Argus always allow")
	}
	// Losing the way in: the firewall blocks by default, and nothing lets
	// the remote access port in.
	port := t.remotePort()
	blocks := d.TurnOn || (t.fw.Active && t.fw.DefaultIn != "allow" && t.fw.DefaultIn != "")
	if blocks && d.PutBack == "" && !fwPortOpen(t.fw, built.lines, d.TakeOver, port) {
		p.NeedsConfirm = fmt.Sprintf("After this change nothing lets port %d in, which is how people reach %s. "+
			"Sessions through Argus keep working, but a direct login does not. Type the server's name to go ahead.",
			port, t.asset.Hostname)
	}
	needs := sess.Role == "operator" || d.TurnOn || len(d.TakeOver) > 0 || envOfTags(t.asset.Tags) == EnvProduction ||
		built.spec.DefaultOut == "deny"
	for _, l := range d.Add {
		needs = needs || l.opensToEveryone()
	}
	p.NeedsApproval = a.runWaits(ctx, needs, KindServerFirewall, t.asset.Tags, sess.Email, t.asset.Hostname, p.dr)
	p.Summary = clip(strings.Join(words, "; "), 500)
	p.Preview, p.Lines, p.Keep, p.delta = built.preview, built.lines, built.spec.Keep, d
	if p.Lines == nil {
		p.Lines = []tunnel.FirewallLine{}
	}
	return p, nil
}

// Apply is d applied to st; exported for the tests' reading.
func (d FWDelta) Apply(st fwServerState, now time.Time) fwServerState { return d.apply(st, now) }

// fwPortOpen says traffic to port gets in: by a line Argus keeps, or a
// rule made by hand that is not being taken over.
func fwPortOpen(fw tunnel.HostFirewall, lines []tunnel.FirewallLine, takeOver []string, port int) bool {
	for _, l := range lines {
		if l.Direction == "in" && l.Action == "allow" && (l.Protocol == "any" || (l.Protocol == "tcp" && portsCover(l.Ports, port))) {
			return true
		}
	}
	for _, r := range fw.Rules {
		if r.Argus || r.Direction != "in" || r.Action != "allow" || slices.Contains(takeOver, r.Raw) {
			continue
		}
		if tunnel.ArgusLineKey(r.Name) != "" {
			continue // a line of Argus's own: counted above
		}
		if r.Protocol == "" || r.Protocol == "any" || (r.Protocol == "tcp" && (r.Ports == "" || portsCover(r.Ports, port))) ||
			(port == 22 && r.Service == "ssh") || (port == 3389 && strings.Contains(strings.ToLower(r.Service), "rdp")) {
			return true
		}
	}
	return false
}

// lineOfRule is a rule made by hand as a line Argus keeps.
func lineOfRule(r tunnel.FirewallRule) (FWLine, error) {
	l := FWLine{Direction: r.Direction, Action: "allow", Protocol: r.Protocol, Ports: r.Ports}
	if r.Direction == "" {
		l.Direction = "in"
	}
	if r.Action == "deny" || r.Action == "reject" {
		l.Action = "deny"
	}
	if l.Protocol == "" {
		l.Protocol = "any"
	}
	if l.Protocol != "tcp" && l.Protocol != "udp" {
		l.Ports = ""
	}
	if (l.Protocol == "tcp" || l.Protocol == "udp") && l.Ports == "" {
		if r.Service != "" {
			return l, fmt.Errorf("this rule names the service %q, not ports; add a line for its ports instead", r.Service)
		}
		l.Protocol = "any"
	}
	l.Ports = strings.ReplaceAll(l.Ports, ":", "-")
	for _, s := range r.Sources {
		if s == "" || s == "any" || s == "Any" || s == "0.0.0.0/0" || s == "::/0" || strings.EqualFold(s, "anywhere") {
			l.From = []FWSource{{Kind: FWFromEveryone}}
			break
		}
		pre, err := tunnel.ParseSource(s)
		if err != nil {
			return l, fmt.Errorf("its source %q is not an address Argus can keep", s)
		}
		l.From = append(l.From, FWSource{Kind: FWFromAddress, Value: tunnel.SourceText(pre)})
	}
	if len(l.From) == 0 {
		l.From = []FWSource{{Kind: FWFromEveryone}}
	}
	return l, l.validate()
}

func (a *API) fwTargetOf(w http.ResponseWriter, r *http.Request, sess auth.Session) (fwTarget, bool) {
	asset, err := a.store.AssetByID(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failAsset(w, "asset", err)
		return fwTarget{}, false
	}
	t, err := a.fwTargetFor(r.Context(), asset)
	if err != nil {
		a.fail(w, "firewall", err)
		return t, false
	}
	if !fwCanChange(sess, t.managers) {
		writeErr(w, http.StatusForbidden, "changing this server's firewall requires the admin or owner role, "+
			"or being one of its firewall managers")
		return t, false
	}
	return t, true
}

func (a *API) failFWPlan(w http.ResponseWriter, err error) {
	var pe fwPlanError
	if errors.As(err, &pe) {
		writeErr(w, pe.status, pe.msg)
		return
	}
	a.failFWChange(w, err)
}

func (a *API) postFirewallPlan(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var in fwAsk
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	t, ok := a.fwTargetOf(w, r, sess)
	if !ok {
		return
	}
	p, err := a.planFW(r.Context(), sess, a.clientAddr(r), t, &in)
	if err != nil {
		a.failFWPlan(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *API) postFirewallChange(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var in fwAsk
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	t, ok := a.fwTargetOf(w, r, sess)
	if !ok {
		return
	}
	p, err := a.planFW(r.Context(), sess, a.clientAddr(r), t, &in)
	if err != nil {
		a.failFWPlan(w, err)
		return
	}
	if p.NeedsConfirm != "" && !strings.EqualFold(strings.TrimSpace(in.Confirm), t.asset.Hostname) {
		writeErr(w, http.StatusConflict, p.NeedsConfirm)
		return
	}
	state := FWQueued
	if p.NeedsApproval {
		state = FWAwaitingApproval
	}
	c, err := a.store.CreateFWChange(r.Context(), newFWChange{AssetID: t.asset.ID, Hostname: t.agent,
		AssetHostname: t.asset.Hostname, Kind: p.Kind, Summary: p.Summary, Reason: in.Reason, State: state,
		RequestedBy: sess.Email, Delta: p.delta, Preview: p.Preview})
	if err != nil {
		a.failFWChange(w, err)
		return
	}
	p.dr.record(r.Context(), a, sess.Email, t.asset.Hostname, "Changed the firewall on "+t.asset.Hostname+": "+p.Summary)
	waits := ""
	if state == FWAwaitingApproval {
		waits = "; it waits for another admin to approve it"
	}
	detail := fmt.Sprintf("Asked to change the firewall on %s: %s%s.", t.asset.Hostname, p.Summary, waits)
	if in.Reason != "" {
		detail += " Reason: " + in.Reason
	}
	if p.NeedsConfirm != "" {
		detail += " They typed the server's name to confirm nothing would let port " + fmt.Sprint(t.remotePort()) + " in."
	}
	a.auditAsset(r, AuditEvent{Action: "firewall.change_requested", Severity: "notice", ActorEmail: sess.Email,
		Target: t.asset.Hostname, Detail: clip(detail, 4000)})
	if state == FWAwaitingApproval {
		a.raiseAlert(r.Context(), ConsoleAlert{
			Kind: AlertApproval, Severity: "warning", Audience: AudienceAdmins, Actor: sess.Email,
			Title:   sess.Email + " asks to change the firewall on " + t.asset.Hostname,
			Detail:  clip(p.Summary, 900) + ". Approve or deny it on the server's Firewall tab.",
			Link:    "/assets/" + t.asset.ID + "?tab=firewall",
			Subject: fwChangeSubject(c.ID),
		})
	}
	writeJSON(w, http.StatusAccepted, c)
}

func fwChangeSubject(id string) string  { return "firewall-change:" + id }
func fwRolloutSubject(id string) string { return "firewall-rollout:" + id }

func (a *API) failFWChange(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrFWChangeNotFound), errors.Is(err, ErrFWPolicyNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrRunState):
		writeErr(w, http.StatusConflict, strings.TrimPrefix(err.Error(), ErrRunState.Error()+": "))
	case errors.Is(err, ErrFWBusy), errors.Is(err, ErrFWPolicyName), errors.Is(err, ErrFWPolicyInUse):
		writeErr(w, http.StatusConflict, err.Error())
	default:
		a.fail(w, "firewall change", err)
	}
}

func (a *API) getFirewallChange(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	c, err := a.store.FWChangeByID(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failFWChange(w, err)
		return
	}
	if !seesWholeFleet(sess.Role) {
		_, managers, _, err := a.store.fwServerFor(r.Context(), c.AssetID)
		if err != nil || !fwCanChange(sess, managers) {
			writeErr(w, http.StatusForbidden, "firewall changes are for administrators, owners, auditors and the server's firewall managers")
			return
		}
	}
	writeJSON(w, http.StatusOK, c)
}

func (a *API) postFirewallChangeDecision(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := a.authenticate(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if !a.privileged(w, sess, "approving firewall changes", "admin", "owner") {
			return
		}
		c, err := a.store.DecideFWChange(r.Context(), r.PathValue("id"), sess.Email, approve, false)
		if errors.Is(err, ErrOwnRequest) {
			confirm, cerr := readConfirmSelf(r)
			if cerr != nil {
				writeErr(w, http.StatusBadRequest, cerr.Error())
				return
			}
			if !a.approveOwn(w, r, sess.Email, approve, runDeciders, false, confirm,
				strings.TrimPrefix(err.Error(), ErrRunState.Error()+": "), "firewall change", c.AssetHostname) {
				return
			}
			c, err = a.store.DecideFWChange(r.Context(), r.PathValue("id"), sess.Email, approve, true)
		}
		if err != nil {
			a.failFWChange(w, err)
			return
		}
		action, verb := "firewall.change_approved", "Approved"
		if !approve {
			action, verb = "firewall.change_denied", "Denied"
		}
		a.auditAsset(r, AuditEvent{Action: action, Severity: "notice", ActorEmail: sess.Email, Target: c.AssetHostname,
			Detail: fmt.Sprintf("%s the firewall change on %s (%s), asked by %s.", verb, c.AssetHostname, c.Summary, c.RequestedBy)})
		a.closeAlerts(r.Context(), fwChangeSubject(c.ID), verb+" by "+sess.Email)
		writeJSON(w, http.StatusOK, c)
	}
}

func (a *API) postFirewallChangeCancel(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	c, err := a.store.FWChangeByID(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failFWChange(w, err)
		return
	}
	_, managers, _, err := a.store.fwServerFor(r.Context(), c.AssetID)
	if err != nil {
		a.fail(w, "firewall", err)
		return
	}
	if !fwCanChange(sess, managers) || (sess.Role == "operator" && !strings.EqualFold(c.RequestedBy, sess.Email)) {
		writeErr(w, http.StatusForbidden, "taking back a firewall change requires the admin or owner role, or having asked for it")
		return
	}
	if c, err = a.store.CancelFWChange(r.Context(), c.ID, sess.Email); err != nil {
		a.failFWChange(w, err)
		return
	}
	a.auditAsset(r, AuditEvent{Action: "firewall.change_cancelled", Severity: "info", ActorEmail: sess.Email,
		Target: c.AssetHostname, Detail: "Took back the firewall change on " + c.AssetHostname + ": " + c.Summary + "."})
	a.closeAlerts(r.Context(), fwChangeSubject(c.ID), "Taken back by "+sess.Email)
	writeJSON(w, http.StatusOK, c)
}

func (a *API) postFirewallChangeKeep(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	c, err := a.store.FWChangeByID(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failFWChange(w, err)
		return
	}
	_, managers, _, err := a.store.fwServerFor(r.Context(), c.AssetID)
	if err != nil {
		a.fail(w, "firewall", err)
		return
	}
	if !fwCanChange(sess, managers) {
		writeErr(w, http.StatusForbidden, "keeping a firewall change requires being able to change the server's firewall")
		return
	}
	if c, err = a.store.KeepFWChange(r.Context(), c.ID, sess.Email); err != nil {
		a.failFWChange(w, err)
		return
	}
	a.auditAsset(r, AuditEvent{Action: "firewall.change_kept", Severity: "notice", ActorEmail: sess.Email,
		Target: c.AssetHostname, Detail: "Kept the firewall change on " + c.AssetHostname + " (" + c.Summary +
			"), having checked the server still answers."})
	writeJSON(w, http.StatusOK, c)
}

type fwManagersIn struct {
	Emails []string `json:"emails"`
}

func (a *API) putFirewallManagers(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "choosing who manages a server's firewall", "admin", "owner") {
		return
	}
	var in fwManagersIn
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(in.Emails) > 50 {
		writeErr(w, http.StatusBadRequest, "at most 50 managers")
		return
	}
	asset, err := a.store.AssetByID(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failAsset(w, "asset", err)
		return
	}
	emails := []string{}
	for _, e := range in.Emails {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if len(e) > 320 || !strings.Contains(e, "@") {
			writeErr(w, http.StatusBadRequest, e+" is not an email address")
			return
		}
		if !slices.Contains(emails, e) {
			emails = append(emails, e)
		}
	}
	if err := a.store.SetFWManagers(r.Context(), asset.ID, emails); err != nil {
		a.fail(w, "firewall managers", err)
		return
	}
	who := "nobody but admins and owners"
	if len(emails) > 0 {
		who = strings.Join(emails, ", ")
	}
	a.auditAsset(r, AuditEvent{Action: "firewall.managers_set", Severity: "notice", ActorEmail: sess.Email,
		Target: asset.Hostname, Detail: "Set who may change the firewall on " + asset.Hostname + ": " + who + "."})
	writeJSON(w, http.StatusOK, map[string]any{"managers": emails})
}

/* ── Policies ────────────────────────────────────────────────────────────── */

func (a *API) fwPolicyReader(w http.ResponseWriter, r *http.Request) (auth.Session, bool) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return sess, false
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden, "firewall policies are for administrators, owners and auditors")
		return sess, false
	}
	return sess, true
}

func (a *API) fwPolicyWriter(w http.ResponseWriter, r *http.Request) (auth.Session, bool) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return sess, false
	}
	return sess, a.privileged(w, sess, "changing firewall policies", "admin", "owner")
}

func (a *API) getFirewallPolicies(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.fwPolicyReader(w, r); !ok {
		return
	}
	ps, err := a.store.FWPolicies(r.Context())
	if err != nil {
		a.fail(w, "firewall policies", err)
		return
	}
	names := map[string]string{}
	for _, p := range ps {
		names[p.ID] = p.Name
	}
	for i := range ps {
		if err := a.labelFWLines(r.Context(), names, ps[i].Lines); err != nil {
			a.fail(w, "firewall policies", err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"policies": ps})
}

func (a *API) getFirewallStarters(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.fwPolicyReader(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"starters": fwStarters()})
}

func (a *API) getFirewallPolicy(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.fwPolicyReader(w, r); !ok {
		return
	}
	p, err := a.store.FWPolicyByID(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failFWChange(w, err)
		return
	}
	names, err := a.store.fwPolicyNames(r.Context())
	if err == nil {
		err = a.labelFWLines(r.Context(), names, p.Lines)
	}
	if err != nil {
		a.fail(w, "firewall policy", err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *API) postFirewallPolicy(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.fwPolicyWriter(w, r)
	if !ok {
		return
	}
	var in fwPolicyInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := in.validate(nil, sess.Email, time.Now().UTC()); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := a.store.CreateFWPolicy(r.Context(), in, sess.Email)
	if err != nil {
		a.failFWChange(w, err)
		return
	}
	a.auditAsset(r, AuditEvent{Action: "firewall.policy_created", Severity: "notice", ActorEmail: sess.Email,
		Target: p.Name, Detail: fmt.Sprintf("Made the firewall policy %s: %s.", p.Name, fwPolicyWords(p))})
	writeJSON(w, http.StatusCreated, p)
}

func fwPolicyWords(p FirewallPolicy) string {
	var ws []string
	for _, l := range p.Lines {
		ws = append(ws, l.words())
	}
	if p.DefaultOut == "deny" {
		ws = append(ws, "block everything else going out")
	}
	if len(ws) == 0 {
		return "no lines"
	}
	return strings.Join(ws, "; ")
}

func (a *API) putFirewallPolicy(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.fwPolicyWriter(w, r)
	if !ok {
		return
	}
	var in fwPolicyInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	old, err := a.store.FWPolicyByID(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failFWChange(w, err)
		return
	}
	if err := in.validate(old.Lines, sess.Email, time.Now().UTC()); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	for _, l := range in.Lines {
		for _, s := range l.From {
			if s.Kind == FWFromPolicy && s.Value == old.ID {
				writeErr(w, http.StatusBadRequest, "a policy's line cannot be for servers with the same policy; "+
					"use a tag instead")
				return
			}
		}
	}
	p, changed, err := a.store.UpdateFWPolicy(r.Context(), old.ID, in, sess.Email)
	if err != nil {
		a.failFWChange(w, err)
		return
	}
	detail := fmt.Sprintf("Changed the firewall policy %s", p.Name)
	if changed {
		detail += fmt.Sprintf(" to version %d: %s", p.Version, fwPolicyWords(p))
	}
	a.auditAsset(r, AuditEvent{Action: "firewall.policy_changed", Severity: "notice", ActorEmail: sess.Email,
		Target: p.Name, Detail: detail + "."})
	if changed {
		if _, err := a.rolloutFWPolicy(r.Context(), p, sess); err != nil {
			a.fail(w, "firewall rollout", err)
			return
		}
	}
	p, err = a.store.FWPolicyByID(r.Context(), p.ID)
	if err != nil {
		a.fail(w, "firewall policy", err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *API) deleteFirewallPolicy(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.fwPolicyWriter(w, r)
	if !ok {
		return
	}
	p, err := a.store.DeleteFWPolicy(r.Context(), r.PathValue("id"))
	if err != nil {
		if strings.HasPrefix(err.Error(), "another policy") {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		a.failFWChange(w, err)
		return
	}
	a.auditAsset(r, AuditEvent{Action: "firewall.policy_deleted", Severity: "notice", ActorEmail: sess.Email,
		Target: p.Name, Detail: "Deleted the firewall policy " + p.Name + ", which no server held."})
	w.WriteHeader(http.StatusNoContent)
}

type fwTargetIn struct {
	AssetID string `json:"assetId"`
	Tag     string `json:"tag"`
}

func (a *API) postFirewallTarget(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.fwPolicyWriter(w, r)
	if !ok {
		return
	}
	var in fwTargetIn
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	in.Tag = strings.TrimSpace(in.Tag)
	if (in.AssetID == "") == (in.Tag == "") || len(in.Tag) > 64 {
		writeErr(w, http.StatusBadRequest, "put the policy on one server or one tag")
		return
	}
	p, err := a.store.FWPolicyByID(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failFWChange(w, err)
		return
	}
	where := "servers tagged " + in.Tag
	if in.AssetID != "" {
		asset, err := a.store.AssetByID(r.Context(), in.AssetID)
		if err != nil {
			a.failAsset(w, "asset", err)
			return
		}
		if asset.Kind != "server" && asset.Kind != "workstation" {
			writeErr(w, http.StatusBadRequest, "firewall policies go on servers and workstations")
			return
		}
		in.AssetID, where = asset.ID, asset.Hostname
	}
	if err := a.store.AddFWTarget(r.Context(), p.ID, in.AssetID, in.Tag, sess.Email); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	a.auditAsset(r, AuditEvent{Action: "firewall.policy_placed", Severity: "notice", ActorEmail: sess.Email,
		Target: p.Name, Detail: "Put the firewall policy " + p.Name + " on " + where + "."})
	ro, err := a.rolloutFWPolicy(r.Context(), p, sess)
	if err != nil {
		a.fail(w, "firewall rollout", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rollout": ro})
}

func (a *API) deleteFirewallTarget(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.fwPolicyWriter(w, r)
	if !ok {
		return
	}
	p, err := a.store.FWPolicyByID(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failFWChange(w, err)
		return
	}
	i := slices.IndexFunc(p.Targets, func(t FWTarget) bool { return t.ID == r.PathValue("target") })
	if i < 0 {
		writeErr(w, http.StatusNotFound, "the policy is not there")
		return
	}
	if err := a.store.RemoveFWTarget(r.Context(), p.ID, p.Targets[i].ID); err != nil {
		a.failFWChange(w, err)
		return
	}
	where := p.Targets[i].Hostname
	if p.Targets[i].Tag != "" {
		where = "servers tagged " + p.Targets[i].Tag
	}
	a.auditAsset(r, AuditEvent{Action: "firewall.policy_removed", Severity: "notice", ActorEmail: sess.Email,
		Target: p.Name, Detail: "Took the firewall policy " + p.Name + " off " + where + "."})
	ro, err := a.rolloutFWPolicy(r.Context(), p, sess)
	if err != nil {
		a.fail(w, "firewall rollout", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rollout": ro})
}

func (a *API) postFirewallRolloutStart(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.fwPolicyWriter(w, r)
	if !ok {
		return
	}
	p, err := a.store.FWPolicyByID(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failFWChange(w, err)
		return
	}
	ro, err := a.rolloutFWPolicy(r.Context(), p, sess)
	if err != nil {
		a.fail(w, "firewall rollout", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rollout": ro})
}

// rolloutFWPolicy sends a policy's latest version to every server that
// should hold it, and takes it off those that should not, one server and
// then five at a time. nil when every server is up to date.
func (a *API) rolloutFWPolicy(ctx context.Context, p FirewallPolicy, sess auth.Session) (*FWRollout, error) {
	servers, err := a.store.fwPolicyServers(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	var changes []newFWChange
	skipped := 0
	needs := p.DefaultOut == "deny"
	for _, l := range p.Lines {
		needs = needs || l.opensToEveryone()
	}
	var prodTags []string
	for _, s := range servers {
		want := 0
		if s.Want {
			want = p.Version
		}
		if (s.Want && s.Has && s.HasVersion == p.Version) || (!s.Want && !s.Has) {
			continue
		}
		agent, err := a.store.AgentHostname(ctx, s.Hostname)
		if err != nil {
			return nil, err
		}
		beat, err := a.store.fwBeatFor(ctx, agent)
		if err != nil {
			return nil, err
		}
		if beat == nil || !beat.Supported {
			skipped++
			continue
		}
		if envOfTags(s.Tags) == EnvProduction {
			needs, prodTags = true, s.Tags
		}
		summary := fmt.Sprintf("Put version %d of the policy %s on", p.Version, p.Name)
		if s.Has && s.Want {
			summary = fmt.Sprintf("Move the policy %s to version %d", p.Name, p.Version)
		}
		if want == 0 {
			summary = "Take the policy " + p.Name + " off"
		}
		wave := 0
		if n := len(changes); n > 0 {
			wave = 1 + (n-1)/5
		}
		changes = append(changes, newFWChange{AssetID: s.AssetID, Hostname: agent, AssetHostname: s.Hostname,
			Kind: "policy", Summary: summary, State: FWQueued, RequestedBy: sess.Email, Wave: wave,
			Delta: FWDelta{Policies: map[string]int{p.ID: want}}})
	}
	if len(changes) == 0 {
		return nil, nil
	}
	if prodTags == nil && len(servers) > 0 {
		prodTags = servers[0].Tags
	}
	dr := directRuns{}
	state := FWRolloutRunning
	if a.runWaits(ctx, needs, KindServerFirewall, prodTags, sess.Email, p.Name, dr) {
		state = FWRolloutWaiting
	}
	summary := fmt.Sprintf("Version %d of %s to %d server(s)", p.Version, p.Name, len(changes))
	if skipped > 0 {
		summary += fmt.Sprintf("; %d server(s) left out because their agent cannot change the firewall", skipped)
	}
	ro, err := a.store.createFWRollout(ctx, p, state, summary, sess.Email, changes)
	if err != nil {
		return nil, err
	}
	dr.record(ctx, a, sess.Email, p.Name, "Rolled out the firewall policy "+p.Name+": "+summary)
	if _, err := a.store.AppendAudit(ctx, AuditEvent{Action: "firewall.rollout_started", Severity: "notice",
		ActorEmail: sess.Email, Target: p.Name, Detail: summary + ", one server first, then five at a time."}); err != nil {
		a.log.Error("audit append failed", "error", err)
	}
	if state == FWRolloutWaiting {
		a.raiseAlert(ctx, ConsoleAlert{
			Kind: AlertApproval, Severity: "warning", Audience: AudienceAdmins, Actor: sess.Email,
			Title:   sess.Email + " asks to roll out the firewall policy " + p.Name,
			Detail:  summary + ". Approve or deny it on the policy's page.",
			Link:    "/assets/firewall-policies/" + p.ID,
			Subject: fwRolloutSubject(ro.ID),
		})
	}
	return &ro, nil
}

func (a *API) getFirewallRollout(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.fwPolicyReader(w, r); !ok {
		return
	}
	ro, err := a.store.FWRolloutByID(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failFWChange(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ro)
}

func (a *API) postFirewallRolloutDecision(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := a.authenticate(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if !a.privileged(w, sess, "approving firewall rollouts", "admin", "owner") {
			return
		}
		ro, err := a.store.DecideFWRollout(r.Context(), r.PathValue("id"), sess.Email, approve, false)
		if errors.Is(err, ErrOwnRequest) {
			confirm, cerr := readConfirmSelf(r)
			if cerr != nil {
				writeErr(w, http.StatusBadRequest, cerr.Error())
				return
			}
			if !a.approveOwn(w, r, sess.Email, approve, runDeciders, false, confirm,
				strings.TrimPrefix(err.Error(), ErrRunState.Error()+": "), "firewall rollout", ro.PolicyName) {
				return
			}
			ro, err = a.store.DecideFWRollout(r.Context(), r.PathValue("id"), sess.Email, approve, true)
		}
		if err != nil {
			a.failFWChange(w, err)
			return
		}
		action, verb := "firewall.rollout_approved", "Approved"
		if !approve {
			action, verb = "firewall.rollout_denied", "Denied"
		}
		a.auditAsset(r, AuditEvent{Action: action, Severity: "notice", ActorEmail: sess.Email, Target: ro.PolicyName,
			Detail: fmt.Sprintf("%s the rollout of %s (%s), asked by %s.", verb, ro.PolicyName, ro.Summary, ro.RequestedBy)})
		a.closeAlerts(r.Context(), fwRolloutSubject(ro.ID), verb+" by "+sess.Email)
		writeJSON(w, http.StatusOK, ro)
	}
}

func (a *API) postFirewallRolloutStop(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.fwPolicyWriter(w, r)
	if !ok {
		return
	}
	ro, err := a.store.StopFWRollout(r.Context(), r.PathValue("id"), "stopped by "+sess.Email+
		"; servers not reached yet keep what they hold")
	if err != nil {
		a.failFWChange(w, err)
		return
	}
	a.auditAsset(r, AuditEvent{Action: "firewall.rollout_stopped", Severity: "notice", ActorEmail: sess.Email,
		Target: ro.PolicyName, Detail: "Stopped the rollout of " + ro.PolicyName + ": " + ro.Summary + "."})
	a.closeAlerts(r.Context(), fwRolloutSubject(ro.ID), "Stopped by "+sess.Email)
	writeJSON(w, http.StatusOK, ro)
}
