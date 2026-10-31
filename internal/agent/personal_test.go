package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"
)

/*
Direct logins to personal accounts. A personal account a team may reach
directly decides for itself, whatever the server's setting for every other
account; everything else is decided exactly as before.
*/

func startAsAccount(t *testing.T, sock, principal string) (net.Conn, *json.Decoder) {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := json.NewEncoder(conn).Encode(SessionStart{
		Type: "start", Principal: principal, Hostname: "db-01", Origin: Direct,
		ClientAddr: "10.0.0.9", StartedAt: time.Now().UTC(), Cols: 80, Rows: 24,
	}); err != nil {
		t.Fatal(err)
	}
	return conn, json.NewDecoder(bufio.NewReader(conn))
}

// On a server that refuses direct logins, the deploy team's personal account
// is let in and recorded in its owner's name, and the same login to any
// other account is still refused.
func TestAPersonalAccountAllowedDirectIsLetInAndRecorded(t *testing.T) {
	c, sock := testCollector(t)
	defer c.CloseWithin(time.Second)
	c.SetDirectSSH(DirectSSHRefuse)
	c.SetPersonalAccounts([]PersonalAccount{
		{Name: "ana", Owner: "ana@corp.example", Direct: DirectSSHAllow},
		{Name: "budi", Owner: "budi@corp.example"},
	})
	sealed := make(chan SessionRecord, 4)
	c.OnSession = func(r SessionRecord) { sealed <- r }
	allowed := make(chan SessionStart, 1)
	c.OnAllowed = func(m SessionStart) { allowed <- m }

	conn, dec := startAsAccount(t, sock, "ana")
	r := readReply(t, conn, dec)
	if r.Refused || !r.Record || r.SessionID == "" {
		t.Fatalf("ana: %+v, want recorded", r)
	}
	select {
	case m := <-allowed:
		if m.Actor != "ana@corp.example" {
			t.Errorf("reported as allowed for %q", m.Actor)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("an allowed personal login was not reported")
	}
	_ = json.NewEncoder(conn).Encode(SessionEnd{Type: "end"})
	select {
	case rec := <-sealed:
		if rec.Actor != "ana@corp.example" || rec.DirectApproval != "personal" {
			t.Errorf("sealed as actor %q approval %q", rec.Actor, rec.DirectApproval)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the session was never sealed")
	}

	// An account with no direct SSH, a shared account, and one the control
	// plane never named all follow the server, which refuses.
	for _, who := range []string{"budi", "ops", "mallory"} {
		conn, dec := startAsAccount(t, sock, who)
		if r := readReply(t, conn, dec); !r.Refused || r.Record {
			t.Errorf("%s: %+v, want refused", who, r)
		}
	}
}

// The ops team's personal account is held for an admin, and refused when
// nobody approves.
func TestAPersonalAccountOnApprovalWaitsForAnAdmin(t *testing.T) {
	c, sock := testCollector(t)
	defer c.CloseWithin(time.Second)
	c.SetDirectSSH(DirectSSHAllow) // the server's own setting does not apply
	c.SetPersonalAccounts([]PersonalAccount{{Name: "siti", Owner: "siti@corp.example", Direct: DirectSSHApprove}})
	asked := make(chan SessionStart, 1)
	c.AskApproval = func(_ context.Context, m SessionStart) Approval {
		asked <- m
		return Approval{Reason: "denied in a test"}
	}

	conn, dec := startAsAccount(t, sock, "siti")
	if r := readReply(t, conn, dec); !r.Pending {
		t.Fatalf("first answer %+v, want pending", r)
	}
	select {
	case m := <-asked:
		if m.Principal != "siti" {
			t.Errorf("asked about %q", m.Principal)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no admin was asked")
	}
	if r := readReply(t, conn, dec); !r.Refused || r.Record {
		t.Fatalf("after a denial: %+v", r)
	}
}

// Before the control plane names any personal account, none is treated as
// one, and a malformed name is never kept.
func TestPersonalAccountsAreOnlyWhatTheControlPlaneSaid(t *testing.T) {
	c, _ := testCollector(t)
	defer c.CloseWithin(time.Second)
	if _, ok := c.PersonalAccount("ana"); ok {
		t.Fatal("a personal account before any list")
	}
	c.SetPersonalAccounts([]PersonalAccount{
		{Name: "../etc", Direct: DirectSSHAllow},
		{Name: "ana", Direct: "always"},
	})
	if _, ok := c.PersonalAccount("../etc"); ok {
		t.Error("kept a name no account can have")
	}
	if p, _ := c.PersonalAccount("ana"); p.Direct != "" {
		t.Errorf("kept an unknown direct mode %q", p.Direct)
	}
}
