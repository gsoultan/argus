//go:build !windows

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"

	"github.com/gsoultan/argus/internal/tunnel"
)

// Users for apps on a real server: Admin signs one for a key made elsewhere
// (the browser), the .creds file made from its JWT and that key's seed signs
// in, NATS holds the app to the rules it was signed with, the tab lists it
// with its connections beside a user Argus did not sign, and taking it off
// the list says how to revoke it. Below Admin nothing is signed.
func TestNATSAppUsers(t *testing.T) {
	l := newNATSLab(t)
	admin, arec, afile := l.open(t, tunnel.LevelDBA)
	state := t.TempDir()
	admin.f.cfg.StateDir = state
	if a := natsDo(t, admin, arec, "stream_create", &tunnel.NATSRequest{StreamSpec: orders()}); a.Type != "result" {
		t.Fatalf("stream: %s", a.Error)
	}

	ukp, _ := nkeys.CreateUser()
	upub, _ := ukp.PublicKey()
	useed, _ := ukp.Seed()
	spec := &tunnel.NATSAppUser{PubAllow: []string{"orders.>"}, SubAllow: []string{"orders.eu.>"}, JetStream: "use", Days: 30}
	set := func(s *natsSession, rec *stmtRecorder, name, key string, sp *tunnel.NATSAppUser) tunnel.StoreAnswer {
		return natsDo(t, s, rec, tunnel.BrokerOpUserSet, nil, func(r *tunnel.StoreRequest) {
			r.BrokerUser = &tunnel.BrokerUser{Name: name, Create: true, PublicKey: key, NATS: sp}
		})
	}

	// Below Admin, nothing is signed.
	owner, orec, _ := l.open(t, tunnel.LevelOwner)
	owner.f.cfg.StateDir = state
	if a := set(owner, orec, "orders-api", upub, spec); a.Type != "error" {
		t.Fatal("Manage signed a user")
	}
	if a := natsDo(t, owner, orec, tunnel.BrokerOpUsers, nil); a.Type != "result" {
		t.Fatalf("Manage could not list users: %s", a.Error)
	}

	// A key that is not a user's, or a seed sent by mistake, is refused.
	if a := set(admin, arec, "orders-api", string(useed[:56]), spec); a.Type != "error" {
		t.Fatal("a seed was taken as a public key")
	}
	if a := set(admin, arec, "argus-ana-owner", upub, spec); a.Type != "error" {
		t.Fatal("an Argus session name was signed")
	}

	a := set(admin, arec, "orders-api", upub, spec)
	if a.Type != "result" {
		t.Fatalf("sign: %s", a.Error)
	}
	issued := a.Data.(NATSIssued)
	uc, err := jwt.DecodeUserClaims(issued.JWT)
	if err != nil {
		t.Fatal(err)
	}
	if uc.Subject != upub || uc.Name != "orders-api" || uc.IssuerAccount != l.accPub {
		t.Fatalf("claims: %+v", uc.ClaimsData)
	}
	if left := time.Until(time.Unix(uc.Expires, 0)); left < 29*24*time.Hour || left > 30*24*time.Hour+time.Minute {
		t.Fatalf("expires in %s, want 30 days", left)
	}
	if !uc.Pub.Deny.Contains("$SYS.>") || !uc.Sub.Allow.Contains("_INBOX.>") || !uc.Pub.Allow.Contains("$JS.API.CONSUMER.>") {
		t.Fatalf("permissions: %+v", uc.Permissions)
	}
	if uc.Pub.Allow.Contains("$JS.API.STREAM.CREATE.*") || uc.Pub.Allow.Contains("$JS.API.>") {
		t.Fatal("a JetStream user may make streams")
	}
	if a := set(admin, arec, "orders-api", upub, spec); a.Type != "error" {
		t.Fatal("one key was signed twice")
	}

	// The .creds file, as the browser makes it, signs in.
	creds, err := jwt.FormatUserConfig(issued.JWT, useed)
	if err != nil {
		t.Fatal(err)
	}
	cf := filepath.Join(t.TempDir(), "orders-api.creds")
	if err := os.WriteFile(cf, creds, 0o600); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 8)
	app, err := nats.Connect(admin.url, nats.UserCredentials(cf), nats.Name("orders-api"),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) { errs <- err }))
	if err != nil {
		t.Fatalf("the .creds file did not sign in: %v", err)
	}
	defer app.Close()

	// NATS holds it to its rules: publish to orders, use the stream, not
	// make one, not read payments.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	js, _ := jetstream.New(app)
	if _, err := js.Publish(ctx, "orders.eu.created", []byte(`{"n":1}`)); err != nil {
		t.Fatalf("the app could not publish to its stream: %v", err)
	}
	if _, err := js.Stream(ctx, "ORDERS_EVENTS"); err != nil {
		t.Fatalf("the app could not read its stream: %v", err)
	}
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "MINE", Subjects: []string{"orders.mine"}}); err == nil {
		t.Fatal("the app made a stream")
	}
	_, _ = app.SubscribeSync("payments.>")
	_ = app.Flush()
	select {
	case err := <-errs:
		if !strings.Contains(err.Error(), "Permissions Violation") {
			t.Fatalf("subscribe payments: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the app subscribed to payments")
	}

	// A user Argus did not sign, connected now, is listed by its name.
	okp, _ := nkeys.CreateUser()
	opub, _ := okp.PublicKey()
	oc := jwt.NewUserClaims(opub)
	oc.Name = "legacy-worker"
	oc.IssuerAccount = l.accPub
	otok, _ := oc.Encode(l.signKP)
	other, err := nats.Connect(admin.url, nats.UserJWT(func() (string, error) { return otok, nil }, func(n []byte) ([]byte, error) { return okp.Sign(n) }))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	var data NATSUsersData
	for range 20 {
		a := natsDo(t, admin, arec, tunnel.BrokerOpUsers, nil)
		if a.Type != "result" {
			t.Fatalf("users: %s", a.Error)
		}
		data = a.Data.(NATSUsersData)
		if len(data.Users) == 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if data.ConnsError != "" || len(data.Users) != 2 {
		t.Fatalf("users: %+v", data)
	}
	byName := map[string]NATSAppUserInfo{}
	for _, u := range data.Users {
		byName[u.Name] = u
	}
	if u := byName["orders-api"]; !u.InArgus || u.Connections != 1 || u.By != "ana@example.com" || u.PublicKey != upub {
		t.Fatalf("orders-api: %+v", u)
	}
	if u := byName["legacy-worker"]; u.InArgus || u.Connections != 1 || u.PublicKey != opub {
		t.Fatalf("legacy-worker: %+v", u)
	}

	// The Connections tab names each app by its user, not its account.
	ca := natsDo(t, admin, arec, "connections", nil)
	found := false
	for _, c := range ca.Data.([]NATSConn) {
		found = found || (c.Name == "orders-api" && c.User == "orders-api")
	}
	if !found {
		t.Fatalf("connections: %+v", ca.Data)
	}

	// The list is on this server, root-only, with no secret in it.
	ledger := filepath.Join(state, "nats-users", l.accPub+".json")
	st, err := os.Stat(ledger)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("ledger: %v %v", st, err)
	}
	if b, _ := os.ReadFile(ledger); strings.Contains(string(b), string(useed)) || strings.Contains(string(b), issued.JWT) {
		t.Fatal("the ledger holds a secret")
	}

	// Taking it off the list says how to revoke it.
	del := func(name, key string) tunnel.StoreAnswer {
		return natsDo(t, admin, arec, tunnel.BrokerOpUserDel, nil, func(r *tunnel.StoreRequest) {
			r.BrokerUser = &tunnel.BrokerUser{Name: name, PublicKey: key}
		})
	}
	if a := del("legacy-worker", opub); a.Type != "error" {
		t.Fatal("a user Argus did not sign was taken off its list")
	}
	a = del("orders-api", upub)
	if a.Type != "result" || !strings.Contains(a.Data.(map[string]string)["revoke"], "nsc revocations add-user --account ORDERS --user-public-key "+upub) {
		t.Fatalf("forget: %+v", a)
	}
	if l, _ := readNATSLedger(ledger); len(l) != 0 {
		t.Fatalf("still listed: %+v", l)
	}

	// The recording has what was signed, not the seed or the JWT.
	rb, _ := os.ReadFile(afile)
	if !strings.Contains(string(rb), `CREATE USER \"orders-api\" in account ORDERS`) || !strings.Contains(string(rb), "FORGET USER") {
		t.Fatalf("recording: %s", rb)
	}
	if strings.Contains(string(rb), string(useed)) || strings.Contains(string(rb), issued.JWT) {
		t.Fatal("the recording holds the seed or the JWT")
	}
}

// A user that may connect only from elsewhere is turned away from here.
func TestNATSAppUserSources(t *testing.T) {
	l := newNATSLab(t)
	admin, arec, _ := l.open(t, tunnel.LevelDBA)
	admin.f.cfg.StateDir = t.TempDir()
	ukp, _ := nkeys.CreateUser()
	upub, _ := ukp.PublicKey()
	a := natsDo(t, admin, arec, tunnel.BrokerOpUserSet, nil, func(r *tunnel.StoreRequest) {
		r.BrokerUser = &tunnel.BrokerUser{Name: "far", Create: true, PublicKey: upub,
			NATS: &tunnel.NATSAppUser{PubAllow: []string{"orders.>"}, Days: 1, Sources: []string{"10.9.0.0/16"}}}
	})
	if a.Type != "result" {
		t.Fatalf("sign: %s", a.Error)
	}
	tok := a.Data.(NATSIssued).JWT
	if _, err := nats.Connect(admin.url, nats.UserJWT(func() (string, error) { return tok, nil }, func(n []byte) ([]byte, error) { return ukp.Sign(n) })); err == nil {
		t.Fatal("a user limited to 10.9.0.0/16 connected from 127.0.0.1")
	}
}
