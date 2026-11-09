//go:build !windows

package agent

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
What ended broker sessions early.

A busy production queue's details -- its consumers, its rates -- came to more
than one tunnel frame, and the frame that could not be written ended the
session: "the connection to the agent ended" for opening a queue. And a
second vhost a person opened at the same level reused the first one's user
without permissions on it, so RabbitMQ refused it and the 401 ended that
session too.
*/

// vhostRabbit is a management API that checks the user's permissions on the
// vhost a request names, as RabbitMQ does, and serves one queue whose details
// are as large as bigQueue says.
type vhostRabbit struct {
	*fakeRabbit
	bigQueue int
}

func (v *vhostRabbit) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, _ := r.BasicAuth()
		v.mu.Lock()
		ok := v.users[u] == p && p != ""
		v.asked = append(v.asked, r.Method+" "+r.URL.EscapedPath())
		perms := v.perms
		v.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"not_authorised","reason":"Login failed"}`))
			return
		}
		path := strings.TrimPrefix(r.URL.EscapedPath(), "/api/")
		switch path {
		case "overview":
			_, _ = w.Write([]byte(`{"rabbitmq_version":"3.13.7"}`))
			return
		case "whoami":
			_, _ = w.Write([]byte(`{"name":"` + u + `"}`))
			return
		}
		// queues/<vhost>/... and the like name the vhost second.
		parts := strings.Split(path, "/")
		if len(parts) >= 2 {
			vhost, _ := url.PathUnescape(parts[1])
			v.mu.Lock()
			_, allowed := perms[u+"@"+vhost]
			v.mu.Unlock()
			if !allowed {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"not_authorised","reason":"Access refused."}`))
				return
			}
		}
		switch {
		case len(parts) == 3 && parts[0] == "queues":
			// One consumer's details are a few hundred bytes; a production
			// queue has hundreds of them.
			_, _ = w.Write([]byte(`{"name":"` + parts[2] + `","messages":1,"consumer_details":[`))
			for i := 0; i < v.bigQueue; i++ {
				if i > 0 {
					_, _ = w.Write([]byte(","))
				}
				_, _ = w.Write([]byte(`{"consumer_tag":"amq.ctag-` + strconv.Itoa(i) + `","channel_details":{"peer_host":"10.0.0.1",` +
					`"connection_name":"10.0.0.1:5000 -> 10.0.0.2:5672","name":"10.0.0.1:5000 -> 10.0.0.2:5672 (1)",` +
					`"user":"orders-service","node":"rabbit@mq-01"},"ack_required":true,"prefetch_count":50,"active":true}`))
			}
			_, _ = w.Write([]byte(`]}`))
		case len(parts) == 4 && parts[0] == "queues" && parts[3] == "bindings":
			_, _ = w.Write([]byte(`[]`))
		case parts[0] == "queues":
			_, _ = w.Write([]byte(`{"items":[],"total_count":0}`))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func openVhost(t *testing.T, svc TunnelService, srv *httptest.Server, vhost, level string, parts bool, dir string) *storeHarness {
	t.Helper()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	p, _ := strconv.Atoi(port)
	h, err := openStoreSession(t, svc, tunnel.RabbitMQService, tunnel.GrantDB{ID: "q-" + vhost, Engine: tunnel.EngineRabbitMQ,
		Name: vhost, Host: "127.0.0.1", Port: p, Level: level, AnswerParts: parts}, dir)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// A queue whose details pass one frame reaches the console whole, and the
// session goes on. Before, the agent could not write the frame and ended
// the session.
func TestRabbitLargeQueueDetailKeepsTheSession(t *testing.T) {
	v := &vhostRabbit{fakeRabbit: newFakeRabbit(), bigQueue: 2000}
	srv := v.server(t)
	dir := t.TempDir()
	svc := RabbitMQ(RabbitConfig{StoreConfig: StoreConfig{Verifier: &tunnel.GrantVerifier{Host: "cache-01"},
		RecordDir: dir}, Ctl: v.ctl})
	h := openVhost(t, svc, srv, "/", tunnel.LevelWrite, true, dir)
	defer h.close()

	a := h.ask(tunnel.StoreRequest{Op: "queue", Name: "orders"})
	if a.Type != "result" {
		t.Fatalf("queue detail: %+v", a)
	}
	q := a.Data.(map[string]any)["queue"].(map[string]any)
	if n := len(q["consumer_details"].([]any)); n != 2000 {
		t.Fatalf("the queue arrived with %d consumers, want 2000", n)
	}
	if a := h.ask(tunnel.StoreRequest{Op: "queues"}); a.Type != "result" {
		t.Fatalf("the session did not go on after a large answer: %+v", a)
	}
}

// A control plane that cannot join parts gets an error for that request,
// not a session that ends.
func TestRabbitLargeAnswerWithoutPartsIsAnError(t *testing.T) {
	v := &vhostRabbit{fakeRabbit: newFakeRabbit(), bigQueue: 2000}
	srv := v.server(t)
	dir := t.TempDir()
	svc := RabbitMQ(RabbitConfig{StoreConfig: StoreConfig{Verifier: &tunnel.GrantVerifier{Host: "cache-01"},
		RecordDir: dir}, Ctl: v.ctl})
	h := openVhost(t, svc, srv, "/", tunnel.LevelWrite, false, dir)
	defer h.close()

	if a := h.ask(tunnel.StoreRequest{Op: "queue", Name: "orders"}); a.Type != "error" ||
		!strings.Contains(a.Error, "too large") {
		t.Fatalf("an answer past a frame without parts: %+v", a)
	}
	if a := h.ask(tunnel.StoreRequest{Op: "queues"}); a.Type != "result" {
		t.Fatalf("the session did not go on: %+v", a)
	}
}

// Two vhosts on one broker at one level share the person's user; each
// session must still find it allowed on its own vhost, and closing one
// takes that vhost's permissions without touching the other's.
func TestRabbitSecondVhostGetsPermissions(t *testing.T) {
	v := &vhostRabbit{fakeRabbit: newFakeRabbit(), bigQueue: 1}
	srv := v.server(t)
	dir := t.TempDir()
	svc := RabbitMQ(RabbitConfig{StoreConfig: StoreConfig{Verifier: &tunnel.GrantVerifier{Host: "cache-01"},
		RecordDir: dir}, Ctl: v.ctl})
	staging := openVhost(t, svc, srv, "staging", tunnel.LevelWrite, true, dir)
	prod := openVhost(t, svc, srv, "production", tunnel.LevelWrite, true, dir)
	if staging.ready.User != prod.ready.User {
		t.Fatalf("one level, one user: %s and %s", staging.ready.User, prod.ready.User)
	}
	user := prod.ready.User

	if a := prod.ask(tunnel.StoreRequest{Op: "queue", Name: "orders"}); a.Type != "result" {
		t.Fatalf("the second vhost was refused: %+v", a)
	}
	if a := staging.ask(tunnel.StoreRequest{Op: "queue", Name: "orders"}); a.Type != "result" {
		t.Fatalf("the first vhost after the second opened: %+v", a)
	}

	staging.close()
	v.mu.Lock()
	_, stagingLeft := v.perms[user+"@staging"]
	_, prodLeft := v.perms[user+"@production"]
	_, userLeft := v.users[user]
	v.mu.Unlock()
	if stagingLeft || !prodLeft || !userLeft {
		t.Fatalf("after staging closed: staging perms %v, production perms %v, user %v", stagingLeft, prodLeft, userLeft)
	}
	if a := prod.ask(tunnel.StoreRequest{Op: "queue", Name: "orders"}); a.Type != "result" {
		t.Fatalf("production after staging closed: %+v", a)
	}
	prod.close()
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, left := v.users[user]; left {
		t.Fatal("the user outlived its last session")
	}
}

// RabbitMQ answers 401 for a vhost the user may not use as well as for a
// user it does not know. Only the second ends the session.
func TestRabbitVhostRefusalDoesNotEndTheSession(t *testing.T) {
	v := &vhostRabbit{fakeRabbit: newFakeRabbit(), bigQueue: 1}
	srv := v.server(t)
	dir := t.TempDir()
	svc := RabbitMQ(RabbitConfig{StoreConfig: StoreConfig{Verifier: &tunnel.GrantVerifier{Host: "cache-01"},
		RecordDir: dir}, Ctl: v.ctl})
	h := openVhost(t, svc, srv, "production", tunnel.LevelRead, true, dir)
	defer h.close()
	user := h.ready.User

	// An administrator takes the user's permissions away mid-session.
	v.mu.Lock()
	delete(v.perms, user+"@production")
	v.mu.Unlock()
	if a := h.ask(tunnel.StoreRequest{Op: "queue", Name: "orders"}); a.Type != "error" ||
		!strings.Contains(a.Error, "refused") {
		t.Fatalf("a vhost refusal: %+v", a)
	}
	if a := h.ask(tunnel.StoreRequest{Op: "overview"}); a.Type == "closed" {
		t.Fatalf("the session ended over a vhost refusal: %+v", a)
	}
}
