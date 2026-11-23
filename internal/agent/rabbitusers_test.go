package agent

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

// rabbitHash is rabbit_password_hashing_sha256, as the browser makes it.
func rabbitHash(pw string) string {
	salt := make([]byte, 4)
	_, _ = rand.Read(salt)
	sum := sha256.Sum256(append(append([]byte{}, salt...), pw...))
	return base64.StdEncoding.EncodeToString(append(salt, sum[:]...))
}

func rabbitUsersOf(t *testing.T, a tunnel.StoreAnswer) RabbitUsersData {
	t.Helper()
	if a.Type != "result" {
		t.Fatalf("users: %+v", a)
	}
	b, _ := json.Marshal(a.Data)
	var d RabbitUsersData
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func findRabbitUser(d RabbitUsersData, name string) *RabbitUserInfo {
	for i := range d.Users {
		if d.Users[i].Name == name {
			return &d.Users[i]
		}
	}
	return nil
}

// Against a real RabbitMQ (ARGUS_TEST_RABBITMQ_PORT, as
// TestRabbitAgainstRealBroker): an app user made at DBA from its hash signs
// in, is changed and removed on the entry's vhost only, and the hash never
// reaches the recording.
func TestRabbitUsersAgainstRealBroker(t *testing.T) {
	port, _ := strconv.Atoi(os.Getenv("ARGUS_TEST_RABBITMQ_PORT"))
	if port == 0 {
		t.Skip("ARGUS_TEST_RABBITMQ_PORT is not set")
	}
	dir := t.TempDir()
	svc := RabbitMQ(RabbitConfig{StoreConfig: StoreConfig{Verifier: &tunnel.GrantVerifier{Host: "cache-01"}, RecordDir: dir}})
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	vhost, other := "orders-"+suffix, "billing-"+suffix
	for _, v := range []string{vhost, other} {
		if _, err := runRabbitmqctl(t.Context(), []string{"add_vhost", v}, ""); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = runRabbitmqctl(t.Context(), []string{"delete_vhost", v}, "") })
	}
	open := func(level, vh string, approval bool) *storeHarness {
		h, err := openStoreSession(t, svc, tunnel.RabbitMQService, tunnel.GrantDB{ID: "q1", Engine: tunnel.EngineRabbitMQ,
			Name: vh, Host: "127.0.0.1", Port: port, Level: level, ChangesNeedApproval: approval}, dir)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	owner := open(tunnel.LevelOwner, vhost, false)
	if a := owner.ask(tunnel.StoreRequest{Op: tunnel.BrokerOpUsers}); a.Type != "error" || !strings.Contains(a.Error, "dba") {
		t.Fatalf("an owner listed RabbitMQ users: %+v", a)
	}
	owner.close()

	dba := open(tunnel.LevelDBA, vhost, false)
	defer dba.close()
	name, pw := "invoices-"+suffix, "a-long-password-"+suffix
	t.Cleanup(func() { _, _ = runRabbitmqctl(t.Context(), []string{"delete_user", name}, "") })
	hash := rabbitHash(pw)
	create := tunnel.StoreRequest{Op: tunnel.BrokerOpUserSet, BrokerUser: &tunnel.BrokerUser{Name: name, Create: true, PasswordHash: hash,
		Tags: &[]string{}, Perms: &tunnel.RabbitPerms{Configure: "^invoices\\.", Write: "^invoices\\.", Read: "^invoices\\."},
		TopicPerms: []tunnel.RabbitTopicPerm{{Exchange: "amq.topic", Write: "^invoices\\.", Read: ".*"}},
		Limits:     &tunnel.RabbitLimits{MaxConnections: 5}}}
	if a := dba.ask(create); a.Type != "result" {
		t.Fatalf("create: %+v", a)
	}
	if dba.recordedHas("stmt", hash) || !dba.recordedHas("stmt", "password_hash [redacted]") {
		t.Fatal("the recording holds the hash, or not the redacted line")
	}
	if a := dba.ask(create); a.Type != "error" || !strings.Contains(a.Error, "already") {
		t.Fatalf("a second create: %+v", a)
	}

	login := func(p string) RabbitLoginResult {
		a := dba.ask(tunnel.StoreRequest{Op: tunnel.BrokerOpUserLogin, Password: p, BrokerUser: &tunnel.BrokerUser{Name: name}})
		b, _ := json.Marshal(a.Data)
		var r RabbitLoginResult
		_ = json.Unmarshal(b, &r)
		return r
	}
	if r := login(pw); !r.OK {
		t.Fatalf("the new user did not sign in: %+v", r)
	}
	if r := login("wrong-password-123"); r.OK || r.Detail == "" {
		t.Fatalf("a wrong password signed in: %+v", r)
	}
	if dba.recordedHas("stmt", pw) {
		t.Fatal("the password is in the recording")
	}

	d := rabbitUsersOf(t, dba.ask(tunnel.StoreRequest{Op: tunnel.BrokerOpUsers}))
	u := findRabbitUser(d, name)
	if u == nil || u.Perms.Write != "^invoices\\." || len(u.TopicPerms) != 1 || u.Limits.MaxConnections != 5 || len(u.Tags) != 0 {
		t.Fatalf("listed: %+v", u)
	}
	if len(d.Sessions) == 0 || findRabbitUser(d, d.Sessions[0]) != nil {
		t.Fatalf("the session user is not apart: %+v", d.Sessions)
	}

	// Tags alone keep the password.
	if a := dba.ask(tunnel.StoreRequest{Op: tunnel.BrokerOpUserSet, BrokerUser: &tunnel.BrokerUser{Name: name, Tags: &[]string{"monitoring"}}}); a.Type != "result" {
		t.Fatalf("tags: %+v", a)
	}
	if r := login(pw); !r.OK {
		t.Fatalf("a tag change took the password: %+v", r)
	}
	for _, bad := range []tunnel.BrokerUser{
		{Name: name, Tags: &[]string{"administrator"}},
		{Name: "argus-ana-dba", Perms: &tunnel.RabbitPerms{Configure: ".*", Write: ".*", Read: ".*"}},
		{Name: "guest", PasswordHash: rabbitHash("x")},
	} {
		if a := dba.ask(tunnel.StoreRequest{Op: tunnel.BrokerOpUserSet, BrokerUser: &bad}); a.Type != "error" {
			t.Fatalf("%+v passed: %+v", bad, a)
		}
	}

	// Access on another vhost: then its password and deleting it wait.
	dba2 := open(tunnel.LevelDBA, other, false)
	if a := dba2.ask(tunnel.StoreRequest{Op: tunnel.BrokerOpUserSet, BrokerUser: &tunnel.BrokerUser{Name: name,
		Perms: &tunnel.RabbitPerms{Configure: "^$", Write: "^$", Read: ".*"}}}); a.Type != "result" {
		t.Fatalf("access on another vhost: %+v", a)
	}
	dba2.close()
	if a := dba.ask(tunnel.StoreRequest{Op: tunnel.BrokerOpUserSet, BrokerUser: &tunnel.BrokerUser{Name: name, PasswordHash: rabbitHash("b-long-password")}}); a.Type != "error" || !strings.Contains(a.Error, other) {
		t.Fatalf("a password changed while it has access elsewhere: %+v", a)
	}
	if a := dba.ask(tunnel.StoreRequest{Op: tunnel.BrokerOpUserDel, BrokerUser: &tunnel.BrokerUser{Name: name}}); a.Type != "error" {
		t.Fatalf("deleted while it has access elsewhere: %+v", a)
	}
	if u := findRabbitUser(rabbitUsersOf(t, dba.ask(tunnel.StoreRequest{Op: tunnel.BrokerOpUsers})), name); u == nil || len(u.Elsewhere) != 1 {
		t.Fatalf("the other vhost is not counted: %+v", u)
	}

	// Remove its access here: the other vhost keeps it.
	if a := dba.ask(tunnel.StoreRequest{Op: tunnel.BrokerOpUserScopeDel, BrokerUser: &tunnel.BrokerUser{Name: name}}); a.Type != "result" {
		t.Fatalf("remove access: %+v", a)
	}
	if u := findRabbitUser(rabbitUsersOf(t, dba.ask(tunnel.StoreRequest{Op: tunnel.BrokerOpUsers})), name); u != nil {
		t.Fatalf("still listed here: %+v", u)
	}
	dba3 := open(tunnel.LevelDBA, other, false)
	if a := dba3.ask(tunnel.StoreRequest{Op: tunnel.BrokerOpUserDel, BrokerUser: &tunnel.BrokerUser{Name: name}}); a.Type != "result" {
		t.Fatalf("delete where all it has is in view: %+v", a)
	}
	dba3.close()

	// On production a change waits for its approval.
	prod := open(tunnel.LevelDBA, vhost, true)
	defer prod.close()
	a := prod.ask(create)
	if a.NeedsApproval == nil || a.NeedsApproval.Text != tunnel.RabbitUserText(vhost, create) {
		t.Fatalf("production did not wait: %+v", a)
	}
}
