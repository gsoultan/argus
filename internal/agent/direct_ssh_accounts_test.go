package agent

import (
	"context"
	"testing"
	"time"
)

/*
Direct SSH logins per login account: the console names accounts that do
something other than the server's setting. These hold that the account's own
mode wins in both directions, that an account not named follows the server,
and that a damaged entry refuses rather than falling back to the server.
*/

// On a server that allows direct logins, sys-admin is held for approval and
// staging opens straight away.
func TestANamedAccountIsHeldOnAnOpenServer(t *testing.T) {
	c, sock := testCollector(t)
	defer c.CloseWithin(time.Second)
	c.SetDirectSSH(DirectSSHAllow)
	c.SetDirectSSHAccounts(map[string]string{"sys-admin": DirectSSHApprove})
	asked := make(chan string, 2)
	c.AskApproval = func(ctx context.Context, m SessionStart) Approval {
		asked <- m.Principal
		return Approval{ID: "req-9", Reason: "admin@example.com denied this login"}
	}

	conn, dec := startAsAccount(t, sock, "sys-admin")
	if r := readReply(t, conn, dec); !r.Pending || r.Record {
		t.Fatalf("sys-admin: first answer = %+v, want held for approval", r)
	}
	if r := readReply(t, conn, dec); !r.Refused || r.Record {
		t.Fatalf("sys-admin: after denial = %+v, want refused", r)
	}
	if got := <-asked; got != "sys-admin" {
		t.Fatalf("asked about %q", got)
	}

	conn, dec = startAsAccount(t, sock, "staging")
	if r := readReply(t, conn, dec); !r.Record || r.Refused || r.Pending {
		t.Fatalf("staging: answer = %+v, want recorded straight away", r)
	}
	select {
	case p := <-asked:
		t.Fatalf("an approval was asked for %q, which the server allows", p)
	default:
	}
}

// On a server that refuses, a named account may still be allowed and
// recorded, and every other account is still refused.
func TestANamedAccountIsAllowedOnAClosedServer(t *testing.T) {
	c, sock := testCollector(t)
	defer c.CloseWithin(time.Second)
	c.SetDirectSSH(DirectSSHRefuse)
	c.SetDirectSSHAccounts(map[string]string{"staging": DirectSSHAllow})
	sealed := make(chan SessionRecord, 1)
	c.OnSession = func(r SessionRecord) { sealed <- r }

	conn, dec := startAsAccount(t, sock, "staging")
	if r := readReply(t, conn, dec); !r.Record || r.Refused {
		t.Fatalf("staging: answer = %+v, want recorded", r)
	}
	conn2, dec2 := startAsAccount(t, sock, "ops")
	if r := readReply(t, conn2, dec2); !r.Refused || r.Record {
		t.Fatalf("ops: answer = %+v, want refused", r)
	}
}

// An entry that is not a mode refuses that account, rather than leaving it
// to a server that allows everyone.
func TestADamagedAccountEntryRefuses(t *testing.T) {
	c := &Collector{}
	c.SetDirectSSH(DirectSSHAllow)
	c.SetDirectSSHAccounts(map[string]string{"sys-admin": "sometimes"})
	if got := c.DirectSSHFor("sys-admin"); got != DirectSSHRefuse {
		t.Fatalf("damaged entry applied as %q", got)
	}
	if got := c.DirectSSHFor("staging"); got != DirectSSHAllow {
		t.Fatalf("an account not named got %q, want the server's", got)
	}
}
