package tunnel

import (
	"encoding/base64"
	"strings"
	"testing"
)

func testCred(n int) *ScramCredential {
	return &ScramCredential{Salt: base64.StdEncoding.EncodeToString(make([]byte, 24)),
		SaltedPassword: base64.StdEncoding.EncodeToString(make([]byte, n)), Iterations: 8192}
}

func TestKafkaUserChangesStayInTheScope(t *testing.T) {
	topic := func(pattern, name, op string) KafkaACL {
		return KafkaACL{Resource: "topic", Pattern: pattern, Name: name, Operation: op, Permission: "ALLOW", Host: "*"}
	}
	set := func(u BrokerUser) StoreRequest { return StoreRequest{Op: BrokerOpUserSet, BrokerUser: &u} }
	cases := []struct {
		scope string
		req   StoreRequest
		want  string // "" is allowed; else a word of why not
	}{
		{"orders.", set(BrokerUser{Name: "app", Create: true, Mechanism: "SCRAM-SHA-512", Credential: testCred(64),
			ACLs: []KafkaACL{topic("PREFIXED", "orders.inv", "READ"), {Resource: "group", Pattern: "PREFIXED", Name: "app", Operation: "READ", Permission: "ALLOW", Host: "*"}}}), ""},
		{"orders.", set(BrokerUser{Name: "app", ACLs: []KafkaACL{topic("PREFIXED", "pay", "READ")}}), "outside"},
		{"orders.", set(BrokerUser{Name: "app", ACLs: []KafkaACL{topic("LITERAL", "*", "READ")}}), "outside"},
		{"*", set(BrokerUser{Name: "app", ACLs: []KafkaACL{topic("LITERAL", "*", "READ")}}), ""},
		{"orders.", set(BrokerUser{Name: "app", ACLs: []KafkaACL{{Resource: "cluster", Pattern: "LITERAL", Name: "kafka-cluster", Operation: "ALTER", Permission: "ALLOW", Host: "*"}}}), "topic"},
		{"orders.", set(BrokerUser{Name: "app", ACLs: []KafkaACL{topic("PREFIXED", "orders.", "ALL")}}), "not one Argus gives"},
		{"orders.", set(BrokerUser{Name: "app", ACLs: []KafkaACL{topic("PREFIXED", "orders.", "READ"), topic("PREFIXED", "orders.", "READ")}}), "twice"},
		{"orders.", set(BrokerUser{Name: "argus-ana-dba", ACLs: []KafkaACL{}}), "Argus's own"},
		{"orders.", set(BrokerUser{Name: "bad name", Create: true, Mechanism: "SCRAM-SHA-512", Credential: testCred(64)}), "letters"},
		{"orders.", set(BrokerUser{Name: "app", Create: true}), "password"},
		{"orders.", set(BrokerUser{Name: "app", Create: true, Mechanism: "SCRAM-SHA-256", Credential: testCred(64)}), "32 bytes"},
		{"orders.", set(BrokerUser{Name: "app", Create: true, Mechanism: "PLAIN", Credential: testCred(64)}), "SCRAM"},
		{"orders.", set(BrokerUser{Name: "app"}), "nothing to change"},
		{"orders.", set(BrokerUser{Name: "CN=legacy-etl,OU=ops", ACLs: []KafkaACL{}}), ""},
		{"orders.", StoreRequest{Op: BrokerOpUserLogin, BrokerUser: &BrokerUser{Name: "app"}}, "password"},
	}
	for i, c := range cases {
		got := ValidKafkaUserChange(c.scope, c.req)
		if (c.want == "") != (got == "") || (c.want != "" && !strings.Contains(got, c.want)) {
			t.Errorf("%d: %q, want %q", i, got, c.want)
		}
	}
}

func TestKafkaUserTextRedactsTheCredential(t *testing.T) {
	cred := testCred(64)
	cred.SaltedPassword = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 64)))
	req := StoreRequest{Op: BrokerOpUserSet, BrokerUser: &BrokerUser{Name: "app", Create: true, Mechanism: "SCRAM-SHA-512",
		Credential: cred, ACLs: []KafkaACL{{Resource: "topic", Pattern: "PREFIXED", Name: "orders.", Operation: "READ", Permission: "ALLOW", Host: "*"}},
		Quotas: &KafkaQuotas{ConsumeBytes: 1024}}}
	text := KafkaUserText("orders.", req)
	for _, want := range []string{`CREATE USER "app"`, "salted [redacted]", `ALLOW topic PREFIXED "orders." READ`, "consume 1024 bytes/s"} {
		if !strings.Contains(text, want) {
			t.Errorf("%q is not in %s", want, text)
		}
	}
	if strings.Contains(text, cred.SaltedPassword) || strings.Contains(text, cred.Salt) {
		t.Fatalf("the credential is in the text: %s", text)
	}
	// The same change asked twice reads the same, whatever order the ACLs came in.
	req2 := req
	u := *req.BrokerUser
	u.ACLs = append([]KafkaACL{{Resource: "group", Pattern: "PREFIXED", Name: "app", Operation: "READ", Permission: "ALLOW", Host: "*"}}, u.ACLs...)
	req2.BrokerUser = &u
	u3 := u
	u3.ACLs = []KafkaACL{u.ACLs[1], u.ACLs[0]}
	if KafkaUserText("orders.", req2) != KafkaUserText("orders.", StoreRequest{Op: BrokerOpUserSet, BrokerUser: &u3}) {
		t.Fatal("the text depends on the order of the ACLs")
	}
}

func TestBrokerUserLevels(t *testing.T) {
	if BrokerUserOpLevel(BrokerOpUsers) != LevelOwner || BrokerUserOpLevel(BrokerOpUserSet) != LevelDBA || BrokerUserOpLevel("peek") != "" {
		t.Fatal("levels")
	}
	for _, op := range KafkaStoreOps(LevelOwner) {
		if op == BrokerOpUserSet {
			t.Fatal("an owner may change users")
		}
	}
}
