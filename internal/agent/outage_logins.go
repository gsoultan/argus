package agent

import (
	"fmt"
	"os/user"
	"slices"
	"strings"
	"time"
)

/*
Logins while the control plane cannot be reached.

A server set to refuse direct SSH, or to ask for approval, refuses every
plain SSH login while the agent cannot reach Argus. When the console has
turned on "let in after", a member of one of its groups is let in once the
agent has gone that long without a heartbeat reply it accepted: still
recorded, reported when Argus is back, and told so on their terminal.
Everyone else is still refused, and told which groups are let in.

"Heard" is a reply that carried a valid signature (OrderGate), so a control
plane that has been replaced, and cannot sign, counts as not heard from.
*/

// OutageLogins is the console's setting as this agent applies it.
type OutageLogins struct {
	// After is how long without hearing from the control plane; 0 is off.
	After  time.Duration
	Groups []string
}

// SetOutageLogins applies the console's setting. Minutes outside what the
// console offers, or no groups, is off.
func (c *Collector) SetOutageLogins(afterMinutes int, groups []string) {
	o := OutageLogins{Groups: slices.Clone(groups)}
	if afterMinutes > 0 && afterMinutes <= 24*60 && len(groups) > 0 {
		o.After = time.Duration(afterMinutes) * time.Minute
	}
	c.outageLogins.Store(&o)
}

// OutageLoginsSetting is the setting being applied; the zero value is off.
func (c *Collector) OutageLoginsSetting() OutageLogins {
	if p := c.outageLogins.Load(); p != nil {
		return *p
	}
	return OutageLogins{}
}

// SetHeard records when the control plane last answered, as of a reply just
// accepted.
func (c *Collector) SetHeard(at time.Time) {
	c.heard.Store(at.UnixNano())
	c.unreachable.Store(false)
}

// RestoreHeard sets when the control plane last answered from what was kept
// across a restart, without saying it answered now.
func (c *Collector) RestoreHeard(at time.Time) { c.heard.Store(at.UnixNano()) }

// SetUnreachable records that the latest heartbeat got no reply the agent
// accepted. Nothing is let in on time alone: the agent must have tried and
// failed since it last heard, so an agent that was simply stopped for a
// while does not let anyone in before its first heartbeat goes through.
func (c *Collector) SetUnreachable() { c.unreachable.Store(true) }

// Heard is when the control plane last answered; zero before SetHeard.
func (c *Collector) Heard() time.Time {
	if n := c.heard.Load(); n != 0 {
		return time.Unix(0, n)
	}
	return time.Time{}
}

// outageFor reports how long the control plane has not been heard from, when
// that is long enough for the setting to apply.
func (c *Collector) outageFor() (time.Duration, bool) {
	o := c.OutageLoginsSetting()
	heard := c.Heard()
	if o.After <= 0 || heard.IsZero() || !c.unreachable.Load() {
		return 0, false
	}
	since := c.now().Sub(heard)
	return since, since >= o.After
}

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// groupsOf is the groups a local account is in, by name. A lookup that fails
// gives none, so the answer falls back to refusing.
func groupsOf(account string) []string {
	u, err := user.Lookup(account)
	if err != nil {
		return nil
	}
	ids, _ := u.GroupIds()
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if g, err := user.LookupGroupId(id); err == nil {
			out = append(out, g.Name)
		}
	}
	return out
}

// outageAdmit decides a login that would be refused or held while the
// control plane cannot be reached. handled is false when the setting does not
// apply now, and the login goes on to the usual answer.
func (c *Collector) outageAdmit(m SessionStart) (a Approval, admit, handled bool, reason string) {
	since, on := c.outageFor()
	if !on {
		return Approval{}, false, false, ""
	}
	o := c.OutageLoginsSetting()
	lookup := c.GroupsOf
	if lookup == nil {
		lookup = groupsOf
	}
	have := lookup(m.Principal)
	for _, g := range o.Groups {
		if slices.Contains(have, g) {
			c.log.Warn("DIRECT LOGIN LET IN — the control plane cannot be reached",
				"principal", m.Principal, "group", g, "client", m.ClientAddr,
				"unreachable_for", since.Round(time.Second).String(), "command", m.Command)
			return Approval{Approved: true, Outage: true, Notice: outageNotice(since, g)}, true, true, ""
		}
	}
	c.log.Warn("DIRECT LOGIN REFUSED — the control plane cannot be reached and the account is in none "+
		"of the groups let in meanwhile", "principal", m.Principal, "client", m.ClientAddr,
		"groups", strings.Join(o.Groups, ","))
	return Approval{}, false, true, fmt.Sprintf("this server cannot reach Argus; only members of %s "+
		"are let in until it is back", strings.Join(o.Groups, ", "))
}

// outageNotice is what someone let in is told on their terminal.
func outageNotice(since time.Duration, group string) string {
	return fmt.Sprintf("this server has not heard from Argus for %s.\n"+
		"you are in the %q group, so you are let in to fix it.\n"+
		"this session is recorded on the server and sent to Argus when it is back.",
		durationWords(since), group)
}

func durationWords(d time.Duration) string {
	m := int(d / time.Minute)
	switch {
	case m < 2:
		return "a minute"
	case m < 120:
		return fmt.Sprintf("%d minutes", m)
	}
	return fmt.Sprintf("%d hours", m/60)
}
