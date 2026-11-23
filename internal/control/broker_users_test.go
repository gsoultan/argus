package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

// A Kafka user change is DBA work, waits for approval of exactly its text on
// production, and reaches Activity only once the agent says it ran; the
// password reaches the agent only for the sign-in check.
func TestKafkaUserChangesWaitAndAreAudited(t *testing.T) {
	a, s, h, pg, admin := dbFixture(t)
	ctx := context.Background()
	db, err := s.CreateDatabase(ctx, Database{AssetID: pg.AssetID, Engine: tunnel.EngineKafka,
		Name: "orders.", Host: "127.0.0.1", Port: 9092}, admin)
	if err != nil {
		t.Fatalf("CreateDatabase kafka: %v", err)
	}
	ana, _ := newAccount(t, s, "operator")
	a.UserTokens["ana"] = ana
	if w := do(t, h, http.MethodPut, "/api/v1/databases/"+db.ID+"/assignments", "admin-token",
		fmt.Sprintf(`{"userEmail":%q,"level":"dba"}`, ana)); w.Code >= 300 {
		t.Fatalf("assign: %d %s", w.Code, w.Body)
	}
	audit := newBrokerAudit(a)
	gate := func(level string, req *tunnel.StoreRequest) *tunnel.StoreAnswer {
		no, ok := a.brokerUserGate(ctx, ana, db, level, req, audit)
		if !ok {
			t.Fatalf("%s is not a broker user operation", req.Op)
		}
		return no
	}
	cred := &tunnel.ScramCredential{Salt: base64.StdEncoding.EncodeToString(make([]byte, 24)),
		SaltedPassword: base64.StdEncoding.EncodeToString(make([]byte, 64)), Iterations: 8192}
	create := tunnel.StoreRequest{ID: 7, Op: tunnel.BrokerOpUserSet, Password: "smuggled", BrokerUser: &tunnel.BrokerUser{
		Name: "invoices-app", Create: true, Mechanism: "SCRAM-SHA-512", Credential: cred,
		ACLs: []tunnel.KafkaACL{{Resource: "topic", Pattern: "PREFIXED", Name: "orders.inv", Operation: "READ", Permission: "ALLOW", Host: "*"}}}}

	owner := create
	if no := gate(tunnel.LevelOwner, &owner); no == nil || !strings.Contains(no.Error, "dba") {
		t.Fatalf("an owner made a user: %+v", no)
	}
	outside := create
	outside.BrokerUser = &tunnel.BrokerUser{Name: "x", ACLs: []tunnel.KafkaACL{{Resource: "topic", Pattern: "PREFIXED", Name: "payments.", Operation: "READ", Permission: "ALLOW", Host: "*"}}}
	if no := gate(tunnel.LevelDBA, &outside); no == nil || !strings.Contains(no.Error, "outside") {
		t.Fatalf("an ACL outside the scope passed: %+v", no)
	}

	req := create
	no := gate(tunnel.LevelDBA, &req)
	if no == nil || no.NeedsApproval == nil || no.NeedsApproval.Text != tunnel.KafkaUserText("orders.", create) {
		t.Fatalf("production did not wait: %+v", no)
	}
	if strings.Contains(no.NeedsApproval.Text, cred.SaltedPassword) {
		t.Fatal("the approval text holds the credential")
	}
	body, _ := json.Marshal(map[string]any{"items": []map[string]any{{"sql": no.NeedsApproval.Text}}, "justification": "the invoices app goes live"})
	w := do(t, h, http.MethodPost, "/api/v1/databases/"+db.ID+"/statements", "ana", string(body))
	if w.Code != http.StatusCreated {
		t.Fatalf("ask: %d %s", w.Code, w.Body)
	}
	var ask StatementAsk
	_ = json.Unmarshal(w.Body.Bytes(), &ask)
	if _, err := s.DecideRequest(ctx, ask.RequestID, "approved", "", admin, false); err != nil {
		t.Fatal(err)
	}
	wider := create
	wider.Approval = ask.RequestID
	wider.BrokerUser = &tunnel.BrokerUser{Name: "invoices-app", Create: true, Mechanism: "SCRAM-SHA-512", Credential: cred,
		ACLs: []tunnel.KafkaACL{{Resource: "topic", Pattern: "PREFIXED", Name: "orders.", Operation: "WRITE", Permission: "ALLOW", Host: "*"}}}
	if no := gate(tunnel.LevelDBA, &wider); no == nil {
		t.Fatal("a wider change ran under the approval")
	}
	req = create
	req.Approval = ask.RequestID
	if no := gate(tunnel.LevelDBA, &req); no != nil {
		t.Fatalf("the approved change was stopped: %+v", no)
	}
	if req.Password != "" {
		t.Fatal("a password went to the agent with a change")
	}
	events := func() []BrokerUserEvent {
		ev, err := s.BrokerUserEvents(ctx, db.Hostname, db.ID, 10)
		if err != nil {
			t.Fatal(err)
		}
		return ev
	}
	if len(events()) != 0 {
		t.Fatal("audited before the agent answered")
	}
	audit.answer(ctx, []byte(`{"id":7,"type":"result"}`))
	if ev := events(); len(ev) != 1 || ev[0].Action != "broker.user.created" {
		t.Fatalf("the change is not in Activity: %+v", ev)
	}

	// A refused change is not audited.
	db.Label = "staging"
	del := tunnel.StoreRequest{ID: 8, Op: tunnel.BrokerOpUserDel, BrokerUser: &tunnel.BrokerUser{Name: "invoices-app"}}
	if no := gate(tunnel.LevelDBA, &del); no != nil {
		t.Fatalf("staging waited: %+v", no)
	}
	audit.answer(ctx, []byte(`{"id":8,"type":"error","error":"no"}`))
	if len(events()) != 1 {
		t.Fatal("a change the agent refused is in Activity")
	}

	// The sign-in check carries the password; nothing else does.
	login := tunnel.StoreRequest{Op: tunnel.BrokerOpUserLogin, Password: "pw-1234567890", BrokerUser: &tunnel.BrokerUser{Name: "invoices-app"}}
	if no := gate(tunnel.LevelDBA, &login); no != nil || login.Password == "" {
		t.Fatalf("the sign-in check: %+v %+v", no, login)
	}
	peek := tunnel.StoreRequest{Op: "peek", Name: "orders.x", Password: "pw", BrokerUser: &tunnel.BrokerUser{Name: "x"}}
	if _, ok := a.brokerUserGate(ctx, ana, db, tunnel.LevelDBA, &peek, audit); ok || peek.Password != "" || peek.BrokerUser != nil {
		t.Fatalf("a peek kept a password or a user: %+v", peek)
	}

	// The Users tab reads who made what from Activity.
	w = do(t, h, http.MethodGet, "/api/v1/databases/"+db.ID+"/broker-users", "ana", "")
	var got struct {
		Events []BrokerUserEvent `json:"events"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.Events) != 1 ||
		got.Events[0].User != "invoices-app" || got.Events[0].By != ana || got.Events[0].Action != "broker.user.created" {
		t.Fatalf("events: %d %s", w.Code, w.Body)
	}
}

// Other engines keep their own fields: a Redis sign-in check keeps its password.
func TestBrokerUserGateLeavesOtherEngines(t *testing.T) {
	a := &API{}
	req := tunnel.StoreRequest{Op: tunnel.RedisOpACLLogin, Password: "pw"}
	if _, ok := a.brokerUserGate(context.Background(), "ana", Database{Engine: tunnel.EngineRedis}, tunnel.LevelDBA, &req, nil); ok || req.Password != "pw" {
		t.Fatalf("a Redis request was changed: %+v", req)
	}
}
