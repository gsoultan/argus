package control

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

/*
Renaming a server in Argus keeps it the same machine.

The agent was enrolled under the old name: its credential, every grant it
accepts and its tunnel are under it. A rename that only rewrote the hostname
left the server "not connected" in the console, refused every terminal,
desktop and database session with "the agent on <new name> is not connected",
dropped the sessions its agent reported onto no server, and stopped an
approved access request from opening it.

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable \
	  go test ./internal/control/ -run 'Renamed|Renaming'
*/

func renameAsset(t *testing.T, h http.Handler, asset Asset, hostname, address string) (int, Asset, string) {
	t.Helper()
	in := AssetInput{
		Hostname: hostname, Address: address, Port: asset.Port, Protocol: asset.Protocol,
		OS: asset.OS, Tags: asset.Tags, GroupName: asset.GroupName, Principals: asset.Principals,
		CredentialMode: asset.CredentialMode, CredentialRef: asset.CredentialRef, Domain: asset.Domain,
		DesktopMode: asset.DesktopMode, Kind: asset.Kind, ConnectAsk: asset.ConnectAsk,
	}
	body, _ := json.Marshal(in)
	w := do(t, h, http.MethodPatch, "/api/v1/assets/"+asset.ID, "admin-token", string(body))
	var got Asset
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	return w.Code, got, w.Body.String()
}

func TestARenamedServerIsStillReachedThroughItsAgent(t *testing.T) {
	e := newTunnelEnv(t)
	signer := e.withTerminal(t)
	ctx := context.Background()

	host := unique("rename-old")
	cred, _ := enrolled(t, e.store, e.h, host)
	asset, err := e.store.CreateAsset(ctx, AssetInput{
		Hostname: host, Address: "192.0.2.10", Principals: []string{me(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.runTerminalAgent(t, cred, host, t.TempDir())

	// One approval still running, one long over.
	email := unique("oncall") + "@corp.example"
	if _, err := e.store.pool.Exec(ctx, `
		INSERT INTO access_requests (requester_email, asset_hostnames, principal,
		                             justification, duration_minutes, state, expires_at)
		VALUES ($1, ARRAY[$2], 'root', 'incident', 30, 'approved', now() + interval '30 minutes'),
		       ($1, ARRAY[$2], 'root', 'last week', 30, 'approved', now() - interval '7 days')`,
		email, host); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.store.pool.Exec(ctx, `DELETE FROM access_requests WHERE requester_email = $1`, email)
	})

	renamed := unique("rename-new") + ".corp.example"
	cleanHost(t, e.store, renamed)
	code, got, body := renameAsset(t, e.h, asset, renamed, "198.51.100.7")
	if code != http.StatusOK {
		t.Fatalf("rename: %d %s", code, body)
	}
	if got.Hostname != renamed || got.Address != "198.51.100.7" {
		t.Fatalf("the rename did not take: %+v", got)
	}
	if got.AgentHostname != host {
		t.Errorf("the agent's name was not kept: agentHostname %q, want %q", got.AgentHostname, host)
	}
	if !got.TunnelConnected {
		t.Error("the renamed server's agent is connected, and the console says it is not")
	}

	// The audit chain says both names, and that the agent kept its own.
	if ev := lastAuditOn(t, e.store, renamed); ev.Action != "asset.updated" ||
		!strings.Contains(ev.Detail, "Renamed "+host+" to "+renamed) ||
		!strings.Contains(ev.Detail, "still goes by "+host) ||
		!strings.Contains(ev.Detail, "1 open access request") ||
		!strings.Contains(ev.Detail, "198.51.100.7") {
		t.Errorf("the audit entry does not say what the rename did: %s %s", ev.Action, ev.Detail)
	}

	// The running approval follows the server; the finished one keeps the
	// name it was decided under.
	if ok, _, err := e.store.ActiveGrant(ctx, email, renamed, "root"); err != nil || !ok {
		t.Errorf("an approved request stopped applying to the renamed server: %v %v", ok, err)
	}
	var stale int
	if err := e.store.pool.QueryRow(ctx, `
		SELECT count(*) FROM access_requests
		 WHERE requester_email = $1 AND $2 = ANY(asset_hostnames)`, email, host).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if stale != 1 {
		t.Errorf("%d requests still name the old name; want the expired one only", stale)
	}

	// A terminal opened under the new name reaches the same agent.
	ticket, _ := signer.IssueTicket("ana@example.com", renamed, me(t), time.Minute)
	c, _, err := e.openTerminal(t, renamed, me(t), ticket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	run := readTerminal(t, c, 15*time.Second, "echo renamed-$((6*7))\n", "exit 0\n")
	if run.ready.Session == "" {
		t.Fatalf("the renamed server's terminal did not open: %q", run.errors)
	}
	if !strings.Contains(run.output.String(), "renamed-42") {
		t.Errorf("the shell's output did not arrive: %q", run.output.String())
	}

	// Its agent keeps reporting under the old name, and lands on the server.
	if w := heartbeatAs(t, e.h, cred, host); w.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", w.Code, w.Body)
	}
	after, err := e.store.AssetByID(ctx, asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.AgentState != "healthy" || after.AgentLastSeenAt == nil {
		t.Errorf("the agent's heartbeat no longer reaches the renamed server: %s", after.AgentState)
	}
	sid := newUUID(t)
	dropSession(t, e.store, sid)
	if err := e.store.UpsertSession(ctx, Session{
		ID: sid, UserEmail: "ana@example.com", AssetHostname: host,
		Principal: me(t), Protocol: "ssh", Origin: "brokered", State: "active",
		StartedAt: time.Now().UTC(), Fidelity: "pty", RiskFlags: []string{}, ReportedBy: "agent",
	}); err != nil {
		t.Fatal(err)
	}
	var sessAsset *string
	if err := e.store.pool.QueryRow(ctx,
		`SELECT asset_id::text FROM sessions WHERE id = $1`, sid).Scan(&sessAsset); err != nil {
		t.Fatal(err)
	}
	if sessAsset == nil || *sessAsset != asset.ID {
		t.Errorf("a session the agent reported landed on %v, not the renamed server", sessAsset)
	}

	// Renaming it back to the agent's own name needs no separate name.
	code, back, body := renameAsset(t, e.h, after, host, "192.0.2.10")
	if code != http.StatusOK {
		t.Fatalf("rename back: %d %s", code, body)
	}
	if back.AgentHostname != "" || !back.TunnelConnected {
		t.Errorf("renamed back to its agent's name: agentHostname %q, connected %v",
			back.AgentHostname, back.TunnelConnected)
	}
	if ok, _, _ := e.store.ActiveGrant(ctx, email, host, "root"); !ok {
		t.Error("the approval did not follow the server back to its old name")
	}
}

// Renaming a server whose agent knows only its short name keeps that name.
func TestRenamingAServerFoundByItsShortNameKeepsItsAgent(t *testing.T) {
	e := newTunnelEnv(t)
	ctx := context.Background()
	short := unique("rename-short")
	cred, _ := enrolled(t, e.store, e.h, short)
	asset, err := e.store.CreateAsset(ctx, AssetInput{
		Hostname: short + ".corp.example", Principals: []string{"ops"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = e.store.pool.Exec(ctx, `DELETE FROM assets WHERE id = $1::uuid`, asset.ID) })
	e.runAgent(t, cred, short)
	e.waitTunnel(t, short, "")

	if got, err := e.store.AgentNameFor(ctx, asset.Hostname); err != nil || got != short {
		t.Fatalf("the agent of a server found by its short name is %q (%v), want %q", got, err, short)
	}
	renamed := unique("rename-other") + ".corp.example"
	cleanHost(t, e.store, renamed)
	code, got, body := renameAsset(t, e.h, asset, renamed, "")
	if code != http.StatusOK {
		t.Fatalf("rename: %d %s", code, body)
	}
	if got.AgentHostname != short || !got.TunnelConnected {
		t.Errorf("after the rename: agentHostname %q, connected %v", got.AgentHostname, got.TunnelConnected)
	}
	if name, _ := e.store.AgentNameFor(ctx, renamed); name != short {
		t.Errorf("sessions on the renamed server would go to %q, not its agent %q", name, short)
	}
}

// A removed server keeps its name, so its history still reads; renaming
// another one to it says so rather than claiming it is in the inventory.
func TestRenamingToARemovedServersNameSaysWhy(t *testing.T) {
	_, s, h, _ := enrollAPI(t)
	ctx := context.Background()
	gone := testAsset(t, s, "ops")
	if err := s.ArchiveAsset(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	live := testAsset(t, s, "ops")
	code, _, body := renameAsset(t, h, live, gone.Hostname, "")
	if code != http.StatusBadRequest || !strings.Contains(body, "removed from Argus") {
		t.Fatalf("renaming to a removed server's name: %d %s", code, body)
	}
}
