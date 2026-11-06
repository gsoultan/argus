//go:build !windows

package agent

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
The broker operations the console's broker workspace uses: the overview,
moving messages between queues, declaring an exchange and closing a
connection with a reason. A tiny in-memory broker stands behind the
management API: two queues of messages, and a target that can be told to
refuse what is published to it.
*/

type miniBroker struct {
	fakeRabbit
	queues map[string][]string
	// refuse is a queue that publishes to it do not reach.
	refuse string
	// noMonitoring answers /api/nodes as RabbitMQ does for a user without
	// the monitoring tag.
	noMonitoring bool
	reasons      []string
}

func newMiniBroker() *miniBroker {
	return &miniBroker{fakeRabbit: *newFakeRabbit(),
		queues: map[string][]string{"orders.failed": {"a", "b", "c"}, "orders.created": nil}}
}

func (b *miniBroker) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, _ := r.BasicAuth()
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.users[u] != p || p == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		b.asked = append(b.asked, r.Method+" "+r.URL.EscapedPath())
		path := strings.TrimPrefix(r.URL.EscapedPath(), "/api/")
		var body map[string]any
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			_ = json.Unmarshal(raw, &body)
		}
		switch {
		case path == "overview":
			_, _ = w.Write([]byte(`{"rabbitmq_version":"3.13.7"}`))
		case path == "vhosts/%2F":
			_, _ = w.Write([]byte(`{"name":"/","messages_ready":3,"message_stats":{"publish_details":{"rate":4.5}}}`))
		case path == "nodes":
			if b.noMonitoring {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"not_authorised","reason":"Not monitor user"}`))
				return
			}
			_, _ = w.Write([]byte(`[{"name":"rabbit@mq-01","mem_used":10,"mem_limit":100}]`))
		case strings.HasPrefix(path, "queues/%2F/") && strings.HasSuffix(path, "/get"):
			q := strings.TrimSuffix(strings.TrimPrefix(path, "queues/%2F/"), "/get")
			if len(b.queues[q]) == 0 {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			m := b.queues[q][0]
			b.queues[q] = b.queues[q][1:]
			_ = json.NewEncoder(w).Encode([]map[string]any{{"payload": m, "payload_encoding": "string",
				"properties": map[string]any{"headers": map[string]any{"x-death": "x"}}}})
		case strings.HasPrefix(path, "queues/%2F/") && r.Method == http.MethodGet:
			q := strings.TrimPrefix(path, "queues/%2F/")
			msgs, ok := b.queues[q]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"Object Not Found","reason":"Not Found"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"messages_ready": len(msgs)})
		case path == "exchanges/%2F/amq.default/publish":
			key, _ := body["routing_key"].(string)
			if _, ok := b.queues[key]; !ok || key == b.refuse {
				_, _ = w.Write([]byte(`{"routed":false}`))
				return
			}
			b.queues[key] = append(b.queues[key], body["payload"].(string))
			_, _ = w.Write([]byte(`{"routed":true}`))
		case strings.HasPrefix(path, "connections/"):
			b.reasons = append(b.reasons, r.Header.Get("X-Reason"))
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func brokerSession(t *testing.T, b *miniBroker, level string) *storeHarness {
	t.Helper()
	dir := t.TempDir()
	svc := RabbitMQ(RabbitConfig{StoreConfig: StoreConfig{Verifier: &tunnel.GrantVerifier{Host: "cache-01"},
		RecordDir: dir}, Ctl: b.ctl})
	h, err := openRabbit(t, svc, b.server(t), level, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.close)
	return h
}

func TestRabbitOverviewWithoutMonitoringTag(t *testing.T) {
	b := newMiniBroker()
	b.noMonitoring = true
	h := brokerSession(t, b, tunnel.LevelRead)
	a := h.ask(tunnel.StoreRequest{Op: "overview"})
	if a.Type != "result" {
		t.Fatalf("overview: %+v", a)
	}
	d := a.Data.(map[string]any)
	if d["nodes"] != nil || d["vhost"] == nil {
		t.Fatalf("a read level without node access should still get the vhost: %+v", d)
	}
	if len(h.rec) != 0 && h.recorded("stmt", "OVERVIEW") {
		t.Fatal("browsing the overview was recorded as a statement")
	}
}

func TestRabbitMoveMessages(t *testing.T) {
	b := newMiniBroker()
	read := brokerSession(t, b, tunnel.LevelWrite)
	if a := read.ask(tunnel.StoreRequest{Op: "move", Name: "orders.failed", Target: "orders.created"}); a.Type != "error" {
		t.Fatalf("a write level moved messages: %+v", a)
	}

	h := brokerSession(t, b, tunnel.LevelOwner)
	a := h.ask(tunnel.StoreRequest{Op: "move", Name: "orders.failed", Target: "orders.created", Count: 2})
	if a.Type != "result" {
		t.Fatalf("move: %+v", a)
	}
	b.mu.Lock()
	if got := strings.Join(b.queues["orders.created"], ","); got != "a,b" {
		t.Fatalf("moved %q, want a,b", got)
	}
	if got := strings.Join(b.queues["orders.failed"], ","); got != "c" {
		t.Fatalf("left %q, want c", got)
	}
	b.mu.Unlock()
	if !h.recorded("stmt", "MOVE from orders.failed to orders.created count 2") {
		t.Fatalf("move not recorded: %+v", h.rec)
	}

	// A message the target does not take goes back where it came from.
	b.mu.Lock()
	b.refuse = "orders.created"
	b.mu.Unlock()
	a = h.ask(tunnel.StoreRequest{Op: "move", Name: "orders.failed", Target: "orders.created"})
	if a.Type != "result" {
		t.Fatalf("move to a refusing queue: %+v", a)
	}
	res := a.Data.(map[string]any)
	if res["moved"].(float64) != 0 || !strings.Contains(res["stopped"].(string), "put back") {
		t.Fatalf("move result: %+v", res)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if got := strings.Join(b.queues["orders.failed"], ","); got != "c" {
		t.Fatalf("the refused message was lost: queue holds %q", got)
	}

	if a := h.ask(tunnel.StoreRequest{Op: "move", Name: "orders.failed", Target: "orders.failed"}); a.Type != "error" {
		t.Fatalf("a move onto itself: %+v", a)
	}
}

func TestRabbitMoveToMissingQueue(t *testing.T) {
	b := newMiniBroker()
	h := brokerSession(t, b, tunnel.LevelOwner)
	if a := h.ask(tunnel.StoreRequest{Op: "move", Name: "orders.failed", Target: "nope"}); a.Type != "error" {
		t.Fatalf("move to a queue that does not exist: %+v", a)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.queues["orders.failed"]) != 3 {
		t.Fatal("messages were taken before the target was checked")
	}
}

func TestRabbitDeclareExchangeAndCloseReason(t *testing.T) {
	b := newMiniBroker()
	h := brokerSession(t, b, tunnel.LevelDBA)
	if a := h.ask(tunnel.StoreRequest{Op: "declare_exchange", Name: "x", Kind: "x-delayed"}); a.Type != "error" {
		t.Fatalf("an unknown exchange type: %+v", a)
	}
	if a := h.ask(tunnel.StoreRequest{Op: "declare_exchange", Name: "orders", Kind: "topic", Durable: true}); a.Type != "result" {
		t.Fatalf("declare exchange: %+v", a)
	}
	if a := h.ask(tunnel.StoreRequest{Op: "close_connection", Name: "c1", Reason: "stuck consumer", Confirm: "cache-01"}); a.Type != "result" {
		t.Fatalf("close: %+v", a)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.reasons) != 1 || !strings.HasPrefix(b.reasons[0], "stuck consumer (from Argus by ana.sari@") {
		t.Fatalf("close reason sent: %q", b.reasons)
	}
	if !h.recorded("stmt", "CLOSE_CONNECTION c1 reason \"stuck consumer\"") && !h.recorded("stmt", `CLOSE_CONNECTION c1 reason 'stuck consumer'`) {
		t.Fatalf("close not recorded with its reason: %+v", h.rec)
	}
}
