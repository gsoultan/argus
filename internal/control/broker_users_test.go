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

// A RabbitMQ entry's users are its administrators' to see: the list needs DBA.
func TestRabbitUsersNeedDBA(t *testing.T) {
	a := &API{}
	db := Database{Engine: tunnel.EngineRabbitMQ, Name: "/orders", Hostname: "mq-01"}
	req := tunnel.StoreRequest{Op: tunnel.BrokerOpUsers}
	if no, ok := a.brokerUserGate(context.Background(), "ana", db, tunnel.LevelOwner, &req, nil); !ok || no == nil || !strings.Contains(no.Error, "dba") {
		t.Fatalf("an owner listed RabbitMQ users: %+v", no)
	}
	req = tunnel.StoreRequest{Op: tunnel.BrokerOpUserSet, Password: "smuggled", BrokerUser: &tunnel.BrokerUser{Name: "x", Tags: &[]string{"administrator"}}}
	if no, _ := a.brokerUserGate(context.Background(), "ana", db, tunnel.LevelDBA, &req, nil); no == nil || req.Password != "" {
		t.Fatalf("an administrator tag passed, or the password stayed: %+v %+v", no, req)
	}
}

// A Kafka topic or group change needs Manage and, on production, waits for
// approval of its exact text; a dry run waits for nothing.
func TestKafkaChangesWait(t *testing.T) {
	a := &API{}
	db := Database{Engine: tunnel.EngineKafka, Name: "orders.", Hostname: "stream-01", Label: "production"}
	add := func() *tunnel.StoreRequest {
		return &tunnel.StoreRequest{Op: tunnel.KafkaOpAddPartitions, Name: "orders.eu", KafkaChange: &tunnel.KafkaChange{Add: 2}}
	}
	if no, ok := a.brokerUserGate(context.Background(), "ana", db, tunnel.LevelWrite, add(), nil); !ok || no == nil || !strings.Contains(no.Error, "owner") {
		t.Fatalf("write added partitions: %+v", no)
	}
	req := add()
	no, _ := a.brokerUserGate(context.Background(), "ana", db, tunnel.LevelOwner, req, nil)
	if no == nil || no.NeedsApproval == nil || no.NeedsApproval.Text != "ADD 2 PARTITIONS TO TOPIC orders.eu" {
		t.Fatalf("production did not wait: %+v", no)
	}
	out := &tunnel.StoreRequest{Op: tunnel.KafkaOpAlterConfigs, Name: "payments", KafkaChange: &tunnel.KafkaChange{Set: map[string]string{"retention.ms": "1"}}}
	if no, _ := a.brokerUserGate(context.Background(), "ana", db, tunnel.LevelOwner, out, nil); no == nil || no.NeedsApproval != nil {
		t.Fatalf("a topic outside the entry passed: %+v", no)
	}
	p := int32(0)
	dry := &tunnel.StoreRequest{Op: tunnel.KafkaOpDeleteRecords, Name: "orders.eu", KafkaChange: &tunnel.KafkaChange{DryRun: true, To: "offset", Partition: &p, Offset: 5}}
	if no, _ := a.brokerUserGate(context.Background(), "ana", db, tunnel.LevelOwner, dry, nil); no != nil {
		t.Fatalf("a dry run waited: %+v", no)
	}
	peek := &tunnel.StoreRequest{Op: "peek", Name: "orders.eu", KafkaChange: &tunnel.KafkaChange{Add: 1}}
	a.brokerUserGate(context.Background(), "ana", db, tunnel.LevelOwner, peek, nil)
	if peek.KafkaChange != nil {
		t.Fatal("a peek kept a change")
	}
}

// A NATS entry's users: listing is Manage, signing one is Admin and on
// production waits for approval of its text, which names the key and never
// a seed; the entry's other operations pass to their own gate.
func TestNATSUsersGate(t *testing.T) {
	a := &API{}
	ctx := context.Background()
	db := Database{Engine: tunnel.EngineNATS, Name: "ORDERS", Hostname: "nats-01", Label: "production"}
	list := tunnel.StoreRequest{Op: tunnel.BrokerOpUsers}
	if no, ok := a.brokerUserGate(ctx, "ana", db, tunnel.LevelOwner, &list, nil); !ok || no != nil {
		t.Fatalf("Manage could not list NATS users: %+v", no)
	}
	key := "UDXU4RCSJNZOIQHZNWXHXORDPRTGNJAHAHFRGZNEEJCPQTT2M7NLCNF4"
	set := func() tunnel.StoreRequest {
		return tunnel.StoreRequest{ID: 3, Op: tunnel.BrokerOpUserSet, Password: "smuggled", BrokerUser: &tunnel.BrokerUser{Name: "orders-api", Create: true,
			PublicKey: key, NATS: &tunnel.NATSAppUser{PubAllow: []string{"orders.>"}, Days: 90}}}
	}
	req := set()
	if no, _ := a.brokerUserGate(ctx, "ana", db, tunnel.LevelOwner, &req, nil); no == nil || !strings.Contains(no.Error, "dba") {
		t.Fatalf("Manage signed a user: %+v", no)
	}
	req = set()
	no, _ := a.brokerUserGate(ctx, "ana", db, tunnel.LevelDBA, &req, nil)
	if no == nil || no.NeedsApproval == nil || !strings.Contains(no.NeedsApproval.Text, `CREATE USER "orders-api" in account ORDERS, key `+key) {
		t.Fatalf("production did not wait: %+v", no)
	}
	req = set()
	req.BrokerUser.NATS.SubAllow = []string{"$SYS.>"}
	if no, _ := a.brokerUserGate(ctx, "ana", db, tunnel.LevelDBA, &req, nil); no == nil || no.NeedsApproval != nil {
		t.Fatalf("$SYS was offered for approval: %+v", no)
	}
	db.Label = "staging"
	req = set()
	if no, _ := a.brokerUserGate(ctx, "ana", db, tunnel.LevelDBA, &req, newBrokerAudit(a)); no != nil || req.Password != "" {
		t.Fatalf("staging: %+v %+v", no, req)
	}
	other := tunnel.StoreRequest{Op: "stream_create", BrokerUser: &tunnel.BrokerUser{Name: "x"}}
	if _, ok := a.brokerUserGate(ctx, "ana", db, tunnel.LevelOwner, &other, nil); ok || other.BrokerUser != nil {
		t.Fatalf("a stream change was taken as a user change: %+v", other)
	}
}
