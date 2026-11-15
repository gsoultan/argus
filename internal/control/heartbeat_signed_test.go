package control

import (
	"crypto/ed25519"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gsoultan/argus/internal/agent"
	"github.com/gsoultan/argus/internal/agent/reporter"
	"github.com/gsoultan/argus/internal/tunnel"
)

// The two halves agree on the bytes: what writeHeartbeatReply signs is what
// the agent's reporter reads, so a real reply passes the agent's gate, and the
// same body changed by one field on the way does not.
func TestASignedHeartbeatReplyPassesTheAgentsGate(t *testing.T) {
	key, err := tunnel.GrantKeyFromSecret("a-test-signing-secret-that-is-long-enough-0123")
	if err != nil {
		t.Fatal(err)
	}
	a := &API{Tunnels: &TunnelHub{GrantKey: key}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	tamper := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply := map[string]any{"status": "ok", "personal_accounts": []map[string]any{
			{"name": "ana", "owner": "ana@example.com", "sudo": true}}}
		if !tamper {
			a.writeHeartbeatReply(w, r, "web-01", reply)
			return
		}
		// A party on the link: the control plane's signature, a body of
		// its own.
		rec := httptest.NewRecorder()
		a.writeHeartbeatReply(rec, r, "web-01", reply)
		w.Header().Set(tunnel.OrdersHeader, rec.Header().Get(tunnel.OrdersHeader))
		reply["personal_accounts"] = []map[string]any{{"name": "eve", "owner": "eve", "sudo": true}}
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	if _, err := agent.NewGrantPin(dir).Check(key.Public().(ed25519.PublicKey)); err != nil {
		t.Fatal(err)
	}
	gate := &agent.OrderGate{StateDir: dir, Host: "web-01"}
	rep := reporter.New(srv.URL, "argus-agent-test", "", nil)

	reply, ok := rep.HeartbeatWithReply(t.Context(), map[string]any{"hostname": "web-01"})
	if !ok || reply.Signature == "" {
		t.Fatalf("no signed reply: ok=%v signature=%q", ok, reply.Signature)
	}
	if take, err := gate.Accept(reply.Signature, reply.Body); !take {
		t.Fatalf("the agent refused the control plane's own reply: %v", err)
	}
	// What an agent older than this reads: the same fields, header unseen.
	if len(reply.PersonalAccounts) == 0 || reply.Status != "ok" {
		t.Errorf("the reply's fields did not decode as before: %+v", reply)
	}

	tamper = true
	reply, ok = rep.HeartbeatWithReply(t.Context(), map[string]any{"hostname": "web-01"})
	if !ok {
		t.Fatal("no reply")
	}
	if take, _ := gate.Accept(reply.Signature, reply.Body); take {
		t.Fatal("a body changed on the way was acted on")
	}
}

// With no signing_secret there is no key: the reply goes out as it always
// did, unsigned, rather than failing.
func TestAHeartbeatReplyWithoutAKeyIsUnsigned(t *testing.T) {
	a := &API{Tunnels: &TunnelHub{}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	rec := httptest.NewRecorder()
	a.writeHeartbeatReply(rec, httptest.NewRequest(http.MethodPost, "/", nil), "web-01",
		map[string]any{"status": "ok"})
	if rec.Code != http.StatusOK || rec.Header().Get(tunnel.OrdersHeader) != "" {
		t.Fatalf("code %d, signature %q", rec.Code, rec.Header().Get(tunnel.OrdersHeader))
	}
}
