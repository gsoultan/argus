package control

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

/*
The heartbeat has to accept what the agent actually sends.

decode() rejects unknown fields on purpose, so a field the agent adds and this
struct does not know about does not lose that field -- it loses the whole
message. The agent has always sent exec_tracing and exec_reason; the handler
never accepted them; every heartbeat from every agent came back 400 for a week
before anyone looked, because the agent spools and retries and the only symptom
is agents quietly going stale.

Everything a heartbeat carries went with it: active session counts, posture,
drift, and the probe state that "require eBPF for root" needs to be a control
rather than a switch.
*/

// postHeartbeatJSON drives the handler the way a reporter does.
func postHeartbeatJSON(t *testing.T, a *API, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/report/heartbeat",
		bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	a.postHeartbeat(w, r)
	return w
}

// Exactly what cmd/argus-agent sends.
func TestHeartbeatAcceptsWhatTheAgentSends(t *testing.T) {
	s := testStore(t)
	a := NewAPI(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	host := unique("agent-host")

	w := postHeartbeatJSON(t, a, `{
		"hostname": "`+host+`",
		"version": "dev",
		"active_sessions": 2,
		"posture": null,
		"exec_tracing": true,
		"exec_reason": ""
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 -- body: %s", w.Code, w.Body.String())
	}

	tracing, why, err := s.ExecTracingFor(context.Background(), host)
	if err != nil {
		t.Fatalf("ExecTracingFor: %v", err)
	}
	if !tracing {
		t.Errorf("exec tracing was not stored (reason %q)", why)
	}
}

// And it keeps the reason when the probe is not loaded, because that is what an
// operator needs in order to fix it.
func TestHeartbeatKeepsWhyTheProbeIsMissing(t *testing.T) {
	s := testStore(t)
	a := NewAPI(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	host := unique("agent-noprobe")
	const why = "kernel has no BTF; CONFIG_DEBUG_INFO_BTF is unset"

	body, _ := json.Marshal(map[string]any{
		"hostname": host, "version": "dev", "active_sessions": 0,
		"posture": nil, "exec_tracing": false, "exec_reason": why,
	})
	if w := postHeartbeatJSON(t, a, string(body)); w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}

	tracing, got, err := s.ExecTracingFor(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	if tracing {
		t.Error("a host with no probe reported tracing")
	}
	if got != why {
		t.Errorf("reason = %q, want %q", got, why)
	}
}

// A host nobody has reported from is not silently "fine".
func TestExecTracingForAnUnknownHost(t *testing.T) {
	s := testStore(t)
	tracing, why, err := s.ExecTracingFor(context.Background(), unique("never-seen"))
	if err != nil {
		t.Fatal(err)
	}
	if tracing {
		t.Error("an unknown host reported kernel tracing")
	}
	if why == "" {
		t.Error("no reason given for an unknown host; a refusal has to say why")
	}
}
