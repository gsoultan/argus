package control

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/gsoultan/argus/internal/agent"
	"github.com/gsoultan/argus/internal/tunnel"
)

/*
A RabbitMQ session end to end -- ticket, relay, agent -- against a stand-in
management API, for the answer that ended broker sessions: a queue whose
details are larger than one tunnel frame. The relay must join the agent's
parts into one answer for the browser, and the session must go on after it.
*/

func TestBrokerLargeAnswerThroughTheTunnel(t *testing.T) {
	var mu sync.Mutex
	users := map[string]string{}
	ctl := func(_ context.Context, args []string, stdin string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		switch args[0] {
		case "list_users":
			return []byte("[]"), nil
		case "add_user", "change_password":
			users[args[1]] = strings.TrimSuffix(stdin, "\n")
		case "delete_user":
			delete(users, args[1])
		}
		return nil, nil
	}
	consumers := make([]map[string]any, 3000)
	for i := range consumers {
		consumers[i] = map[string]any{"consumer_tag": "amq.ctag-" + strconv.Itoa(i), "prefetch_count": 50,
			"channel_details": map[string]any{"peer_host": "10.0.0.1", "connection_name": "10.0.0.1:5000 -> 10.0.0.2:5672",
				"user": "orders-service", "node": "rabbit@mq-01"}}
	}
	big, _ := json.Marshal(map[string]any{"name": "orders", "messages": 1, "consumer_details": consumers})
	if len(big) <= tunnel.MaxFrame {
		t.Fatalf("the queue is %d bytes, not past a frame", len(big))
	}
	mgmt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, _ := r.BasicAuth()
		mu.Lock()
		ok := users[u] == p && p != ""
		mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch path := r.URL.EscapedPath(); {
		case path == "/api/overview":
			_, _ = w.Write([]byte(`{"rabbitmq_version":"3.13.7"}`))
		case path == "/api/queues/production/orders":
			_, _ = w.Write(big)
		case strings.HasSuffix(path, "/bindings"):
			_, _ = w.Write([]byte(`[]`))
		default:
			_, _ = w.Write([]byte(`{"items":[]}`))
		}
	}))
	t.Cleanup(mgmt.Close)
	_, portS, _ := net.SplitHostPort(strings.TrimPrefix(mgmt.URL, "http://"))
	port, _ := strconv.Atoi(portS)

	e := newTunnelEnv(t)
	e.withTerminal(t)
	host := unique("mq-host")
	cred, _ := enrolled(t, e.store, e.h, host)
	ctx := context.Background()
	asset, err := e.store.CreateAsset(ctx, AssetInput{Hostname: host, Address: "192.0.2.21", Principals: []string{"ops"}})
	if err != nil {
		t.Fatal(err)
	}
	admin := e.api.UserTokens["admin-token"]
	db, err := e.store.CreateDatabase(ctx, Database{AssetID: asset.ID, Engine: tunnel.EngineRabbitMQ, Host: "127.0.0.1",
		Port: port, Name: "production", Label: "production"}, admin)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	services := agent.DefaultTunnelServices()
	services[agent.RabbitMQService] = agent.RabbitMQ(agent.RabbitConfig{StoreConfig: agent.StoreConfig{
		Verifier: &tunnel.GrantVerifier{Host: host}, RecordDir: t.TempDir(), Log: log}, Ctl: ctl})
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
	c.SetReadLimit(64 << 20)
	t.Cleanup(func() { c.CloseNow() })
	r := &redisSession{e: e, db: db, host: host, c: c, ctx: wctx}
	if m := r.read(t); m.Type != "ready" {
		t.Fatalf("not ready: %+v", m)
	}
	// The agent's own ready answer.
	if m := r.read(t); m.Type != "ans" {
		t.Fatalf("no ready answer: %+v", m)
	}

	a := r.send(t, tunnel.StoreRequest{ID: 1, Op: "queue", Name: "orders"})
	if a.Type != "result" {
		t.Fatalf("queue detail: %+v", a)
	}
	q := a.Data.(map[string]any)["queue"].(map[string]any)
	if n := len(q["consumer_details"].([]any)); n != len(consumers) {
		t.Fatalf("the queue arrived with %d consumers, want %d", n, len(consumers))
	}
	if a := r.send(t, tunnel.StoreRequest{ID: 2, Op: "queues"}); a.Type != "result" {
		t.Fatalf("the session did not go on after a large answer: %+v", a)
	}
}

// The bell and the audit describe a broker request as one, in the broker's
// level names, not as "dba access to a database".
func TestBrokerRequestIsDescribedAsABroker(t *testing.T) {
	got := describeAsk(AccessRequest{Principal: "db:dba", Broker: true, AssetHostnames: []string{"mq-01"}})
	if got != "admin access to a broker on mq-01" {
		t.Fatalf("a broker request reads %q", got)
	}
	if got := describeAsk(AccessRequest{Principal: "db:dba", AssetHostnames: []string{"pg-01"}}); got != "dba access to a database on pg-01" {
		t.Fatalf("a database request reads %q", got)
	}
}
