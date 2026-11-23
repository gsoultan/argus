//go:build !windows

package agent

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

// scramCredential is what the browser sends for a password: a salt and
// the salted password, never the password.
func scramCredential(t *testing.T, password string) *tunnel.ScramCredential {
	t.Helper()
	salt := make([]byte, 24)
	_, _ = rand.Read(salt)
	key, err := pbkdf2.Key(sha512.New, password, salt, 8192, 64)
	if err != nil {
		t.Fatal(err)
	}
	return &tunnel.ScramCredential{Salt: base64.StdEncoding.EncodeToString(salt),
		SaltedPassword: base64.StdEncoding.EncodeToString(key), Iterations: 8192}
}

func TestKafkaUsersAgainstRealKafka(t *testing.T) {
	k := startKafka(t)
	ctx := context.Background()
	inv, other := k.scope+"invoices", "zz-"+k.scope+"other"
	k.topic(t, inv, 1)
	k.topic(t, other, 1)
	name := "app-" + strconv.FormatInt(time.Now().UnixNano()%1e9, 36)
	t.Cleanup(func() {
		_, _ = k.adm.DeleteTopics(ctx, other)
		_ = dropKafkaUser(ctx, k.adm, name)
	})
	svc := Kafka(StoreConfig{Verifier: &tunnel.GrantVerifier{Host: "cache-01"}, RecordDir: t.TempDir()})
	dba := openKafka(t, k, svc, tunnel.LevelDBA, k.scope)
	defer dba.close()
	owner := openKafka(t, k, svc, tunnel.LevelOwner, k.scope)
	defer owner.close()

	const pw = "Hq7mXe-p4RtaKv-9wNcZd-yB3sLu"
	cred := scramCredential(t, pw)
	acls := []tunnel.KafkaACL{
		{Resource: "topic", Pattern: "PREFIXED", Name: k.scope + "inv", Operation: "READ", Permission: "ALLOW", Host: "*"},
		{Resource: "topic", Pattern: "PREFIXED", Name: k.scope + "inv", Operation: "DESCRIBE", Permission: "ALLOW", Host: "*"},
		{Resource: "group", Pattern: "PREFIXED", Name: name, Operation: "READ", Permission: "ALLOW", Host: "*"},
	}
	create := tunnel.StoreRequest{Op: tunnel.BrokerOpUserSet, BrokerUser: &tunnel.BrokerUser{Name: name, Create: true,
		Mechanism: "SCRAM-SHA-512", Credential: cred, ACLs: acls}}

	if a := owner.ask(create); a.Type != "error" || !strings.Contains(a.Error, "dba") {
		t.Fatalf("an owner made a user: %+v", a)
	}
	if a := dba.ask(create); a.Type != "result" {
		t.Fatalf("create: %+v", a)
	}
	for _, r := range dba.rec {
		if strings.Contains(r.SQL, cred.SaltedPassword) || strings.Contains(r.SQL, cred.Salt) {
			t.Fatalf("the credential is in the recording: %s", r.SQL)
		}
	}
	if !dba.recordedHas("stmt", "CREATE USER \""+name+"\"") || !dba.recordedHas("stmt", "salted [redacted]") {
		t.Fatalf("the creation was not recorded as it ran: %+v", dba.rec)
	}
	if a := dba.ask(create); a.Type != "error" || !strings.Contains(a.Error, "already a user") {
		t.Fatalf("made twice: %+v", a)
	}

	login := dba.ask(tunnel.StoreRequest{Op: tunnel.BrokerOpUserLogin, Password: pw,
		BrokerUser: &tunnel.BrokerUser{Name: name, Mechanism: "SCRAM-SHA-512"}})
	lr := dataOf[KafkaLoginResult](t, login)
	if !lr.OK || !slices.Contains(lr.Topics, inv) || lr.Elsewhere != 0 {
		t.Fatalf("the new user's sign-in: %+v %+v", login, lr)
	}
	for _, r := range dba.rec {
		if strings.Contains(r.SQL, pw) {
			t.Fatal("the password is in the recording")
		}
	}
	if a := dba.ask(tunnel.StoreRequest{Op: tunnel.BrokerOpUserLogin, Password: "wrong-password-1",
		BrokerUser: &tunnel.BrokerUser{Name: name}}); dataOf[KafkaLoginResult](t, a).OK {
		t.Fatal("a wrong password signed in")
	}

	list := dataOf[KafkaUsersData](t, owner.ask(tunnel.StoreRequest{Op: tunnel.BrokerOpUsers}))
	i := slices.IndexFunc(list.Users, func(u KafkaUserInfo) bool { return u.Name == name })
	if i < 0 || len(list.Users[i].ACLs) != 3 || len(list.Users[i].Outside) != 0 || list.Users[i].Mechanisms[0] != "SCRAM-SHA-512" {
		t.Fatalf("the list: %+v", list)
	}
	if len(list.Sessions) < 2 {
		t.Fatalf("Argus's session users are not listed apart: %+v", list.Sessions)
	}

	// Outside the scope is refused before Kafka is asked.
	out := tunnel.StoreRequest{Op: tunnel.BrokerOpUserSet, BrokerUser: &tunnel.BrokerUser{Name: name,
		ACLs: append(slices.Clone(acls), tunnel.KafkaACL{Resource: "topic", Pattern: "LITERAL", Name: other, Operation: "READ", Permission: "ALLOW", Host: "*"})}}
	if a := dba.ask(out); a.Type != "error" || !strings.Contains(a.Error, "outside this entry") {
		t.Fatalf("an ACL outside the scope: %+v", a)
	}
	for _, bad := range []string{"argus-ana-dba", "admin"} {
		r := create
		r.BrokerUser = &tunnel.BrokerUser{Name: bad, Create: bad != "admin", Mechanism: "SCRAM-SHA-512", Credential: cred, ACLs: acls}
		if a := dba.ask(r); a.Type != "error" {
			t.Fatalf("%s was changed: %+v", bad, a)
		}
	}

	// Narrowed: WRITE added, the group ACL gone; new ones first.
	narrow := tunnel.StoreRequest{Op: tunnel.BrokerOpUserSet, BrokerUser: &tunnel.BrokerUser{Name: name, ACLs: []tunnel.KafkaACL{acls[0], acls[1],
		{Resource: "topic", Pattern: "PREFIXED", Name: k.scope + "inv", Operation: "WRITE", Permission: "ALLOW", Host: "*"}}}}
	res := dataOf[KafkaUserResult](t, dba.ask(narrow))
	if res.Added != 1 || res.Removed != 1 {
		t.Fatalf("narrowing: %+v", res)
	}

	// An ACL outside the scope, made on the server: the entry can take away
	// its access here, not delete it or change its password.
	b, _ := kafkaACLBuilder(name, tunnel.KafkaACL{Resource: "topic", Pattern: "LITERAL", Name: other, Operation: "READ", Permission: "ALLOW", Host: "*"})
	if _, err := k.adm.CreateACLs(ctx, b); err != nil {
		t.Fatal(err)
	}
	if a := dba.ask(tunnel.StoreRequest{Op: tunnel.BrokerOpUserDel, BrokerUser: &tunnel.BrokerUser{Name: name}}); a.Type != "error" || !strings.Contains(a.Error, "outside") {
		t.Fatalf("deleted a user with ACLs out of view: %+v", a)
	}
	renew := tunnel.StoreRequest{Op: tunnel.BrokerOpUserSet, BrokerUser: &tunnel.BrokerUser{Name: name, Mechanism: "SCRAM-SHA-512", Credential: scramCredential(t, "another-password-1")}}
	if a := dba.ask(renew); a.Type != "error" || !strings.Contains(a.Error, "outside") {
		t.Fatalf("a new password for a user with ACLs out of view: %+v", a)
	}
	if a := dba.ask(tunnel.StoreRequest{Op: tunnel.BrokerOpUserScopeDel, BrokerUser: &tunnel.BrokerUser{Name: name}}); a.Type != "result" {
		t.Fatalf("remove access here: %+v", a)
	}
	left, err := allKafkaACLs(ctx, k.adm)
	if err != nil {
		t.Fatal(err)
	}
	if len(left[name]) != 1 || left[name][0].Name != other {
		t.Fatalf("remove access here took %+v", left[name])
	}

	// From a * entry it is all in view: limits, a new password, delete.
	all := openKafka(t, k, svc, tunnel.LevelDBA, tunnel.KafkaAllTopics)
	defer all.close()
	if a := all.ask(tunnel.StoreRequest{Op: tunnel.BrokerOpUserSet, BrokerUser: &tunnel.BrokerUser{Name: name,
		Quotas: &tunnel.KafkaQuotas{ConsumeBytes: 1 << 20}}}); a.Type != "result" {
		t.Fatalf("limits: %+v", a)
	}
	if q := kafkaQuotasOf(ctx, k.adm); q[name].ConsumeBytes != 1<<20 {
		t.Fatalf("quota: %+v", q[name])
	}
	if a := all.ask(tunnel.StoreRequest{Op: tunnel.BrokerOpUserDel, BrokerUser: &tunnel.BrokerUser{Name: name}}); a.Type != "result" {
		t.Fatalf("delete: %+v", a)
	}
	for _, u := range k.users(t) {
		if u == name {
			t.Fatal("the deleted user still has a credential")
		}
	}

	// Where changes need approval, a change waits for one, with the text.
	approve, err := openStoreSession(t, svc, tunnel.KafkaService, tunnel.GrantDB{ID: "k1", Engine: tunnel.EngineKafka,
		Name: k.scope, Host: "127.0.0.1", Port: k.port, Level: tunnel.LevelDBA, ConfigPath: k.admin, ChangesNeedApproval: true}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer approve.close()
	a := approve.ask(create)
	if a.NeedsApproval == nil || !strings.Contains(a.NeedsApproval.Text, "CREATE USER") || strings.Contains(a.NeedsApproval.Text, cred.SaltedPassword) {
		t.Fatalf("approval: %+v", a)
	}
}
