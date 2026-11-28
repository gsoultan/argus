package vault

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/secretwire"
	"go.etcd.io/bbolt"
)

func testStore(t *testing.T) (*Store, string, []byte) {
	t.Helper()
	dir := t.TempDir()
	root, err := randomKey()
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, root, "impack")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir, root
}

var alice = Actor{Kind: ActorPerson, ID: "alice@example.com"}

func put(t *testing.T, s *Store, name, typ, value string) Secret {
	t.Helper()
	sec, err := s.Put(PutInput{Name: name, Type: typ, Value: []byte(value), Actor: alice, Route: RouteConsole})
	if err != nil {
		t.Fatal(err)
	}
	return sec
}

func TestStoreRefusesTheWrongRootKeyAndOrganization(t *testing.T) {
	s, dir, root := testStore(t)
	put(t, s, "prod/db", TypeText, "v1")
	s.Close()

	other, _ := randomKey()
	if _, err := Open(dir, other, "impack"); !errors.Is(err, ErrWrongRootKey) {
		t.Fatalf("wrong root key: %v", err)
	}
	if _, err := Open(dir, root, "someone-else"); err == nil {
		t.Fatal("opened as another organization")
	}
	again, err := Open(dir, root, "impack")
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	v, _, err := again.Read(ReadInput{Name: "prod/db", Actor: alice})
	if err != nil || string(v) != "v1" {
		t.Fatalf("after reopening: %q %v", v, err)
	}
}

func TestValuesAreNotStoredInTheClear(t *testing.T) {
	s, dir, _ := testStore(t)
	put(t, s, "prod/db", TypeText, "correct-horse-battery-staple")
	s.Close()
	raw, err := os.ReadFile(filepath.Join(dir, "secrets.db"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("correct-horse-battery-staple")) {
		t.Fatal("the value is in the database file in the clear")
	}
}

func TestVersionsAndLabels(t *testing.T) {
	s, _, _ := testStore(t)
	put(t, s, "prod/db", TypeKV, `{"username":"app","password":"one"}`)
	sec := put(t, s, "prod/db", TypeKV, `{"username":"app","password":"two"}`)
	if sec.Labels[LabelCurrent] != 2 || sec.Labels[LabelPrevious] != 1 {
		t.Fatalf("labels after second version: %v", sec.Labels)
	}
	if got := sec.Versions[1].Fields; len(got) != 2 || got[0] != "password" {
		t.Fatalf("field names: %v", got)
	}
	v, ver, err := s.Read(ReadInput{Name: "prod/db", Field: "password", Actor: alice})
	if err != nil || string(v) != "two" || ver != 2 {
		t.Fatalf("current: %q v%d %v", v, ver, err)
	}
	v, _, err = s.Read(ReadInput{Name: "prod/db", Label: LabelPrevious, Field: "password", Actor: alice})
	if err != nil || string(v) != "one" {
		t.Fatalf("previous: %q %v", v, err)
	}
	// Rolling back: current points at 1, previous at 2.
	sec, err = s.SetLabel("prod/db", LabelCurrent, 1, alice, "bad rotation")
	if err != nil || sec.Labels[LabelCurrent] != 1 || sec.Labels[LabelPrevious] != 2 {
		t.Fatalf("rollback: %v %v", sec.Labels, err)
	}
	if _, err := s.Put(PutInput{Name: "prod/db", Type: TypeText, Value: []byte("x"), Actor: alice}); err == nil {
		t.Fatal("changed a secret's type")
	}
	if _, err := s.Put(PutInput{Name: "prod/kv", Type: TypeKV, Value: []byte("not json"), Actor: alice}); err == nil {
		t.Fatal("stored a key-value secret that is not a JSON object")
	}
	if _, _, err := s.Read(ReadInput{Name: "prod/db", Field: "nope", Actor: alice}); err == nil {
		t.Fatal("read a field that does not exist")
	}
}

func TestNamesAreNarrow(t *testing.T) {
	for _, bad := range []string{"", "Prod/db", "prod//db", "prod/../db", "/prod", "prod/", "-x", "a b", "prod/db\x00"} {
		if ValidName(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, good := range []string{"prod", "prod/payments/db", "ci/github.deploy-key_2"} {
		if err := ValidName(good); err != nil {
			t.Errorf("%q refused: %v", good, err)
		}
	}
	if underPrefix("production/db", "prod") || !underPrefix("prod/db", "prod") || !underPrefix("prod/db", "prod/") {
		t.Fatal("folder matching is wrong")
	}
}

func TestDeletedSecretsCannotBeReadAndArePurged(t *testing.T) {
	s, _, _ := testStore(t)
	put(t, s, "prod/db", TypeText, "v1")
	if _, err := s.Delete("prod/db", 3, alice, ""); err == nil {
		t.Fatal("accepted a 3-day recovery window")
	}
	if _, err := s.Delete("prod/db", 7, alice, "gone"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Read(ReadInput{Name: "prod/db", Actor: alice}); !errors.Is(err, ErrDeleted) {
		t.Fatalf("read a deleted secret: %v", err)
	}
	if _, err := s.Restore("prod/db", alice); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Read(ReadInput{Name: "prod/db", Actor: alice}); err != nil {
		t.Fatalf("read after restore: %v", err)
	}
	if _, err := s.Delete("prod/db", 7, alice, ""); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }
	names, err := s.PurgeDue()
	if err != nil || len(names) != 1 {
		t.Fatalf("purge: %v %v", names, err)
	}
	if _, err := s.Get("prod/db"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("purged secret still there: %v", err)
	}
	err = s.db.View(func(tx *bbolt.Tx) error {
		if k, _ := tx.Bucket(bucketValues).Cursor().First(); k != nil {
			return errors.New("a purged value is still stored")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A read is in the log before the value is returned; the log is a chain that
// shows any edit, and stays verifiable after entries argus-control has are
// pruned.
func TestReadLogRecordsEveryReadAndShowsTampering(t *testing.T) {
	s, _, _ := testStore(t)
	put(t, s, "prod/db", TypeText, "v1")
	if _, _, err := s.Read(ReadInput{Name: "prod/db", Actor: Actor{Kind: ActorServer, ID: "srv-1", Name: "app-02"},
		Route: RouteDirect, Reason: "argus-agent run", Remote: "10.0.0.5"}); err != nil {
		t.Fatal(err)
	}
	items, err := s.LogAfter(0, 0)
	if err != nil || len(items) != 2 {
		t.Fatalf("log: %d entries, %v", len(items), err)
	}
	var e Entry
	if err := json.Unmarshal(items[1].Line, &e); err != nil {
		t.Fatal(err)
	}
	if e.Action != "read" || e.Actor.Name != "app-02" || e.Secret != "prod/db" || e.Version != 1 || e.Remote != "10.0.0.5" || e.Org != "impack" {
		t.Fatalf("read entry: %+v", e)
	}
	if bytes.Contains(items[0].Line, []byte("v1\"")) {
		t.Fatal("a value is in the log")
	}
	if items[1].Line == nil || e.Prev != items[0].Hash {
		t.Fatal("entries are not chained")
	}
	if err := s.VerifyLog(); err != nil {
		t.Fatal(err)
	}

	// Edit the first entry in place.
	err = s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketLog)
		k, v := b.Cursor().First()
		return b.Put(k, bytes.Replace(v, []byte("alice"), []byte("mallory"), 1))
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyLog(); err == nil {
		t.Fatal("an edited entry verified")
	}
}

func TestPrunedLogStillVerifies(t *testing.T) {
	s, _, _ := testStore(t)
	for range 3 {
		put(t, s, "prod/db", TypeText, "v")
	}
	if err := s.AckLog(99); err == nil {
		t.Fatal("acknowledged entries never written")
	}
	if err := s.AckLog(2); err != nil {
		t.Fatal(err)
	}
	n, err := s.PruneLog(time.Now().Add(time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	if err := s.VerifyLog(); err != nil {
		t.Fatalf("after pruning: %v", err)
	}
	items, _ := s.LogAfter(0, 0)
	if len(items) != 1 || items[0].Seq != 3 {
		t.Fatalf("kept: %+v", items)
	}
}

func bundle(serial int64) Bundle {
	return Bundle{
		Org: "impack", Serial: serial, IssuedAt: time.Now(), Folders: []string{"prod"},
		Servers: []ServerIdentity{{ID: "srv-1", Hostname: "app-02", CredentialHash: hashSecret("argus-agent-good"), Groups: []string{"payments"}}},
		Apps:    []AppIdentity{{ID: "app-1", Name: "ci", TokenHash: hashSecret("argus-app-ci"), ExpiresAt: time.Now().Add(time.Hour)}},
		Rules: []Rule{
			{Path: "prod/payments", Subjects: []Subject{{Kind: SubjectGroup, ID: "payments"}}, Can: []string{CanRead}},
			{Path: "prod/ci", Subjects: []Subject{{Kind: SubjectApp, ID: "app-1"}}, Can: []string{CanRead}},
			{Path: "prod", Subjects: []Subject{{Kind: SubjectRole, ID: "admin"}}, Can: []string{CanWrite, CanManage}},
			{Path: "prod/payments", Subjects: []Subject{{Kind: SubjectTeam, ID: "payments-leads"}}, Can: []string{CanRead}, NeedsApproval: true},
		},
	}
}

func TestRulesRefuseWhatTheyDoNotGrant(t *testing.T) {
	r := NewRules(15 * time.Minute)
	if _, err := r.Allow(Principal{Actor: Actor{Kind: ActorServer, ID: "srv-1"}}, CanRead, "prod/payments/db"); !errors.Is(err, ErrRulesStale) {
		t.Fatalf("no rules yet: %v", err)
	}
	if err := r.Replace(bundle(2), "impack"); err != nil {
		t.Fatal(err)
	}
	if err := r.Replace(bundle(2), "impack"); err == nil {
		t.Fatal("accepted a bundle that is not newer")
	}
	if err := r.Replace(Bundle{Org: "other", Serial: 9}, "impack"); err == nil {
		t.Fatal("accepted another organization's rules")
	}

	srv, ok := r.Authenticate("argus-agent-good")
	if !ok || srv.Name != "app-02" {
		t.Fatalf("known server: %+v %v", srv, ok)
	}
	if _, ok := r.Authenticate("argus-agent-unknown"); ok {
		t.Fatal("unknown credential accepted")
	}
	if _, ok := r.Authenticate("not-a-credential"); ok {
		t.Fatal("unprefixed secret accepted")
	}
	allowed := func(p Principal, action, name string) bool {
		d, err := r.Allow(p, action, name)
		if err != nil {
			t.Fatal(err)
		}
		return d.Allowed
	}
	if !allowed(srv, CanRead, "prod/payments/db") {
		t.Fatal("server in group refused")
	}
	if allowed(srv, CanWrite, "prod/payments/db") {
		t.Fatal("read grant allowed a write")
	}
	if allowed(srv, CanRead, "prod/crm/db") {
		t.Fatal("read outside the granted folder")
	}
	if !allowed(srv, CanList, "prod/payments/db") {
		t.Fatal("read grant does not allow seeing it exists")
	}
	admin := Principal{Actor: Actor{Kind: ActorPerson, ID: "a@x"}, Role: "admin"}
	if allowed(admin, CanRead, "prod/payments/db") {
		t.Fatal("an admin read a value without a read rule")
	}
	if !allowed(admin, CanWrite, "prod/payments/db") {
		t.Fatal("admin write refused")
	}
	lead := Principal{Actor: Actor{Kind: ActorPerson, ID: "l@x"}, Groups: []string{"payments-leads"}}
	d, _ := r.Allow(lead, CanRead, "prod/payments/db")
	if d.Allowed || !d.NeedsApproval {
		t.Fatalf("read without the approval it needs: %+v", d)
	}
	lead.Approved = true
	if !allowed(lead, CanRead, "prod/payments/db") {
		t.Fatal("approved read refused")
	}

	// A bundle without the server revokes it.
	b := bundle(3)
	b.Servers = nil
	if err := r.Replace(b, "impack"); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Authenticate("argus-agent-good"); ok {
		t.Fatal("revoked server still accepted")
	}
	// Folders this server does not keep are not here.
	b = bundle(4)
	b.Folders = []string{"staging"}
	if err := r.Replace(b, "impack"); err != nil {
		t.Fatal(err)
	}
	if allowed(srv, CanRead, "prod/payments/db") {
		t.Fatal("read in a folder this server does not keep")
	}
}

func TestRulesGoStaleWithoutControl(t *testing.T) {
	r := NewRules(15 * time.Minute)
	now := time.Now()
	r.now = func() time.Time { return now }
	if err := r.Replace(bundle(1), "impack"); err != nil {
		t.Fatal(err)
	}
	p, _ := r.Authenticate("argus-agent-good")
	if _, err := r.Allow(p, CanRead, "prod/payments/db"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(16 * time.Minute)
	if _, err := r.Allow(p, CanRead, "prod/payments/db"); !errors.Is(err, ErrRulesStale) {
		t.Fatalf("16 minutes without argus-control: %v", err)
	}
	r.Contacted()
	if _, err := r.Allow(p, CanRead, "prod/payments/db"); err != nil {
		t.Fatalf("after hearing from argus-control: %v", err)
	}
}

func TestExpiredAppTokenIsRefused(t *testing.T) {
	r := NewRules(15 * time.Minute)
	if err := r.Replace(bundle(1), "impack"); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Authenticate("argus-app-ci"); !ok {
		t.Fatal("live app token refused")
	}
	r.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if _, ok := r.Authenticate("argus-app-ci"); ok {
		t.Fatal("expired app token accepted")
	}
}

func TestRulesSurviveARestartButNotForever(t *testing.T) {
	s, _, _ := testStore(t)
	r := NewRules(15 * time.Minute)
	if err := s.ApplyRules(r, bundle(5)); err != nil {
		t.Fatal(err)
	}
	r2 := NewRules(15 * time.Minute)
	if err := s.LoadRules(r2); err != nil {
		t.Fatal(err)
	}
	if serial, _, fresh := r2.Status(); serial != 5 || !fresh {
		t.Fatalf("restored: serial %d fresh %v", serial, fresh)
	}
	r3 := NewRules(15 * time.Minute)
	r3.now = func() time.Time { return time.Now().Add(time.Hour) }
	if err := s.LoadRules(r3); err != nil {
		t.Fatal(err)
	}
	if _, _, fresh := r3.Status(); fresh {
		t.Fatal("rules saved an hour ago were trusted after a restart without argus-control")
	}
}

func TestConfigRefusesPlainListeningOffLoopback(t *testing.T) {
	key, _, _ := ed25519.GenerateKey(rand.Reader)
	c := Config{Organization: "impack", ControlPublicKey: secretwire.EncodeKey(key), BehindTLSProxy: true, Listen: "0.0.0.0:8460"}
	c.Defaults()
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("plain listener on every address: %v", err)
	}
	c.Listen = "127.0.0.1:8460"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.ControlPublicKey = "nope"
	if err := c.Validate(); err == nil {
		t.Fatal("accepted a control key that is not one")
	}
}

func TestRootKeyIsMadeOnceAndMustBePrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "root.key")
	k1, created, err := LoadRootKey(path)
	if err != nil || !created || len(k1) != KeySize {
		t.Fatalf("first: %v %v", created, err)
	}
	k2, created, err := LoadRootKey(path)
	if err != nil || created || !bytes.Equal(k1, k2) {
		t.Fatalf("second: %v %v", created, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadRootKey(path); err == nil {
		t.Fatal("loaded a root key others can read")
	}
}

func TestClientIPBelievesOnlyTrustedProxies(t *testing.T) {
	trusted, _ := ParsePrefixes([]string{"127.0.0.1", "10.0.0.0/8"})
	c := ClientIP{Trusted: trusted}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.9:5555"
	r.Header.Set("CF-Connecting-IP", "198.51.100.1")
	if got := c.Of(r); got != "203.0.113.9" {
		t.Fatalf("untrusted peer's header believed: %s", got)
	}
	r.RemoteAddr = "127.0.0.1:5555"
	if got := c.Of(r); got != "198.51.100.1" {
		t.Fatalf("cloudflared on loopback: %s", got)
	}
	r.Header.Del("CF-Connecting-IP")
	r.Header.Set("X-Forwarded-For", "198.51.100.7, 10.1.2.3")
	if got := c.Of(r); got != "198.51.100.7" {
		t.Fatalf("forwarded through a trusted hop: %s", got)
	}
}

func TestFailLimiterIsBounded(t *testing.T) {
	l := newFailLimiter(2)
	l.cap = 3
	l.Failed("a")
	l.Failed("a")
	if !l.Blocked("a") {
		t.Fatal("two failures did not block")
	}
	for _, k := range []string{"b", "c", "d"} {
		l.Failed(k)
	}
	if len(l.counts) > 3 {
		t.Fatalf("limiter grew to %d entries", len(l.counts))
	}
}
