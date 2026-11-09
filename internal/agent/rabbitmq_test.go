//go:build !windows

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
RabbitMQ against a stand-in: rabbitmqctl is a function keeping users in a
map, and the management API is an httptest server that checks those users'
passwords and records what it was asked. What is under test is what the agent
does with them -- which user it makes with which permissions, how the password
travels, which operations a level may send at all, and that the user goes
when the session does.
*/

type fakeRabbit struct {
	mu    sync.Mutex
	users map[string]string // name -> password
	tags  map[string]string
	perms map[string][3]string
	calls [][]string
	// argv holds every argument rabbitmqctl was given, to check no
	// password is ever on a command line.
	argv  []string
	asked []string // management API requests: METHOD path
}

func newFakeRabbit() *fakeRabbit {
	return &fakeRabbit{users: map[string]string{"guest": "guest", "argus-ghost-read": "x"},
		tags: map[string]string{}, perms: map[string][3]string{}}
}

func (f *fakeRabbit) ctl(_ context.Context, args []string, stdin string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	f.argv = append(f.argv, args...)
	switch args[0] {
	case "list_users":
		var out []map[string]any
		for u := range f.users {
			out = append(out, map[string]any{"user": u, "tags": []string{}})
		}
		return json.Marshal(out)
	case "add_user":
		if _, ok := f.users[args[1]]; ok {
			return nil, errors.New("user already exists")
		}
		f.users[args[1]] = strings.TrimSuffix(stdin, "\n")
	case "change_password":
		f.users[args[1]] = strings.TrimSuffix(stdin, "\n")
	case "set_user_tags":
		f.tags[args[1]] = args[2]
	case "set_permissions":
		// -p vhost user conf write read
		f.perms[args[3]+"@"+args[2]] = [3]string{args[4], args[5], args[6]}
	case "clear_permissions":
		// -p vhost user
		delete(f.perms, args[3]+"@"+args[2])
	case "delete_user":
		delete(f.users, args[1])
		for k := range f.perms {
			if strings.HasPrefix(k, args[1]+"@") {
				delete(f.perms, k)
			}
		}
	}
	return nil, nil
}

func (f *fakeRabbit) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, _ := r.BasicAuth()
		f.mu.Lock()
		ok := f.users[u] == p && p != ""
		f.asked = append(f.asked, r.Method+" "+r.URL.EscapedPath())
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"not_authorised","reason":"Login failed"}`))
			return
		}
		switch {
		case r.URL.Path == "/api/overview":
			_, _ = w.Write([]byte(`{"rabbitmq_version":"3.13.7"}`))
		case strings.HasPrefix(r.URL.EscapedPath(), "/api/queues/%2F") && r.Method == http.MethodGet &&
			!strings.Contains(r.URL.Path, "orders"):
			_, _ = w.Write([]byte(`{"items":[{"name":"orders","messages":3,"messages_ready":3,"consumers":0}],"total_count":1}`))
		case strings.HasSuffix(r.URL.Path, "/get"):
			_, _ = w.Write([]byte(`[{"payload":"{\"id\":1}","redelivered":false,"routing_key":"orders","message_count":2}]`))
		case strings.HasSuffix(r.URL.Path, "/publish"):
			_, _ = w.Write([]byte(`{"routed":true}`))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func openRabbit(t *testing.T, svc TunnelService, srv *httptest.Server, level, dir string) (*storeHarness, error) {
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	p, _ := strconv.Atoi(port)
	return openStoreSession(t, svc, tunnel.RabbitMQService, tunnel.GrantDB{ID: "q1", Engine: tunnel.EngineRabbitMQ,
		Name: "/", Host: "127.0.0.1", Port: p, Level: level}, dir)
}

func TestRabbitLevelsAndUsers(t *testing.T) {
	f := newFakeRabbit()
	srv := f.server(t)
	dir := t.TempDir()
	svc := RabbitMQ(RabbitConfig{StoreConfig: StoreConfig{Verifier: &tunnel.GrantVerifier{Host: "cache-01"},
		RecordDir: dir}, Ctl: f.ctl})

	read, err := openRabbit(t, svc, srv, tunnel.LevelRead, dir)
	if err != nil {
		t.Fatal(err)
	}
	if read.ready.Version != "3.13.7" || read.ready.User != "argus-ana_sari-read" {
		t.Fatalf("ready: %+v", read.ready)
	}
	f.mu.Lock()
	if _, left := f.users["argus-ghost-read"]; left {
		t.Fatal("a user an earlier agent left behind was not swept")
	}
	if _, kept := f.users["guest"]; !kept {
		t.Fatal("the sweep removed a user Argus did not make")
	}
	if got := f.perms["argus-ana_sari-read@/"]; got != [3]string{"^$", "^$", ".*"} || f.tags["argus-ana_sari-read"] != "management" {
		t.Fatalf("read permissions: %v tag %q", got, f.tags["argus-ana_sari-read"])
	}
	pw := f.users["argus-ana_sari-read"]
	for _, a := range f.argv {
		if a == pw {
			t.Fatal("the session's password was on rabbitmqctl's command line")
		}
	}
	f.mu.Unlock()

	if a := read.ask(tunnel.StoreRequest{Op: "queues"}); a.Type != "result" || a.Data == nil {
		t.Fatalf("queues: %+v", a)
	}
	if a := read.ask(tunnel.StoreRequest{Op: "peek", Name: "orders", Count: 5}); a.Type != "result" {
		t.Fatalf("peek: %+v", a)
	}
	if !read.recorded("stmt", "PEEK queue orders count 5") {
		t.Fatalf("peek not recorded: %+v", read.rec)
	}
	// RabbitMQ would let a reader purge; the agent does not offer it.
	before := len(f.asked)
	if a := read.ask(tunnel.StoreRequest{Op: "purge", Name: "orders", Confirm: "cache-01"}); a.Type != "error" {
		t.Fatalf("purge at read: %+v", a)
	}
	if a := read.ask(tunnel.StoreRequest{Op: "publish", Exchange: "", RoutingKey: "orders", Payload: "x"}); a.Type != "error" {
		t.Fatalf("publish at read: %+v", a)
	}
	if len(f.asked) != before {
		t.Fatalf("a refused operation reached RabbitMQ: %v", f.asked[before:])
	}

	owner, err := openRabbit(t, svc, srv, tunnel.LevelOwner, dir)
	if err != nil {
		t.Fatal(err)
	}
	if a := owner.ask(tunnel.StoreRequest{Op: "purge", Name: "orders"}); !a.NeedsConfirm {
		t.Fatalf("purge without confirm: %+v", a)
	}
	if a := owner.ask(tunnel.StoreRequest{Op: "purge", Name: "orders", Confirm: "cache-01"}); a.Type != "result" {
		t.Fatalf("confirmed purge: %+v", a)
	}
	if a := owner.ask(tunnel.StoreRequest{Op: "publish", RoutingKey: "orders", Payload: `{"id":2}`}); a.Type != "result" {
		t.Fatalf("publish at owner: %+v", a)
	}
	purged := false
	for _, q := range f.asked {
		if q == "DELETE /api/queues/%2F/orders/contents" {
			purged = true
		}
	}
	if !purged {
		t.Fatalf("purge went somewhere else: %v", f.asked)
	}
	if a := owner.ask(tunnel.StoreRequest{Op: "connections"}); a.Type != "error" {
		t.Fatalf("connections at owner: %+v", a)
	}

	read.close()
	owner.close()
	f.mu.Lock()
	defer f.mu.Unlock()
	for u := range f.users {
		if tunnel.IsStoreUser(u) {
			t.Fatalf("user %s is still there after its session closed", u)
		}
	}
}

func TestRabbitRefusesARemoteAPI(t *testing.T) {
	f := newFakeRabbit()
	dir := t.TempDir()
	svc := RabbitMQ(RabbitConfig{StoreConfig: StoreConfig{Verifier: &tunnel.GrantVerifier{Host: "cache-01"},
		RecordDir: dir}, Ctl: f.ctl})
	_, err := openStoreSession(t, svc, tunnel.RabbitMQService, tunnel.GrantDB{ID: "q1", Engine: tunnel.EngineRabbitMQ,
		Name: "/", Host: "mq.internal", Port: 15672, Level: tunnel.LevelRead}, dir)
	if err == nil || !strings.Contains(err.Error(), "on this server") {
		t.Fatalf("a remote management API was not refused: %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("rabbitmqctl ran for a refused grant: %v", f.calls)
	}
}

// Against a real RabbitMQ when ARGUS_TEST_RABBITMQ_PORT names its management
// port on 127.0.0.1 and rabbitmqctl on this machine can manage it.
func TestRabbitAgainstRealBroker(t *testing.T) {
	port, _ := strconv.Atoi(os.Getenv("ARGUS_TEST_RABBITMQ_PORT"))
	if port == 0 {
		t.Skip("ARGUS_TEST_RABBITMQ_PORT is not set")
	}
	dir := t.TempDir()
	svc := RabbitMQ(RabbitConfig{StoreConfig: StoreConfig{Verifier: &tunnel.GrantVerifier{Host: "cache-01"}, RecordDir: dir}})
	open := func(level string) *storeHarness {
		h, err := openStoreSession(t, svc, tunnel.RabbitMQService, tunnel.GrantDB{ID: "q1", Engine: tunnel.EngineRabbitMQ,
			Name: "/", Host: "127.0.0.1", Port: port, Level: level}, dir)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	owner := open(tunnel.LevelOwner)
	q := "argus-test-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if a := owner.ask(tunnel.StoreRequest{Op: "declare_queue", Name: q}); a.Type != "result" {
		t.Fatalf("declare: %+v", a)
	}
	if a := owner.ask(tunnel.StoreRequest{Op: "publish", RoutingKey: q, Payload: `{"id":1}`}); a.Type != "result" {
		t.Fatalf("publish: %+v", a)
	}
	read := open(tunnel.LevelRead)
	if a := read.ask(tunnel.StoreRequest{Op: "peek", Name: q, Count: 5}); a.Type != "result" {
		t.Fatalf("peek: %+v", a)
	} else if msgs, _ := a.Data.([]any); len(msgs) != 1 {
		t.Fatalf("peek found %d messages", len(msgs))
	}
	// Peek put it back.
	if a := read.ask(tunnel.StoreRequest{Op: "peek", Name: q, Count: 5}); len(a.Data.([]any)) != 1 {
		t.Fatalf("peek took the message: %+v", a)
	}
	if a := read.ask(tunnel.StoreRequest{Op: "purge", Name: q, Confirm: "cache-01"}); a.Type != "error" {
		t.Fatalf("purge at read: %+v", a)
	}
	if a := owner.ask(tunnel.StoreRequest{Op: "purge", Name: q, Confirm: "cache-01"}); a.Type != "result" {
		t.Fatalf("purge at owner: %+v", a)
	}
	if a := owner.ask(tunnel.StoreRequest{Op: "delete_queue", Name: q, Confirm: "cache-01"}); a.Type != "result" {
		t.Fatalf("delete: %+v", a)
	}
	read.close()
	owner.close()
	out, err := runRabbitmqctl(context.Background(), []string{"list_users", "--silent", "--formatter", "json"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), tunnel.StoreUserPrefix) {
		t.Fatalf("a user outlived its session: %s", out)
	}
}
