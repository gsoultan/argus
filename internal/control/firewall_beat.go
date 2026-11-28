package control

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
	"github.com/gsoultan/caduceus"
)

/*
Sending firewall changes to servers, on their heartbeats.

One change at a time per server. A change is worked out into the whole set
of lines Argus keeps there just before it goes, from what the server holds
now (firewall_servers), so the lines that name other servers carry their
addresses as they are at that moment. A server that blocks going out, or
whose lines name other servers, gets the set again every hour so addresses
that moved are followed; a line with an end date comes off at the first
heartbeat after it.
*/

// fwTarget is one server, with all a firewall change needs.
type fwTarget struct {
	asset    Asset
	agent    string
	fw       tunnel.HostFirewall
	reported bool
	beat     *tunnel.FirewallBeat
	state    fwServerState
	managers []string
	synced   *time.Time
}

// hostFirewallOf is the firewall the agent on host last read.
func (s *Store) hostFirewallOf(ctx context.Context, host string) (tunnel.HostFirewall, bool, error) {
	var raw []byte
	err := s.db(ctx).QueryRow(ctx, `SELECT firewall FROM agents WHERE lower(hostname) = lower($1)
		 ORDER BY last_seen_at DESC LIMIT 1`, host).Scan(&raw)
	if errors.Is(err, caduceus.ErrNoRows) {
		return tunnel.HostFirewall{}, false, nil
	}
	if err != nil {
		return tunnel.HostFirewall{}, false, err
	}
	fw, ok := decodeFirewall(raw)
	return fw, ok, nil
}

func (a *API) fwTargetFor(ctx context.Context, asset Asset) (fwTarget, error) {
	t := fwTarget{asset: asset}
	agent, err := a.store.AgentHostname(ctx, asset.Hostname)
	if err != nil {
		return t, err
	}
	t.agent = agent
	if t.fw, t.reported, err = a.store.hostFirewallOf(ctx, agent); err != nil {
		return t, err
	}
	if t.beat, err = a.store.fwBeatFor(ctx, agent); err != nil {
		return t, err
	}
	t.state, t.managers, t.synced, err = a.store.fwServerFor(ctx, asset.ID)
	return t, err
}

// fwKind is the firewall a change is for: the one in charge, or the one
// the change installs.
func (t fwTarget) fwKind(d FWDelta) string {
	if t.fw.Kind == "" || (d.Install != "" && t.fw.Kind != tunnel.FirewallUFW && t.fw.Kind != tunnel.FirewallFirewalld) {
		return d.Install
	}
	return t.fw.Kind
}

func (t fwTarget) zone() string {
	if len(t.fw.Zones) > 0 {
		return t.fw.Zones[0]
	}
	return "public"
}

// fwBuilt is a change worked out for the agent.
type fwBuilt struct {
	spec    tunnel.FirewallChangeSpec
	after   fwServerState
	lines   []tunnel.FirewallLine
	preview []string
	// skipped are lines left off because the servers they name have no
	// address yet.
	skipped []string
}

// buildFWChange works d out against what t holds, as change id.
func (a *API) buildFWChange(ctx context.Context, t fwTarget, d FWDelta, id string, now time.Time) (fwBuilt, error) {
	var b fwBuilt
	b.spec = tunnel.FirewallChangeSpec{ID: id}
	if d.PutBack != "" {
		prev, err := a.store.FWChangeByID(ctx, d.PutBack)
		if err != nil {
			return b, err
		}
		if prev.AssetID != t.asset.ID || prev.before == nil {
			return b, errors.New("that change's saved copy is not on this server")
		}
		b.spec.PutBack = d.PutBack
		b.after = *prev.before
		b.preview = tunnel.FirewallPreview(t.fw.Kind, t.zone(), t.fw.ArgusLines(), b.spec)
		return b, nil
	}
	b.after = d.apply(t.state, now)
	res, err := a.store.fwResolverNow(ctx)
	if err != nil {
		return b, err
	}
	for _, l := range b.after.Own {
		fl, ok := res.resolve(l, l.Key)
		if !ok {
			b.skipped = append(b.skipped, l.words())
			continue
		}
		b.lines = append(b.lines, fl)
	}
	denyAll, anyPolicy := true, false
	ids := make([]string, 0, len(b.after.Policies))
	for id := range b.after.Policies {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, pid := range ids {
		lines, out, err := a.store.fwPolicyVersion(ctx, a.store.db(ctx), pid, b.after.Policies[pid])
		if errors.Is(err, caduceus.ErrNoRows) {
			continue // a policy deleted since; its lines come off
		}
		if err != nil {
			return b, err
		}
		anyPolicy = true
		denyAll = denyAll && out == "deny"
		for _, l := range lines {
			fl, ok := res.resolve(l, policyLineKey(pid, l.Key))
			if !ok {
				b.skipped = append(b.skipped, l.words())
				continue
			}
			b.lines = append(b.lines, fl)
		}
	}
	if len(b.lines) > tunnel.MaxFirewallLines-10 {
		return b, fmt.Errorf("this server would hold %d lines; Argus keeps at most %d", len(b.lines), tunnel.MaxFirewallLines-10)
	}
	b.spec.Lines = b.lines
	switch {
	case anyPolicy && denyAll:
		b.spec.DefaultOut = "deny"
	case t.state.DefaultOut == "deny":
		b.spec.DefaultOut = "allow"
	}
	b.spec.TakeOver = d.TakeOver
	b.spec.TurnOn, b.spec.Install, b.spec.ExpectHash = d.TurnOn, d.Install, d.ExpectHash
	if d.TurnOn {
		b.spec.DefaultIn = "deny"
	}
	b.spec.Keep = d.TurnOn || (b.spec.DefaultOut == "deny" && t.state.DefaultOut != "deny")
	if t.asset.ControlHost {
		b.spec.Protect = a.FirewallProtect
	}
	if b.spec.Lines == nil {
		b.spec.Lines = []tunnel.FirewallLine{}
	}
	if err := b.spec.Validate(); err != nil {
		return b, err
	}
	b.preview = tunnel.FirewallPreview(t.fwKind(d), t.zone(), t.fw.ArgusLines(), b.spec)
	return b, nil
}

// heartbeatFirewall keeps what the agent on host said about changing its
// firewall and answers with what it should do. nil for an agent older than
// firewall changes.
func (a *API) heartbeatFirewall(ctx context.Context, host string, b *tunnel.FirewallBeat) *tunnel.FirewallOrders {
	if b == nil {
		return nil
	}
	now := time.Now()
	if err := a.store.recordFWBeat(ctx, host, *b); err != nil {
		a.log.Error("heartbeat: cannot keep the firewall's state", "hostname", host, "error", err)
	}
	if b.Run != nil {
		ended, err := a.store.recordFWRun(ctx, host, *b.Run, now)
		if err != nil {
			a.log.Error("heartbeat: cannot keep a firewall change's state", "hostname", host, "error", err)
		} else if ended != nil {
			a.fwChangeEnded(ctx, *ended)
		}
	}
	a.expireFWChanges(ctx, host)
	o := &tunnel.FirewallOrders{}
	if !b.Supported {
		return o
	}
	cur, err := a.store.fwChangeInFlight(ctx, host)
	if err != nil {
		a.log.Error("heartbeat: cannot read the firewall change in flight", "hostname", host, "error", err)
		return nil
	}
	if cur != nil {
		if cur.State == FWSent && cur.spec != nil {
			o.Change = cur.spec // until the agent says it has it
		}
		if cur.State == FWWaitingKeep && cur.KeptAt != nil {
			o.Keep = cur.ID
		}
		return o
	}
	assetID, _, _, err := a.store.assetForAgent(ctx, host)
	if err != nil {
		if !errors.Is(err, caduceus.ErrNoRows) {
			a.log.Error("heartbeat: cannot find the server for its firewall", "hostname", host, "error", err)
		}
		return o
	}
	asset, err := a.store.AssetByID(ctx, assetID)
	if err != nil {
		return o
	}
	a.syncFirewall(ctx, asset, now)
	next, err := a.store.fwChangeNext(ctx, host)
	if err != nil {
		a.log.Error("heartbeat: cannot read the next firewall change", "hostname", host, "error", err)
		return o
	}
	if next == nil {
		return o
	}
	t, err := a.fwTargetFor(ctx, asset)
	if err != nil {
		a.log.Error("heartbeat: cannot read the server's firewall", "hostname", host, "error", err)
		return o
	}
	built, err := a.buildFWChange(ctx, t, next.Delta, next.ID, now)
	if err != nil {
		if c, ferr := a.store.failFWChange(ctx, next.ID, "could not be sent: "+err.Error()); ferr == nil && c != nil {
			a.fwChangeEnded(ctx, *c)
		}
		return o
	}
	if err := a.store.markFWChangeSent(ctx, next.ID, t.state, built.spec, built.preview); err != nil {
		a.log.Error("heartbeat: cannot send a firewall change", "hostname", host, "error", err)
		return o
	}
	o.Change = &built.spec
	return o
}

// fwSyncEvery is how often a server whose lines depend on other servers'
// addresses, or that blocks going out, gets its lines again.
const fwSyncEvery = time.Hour

// syncFirewall queues a change bringing the server up to date when a line
// has ended, or, hourly, when the addresses its lines stand for moved.
func (a *API) syncFirewall(ctx context.Context, asset Asset, now time.Time) {
	t, err := a.fwTargetFor(ctx, asset)
	if err != nil || !t.reported || t.synced == nil {
		return // Argus keeps nothing on this server yet
	}
	expired := false
	dependent := t.state.DefaultOut == "deny" || len(t.state.Policies) > 0
	for _, l := range t.state.Own {
		expired = expired || l.expired(now)
		for _, s := range l.From {
			dependent = dependent || s.Kind == FWFromServer || s.Kind == FWFromTag || s.Kind == FWFromPolicy
		}
	}
	if !expired && (!dependent || now.Sub(*t.synced) < fwSyncEvery) {
		return
	}
	built, err := a.buildFWChange(ctx, t, FWDelta{}, "00000000-0000-0000-0000-000000000000", now)
	if err != nil {
		return
	}
	have := t.fw.ArgusLines()
	same := !expired && len(have) == len(built.lines) && built.spec.DefaultOut == ""
	for _, l := range built.lines {
		same = same && tunnel.SameLine(have[l.Key], l)
	}
	if same {
		_ = saveFWServer(ctx, a.store.db(ctx), asset.ID, t.state) // nothing moved: just note the check
		return
	}
	summary := "Bring the firewall up to date: addresses of servers its lines name changed"
	if expired {
		summary = "Take off lines whose time ran out"
	}
	if _, err := a.store.CreateFWChange(ctx, newFWChange{AssetID: asset.ID, Hostname: t.agent,
		AssetHostname: asset.Hostname, Kind: "sync", Summary: summary, State: FWQueued, RequestedBy: "argus",
		Preview: built.preview}); err != nil && !errors.Is(err, ErrFWBusy) {
		a.log.Error("cannot queue a firewall sync", "hostname", asset.Hostname, "error", err)
	}
}

func (a *API) expireFWChanges(ctx context.Context, host string) {
	stale, err := a.store.expireFWChanges(ctx, host)
	if err != nil {
		a.log.Error("cannot end quiet firewall changes", "hostname", host, "error", err)
		return
	}
	for _, c := range stale {
		a.fwChangeEnded(ctx, c)
	}
}

// fwChangeEnded records how a change ended, once: in the audit log with
// what it ran, in the bell when it needs someone, and in its rollout.
func (a *API) fwChangeEnded(ctx context.Context, c FirewallChange) {
	action, severity := "firewall.change_applied", "notice"
	bad := false
	switch c.State {
	case FWFailed:
		action, severity, bad = "firewall.change_failed", "warn", true
	case FWRolledBack:
		action, severity, bad = "firewall.change_undone", "warn", true
	case FWInterrupted:
		action, severity, bad = "firewall.change_interrupted", "warn", true
	case FWStopped, FWCancelled, FWDenied:
		return
	}
	who := "asked by " + c.RequestedBy
	if c.DecidedBy != nil {
		who += ", approved by " + *c.DecidedBy
	}
	detail := fmt.Sprintf("The firewall change on %s (%s, %s) %s", c.AssetHostname, c.Summary, who, fwStateWords(c.State))
	if c.Message != "" {
		detail += ": " + strings.TrimSuffix(c.Message, ".")
	}
	var cmds []string
	for _, r := range c.Ran {
		mark := "ok"
		if !r.OK {
			mark = "failed"
		}
		cmds = append(cmds, r.Command+" ("+mark+")")
	}
	if len(cmds) > 0 {
		detail += ". It ran: " + strings.Join(cmds, "; ")
	}
	detail += ". The firewall saved before it is kept on the server (change " + c.ID + ")."
	if _, err := a.store.AppendAudit(ctx, AuditEvent{Action: action, Severity: severity, Target: c.AssetHostname,
		Detail: clip(detail, 4000)}); err != nil {
		a.log.Error("audit append failed", "error", err, "action", action)
	}
	if bad {
		title := "A firewall change on " + c.AssetHostname + " was undone"
		if c.State == FWFailed {
			title = "A firewall change on " + c.AssetHostname + " failed"
		}
		a.raiseAlert(ctx, ConsoleAlert{
			Kind: AlertServer, Severity: "warning", Audience: AudienceAdmins,
			Title: title, Detail: clip(c.Summary+". "+c.Message, 1000), Link: "/assets/" + c.AssetID + "?tab=firewall",
		})
	}
	if c.RolloutID == nil {
		return
	}
	if bad {
		r, err := a.store.StopFWRollout(ctx, *c.RolloutID, "stopped because "+c.AssetHostname+" "+fwStateWords(c.State)+
			"; servers not reached yet keep what they hold")
		if err == nil && r.State == FWRolloutStopped {
			a.raiseAlert(ctx, ConsoleAlert{
				Kind: AlertServer, Severity: "warning", Audience: AudienceAdmins,
				Title:  "The rollout of " + r.PolicyName + " stopped",
				Detail: clip(r.Message, 1000), Link: "/assets/firewall-policies/" + derefOr(r.PolicyID, ""),
			})
		}
		return
	}
	if done, err := a.store.finishFWRollout(ctx, *c.RolloutID); err == nil && done {
		if r, err := a.store.FWRolloutByID(ctx, *c.RolloutID); err == nil {
			_, _ = a.store.AppendAudit(ctx, AuditEvent{Action: "firewall.rollout_done", Severity: "notice",
				Target: r.PolicyName, Detail: fmt.Sprintf("Version %d of the firewall policy %s reached all %d of its servers.",
					r.Version, r.PolicyName, r.Total)})
		}
	}
}

func derefOr(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}

// FirewallProtectLines are what stays open on the control plane's own
// server, whatever its firewall's lines say: the console and the agents'
// way in, over TCP, and the tunnel's QUIC port.
func FirewallProtectLines(tcpAddrs []string, quicAddr string) []tunnel.FirewallLine {
	ports := []string{"80", "443"}
	for _, addr := range tcpAddrs {
		if _, p, err := net.SplitHostPort(addr); err == nil && p != "" && !slices.Contains(ports, p) {
			if n, err := strconv.Atoi(p); err == nil && n > 0 && n < 65536 {
				ports = append(ports, p)
			}
		}
	}
	out := []tunnel.FirewallLine{{Key: "ctlweb", Direction: "in", Action: "allow", Protocol: "tcp",
		Ports: strings.Join(ports, ",")}}
	if _, p, err := net.SplitHostPort(quicAddr); err == nil {
		if n, err := strconv.Atoi(p); err == nil && n > 0 && n < 65536 {
			out = append(out, tunnel.FirewallLine{Key: "ctlquic", Direction: "in", Action: "allow", Protocol: "udp", Ports: p})
		}
	}
	return out
}
