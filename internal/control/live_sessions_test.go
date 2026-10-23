package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/gsoultan/argus/internal/agent"
	"github.com/gsoultan/argus/internal/auth"
)

/*
Watching and ending a session the control plane relays.

What the gateway used to do for its own sessions, done where the bytes now
pass: a viewer sees what the person working sees and can type nothing into
it, and ending a session from the console ends it on the host with who and
why in the recording's report.
*/

func (e *tunnelEnv) openWatch(t *testing.T, id, ticket string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	u, _ := url.Parse(e.srv.URL)
	u.Scheme = "wss"
	u.Path = "/api/v1/sessions/" + id + "/watch"
	u.RawQuery = url.Values{"ticket": {ticket}}.Encode()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		HTTPClient: e.srv.Client(),
		HTTPHeader: http.Header{"Origin": {e.srv.URL}},
	})
}

// wsEvent is one message off a socket.
type wsEvent struct {
	binary []byte
	msg    terminalMessage
}

// pump reads c until it closes. A read with a deadline would close the
// socket when the deadline passed, so reading is left to this goroutine.
func pump(c *websocket.Conn) <-chan wsEvent {
	ch := make(chan wsEvent, 256)
	go func() {
		defer close(ch)
		for {
			typ, data, err := c.Read(context.Background())
			if err != nil {
				return
			}
			if typ == websocket.MessageBinary {
				ch <- wsEvent{binary: data}
				continue
			}
			var m terminalMessage
			_ = json.Unmarshal(data, &m)
			ch <- wsEvent{msg: m}
		}
	}()
	return ch
}

// readUntil collects from ch until want shows up in the output, a message
// of type stop arrives, or until passes.
func readUntil(ch <-chan wsEvent, want, stop string, until time.Duration) (string, []terminalMessage) {
	var out strings.Builder
	var msgs []terminalMessage
	deadline := time.After(until)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out.String(), msgs
			}
			if ev.binary != nil {
				out.Write(ev.binary)
				if want != "" && strings.Contains(out.String(), want) {
					return out.String(), msgs
				}
				continue
			}
			msgs = append(msgs, ev.msg)
			if ev.msg.Type == stop {
				return out.String(), msgs
			}
		case <-deadline:
			return out.String(), msgs
		}
	}
}

func send(t *testing.T, c *websocket.Conn, m terminalMessage) {
	t.Helper()
	b, _ := json.Marshal(m)
	if err := c.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

func TestAViewerSeesTheSessionAndAnAdminCanEndIt(t *testing.T) {
	e := newTunnelEnv(t)
	signer := e.withTerminal(t)
	host := unique("live")
	cred, _ := enrolled(t, e.store, e.h, host)
	th := e.runTerminalAgent(t, cred, host, t.TempDir())

	ticket, _ := signer.IssueTicket("ana@example.com", host, me(t), time.Minute)
	c, _, err := e.openTerminal(t, host, me(t), ticket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	cev := pump(c)
	_, msgs := readUntil(cev, "", "ready", 10*time.Second)
	var id string
	for _, m := range msgs {
		if m.Type == "ready" {
			id = m.Session
		}
	}
	if id == "" {
		t.Fatalf("no ready: %+v", msgs)
	}
	send(t, c, terminalMessage{Type: "resize", Cols: 132, Rows: 40})

	// A ticket for another session, or for ending rather than watching,
	// does not open this view.
	wrong, _ := signer.IssueSessionScopedTicket("aud@example.com", auth.ScopeShadow,
		"00000000-0000-0000-0000-000000000000", time.Minute)
	if _, resp, err := e.openWatch(t, id, wrong); err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a ticket for another session: %v", err)
	}
	kill, _ := signer.IssueSessionScopedTicket("aud@example.com", auth.ScopeTerminate, id, time.Minute)
	if _, resp, err := e.openWatch(t, id, kill); err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a terminate ticket opened a view: %v", err)
	}

	watch, _ := signer.IssueSessionScopedTicket("aud@example.com", auth.ScopeShadow, id, time.Minute)
	v, _, err := e.openWatch(t, id, watch)
	if err != nil {
		t.Fatal(err)
	}
	defer v.CloseNow()
	vev := pump(v)

	// Whatever the viewer types goes nowhere.
	send(t, v, terminalMessage{Type: "input", Data: "echo viewer-$((1+1))\n"})
	send(t, c, terminalMessage{Type: "input", Data: "echo worker-$((20+1))\n"})
	seen, vmsgs := readUntil(vev, "worker-21", "closed", 10*time.Second)
	if !strings.Contains(seen, "worker-21") {
		t.Fatalf("the viewer did not see the session: %q %+v", seen, vmsgs)
	}
	sized := false
	for _, m := range vmsgs {
		sized = sized || (m.Type == "resize" && m.Data == "132x40")
	}
	if !sized {
		t.Errorf("the viewer was not told the terminal's size: %+v", vmsgs)
	}

	// Ending it: a reason is required, and then it ends on the host.
	end := func(ticket, body string) int {
		r, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/api/v1/sessions/"+id+"/terminate",
			strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+ticket)
		resp, err := e.srv.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := end(kill, `{}`); code != http.StatusBadRequest {
		t.Errorf("no reason: %d", code)
	}
	kill, _ = signer.IssueSessionScopedTicket("admin@example.com", auth.ScopeTerminate, id, time.Minute)
	if code := end(kill, `{"reason":"left a shell open on prod"}`); code != http.StatusOK {
		t.Fatalf("terminate: %d", code)
	}
	out, cmsgs := readUntil(cev, "", "closed", 10*time.Second)
	closed, told := false, false
	for _, m := range cmsgs {
		closed = closed || m.Type == "closed"
		told = told || (m.Type == "error" && strings.Contains(m.Data, "admin@example.com"))
	}
	if !closed || !told {
		t.Errorf("the person working was not told: %+v %q", cmsgs, out)
	}
	var rec agent.SessionRecord
	select {
	case rec = <-th.sessions:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent sealed no recording")
	}
	if rec.State != "terminated" || rec.TerminatedBy != "admin@example.com" ||
		rec.TerminationReason != "left a shell open on prod" {
		t.Errorf("sealed as %+v", rec)
	}
	if strings.Contains(out+seen, "viewer-2") {
		t.Error("what the viewer typed reached the shell")
	}

	// Once it is over there is nothing here to watch or end.
	kill, _ = signer.IssueSessionScopedTicket("admin@example.com", auth.ScopeTerminate, id, time.Minute)
	if code := end(kill, `{"reason":"again"}`); code != http.StatusNotFound {
		t.Errorf("ending an ended session: %d", code)
	}
}
