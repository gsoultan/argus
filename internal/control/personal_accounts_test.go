package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

/*
Personal server accounts, and direct SSH for chosen teams.

What these hold is the closed side: an account only exists for someone who is
in a team holding the server with personal accounts on, and stops existing the
moment they leave it or are removed; a key reaches a server only where direct
SSH is on; a session names a person only when the control plane agrees the
account is theirs.

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable \
	  go test ./internal/control/ -run Personal
*/

const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl ana@laptop"

func TestPersonalLoginNameFromEmail(t *testing.T) {
	for in, want := range map[string]string{
		"Ana.Sari@corp.example":        "ana.sari",
		"budi+ops@corp.example":        "budi",
		"9lives@corp.example":          "u9lives",
		"root@corp.example":            "root2",
		"-.x@corp.example":             "x",
		"émile@corp.example":           "mile",
		strings.Repeat("a", 40) + "@x": strings.Repeat("a", 30),
	} {
		if got := loginNameCandidates(in)[0]; got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
}

func personalReply(t *testing.T, h http.Handler, cred, host string) []PersonalAccount {
	t.Helper()
	w := do(t, h, http.MethodPost, "/api/v1/report/heartbeat", cred,
		`{"hostname":"`+host+`","version":"1.0.0","active_sessions":0,`+
			`"personal_accounts":[{"name":"x","state":"ready"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", w.Code, w.Body)
	}
	var out struct {
		Personal *[]PersonalAccount `json:"personal_accounts"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out.Personal == nil {
		t.Fatalf("heartbeat reply has no personal accounts: %s", w.Body)
	}
	return *out.Personal
}

func loginNameOf(t *testing.T, s *Store, email string) string {
	t.Helper()
	acct, err := s.ServerAccountOf(context.Background(), email)
	if err != nil {
		t.Fatalf("ServerAccountOf: %v", err)
	}
	return acct.LoginName
}

// A team given a server with personal accounts gets one account each, and
// the agent is told only about them.
func TestPersonalAccountsFollowTheTeam(t *testing.T) {
	s, h, _ := reportAPI(t)
	ctx := context.Background()
	asset, cred := enrolledAsset(t, s, h)
	ana, _ := newAccount(t, s, "operator")
	budi, _ := newAccount(t, s, "operator")
	team := testTeam(t, s, ana, budi)

	if got := personalReply(t, h, cred, asset.Hostname); len(got) != 0 {
		t.Fatalf("accounts before anyone asked for them: %+v", got)
	}
	if err := s.SetTeamAccess(ctx, asset.ID, team, TeamAccess{PersonalAccounts: true}, "admin@corp.example"); err != nil {
		t.Fatalf("SetTeamAccess: %v", err)
	}
	got := personalReply(t, h, cred, asset.Hostname)
	if len(got) != 2 {
		t.Fatalf("want two accounts, got %+v", got)
	}
	for _, p := range got {
		if p.Direct != "" || len(p.Keys) != 0 || p.Sudo {
			t.Errorf("%s was given more than an account: %+v", p.Name, p)
		}
	}

	// Leaving the team, or being removed from Argus, takes the account off
	// the list, which is what tells the agent to lock it.
	if err := s.RemoveTeamMember(ctx, team, budi); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAccountDisabledBy(ctx, ana, true, "admin@corp.example"); err != nil {
		t.Fatal(err)
	}
	if got := personalReply(t, h, cred, asset.Hostname); len(got) != 0 {
		t.Fatalf("a removed person still has an account: %+v", got)
	}

	// And the removal says who did it.
	accts, err := s.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range accts {
		if a.Email == ana && (a.DisabledBy != "admin@corp.example" || a.DisabledAt == nil) {
			t.Errorf("removal recorded as %q at %v", a.DisabledBy, a.DisabledAt)
		}
	}

	// The agent's report about them is kept for the server page.
	var states string
	_ = s.pool.QueryRow(ctx, `SELECT agent_personal_accounts::text FROM assets WHERE id = $1::uuid`,
		asset.ID).Scan(&states)
	if !strings.Contains(states, `"ready"`) {
		t.Errorf("agent's account states not kept: %q", states)
	}
}

// A key reaches a server only for a team allowed to SSH there directly, and
// the most open of a person's teams wins.
func TestPersonalDirectSSHIsPerTeam(t *testing.T) {
	s, h, _ := reportAPI(t)
	ctx := context.Background()
	asset, cred := enrolledAsset(t, s, h)
	ana, _ := newAccount(t, s, "operator")
	ops := testTeam(t, s, ana)
	deploy := testTeam(t, s, ana)

	set := func(team string, in TeamAccess) {
		t.Helper()
		if err := s.SetTeamAccess(ctx, asset.ID, team, in, "admin@corp.example"); err != nil {
			t.Fatalf("SetTeamAccess: %v", err)
		}
	}
	set(ops, TeamAccess{PersonalAccounts: true, DirectSSH: DirectSSHApprove})
	if _, err := s.SetSSHKey(ctx, ana, testKey); err != nil {
		t.Fatalf("SetSSHKey: %v", err)
	}
	got := personalReply(t, h, cred, asset.Hostname)
	if len(got) != 1 || got[0].Direct != DirectSSHApprove || len(got[0].Keys) != 1 || got[0].Sudo {
		t.Fatalf("ops: %+v", got)
	}

	set(deploy, TeamAccess{PersonalAccounts: true, DirectSSH: DirectSSHAllow, Sudo: true})
	got = personalReply(t, h, cred, asset.Hostname)
	if len(got) != 1 || got[0].Direct != DirectSSHAllow || !got[0].Sudo {
		t.Fatalf("ops and deploy: %+v", got)
	}

	set(deploy, TeamAccess{PersonalAccounts: true})
	set(ops, TeamAccess{PersonalAccounts: true})
	got = personalReply(t, h, cred, asset.Hostname)
	if len(got) != 1 || got[0].Direct != "" || len(got[0].Keys) != 0 {
		t.Fatalf("direct SSH turned off, key still sent: %+v", got)
	}
}

// Sudo and direct SSH are for a person, so they need a personal account.
func TestPersonalOptionsNeedPersonalAccounts(t *testing.T) {
	s := testStore(t)
	asset := testAsset(t, s, "ops")
	team := testTeam(t, s)
	for _, in := range []TeamAccess{
		{Principals: []string{"ops"}, Sudo: true},
		{Principals: []string{"ops"}, DirectSSH: DirectSSHAllow},
		{PersonalAccounts: true, DirectSSH: "sometimes"},
	} {
		if err := s.SetTeamAccess(context.Background(), asset.ID, team, in, "admin@corp.example"); err == nil {
			t.Errorf("accepted %+v", in)
		}
	}
}

// A person may open a session as their own account and nobody else's.
func TestPersonalAccountOnlyItsOwnerMayUse(t *testing.T) {
	a, s := authorizeAPI(t)
	ctx := context.Background()
	ana, _ := newAccount(t, s, "operator")
	budi, _ := newAccount(t, s, "operator")
	asset := testAsset(t, s, "ops")
	team := testTeam(t, s, ana, budi)
	if err := s.SetTeamAccess(ctx, asset.ID, team, TeamAccess{PersonalAccounts: true}, "admin@corp.example"); err != nil {
		t.Fatal(err)
	}
	anaName, budiName := loginNameOf(t, s, ana), loginNameOf(t, s, budi)
	if anaName == "" || anaName == budiName {
		t.Fatalf("names %q and %q", anaName, budiName)
	}
	if got := askAuthorize(t, a, ana, asset.Hostname, anaName); !got.Allowed {
		t.Fatalf("refused their own account: %s", got.Reason)
	}
	if got := askAuthorize(t, a, ana, asset.Hostname, budiName); got.Allowed {
		t.Fatal("allowed someone else's personal account")
	}
	// Personal accounts alone give no shared account.
	if got := askAuthorize(t, a, ana, asset.Hostname, "ops"); got.Allowed {
		t.Fatal("personal accounts gave a shared account too")
	}
	list, err := s.AssetsForUser(ctx, ana)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || len(list[0].Principals) != 1 || list[0].Principals[0] != anaName {
		t.Fatalf("their server list: %+v", list)
	}
}

func reportPersonalSession(t *testing.T, h http.Handler, cred, host, principal, who, approval string) string {
	t.Helper()
	id := newUUID(t)
	start := time.Now().UTC()
	body := `{"id":"` + id + `","userEmail":"` + who + `","assetHostname":"` + host +
		`","principal":"` + principal + `","protocol":"ssh","origin":"direct","state":"closed",` +
		`"startedAt":"` + start.Format(time.RFC3339Nano) + `","endedAt":"` +
		start.Add(time.Minute).Format(time.RFC3339Nano) + `","exitCode":0,"directApproval":"` + approval + `"}`
	if w := do(t, h, http.MethodPost, "/api/v1/report/session", cred, body); w.Code != http.StatusOK {
		t.Fatalf("report session: %d %s", w.Code, w.Body)
	}
	return id
}

func sessionOwner(t *testing.T, s *Store, id string) string {
	t.Helper()
	var who string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT user_email FROM sessions WHERE id = $1::uuid`, id).Scan(&who); err != nil {
		t.Fatal(err)
	}
	return who
}

// A direct session names a person only through their own account, while a
// team of theirs may SSH directly; an agent's claim otherwise is not believed.
func TestPersonalDirectSessionAttribution(t *testing.T) {
	s, h, _ := reportAPI(t)
	ctx := context.Background()
	asset, cred := enrolledAsset(t, s, h)
	ana, _ := newAccount(t, s, "operator")
	team := testTeam(t, s, ana)
	if err := s.SetTeamAccess(ctx, asset.ID, team,
		TeamAccess{PersonalAccounts: true, DirectSSH: DirectSSHAllow}, "admin@corp.example"); err != nil {
		t.Fatal(err)
	}
	name := loginNameOf(t, s, ana)

	id := reportPersonalSession(t, h, cred, asset.Hostname, name, ana, "personal")
	if who := sessionOwner(t, s, id); who != ana {
		t.Errorf("their own direct session attributed to %q", who)
	}
	if e := lastAuditOn(t, s, asset.Hostname); e.Action != "session.direct_recorded" ||
		!strings.Contains(e.Detail, ana) {
		t.Errorf("audited as %s: %s", e.Action, e.Detail)
	}

	// A shared account cannot be made to name a person.
	id = reportPersonalSession(t, h, cred, asset.Hostname, "ops", ana, "personal")
	if who := sessionOwner(t, s, id); who != "ops@"+asset.Hostname {
		t.Errorf("a shared account's session attributed to %q", who)
	}
	if e := lastAuditOn(t, s, asset.Hostname); e.Action != "session.direct_detected" {
		t.Errorf("a false claim was believed: %s", e.Action)
	}

	// Nor someone else's account name theirs.
	id = reportPersonalSession(t, h, cred, asset.Hostname, name, "mallory@corp.example", "personal")
	if who := sessionOwner(t, s, id); who != name+"@"+asset.Hostname {
		t.Errorf("a session claimed for the wrong person attributed to %q", who)
	}

	// With direct SSH turned off, a personal session is a bypass again,
	// though still theirs.
	if err := s.SetTeamAccess(ctx, asset.ID, team, TeamAccess{PersonalAccounts: true}, "admin@corp.example"); err != nil {
		t.Fatal(err)
	}
	reportPersonalSession(t, h, cred, asset.Hostname, name, ana, "personal")
	if e := lastAuditOn(t, s, asset.Hostname); e.Action != "session.direct_detected" {
		t.Errorf("allowed after direct SSH was turned off: %s", e.Action)
	}
}

func TestPersonalSSHKeyIsOneKeyAlone(t *testing.T) {
	for _, bad := range []string{
		"hello",
		`from="10.0.0.1" ` + testKey,
		testKey + "\n" + testKey,
		"-----BEGIN OPENSSH PRIVATE KEY-----",
	} {
		if _, err := normaliseSSHKey(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	got, err := normaliseSSHKey("  " + testKey + "  ")
	if err != nil || got != testKey {
		t.Errorf("got %q, %v", got, err)
	}
}

// Only the person sets their own key, only once a team of theirs may SSH
// directly somewhere, and the change is audited.
func TestPersonalSSHKeyIsSetByItsOwner(t *testing.T) {
	a, s, h, _ := enrollAPI(t)
	ctx := context.Background()
	ana, _ := newAccount(t, s, "operator")
	a.UserTokens["ana-token"] = ana

	w := do(t, h, http.MethodPut, "/api/v1/me/ssh-key", "ana-token", `{"key":"not a key"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("junk key: %d %s", w.Code, w.Body)
	}

	// Console only: a key would open nothing, so none is taken or asked for.
	w = do(t, h, http.MethodPut, "/api/v1/me/ssh-key", "ana-token", `{"key":"`+testKey+`"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("key with no direct SSH: %d %s", w.Code, w.Body)
	}
	if acct, err := s.ServerAccountOf(ctx, ana); err != nil || acct.DirectSSH {
		t.Fatalf("console-only person told they may SSH directly: %+v %v", acct, err)
	}
	asset := testAsset(t, s, "ops")
	team := testTeam(t, s, ana)
	if err := s.SetTeamAccess(ctx, asset.ID, team, TeamAccess{PersonalAccounts: true},
		"admin@corp.example"); err != nil {
		t.Fatal(err)
	}
	if w = do(t, h, http.MethodPut, "/api/v1/me/ssh-key", "ana-token", `{"key":"`+testKey+`"}`); w.Code != http.StatusForbidden {
		t.Fatalf("key with an own account but no direct SSH: %d %s", w.Code, w.Body)
	}
	if err := s.SetTeamAccess(ctx, asset.ID, team, TeamAccess{PersonalAccounts: true, DirectSSH: DirectSSHApprove},
		"admin@corp.example"); err != nil {
		t.Fatal(err)
	}
	if acct, err := s.ServerAccountOf(ctx, ana); err != nil || !acct.DirectSSH {
		t.Fatalf("direct SSH not reported: %+v %v", acct, err)
	}

	w = do(t, h, http.MethodPut, "/api/v1/me/ssh-key", "ana-token", `{"key":"`+testKey+`"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "SHA256:") {
		t.Fatalf("set key: %d %s", w.Code, w.Body)
	}
	if e := lastAuditOn(t, s, ana); e.Action != "account.ssh_key_changed" || e.ActorEmail != ana {
		t.Errorf("audited as %+v", e)
	}

	// Taking a key off is always allowed, direct SSH or not.
	if err := s.SetTeamAccess(ctx, asset.ID, team, TeamAccess{PersonalAccounts: true},
		"admin@corp.example"); err != nil {
		t.Fatal(err)
	}
	if w = do(t, h, http.MethodPut, "/api/v1/me/ssh-key", "ana-token", `{"key":""}`); w.Code != http.StatusOK {
		t.Fatalf("clear key: %d %s", w.Code, w.Body)
	}
}

// A person's own setting on a server replaces their teams' there, either way,
// and without one the most open of their teams applies.
func TestPersonSettingReplacesTheirTeams(t *testing.T) {
	s, h, _ := reportAPI(t)
	ctx := context.Background()
	asset, cred := enrolledAsset(t, s, h)
	budi, _ := newAccount(t, s, "operator")  // Ops, given more
	citra, _ := newAccount(t, s, "operator") // Deploy, given less
	dewi, _ := newAccount(t, s, "operator")  // Deploy, given no account here
	eko, _ := newAccount(t, s, "operator")   // in no team at all
	fajar, _ := newAccount(t, s, "operator") // Deploy, no setting of their own
	ops := testTeam(t, s, budi)
	deploy := testTeam(t, s, citra, dewi, fajar)
	const admin = "admin@corp.example"
	yes, no := true, false

	for team, in := range map[string]TeamAccess{
		ops:    {PersonalAccounts: true, DirectSSH: DirectSSHApprove},
		deploy: {PersonalAccounts: true, DirectSSH: DirectSSHAllow, Sudo: true},
	} {
		if err := s.SetTeamAccess(ctx, asset.ID, team, in, admin); err != nil {
			t.Fatal(err)
		}
	}
	for email, in := range map[string]PersonAccess{
		budi:  {PersonalAccounts: &yes, DirectSSH: DirectSSHAllow, Sudo: true},
		citra: {PersonalAccounts: &yes},
		dewi:  {PersonalAccounts: &no},
		eko:   {PersonalAccounts: &yes, DirectSSH: DirectSSHApprove},
	} {
		if err := s.SetPersonAccess(ctx, asset.ID, email, in, admin); err != nil {
			t.Fatalf("%s: %v", email, err)
		}
	}

	byOwner := map[string]PersonalAccount{}
	for _, p := range personalReply(t, h, cred, asset.Hostname) {
		byOwner[p.Owner] = p
	}
	want := map[string]struct {
		direct string
		sudo   bool
	}{
		budi:  {DirectSSHAllow, true},    // more than Ops
		citra: {"", false},               // less than Deploy
		eko:   {DirectSSHApprove, false}, // no team needed
		fajar: {DirectSSHAllow, true},    // Deploy, as before
	}
	if _, ok := byOwner[dewi]; ok || len(byOwner) != len(want) {
		t.Fatalf("accounts sent: %+v", byOwner)
	}
	for email, w := range want {
		got, ok := byOwner[email]
		if !ok || got.Direct != w.direct || got.Sudo != w.sudo {
			t.Errorf("%s: got %+v, want direct %q sudo %v", email, got, w.direct, w.sudo)
		}
	}

	// The person's page says the same, and where it comes from.
	for email, w := range want {
		list, err := s.PersonServers(ctx, email)
		if err != nil || len(list) != 1 {
			t.Fatalf("%s servers: %+v %v", email, list, err)
		}
		ps := list[0]
		direct := ps.DirectSSH
		if direct == DirectSSHOff {
			direct = ""
		}
		source := "own"
		if email == fajar {
			source = "teams"
		}
		if !ps.PersonalAccount || direct != w.direct || ps.Sudo != w.sudo || ps.Source != source {
			t.Errorf("%s page: %+v", email, ps)
		}
	}
	if list, _ := s.PersonServers(ctx, dewi); len(list) != 1 || list[0].PersonalAccount || !list[0].OwnSetting {
		t.Errorf("dewi page: %+v", list)
	}

	// Citra may not SSH directly anywhere now, so she is not asked for a key.
	if _, err := s.SetSSHKey(ctx, citra, testKey); !errors.Is(err, ErrNoDirectSSH) {
		t.Errorf("citra's key: %v", err)
	}

	// Taking Citra's own setting away puts her back on Deploy's.
	if err := s.SetPersonAccess(ctx, asset.ID, citra, PersonAccess{}, admin); err != nil {
		t.Fatal(err)
	}
	for _, p := range personalReply(t, h, cred, asset.Hostname) {
		if p.Owner == citra && (p.Direct != DirectSSHAllow || !p.Sudo) {
			t.Errorf("citra after her setting was removed: %+v", p)
		}
	}

	// Sudo and direct SSH still need an account of their own.
	for _, in := range []PersonAccess{
		{Principals: []string{"ops"}, Sudo: true},
		{PersonalAccounts: &no, DirectSSH: DirectSSHAllow},
	} {
		if err := s.SetPersonAccess(ctx, asset.ID, citra, in, admin); err == nil {
			t.Errorf("accepted %+v", in)
		}
	}
}

// The console sets a person's own setting through the same route as their
// shared accounts, it is audited as replacing their teams', and only those
// who see the whole fleet may read someone's servers.
func TestPersonSettingThroughTheAPI(t *testing.T) {
	a, s, h, _ := enrollAPI(t)
	asset := testAsset(t, s, "ops")
	budi, _ := newAccount(t, s, "operator")
	a.UserTokens["budi-token"] = budi

	w := do(t, h, http.MethodPut, "/api/v1/assets/"+asset.ID+"/assignments", "admin-token",
		`{"email":"`+budi+`","principals":[],"personalAccounts":true,"directSsh":"allow","sudo":true}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"directSsh":"allow"`) {
		t.Fatalf("set: %d %s", w.Code, w.Body)
	}
	if e := lastAuditOn(t, s, asset.Hostname); !strings.Contains(e.Detail, "replaces what their teams give them") {
		t.Errorf("audited as %q", e.Detail)
	}

	w = do(t, h, http.MethodGet, "/api/v1/users/"+budi+"/servers", "admin-token", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"source":"own"`) {
		t.Fatalf("servers: %d %s", w.Code, w.Body)
	}
	if w = do(t, h, http.MethodGet, "/api/v1/users/"+budi+"/servers", "budi-token", ""); w.Code != http.StatusForbidden {
		t.Fatalf("an operator read someone's servers: %d", w.Code)
	}
}
