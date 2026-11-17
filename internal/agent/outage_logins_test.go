package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

/*
Logins while the control plane cannot be reached (outage_logins.go). The
closed side holds: off, too soon, never tried, or not in a group is the usual
answer; only a member of a named group is let in, recorded, and told why.
*/

// outageCollector is a collector refusing direct SSH, with "ops" in the
// groups ops and sudo, that last heard from the control plane ago.
func outageCollector(t *testing.T, mode string, ago time.Duration) (*Collector, string) {
	t.Helper()
	c, sock := testCollector(t)
	t.Cleanup(func() { _ = c.CloseWithin(time.Second) })
	c.SetDirectSSH(mode)
	c.GroupsOf = func(account string) []string {
		if account == "ops" {
			return []string{"ops", "sudo"}
		}
		return nil
	}
	c.SetHeard(time.Now().Add(-ago))
	c.SetUnreachable()
	return c, sock
}

// A member of a named group is let in once the control plane has been out of
// reach long enough, recorded, told why, and the session says how.
func TestAnAdminIsLetInWhileArgusIsUnreachable(t *testing.T) {
	c, sock := outageCollector(t, DirectSSHRefuse, 22*time.Minute)
	c.SetOutageLogins(15, []string{"wheel", "sudo"})
	sealed := make(chan SessionRecord, 1)
	c.OnSession = func(r SessionRecord) { sealed <- r }

	conn, dec := startAs(t, sock, Direct)
	r := readReply(t, conn, dec)
	if !r.Record || r.Refused {
		t.Fatalf("answer = %+v, want record", r)
	}
	if !strings.Contains(r.Notice, "22 minutes") || !strings.Contains(r.Notice, `"sudo" group`) {
		t.Errorf("notice = %q", r.Notice)
	}
	_ = json.NewEncoder(conn).Encode(SessionEnd{Type: "end"})
	select {
	case rec := <-sealed:
		if rec.DirectApproval != "outage" {
			t.Errorf("session does not say it was let in during an outage: %+v", rec)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the session was not sealed")
	}
}

// Someone in none of the groups is still refused, and told who is let in.
func TestOthersAreRefusedAndToldWhoIsLetIn(t *testing.T) {
	c, sock := outageCollector(t, DirectSSHRefuse, time.Hour)
	c.SetOutageLogins(15, []string{"wheel", "oncall"})
	conn, dec := startAs(t, sock, Direct)
	r := readReply(t, conn, dec)
	if !r.Refused || r.Record {
		t.Fatalf("answer = %+v, want refused", r)
	}
	if !strings.Contains(r.Reason, "only members of wheel, oncall") {
		t.Errorf("reason = %q", r.Reason)
	}
	if n := recordings(t, c); n != 0 {
		t.Errorf("%d recordings opened for a refused login", n)
	}
}

// Off, too soon, or not yet tried since it last heard: the usual refusal.
func TestNothingIsLetInOffTooSoonOrUntried(t *testing.T) {
	cases := map[string]func(c *Collector){
		"off":      func(c *Collector) { c.SetOutageLogins(0, []string{"sudo"}) },
		"too soon": func(c *Collector) { c.SetOutageLogins(60, []string{"sudo"}) },
		"untried": func(c *Collector) {
			c.SetOutageLogins(15, []string{"sudo"})
			c.RestoreHeard(time.Now().Add(-time.Hour))
			c.unreachable.Store(false)
		},
		"never heard": func(c *Collector) {
			c.SetOutageLogins(15, []string{"sudo"})
			c.heard.Store(0)
		},
		"no groups": func(c *Collector) { c.SetOutageLogins(15, nil) },
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			c, sock := outageCollector(t, DirectSSHRefuse, 30*time.Minute)
			set(c)
			conn, dec := startAs(t, sock, Direct)
			r := readReply(t, conn, dec)
			if !r.Refused || r.Record || r.Reason != ConsoleOnlyReason {
				t.Fatalf("answer = %+v, want the usual refusal", r)
			}
		})
	}
}

// A server that asks for approval lets a member in without asking a control
// plane it cannot reach, and refuses others without holding them.
func TestApproveLetsAnAdminInWithoutAskingDuringAnOutage(t *testing.T) {
	c, sock := outageCollector(t, DirectSSHApprove, 10*time.Minute)
	c.SetOutageLogins(5, []string{"sudo"})
	asked := false
	c.AskApproval = func(ctx context.Context, m SessionStart) Approval {
		asked = true
		return Approval{}
	}
	conn, dec := startAs(t, sock, Direct)
	if r := readReply(t, conn, dec); !r.Record || r.Pending {
		t.Fatalf("answer = %+v, want record", r)
	}
	if asked {
		t.Error("asked a control plane it cannot reach")
	}

	c.SetOutageLogins(5, []string{"wheel"})
	conn, dec = startAs(t, sock, Direct)
	if r := readReply(t, conn, dec); !r.Refused || r.Pending {
		t.Fatalf("answer = %+v, want refused at once", r)
	}
}

// Hearing from the control plane again ends it at once.
func TestHearingFromArgusEndsIt(t *testing.T) {
	c, sock := outageCollector(t, DirectSSHRefuse, time.Hour)
	c.SetOutageLogins(15, []string{"sudo"})
	c.SetHeard(time.Now())
	conn, dec := startAs(t, sock, Direct)
	if r := readReply(t, conn, dec); !r.Refused || r.Reason != ConsoleOnlyReason {
		t.Fatalf("answer = %+v, want the usual refusal", r)
	}
}

// "allow" is untouched: it was already letting everyone in, recorded.
func TestAllowIsUntouchedByAnOutage(t *testing.T) {
	c, sock := outageCollector(t, DirectSSHAllow, time.Hour)
	c.SetOutageLogins(15, []string{"wheel"})
	conn, dec := startAs(t, sock, Direct)
	if r := readReply(t, conn, dec); !r.Record || r.Notice != "" {
		t.Fatalf("answer = %+v, want record with no notice", r)
	}
}
