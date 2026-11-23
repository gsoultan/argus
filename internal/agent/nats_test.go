//go:build !windows

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"

	"github.com/gsoultan/argus/internal/tunnel"
)

// natsLab is a real nats-server in operator mode, inside the test: an
// operator, the system account, and the account ORDERS with JetStream and a
// signing key that the agent's key file holds.
type natsLab struct {
	srv     *server.Server
	port    int
	monitor int
	file    string
	accPub  string
	signKP  nkeys.KeyPair
	other   string // a second account's public key, PAYMENTS
	otherKP nkeys.KeyPair
}

func newNATSLab(t *testing.T) *natsLab {
	t.Helper()
	okp, _ := nkeys.CreateOperator()
	opub, _ := okp.PublicKey()
	oc := jwt.NewOperatorClaims(opub)
	oc.Name = "argus-test"

	sysKP, _ := nkeys.CreateAccount()
	sysPub, _ := sysKP.PublicKey()
	sc := jwt.NewAccountClaims(sysPub)
	sc.Name = "SYS"
	sysJWT, err := sc.Encode(okp)
	if err != nil {
		t.Fatal(err)
	}
	oc.SystemAccount = sysPub
	opJWT, err := oc.Encode(okp)
	if err != nil {
		t.Fatal(err)
	}
	ocl, err := jwt.DecodeOperatorClaims(opJWT)
	if err != nil {
		t.Fatal(err)
	}

	mkAcc := func(name string) (string, nkeys.KeyPair, string) {
		akp, _ := nkeys.CreateAccount()
		apub, _ := akp.PublicKey()
		skp, _ := nkeys.CreateAccount()
		spub, _ := skp.PublicKey()
		ac := jwt.NewAccountClaims(apub)
		ac.Name = name
		ac.SigningKeys.Add(spub)
		ac.Limits.JetStreamLimits = jwt.JetStreamLimits{MemoryStorage: -1, DiskStorage: -1, Streams: -1, Consumer: -1}
		aj, err := ac.Encode(okp)
		if err != nil {
			t.Fatal(err)
		}
		return apub, skp, aj
	}
	accPub, signKP, accJWT := mkAcc("ORDERS")
	otherPub, otherKP, otherJWT := mkAcc("PAYMENTS")

	res := &server.MemAccResolver{}
	for k, v := range map[string]string{sysPub: sysJWT, accPub: accJWT, otherPub: otherJWT} {
		if err := res.Store(k, v); err != nil {
			t.Fatal(err)
		}
	}
	opts := &server.Options{
		Host: "127.0.0.1", Port: -1, HTTPHost: "127.0.0.1", HTTPPort: -1,
		JetStream: true, StoreDir: t.TempDir(), NoLog: true, NoSigs: true,
		TrustedOperators: []*jwt.OperatorClaims{ocl}, SystemAccount: sysPub, AccountResolver: res,
	}
	srv, err := server.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats-server did not start")
	}
	t.Cleanup(srv.Shutdown)
	mon := srv.MonitorAddr()
	if mon == nil {
		t.Fatal("no monitoring port")
	}

	seed, _ := signKP.Seed()
	file := filepath.Join(t.TempDir(), "nats.conf")
	body := fmt.Sprintf("# test keys\naccount ORDERS = %s %s\nmonitor = %d\n", accPub, seed, mon.Port)
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	u := srv.ClientURL()
	port, _ := strconv.Atoi(u[strings.LastIndexByte(u, ':')+1:])
	return &natsLab{srv: srv, port: port, monitor: mon.Port, file: file, accPub: accPub, signKP: signKP,
		other: otherPub, otherKP: otherKP}
}

func (l *natsLab) grant(level string) tunnel.Grant {
	return tunnel.Grant{ID: "g-" + level, Email: "ana@example.com", Host: "nats-01", Principal: "ana", Service: tunnel.NATSService,
		DB: &tunnel.GrantDB{Name: "ORDERS", Host: "127.0.0.1", Port: l.port, Level: level, Engine: tunnel.EngineNATS,
			ConfigPath: l.file, MaxSessionS: 600}}
}

// open opens a session at level, with a recording in a temporary
// directory, and closes it when the test ends.
func (l *natsLab) open(t *testing.T, level string) (*natsSession, *stmtRecorder, string) {
	t.Helper()
	f := &natsFactory{cfg: StoreConfig{}}
	eng, err := f.open(context.Background(), l.grant(level), "s1")
	if err != nil {
		t.Fatalf("open at %s: %v", level, err)
	}
	dir := t.TempDir()
	rec, err := newStmtRecorder(dir, "s-"+level)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.close(context.Background()); rec.close("done") })
	return eng.(*natsSession), rec, filepath.Join(dir, "s-"+level+".argussql")
}

func natsDo(t *testing.T, s *natsSession, rec *stmtRecorder, op string, n *tunnel.NATSRequest, extra ...func(*tunnel.StoreRequest)) tunnel.StoreAnswer {
	t.Helper()
	req := tunnel.StoreRequest{ID: 7, Op: op, NATS: n}
	for _, e := range extra {
		e(&req)
	}
	a, err := s.do(context.Background(), req, rec)
	if err != nil {
		t.Fatalf("%s: %v", op, err)
	}
	return a
}

func orders() *tunnel.NATSStreamSpec {
	return &tunnel.NATSStreamSpec{Name: "ORDERS_EVENTS", Subjects: []string{"orders.>"}, Retention: "limits", Storage: "memory",
		Replicas: 1, MaxAgeS: 3600, Discard: "old", DuplicatesS: 60}
}

func TestNATSAdminFile(t *testing.T) {
	akp, _ := nkeys.CreateAccount()
	apub, _ := akp.PublicKey()
	skp, _ := nkeys.CreateAccount()
	seed, _ := skp.Seed()
	ukp, _ := nkeys.CreateUser()
	useed, _ := ukp.Seed()
	good := fmt.Sprintf("account ORDERS = %s %s\nmonitor = 9222\n", apub, seed)
	a, err := parseNATSAdmin("/etc/argus/nats.conf", []byte(good))
	if err != nil || a.Accounts["ORDERS"].ID != apub || a.MonitorPort != 9222 {
		t.Fatalf("parse: %+v %v", a, err)
	}
	for name, body := range map[string]string{
		"no account":       "monitor = 8222\n",
		"user seed":        fmt.Sprintf("account ORDERS = %s %s\n", apub, useed),
		"seed as id":       fmt.Sprintf("account ORDERS = %s %s\n", seed, seed),
		"system account":   fmt.Sprintf("account SYS = %s %s\n", apub, seed),
		"one key":          fmt.Sprintf("account ORDERS = %s\n", apub),
		"bad monitor port": fmt.Sprintf("account ORDERS = %s %s\nmonitor = x\n", apub, seed),
	} {
		if _, err := parseNATSAdmin("/x.conf", []byte(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	f := filepath.Join(t.TempDir(), "nats.conf")
	_ = os.WriteFile(f, []byte(good), 0o644)
	if _, err := readNATSAdmin(f); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("a key file others can read was taken: %v", err)
	}
}

// TestNATSRefusesOpenServer: a server anyone can join without credentials
// is refused, since no level would hold there.
func TestNATSRefusesOpenServer(t *testing.T) {
	srv, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats-server did not start")
	}
	defer srv.Shutdown()
	err = refuseOpenNATS(context.Background(), srv.ClientURL())
	if err == nil || !strings.Contains(err.Error(), "without credentials") {
		t.Fatalf("an open server was not refused: %v", err)
	}
}

// TestNATSWrongSigningKey: a key that is not one of the account's signing
// keys is refused with a sentence that says what to fix.
func TestNATSWrongSigningKey(t *testing.T) {
	l := newNATSLab(t)
	bad, _ := nkeys.CreateAccount()
	seed, _ := bad.Seed()
	_ = os.WriteFile(l.file, []byte(fmt.Sprintf("account ORDERS = %s %s\nmonitor = %d\n", l.accPub, seed, l.monitor)), 0o600)
	f := &natsFactory{cfg: StoreConfig{}}
	_, err := f.open(context.Background(), l.grant(tunnel.LevelRead), "s")
	if err == nil || !strings.Contains(err.Error(), "signing keys") {
		t.Fatalf("a foreign signing key was taken: %v", err)
	}
	// And an account the file has no key for.
	g := l.grant(tunnel.LevelRead)
	g.DB.Name = "PAYMENTS"
	if _, err := f.open(context.Background(), g, "s"); err == nil {
		t.Fatal("an account with no key in the file was opened")
	}
}

// TestNATSLevelsHold drives a real server: what each level may do is what
// NATS lets its signed user do, not only what the agent offers.
func TestNATSLevelsHold(t *testing.T) {
	l := newNATSLab(t)
	owner, orec, ofile := l.open(t, tunnel.LevelOwner)
	if a := natsDo(t, owner, orec, "stream_create", &tunnel.NATSRequest{StreamSpec: orders()}); a.Type != "result" {
		t.Fatalf("owner could not create a stream: %s", a.Error)
	}

	reader, rrec, _ := l.open(t, tunnel.LevelRead)
	writer, wrec, _ := l.open(t, tunnel.LevelWrite)

	// The agent offers each level its own list.
	if tunnel.NATSOpAllowed("publish", tunnel.LevelRead) || tunnel.NATSOpAllowed("stream_create", tunnel.LevelWrite) {
		t.Fatal("a level is offered more than it holds")
	}
	if a := natsDo(t, reader, rrec, "publish", &tunnel.NATSRequest{Subject: "orders.eu.created"}); a.Type != "error" {
		t.Fatal("a reader published through the agent")
	}

	// And NATS holds the users to it even when asked directly.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	errs := make(chan error, 4)
	reader.nc.SetErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) { errs <- err })
	_ = reader.nc.Publish("orders.eu.created", []byte("x"))
	_ = reader.nc.Flush()
	select {
	case err := <-errs:
		if !strings.Contains(err.Error(), "Permissions Violation") {
			t.Fatalf("reader publish: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("NATS let a reader publish")
	}
	if _, err := writer.js.CreateStream(ctx, natsStreamConfig(tunnel.NATSStreamSpec{Name: "X", Subjects: []string{"x.>"}})); err == nil {
		t.Fatal("NATS let a writer create a stream")
	}
	if _, err := reader.nc.SubscribeSync("_INBOX.>"); err == nil {
		_ = reader.nc.Flush()
		select {
		case err := <-errs:
			if !strings.Contains(err.Error(), "Permissions Violation") {
				t.Fatalf("inbox subscribe: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("NATS let a reader listen to other apps' replies")
		}
	}

	// A writer publishes; the stream stores it; a reader reads it back
	// without making a consumer.
	pub := natsDo(t, writer, wrec, "publish", &tunnel.NATSRequest{Subject: "orders.eu.created", Mode: "stream"}, func(r *tunnel.StoreRequest) {
		r.Payload = `{"order":"ord-1"}`
		r.Headers = map[string]string{"Nats-Msg-Id": "ord-1"}
	})
	if pub.Type != "result" {
		t.Fatalf("writer publish: %s", pub.Error)
	}
	natsDo(t, writer, wrec, "publish", &tunnel.NATSRequest{Subject: "orders.us.created"}, func(r *tunnel.StoreRequest) { r.Payload = "two" })
	if a := natsDo(t, writer, wrec, "publish", &tunnel.NATSRequest{Subject: "$JS.API.STREAM.DELETE.ORDERS_EVENTS"}); a.Type != "error" {
		t.Fatal("a publish to NATS's own subjects was sent")
	}
	time.Sleep(100 * time.Millisecond)
	got := natsDo(t, reader, rrec, "get", &tunnel.NATSRequest{Stream: "ORDERS_EVENTS"})
	msgs, _ := got.Data.([]NATSMessage)
	if got.Type != "result" || len(msgs) != 2 || msgs[0].Subject != "orders.us.created" || msgs[1].Headers["Nats-Msg-Id"][0] != "ord-1" {
		t.Fatalf("get newest: %+v %s", got.Data, got.Error)
	}
	got = natsDo(t, reader, rrec, "get", &tunnel.NATSRequest{Stream: "ORDERS_EVENTS", Subject: "orders.eu.*"})
	if msgs, _ = got.Data.([]NATSMessage); len(msgs) != 1 || msgs[0].Data != `{"order":"ord-1"}` {
		t.Fatalf("get with a filter: %+v", got.Data)
	}
	got = natsDo(t, reader, rrec, "get", &tunnel.NATSRequest{Stream: "ORDERS_EVENTS", From: "seq", Seq: 2})
	if msgs, _ = got.Data.([]NATSMessage); len(msgs) != 1 || msgs[0].Seq != 2 {
		t.Fatalf("get from a sequence: %+v", got.Data)
	}
	got = natsDo(t, reader, rrec, "get", &tunnel.NATSRequest{Stream: "ORDERS_EVENTS", From: "time", Time: time.Now().Add(-time.Hour).Format(time.RFC3339)})
	if msgs, _ = got.Data.([]NATSMessage); len(msgs) != 2 {
		t.Fatalf("get from a time: %+v", got.Data)
	}

	// Consumers: an owner makes, moves, pauses and deletes one; a reader
	// lists it and cannot make one.
	cs := &tunnel.NATSConsumerSpec{Name: "invoicer", FilterSubjects: []string{"orders.*.created"}, Deliver: "all", AckWaitS: 30,
		MaxDeliver: 5, BackoffS: []int64{1, 5}}
	if a := natsDo(t, owner, orec, "consumer_create", &tunnel.NATSRequest{Stream: "ORDERS_EVENTS", ConsumerSpec: cs}); a.Type != "result" {
		t.Fatalf("consumer create: %s", a.Error)
	}
	if a := natsDo(t, reader, rrec, "consumer_create", &tunnel.NATSRequest{Stream: "ORDERS_EVENTS", ConsumerSpec: cs}); a.Type != "error" {
		t.Fatal("a reader made a consumer")
	}
	list := natsDo(t, reader, rrec, "consumers", &tunnel.NATSRequest{})
	if list.Type != "result" || list.Total != 1 {
		t.Fatalf("consumers: %+v %s", list.Data, list.Error)
	}
	if a := natsDo(t, owner, orec, "consumer_move", &tunnel.NATSRequest{Stream: "ORDERS_EVENTS", Consumer: "invoicer", From: "seq", Seq: 2}); a.Type != "result" {
		t.Fatalf("consumer move: %s", a.Error)
	}
	c, err := owner.js.Consumer(context.Background(), "ORDERS_EVENTS", "invoicer")
	if err != nil || c.CachedInfo().Config.OptStartSeq != 2 || c.CachedInfo().Config.FilterSubject != "orders.*.created" {
		t.Fatalf("the moved consumer lost its settings: %+v %v", c, err)
	}
	if a := natsDo(t, owner, orec, "consumer_pause", &tunnel.NATSRequest{Stream: "ORDERS_EVENTS", Consumer: "invoicer",
		Time: time.Now().Add(time.Hour).Format(time.RFC3339)}); a.Type != "result" {
		t.Fatalf("consumer pause: %s", a.Error)
	}
	if a := natsDo(t, owner, orec, "consumer_delete", &tunnel.NATSRequest{Stream: "ORDERS_EVENTS", Consumer: "invoicer"}); a.Type != "result" {
		t.Fatalf("consumer delete: %s", a.Error)
	}

	// Destroying messages needs the stream's name typed.
	if a := natsDo(t, owner, orec, "stream_purge", &tunnel.NATSRequest{Stream: "ORDERS_EVENTS"}); !a.NeedsConfirm {
		t.Fatalf("a full purge ran without the name: %+v", a)
	}
	if a := natsDo(t, owner, orec, "stream_purge", &tunnel.NATSRequest{Stream: "ORDERS_EVENTS", Subject: "orders.us.*"}); a.Type != "result" || a.Total != 1 {
		t.Fatalf("purge by subject: %+v %s", a.Data, a.Error)
	}
	if a := natsDo(t, owner, orec, "stream_delete", &tunnel.NATSRequest{Stream: "ORDERS_EVENTS"}, func(r *tunnel.StoreRequest) { r.Confirm = "ORDERS" }); !a.NeedsConfirm {
		t.Fatal("a stream was deleted with the wrong name typed")
	}
	upd := orders()
	upd.MaxAgeS = 7200
	if a := natsDo(t, owner, orec, "stream_update", &tunnel.NATSRequest{StreamSpec: upd}); a.Type != "result" {
		t.Fatalf("stream update: %s", a.Error)
	}
	if a := natsDo(t, owner, orec, "stream_delete", &tunnel.NATSRequest{Stream: "ORDERS_EVENTS"}, func(r *tunnel.StoreRequest) { r.Confirm = "ORDERS_EVENTS" }); a.Type != "result" {
		t.Fatalf("stream delete: %s", a.Error)
	}

	// Everything that read messages or changed the account is in the
	// recording, before it ran.
	b, err := os.ReadFile(ofile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"STREAM CREATE", "CONSUMER CREATE ORDERS_EVENTS", "CONSUMER MOVE ORDERS_EVENTS invoicer to seq 2",
		"CONSUMER PAUSE", "STREAM PURGE ORDERS_EVENTS subject orders.us.*", "refused: purging every message", "STREAM DELETE ORDERS_EVENTS"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the recording has no %q", want)
		}
	}
}

// TestNATSNoOtherAccount: the session's user lives in its account; another
// account's streams are out of reach.
func TestNATSNoOtherAccount(t *testing.T) {
	l := newNATSLab(t)
	// PAYMENTS gets a stream through its own user.
	ukp, _ := nkeys.CreateUser()
	upub, _ := ukp.PublicKey()
	uc := jwt.NewUserClaims(upub)
	uc.IssuerAccount = l.other
	tok, _ := uc.Encode(l.otherKP)
	pc, err := nats.Connect(l.srv.ClientURL(), nats.UserJWT(func() (string, error) { return tok, nil }, func(n []byte) ([]byte, error) { return ukp.Sign(n) }))
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	pjs, _ := jetstream.New(pc)
	if _, err := pjs.CreateStream(context.Background(), natsStreamConfig(tunnel.NATSStreamSpec{Name: "PAY", Subjects: []string{"pay.>"}, Storage: "memory"})); err != nil {
		t.Fatal(err)
	}
	owner, orec, _ := l.open(t, tunnel.LevelOwner)
	a := natsDo(t, owner, orec, "streams", nil)
	if b, _ := json.Marshal(a.Data); strings.Contains(string(b), `"PAY"`) {
		t.Fatal("another account's stream was listed")
	}
	if a := natsDo(t, owner, orec, "stream", &tunnel.NATSRequest{Stream: "PAY"}); a.Type != "error" {
		t.Fatal("another account's stream was opened")
	}
}

// TestNATSMonitoring: overview, connections and subjects come from the
// server's monitoring port, filtered to the account.
func TestNATSMonitoring(t *testing.T) {
	l := newNATSLab(t)
	owner, orec, _ := l.open(t, tunnel.LevelOwner)
	natsDo(t, owner, orec, "stream_create", &tunnel.NATSRequest{StreamSpec: orders()})
	sub, err := owner.nc.QueueSubscribeSync("orders.*.paid", "invoicer")
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Unsubscribe()
	_ = owner.nc.Flush()

	ov := natsDo(t, owner, orec, "overview", nil)
	o, _ := ov.Data.(NATSOverview)
	if ov.Type != "result" || !o.JetStream || o.Stat == nil || o.Stat.Conns < 1 || o.Info == nil || o.Info.Streams != 1 {
		t.Fatalf("overview: %+v %s", ov.Data, ov.Error)
	}
	cs := natsDo(t, owner, orec, "connections", nil)
	conns, _ := cs.Data.([]NATSConn)
	if cs.Type != "result" || len(conns) < 1 || !conns[0].Argus {
		t.Fatalf("connections: %+v %s", cs.Data, cs.Error)
	}
	sj := natsDo(t, owner, orec, "subjects", nil)
	m, _ := sj.Data.(map[string]any)
	list, _ := m["subjects"].([]NATSSubject)
	var stored bool
	for _, x := range list {
		if x.Subject == "orders.>" && len(x.Streams) == 1 && x.Streams[0] == "ORDERS_EVENTS" {
			stored = true
		}
	}
	if !stored {
		t.Fatalf("subjects: %+v", list)
	}
}

func TestNATSPermissionsShape(t *testing.T) {
	if _, ok := tunnel.NATSPermissions(tunnel.LevelRead, "_INBOX.x"); ok {
		t.Fatal("an inbox outside Argus's prefix was taken")
	}
	for _, lv := range []string{tunnel.LevelRead, tunnel.LevelWrite, tunnel.LevelOwner, tunnel.LevelDBA} {
		p, ok := tunnel.NATSPermissions(lv, tunnel.NATSInboxPrefix+"ab")
		if !ok {
			t.Fatal(lv)
		}
		denied := strings.Join(p.SubDeny, " ")
		if !strings.Contains(denied, "$SYS.>") || !strings.Contains(denied, "_INBOX.>") {
			t.Errorf("%s may subscribe to $SYS or other inboxes", lv)
		}
		if lv != tunnel.LevelRead {
			deny := strings.Join(p.PubDeny, " ")
			for _, must := range []string{"$SYS.>", "_INBOX.>", "$JS.ACK.>", "$JS.API.CONSUMER.MSG.NEXT.>", "$JS.API.ACCOUNT.>"} {
				if !strings.Contains(deny, must) {
					t.Errorf("%s may publish to %s", lv, must)
				}
			}
		}
	}
}
