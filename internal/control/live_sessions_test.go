package control

import (
	"context"
	"encoding/json"
	"fmt"
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

// watchRefusal opens a view that should be refused and returns why it was:
// the first message, which must be an error, with no output before it.
func (e *tunnelEnv) watchRefusal(t *testing.T, id, ticket string) string {
	t.Helper()
	c, _, err := e.openWatch(t, id, ticket)
	if err != nil {
		t.Fatalf("the view did not open to say why it was refused: %v", err)
	}
	defer c.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	typ, data, err := c.Read(ctx)
	if err != nil || typ != websocket.MessageText {
		t.Fatalf("no refusal: %v", err)
	}
	var m terminalMessage
	_ = json.Unmarshal(data, &m)
	if m.Type != "error" {
		t.Fatalf("first message = %+v, want an error", m)
	}
	if _, _, err := c.Read(ctx); err == nil {
		t.Error("the refused view stayed open")
	}
	return m.Data
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
	// Each is told why, in words, rather than seeing a handshake fail.
	wrong, _ := signer.IssueSessionScopedTicket("aud@example.com", auth.ScopeShadow,
		"00000000-0000-0000-0000-000000000000", time.Minute)
	if why := e.watchRefusal(t, id, wrong); !strings.Contains(why, "not authorised") {
		t.Errorf("a ticket for another session: %q", why)
	}
	kill, _ := signer.IssueSessionScopedTicket("aud@example.com", auth.ScopeTerminate, id, time.Minute)
	if why := e.watchRefusal(t, id, kill); !strings.Contains(why, "not authorised") {
		t.Errorf("a terminate ticket opened a view: %q", why)
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

// A session this replica is not relaying -- a login through sshd, which
// never passes through the control plane, or a browser terminal another
// replica holds -- is shown from its server's own recording.
func TestASessionNotRelayedHereIsWatchedThroughItsAgent(t *testing.T) {
	e := newTunnelEnv(t)
	signer := e.withTerminal(t)
	host := unique("live")
	cred, _ := enrolled(t, e.store, e.h, host)
	e.runTerminalAgent(t, cred, host, t.TempDir())

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
	send(t, c, terminalMessage{Type: "resize", Cols: 100, Rows: 30})
	send(t, c, terminalMessage{Type: "input", Data: "echo before-$((1+1))\n"})
	readUntil(cev, "before-2", "closed", 10*time.Second)
	if _, err := e.store.RecordSession(context.Background(), Session{ID: id,
		UserEmail: "ana@example.com", AssetHostname: host, Principal: me(t), Protocol: "ssh",
		Origin: "console", State: "active", StartedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	// As another replica sees it: not in this one's registry.
	e.api.live.mu.Lock()
	ls := e.api.live.m[tunnelKey(DefaultTenantID, id)]
	delete(e.api.live.m, tunnelKey(DefaultTenantID, id))
	e.api.live.mu.Unlock()
	if ls == nil {
		t.Fatal("the session was not registered")
	}

	watch, _ := signer.IssueSessionScopedTicket("aud@example.com", auth.ScopeShadow, id, time.Minute)
	v, _, err := e.openWatch(t, id, watch)
	if err != nil {
		t.Fatal(err)
	}
	defer v.CloseNow()
	vev := pump(v)
	send(t, c, terminalMessage{Type: "input", Data: "echo after-$((20+1))\n"})
	seen, vmsgs := readUntil(vev, "after-21", "closed", 10*time.Second)
	if !strings.Contains(seen, "before-2") || !strings.Contains(seen, "after-21") {
		t.Fatalf("the viewer did not see the session: %q %+v", seen, vmsgs)
	}
	ready, sized := false, false
	for _, m := range vmsgs {
		ready = ready || (m.Type == "ready" && m.Session == id)
		sized = sized || (m.Type == "resize" && m.Data == "100x30")
	}
	if !ready || !sized {
		t.Errorf("the viewer was not told it was watching, at 100x30: %+v", vmsgs)
	}

	// The session ending ends the view, saying so.
	send(t, c, terminalMessage{Type: "input", Data: "exit\n"})
	_, vmsgs = readUntil(vev, "", "closed", 10*time.Second)
	if len(vmsgs) == 0 || vmsgs[len(vmsgs)-1].Type != "closed" {
		t.Errorf("the view did not say the session ended: %+v", vmsgs)
	}

	// Ended, the session has nothing to show, and says that in words.
	if _, err := e.store.RecordSession(context.Background(), Session{ID: id,
		UserEmail: "ana@example.com", AssetHostname: host, Principal: me(t), Protocol: "ssh",
		Origin: "console", State: "closed", StartedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	again, _ := signer.IssueSessionScopedTicket("aud@example.com", auth.ScopeShadow, id, time.Minute)
	w, _, err := e.openWatch(t, id, again)
	if err != nil {
		t.Fatal(err)
	}
	defer w.CloseNow()
	_, wmsgs := readUntil(pump(w), "", "closed", 10*time.Second)
	if len(wmsgs) == 0 || wmsgs[0].Type != "closed" {
		t.Errorf("an ended session: %+v", wmsgs)
	}
}

// A server whose agent is not connected says so, rather than failing the
// handshake.
func TestWatchingASessionWhoseAgentIsGoneSaysWhy(t *testing.T) {
	e := newTunnelEnv(t)
	signer := e.withTerminal(t)
	host := unique("gone")
	id := fmt.Sprintf("%032x", time.Now().UnixNano())
	if _, err := e.store.RecordSession(context.Background(), Session{ID: id,
		UserEmail: "ops@db-01", AssetHostname: host, Principal: "ops", Protocol: "ssh",
		Origin: "direct", State: "active", StartedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	watch, _ := signer.IssueSessionScopedTicket("aud@example.com", auth.ScopeShadow, id, time.Minute)
	if why := e.watchRefusal(t, id, watch); !strings.Contains(why, "not connected") {
		t.Errorf("refusal = %q", why)
	}

	// And a direct login cannot be ended from here; it says that too.
	kill, _ := signer.IssueSessionScopedTicket("admin@example.com", auth.ScopeTerminate, id, time.Minute)
	r, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/api/v1/sessions/"+id+"/terminate",
		strings.NewReader(`{"reason":"why not"}`))
	r.Header.Set("Authorization", "Bearer "+kill)
	resp, err := e.srv.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct{ Error string }
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(body.Error, "direct SSH login") {
		t.Errorf("terminate: %d %q", resp.StatusCode, body.Error)
	}
}

// A database session another replica relays is not sent to the agent's watch
// service, which knows terminals only; it is told plainly where it runs.
func TestWatchingADatabaseSessionRelayedElsewhereSaysSo(t *testing.T) {
	e := newTunnelEnv(t)
	signer := e.withTerminal(t)
	id := fmt.Sprintf("%032x", time.Now().UnixNano())
	if _, err := e.store.RecordSession(context.Background(), Session{ID: id,
		UserEmail: "ana@example.com", AssetHostname: unique("db"), Principal: "ana_sari", Protocol: "postgres",
		Origin: "console", State: "active", StartedAt: time.Now().UTC(), Kind: "database",
		Fidelity: "statement", RiskFlags: []string{}}); err != nil {
		t.Fatal(err)
	}
	watch, _ := signer.IssueSessionScopedTicket("aud@example.com", auth.ScopeShadow, id, time.Minute)
	if why := e.watchRefusal(t, id, watch); why != notRelayedHere {
		t.Errorf("refusal = %q", why)
	}
}
