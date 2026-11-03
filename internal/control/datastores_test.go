package control

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/gsoultan/argus/internal/agent"
	"github.com/gsoultan/argus/internal/auth"
	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Redis and RabbitMQ entries: what the control plane will save, and a Redis
session end to end -- ticket, relay, agent, a real redis-server -- when
redis-server is on PATH.
*/

func TestDatastoreEntries(t *testing.T) {
	_, s, h, pg, admin := dbFixture(t)
	ctx := context.Background()

	r, err := s.CreateDatabase(ctx, Database{AssetID: pg.AssetID, Engine: tunnel.EngineRedis}, admin)
	if err != nil {
		t.Fatalf("redis: %v", err)
	}
	if r.Name != "0" || r.Host != "127.0.0.1" || r.Port != 6379 || r.ConfigPath != "/etc/redis/redis.conf" ||
		r.Label != "production" || r.PoolerPort != 0 || r.AdminUser != "" {
		t.Fatalf("redis defaults: %+v", r)
	}
	// One Redis server is one entry: its users are not per logical db.
	if _, err := s.CreateDatabase(ctx, Database{AssetID: pg.AssetID, Engine: tunnel.EngineRedis, Name: "3"}, admin); err == nil ||
		!strings.Contains(err.Error(), "already has its Redis") {
		t.Fatalf("a second entry for one Redis: %v", err)
	}
	q, err := s.CreateDatabase(ctx, Database{AssetID: pg.AssetID, Engine: tunnel.EngineRabbitMQ}, admin)
	if err != nil {
		t.Fatalf("rabbitmq: %v", err)
	}
	if q.Name != "/" || q.Host != "127.0.0.1" || q.Port != 15672 || q.ConfigPath != "" {
		t.Fatalf("rabbitmq defaults: %+v", q)
	}
	for _, bad := range []Database{
		{Engine: tunnel.EngineRedis, Host: "10.0.0.5", Port: 6380},
		{Engine: tunnel.EngineRedis, Name: "orders", Port: 6381},
		{Engine: tunnel.EngineRedis, Port: 6382, ConfigPath: "/etc/../etc/shadow"},
		{Engine: tunnel.EngineRedis, Port: 6383, ConfigPath: "/etc/shadow"},
		{Engine: tunnel.EngineRabbitMQ, Host: "/var/run/rabbit.sock", Port: 15673},
		{Engine: tunnel.EngineRabbitMQ, Host: "mq.internal", Port: 15674},
		{Engine: tunnel.EngineRabbitMQ, ConfigPath: "/etc/redis/redis.conf", Port: 15675},
		{Engine: "mongodb"},
		{Engine: tunnel.EnginePostgres, Name: "x", ConfigPath: "/etc/redis/redis.conf"},
	} {
		bad.AssetID = pg.AssetID
		if _, err := s.CreateDatabase(ctx, bad, admin); err == nil {
			t.Errorf("saved %+v", bad)
		} else if !errorsAs(err, new(ErrInvalidDatabase)) {
			t.Errorf("%+v: not an invalid-entry error: %v", bad, err)
		}
	}
	// PgBouncer is Postgres's, and the console never shows it for a Redis.
	if w := do(t, h, http.MethodGet, "/api/v1/databases/"+r.ID+"/pooler-setup", "admin-token", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("pooler setup for redis: %d %s", w.Code, w.Body)
	}
	// The same access rules apply: a level, a ticket at it.
	ana, _ := newAccount(t, s, "operator")
	if err := s.SetDBAssignment(ctx, DBAssignment{DatabaseID: q.ID, UserEmail: &ana, Level: tunnel.LevelWrite,
		NeedsApproval: true}, admin); err != nil {
		t.Fatal(err)
	}
	if acc, err := s.DatabaseAccess(ctx, ana, q.ID); err != nil || acc.Level != "" || acc.Approvable != tunnel.LevelWrite {
		t.Fatalf("write needing approval on a vhost: %+v %v", acc, err)
	}
}

func startTestRedis(t *testing.T) (sock, conf string) {
	t.Helper()
	bin, err := exec.LookPath("redis-server")
	if err != nil {
		t.Skip("redis-server is not on PATH")
	}
	dir, err := os.MkdirTemp("", "argus-redis-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock, conf = filepath.Join(dir, "redis.sock"), filepath.Join(dir, "redis.conf")
	if err := os.WriteFile(conf, []byte("port 0\nunixsocket "+sock+"\nsave \"\"\nrequirepass adminpw\ndir "+dir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, conf)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	for i := 0; i < 100; i++ {
		if c, err := net.Dial("unix", sock); err == nil {
			_ = c.Close()
			return sock, conf
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("redis-server did not start")
	return "", ""
}

// redisSession is a Redis session opened through the console's route, with
// a real agent and redis-server behind it.
type redisSession struct {
	e      *tunnelEnv
	signer *auth.Signer
	db     Database
	host   string
	id     string
	c      *websocket.Conn
	ctx    context.Context
}

func openRedisSession(t *testing.T) *redisSession {
	t.Helper()
	sock, conf := startTestRedis(t)
	e := newTunnelEnv(t)
	signer := e.withTerminal(t)
	host := unique("redis-host")
	cred, _ := enrolled(t, e.store, e.h, host)
	ctx := context.Background()
	asset, err := e.store.CreateAsset(ctx, AssetInput{Hostname: host, Address: "192.0.2.20", Principals: []string{"ops"}})
	if err != nil {
		t.Fatal(err)
	}
	admin := e.api.UserTokens["admin-token"]
	db, err := e.store.CreateDatabase(ctx, Database{AssetID: asset.ID, Engine: tunnel.EngineRedis, Host: sock,
		Port: 6390, ConfigPath: conf, Label: "development"}, admin)
	if err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	services := agent.DefaultTunnelServices()
	services[agent.RedisService] = agent.Redis(agent.StoreConfig{Verifier: &tunnel.GrantVerifier{Host: host},
		RecordDir: t.TempDir(), Log: log})
	actx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go agent.RunTunnel(actx, agent.TunnelConfig{Dial: e.dial(cred, host), Services: services, Log: log})
	e.waitTunnel(t, host, "")

	w := do(t, e.h, http.MethodPost, "/api/v1/databases/"+db.ID+"/ticket", "admin-token", `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("ticket: %d %s", w.Code, w.Body)
	}
	var tk struct{ Ticket, Level, Role string }
	_ = json.Unmarshal(w.Body.Bytes(), &tk)

	u, _ := url.Parse(e.srv.URL)
	u.Scheme, u.Path = "wss", "/api/v1/postgres"
	u.RawQuery = url.Values{"database": {db.ID}, "principal": {tk.Role + "@" + tk.Level}, "ticket": {tk.Ticket}}.Encode()
	wctx, wcancel := context.WithTimeout(ctx, 20*time.Second)
	t.Cleanup(wcancel)
	c, _, err := websocket.Dial(wctx, u.String(), &websocket.DialOptions{HTTPClient: e.srv.Client(),
		HTTPHeader: http.Header{"Origin": {e.srv.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	r := &redisSession{e: e, signer: signer, db: db, host: host, c: c, ctx: wctx}
	if m := r.read(t); m.Type != "ready" {
		t.Fatalf("not ready: %+v", m)
	} else {
		r.id = m.Data
	}
	return r
}

func (r *redisSession) read(t *testing.T) pgBrowserMessage {
	t.Helper()
	_, p, err := r.c.Read(r.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var m pgBrowserMessage
	_ = json.Unmarshal(p, &m)
	return m
}

func (r *redisSession) send(t *testing.T, req tunnel.StoreRequest) tunnel.StoreAnswer {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"req": req})
	if err := r.c.Write(r.ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
	var a tunnel.StoreAnswer
	_ = json.Unmarshal(r.read(t).Ans, &a)
	return a
}

func TestRedisSessionThroughTheTunnel(t *testing.T) {
	r := openRedisSession(t)
	e, db, host, c, ctx := r.e, r.db, r.host, r.c, context.Background()
	var ready tunnel.StoreAnswer
	if m := r.read(t); json.Unmarshal(m.Ans, &ready) != nil || ready.Type != "ready" || ready.Engine != "redis" || ready.Level != "dba" {
		t.Fatalf("ready answer: %+v", ready)
	}
	send := func(req tunnel.StoreRequest) tunnel.StoreAnswer {
		t.Helper()
		return r.send(t, req)
	}
	if a := send(tunnel.StoreRequest{ID: 1, Op: "run", Command: `SET greeting "hello world"`}); a.Value == nil || a.Value.V != "OK" {
		t.Fatalf("SET: %+v", a)
	}
	if a := send(tunnel.StoreRequest{ID: 2, Op: "run", Command: "FLUSHALL"}); !a.NeedsConfirm {
		t.Fatalf("FLUSHALL went through without a confirm: %+v", a)
	}
	if a := send(tunnel.StoreRequest{ID: 3, Op: "run", Command: "GET greeting"}); a.Value == nil || a.Value.V != "hello world" {
		t.Fatalf("GET: %+v", a)
	}
	_ = c.Close(websocket.StatusNormalClosure, "")

	// The session is a database session like any other, named for its engine.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var proto, state, kind string
		var n *int
		err := e.store.pool.QueryRow(ctx, `SELECT protocol, state, kind, command_count FROM sessions
			WHERE database_id = $1::uuid`, db.ID).Scan(&proto, &state, &kind, &n)
		if err == nil && state == "ended" {
			if proto != "redis" || kind != "database" || n == nil || *n != 2 {
				t.Fatalf("session row: %s %s %s %v", proto, state, kind, n)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the session never ended: %v %s", err, state)
		}
		time.Sleep(50 * time.Millisecond)
	}
	events, err := e.store.AuditEvents(ctx, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	var started bool
	for _, ev := range events {
		if ev.Action == "db.session_started" && ev.Target == host {
			started = true
		}
	}
	if !started {
		t.Fatal("the session is not in the audit log")
	}
	_ = fmt.Sprint
}

// A database session is watched as its recording: each command as it is
// recorded, and what it answered -- never the values it returned, which are
// not recorded. It used to be watched as a terminal, which drew the result
// bytes as text and no command at all.
func TestADatabaseSessionIsWatchedAsItsStatements(t *testing.T) {
	r := openRedisSession(t)
	r.read(t) // the engine's ready
	if a := r.send(t, tunnel.StoreRequest{ID: 1, Op: "run", Command: `SET secret "s3cr3t-value"`}); a.Value == nil {
		t.Fatalf("SET: %+v", a)
	}

	// Its recording is not missing while it runs, and is not called missing.
	w := do(t, r.e.h, http.MethodGet, "/api/v1/sessions/"+r.id+"/recording", "admin-token", "")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "still running") {
		t.Errorf("recording of a running session: %d %s", w.Code, w.Body)
	}

	ticket, _ := r.signer.IssueSessionScopedTicket("aud@example.com", auth.ScopeShadow, r.id, time.Minute)
	v, _, err := r.e.openWatch(t, r.id, ticket)
	if err != nil {
		t.Fatal(err)
	}
	defer v.CloseNow()
	vev := pump(v)

	if a := r.send(t, tunnel.StoreRequest{ID: 2, Op: "run", Command: "GET secret"}); a.Value == nil || a.Value.V != "s3cr3t-value" {
		t.Fatalf("GET: %+v", a)
	}
	// Too long to record: refused in words, and the session goes on.
	long := "SET big " + strings.Repeat("x", tunnel.MaxCommand)
	if a := r.send(t, tunnel.StoreRequest{ID: 3, Op: "run", Command: long}); a.Type != "error" ||
		!strings.Contains(a.Error, "too long to be recorded") {
		t.Fatalf("a command too long to record: %+v", a)
	}
	if a := r.send(t, tunnel.StoreRequest{ID: 4, Op: "run", Command: "PING"}); a.Value == nil || a.Value.V != "PONG" {
		t.Fatalf("the session did not go on: %+v", a)
	}

	var lines []tunnel.PGRecord
	var binary [][]byte
	deadline := time.After(10 * time.Second)
	for done := false; !done; {
		select {
		case ev, ok := <-vev:
			if !ok {
				t.Fatal("the view closed")
			}
			if ev.binary != nil {
				binary = append(binary, ev.binary)
				continue
			}
			if ev.msg.Type == "resize" {
				t.Errorf("a database view was sent a terminal size")
			}
			if ev.msg.Type != "record" {
				continue
			}
			var rec tunnel.PGRecord
			if err := json.Unmarshal([]byte(ev.msg.Data), &rec); err != nil {
				t.Fatalf("not a recording line: %q", ev.msg.Data)
			}
			lines = append(lines, rec)
			done = rec.Kind == "done" && rec.SQL == "" && rec.Request == 4
		case <-deadline:
			t.Fatalf("the viewer did not see the session: %+v", lines)
		}
	}
	var sqls []string
	for _, l := range lines {
		if l.Kind == "stmt" {
			sqls = append(sqls, l.SQL)
		}
		if l.Hash == "" {
			t.Errorf("a line was sent without its hash: %+v", l)
		}
	}
	// The command from before the viewer arrived comes first, from the
	// backlog; the refused one was never recorded, so it is not there.
	want := []string{"SET secret s3cr3t-value", "GET secret", "PING"}
	if strings.Join(sqls, "|") != strings.Join(want, "|") {
		t.Errorf("statements seen: %q, want %q", sqls, want)
	}
	if len(binary) > 0 {
		t.Errorf("the viewer was sent %d binary frames; results are never shown to a viewer", len(binary))
	}
	for _, l := range lines {
		if strings.Contains(l.SQL+l.Error+l.Tag, "s3cr3t-value") && l.Kind != "stmt" {
			t.Errorf("a value reached the viewer: %+v", l)
		}
	}
}
