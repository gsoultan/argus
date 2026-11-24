package tunnel

import (
	"slices"
	"strings"
	"testing"
)

const testUserKey = "UDXU4RCSJNZOIQHZNWXHXORDPRTGNJAHAHFRGZNEEJCPQTT2M7NLCNF4"

func natsUserReq(op string, u *BrokerUser) StoreRequest {
	return StoreRequest{Op: op, BrokerUser: u}
}

// What a NATS app user may be signed with: a key from the browser, a name
// that is not an Argus session's, subjects NATS takes and never $SYS, an
// expiry within a year, addresses that parse; and nothing changed in place.
func TestNATSUserRules(t *testing.T) {
	ok := &NATSAppUser{PubAllow: []string{"orders.>"}, SubAllow: []string{"orders.eu.*"}, Days: 90, Sources: []string{"10.0.0.0/8", "192.0.2.7"}}
	good := func() *BrokerUser {
		return &BrokerUser{Name: "orders-api", Create: true, PublicKey: testUserKey, NATS: ok}
	}
	if why := ValidNATSUserChange(natsUserReq(BrokerOpUserSet, good())); why != "" {
		t.Fatalf("a good user: %s", why)
	}
	bad := map[string]func(*BrokerUser){
		"argus name":     func(u *BrokerUser) { u.Name = "argus-ana-owner" },
		"name":           func(u *BrokerUser) { u.Name = "orders api" },
		"no create":      func(u *BrokerUser) { u.Create = false },
		"seed as key":    func(u *BrokerUser) { u.PublicKey = "SU" + testUserKey[2:] },
		"account key":    func(u *BrokerUser) { u.PublicKey = "A" + testUserKey[1:] },
		"no rules":       func(u *BrokerUser) { u.NATS = nil },
		"no days":        func(u *BrokerUser) { n := *ok; n.Days = 0; u.NATS = &n },
		"over a year":    func(u *BrokerUser) { n := *ok; n.Days = 366; u.NATS = &n },
		"sys":            func(u *BrokerUser) { n := *ok; n.SubAllow = []string{"$SYS.>"}; u.NATS = &n },
		"bad subject":    func(u *BrokerUser) { n := *ok; n.PubAllow = []string{"orders.>.x"}; u.NATS = &n },
		"twice":          func(u *BrokerUser) { n := *ok; n.PubAllow = []string{"a", "a"}; u.NATS = &n },
		"nothing":        func(u *BrokerUser) { u.NATS = &NATSAppUser{Days: 1} },
		"bad source":     func(u *BrokerUser) { n := *ok; n.Sources = []string{"10.0.0.1/8"}; u.NATS = &n },
		"jetstream word": func(u *BrokerUser) { n := *ok; n.JetStream = "all"; u.NATS = &n },
	}
	for name, mut := range bad {
		u := good()
		mut(u)
		if why := ValidNATSUserChange(natsUserReq(BrokerOpUserSet, u)); why == "" {
			t.Errorf("%s: taken", name)
		}
	}
	for _, op := range []string{BrokerOpUserLogin, BrokerOpUserScopeDel} {
		if ValidNATSUserChange(natsUserReq(op, good())) == "" {
			t.Errorf("%s taken for NATS", op)
		}
	}
	if ValidNATSUserChange(natsUserReq(BrokerOpUserDel, &BrokerUser{Name: "orders-api"})) == "" {
		t.Error("forgetting needs the key")
	}
	if BrokerUserOpLevelFor(EngineNATS, BrokerOpUserSet) != LevelDBA || BrokerUserOpLevelFor(EngineNATS, BrokerOpUsers) != LevelOwner {
		t.Error("levels")
	}
}

// The rules as they go in the JWT, and as an approver reads them.
func TestNATSUserPermsAndText(t *testing.T) {
	p := NATSAppUserPerms(NATSAppUser{PubAllow: []string{"orders.>"}, JetStream: "use", Days: 1})
	if !slices.Contains(p.PubAllow, "$JS.API.CONSUMER.>") || slices.Contains(p.PubAllow, "$JS.API.>") ||
		!slices.Contains(p.SubAllow, NATSInboxes) || !slices.Contains(p.PubDeny, "$SYS.>") || !slices.Contains(p.SubDeny, "$SYS.>") {
		t.Fatalf("use: %+v", p)
	}
	if p := NATSAppUserPerms(NATSAppUser{PubAllow: []string{"a"}, Days: 1}); slices.Contains(p.SubAllow, NATSInboxes) {
		t.Fatal("an app that makes no requests reads every inbox")
	}
	if p := NATSAppUserPerms(NATSAppUser{JetStream: "manage", Days: 1}); !slices.Contains(p.PubAllow, "$JS.API.>") {
		t.Fatal("manage")
	}
	req := natsUserReq(BrokerOpUserSet, &BrokerUser{Name: "orders-api", Create: true, PublicKey: testUserKey,
		NATS: &NATSAppUser{PubAllow: []string{"orders.>"}, PubDeny: []string{"orders.admin.>"}, Requests: true, Replies: true, Days: 30, Sources: []string{"10.0.0.0/8"}}})
	text := NATSUserText("ORDERS", req)
	for _, want := range []string{`CREATE USER "orders-api" in account ORDERS, key ` + testUserKey + ", valid 30 days",
		"publish to orders.>", "except orders.admin.>", "subscribe to nothing", "makes requests", "answers requests", "connects only from 10.0.0.0/8"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	del := NATSUserText("ORDERS", natsUserReq(BrokerOpUserDel, &BrokerUser{Name: "orders-api", PublicKey: testUserKey}))
	if !strings.Contains(del, "nsc revocations add-user --account ORDERS --user-public-key "+testUserKey+" && nsc push --account ORDERS") {
		t.Fatalf("forget: %s", del)
	}
}
