package control

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/gsoultan/argus/internal/agent"
	"github.com/gsoultan/argus/internal/auth"
	"github.com/gsoultan/argus/internal/rdp"
	"github.com/gsoultan/argus/internal/recorder"
	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Remote Desktop through a Windows agent's tunnel, with the desktop itself
replaced: what is under test is that the password reaches the agent's logon
and nowhere else, that what the desktop sends is recorded and then shown,
that input reaches it, and that it can be watched and ended like a terminal.
The RDP client against a real Windows listener is the Windows CI drill's.
*/

// fakeDesktop stands in for the host's Remote Desktop listener.
type fakeDesktop struct {
	mu       sync.Mutex
	tap      func([]byte)
	frames   chan []rdp.Bitmap
	inputs   []rdp.InputEvent
	refresh  int
	closed   chan struct{}
	once     sync.Once
	password string
}

func newFakeDesktop() *fakeDesktop {
	return &fakeDesktop{frames: make(chan []rdp.Bitmap, 16), closed: make(chan struct{})}
}

func (f *fakeDesktop) NextBitmaps() ([]rdp.Bitmap, error) {
	select {
	case b := <-f.frames:
		f.mu.Lock()
		tap := f.tap
		f.mu.Unlock()
		if tap != nil {
			tap([]byte("pdu:" + string(b[0].Data)))
		}
		return b, nil
	case <-f.closed:
		return nil, errors.New("closed")
	}
}

func (f *fakeDesktop) Send(e rdp.InputEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inputs = append(f.inputs, e)
	return nil
}

func (f *fakeDesktop) Refresh() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refresh++
	f.frames <- []rdp.Bitmap{{Width: 4, Height: 1, BPP: 16, Data: []byte("repaint!")}}
	return nil
}

func (f *fakeDesktop) Size() (int, int)       { return 1024, 768 }
func (f *fakeDesktop) SetTap(fn func([]byte)) { f.mu.Lock(); f.tap = fn; f.mu.Unlock() }
func (f *fakeDesktop) Close() error           { f.once.Do(func() { close(f.closed) }); return nil }

func (e *tunnelEnv) runDesktopAgent(t *testing.T, cred, host string, desk *fakeDesktop) *terminalHost {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	col := agent.NewCollector(dir, log)
	th := &terminalHost{host: host, dir: dir, sessions: make(chan agent.SessionRecord, 4)}
	col.OnSession = func(r agent.SessionRecord) { th.sessions <- r }
	services := agent.DefaultTunnelServices()
	services[agent.DesktopService] = agent.Desktop(agent.DesktopConfig{
		Collector: col, Verifier: &tunnel.GrantVerifier{Host: host}, Hostname: host, Log: log,
		Connect: func(_ agent.DesktopConfig, principal string, open tunnel.DesktopOpen,
			_ *slog.Logger) (agent.DesktopClient, agent.DesktopInfo, error) {
			if open.Password != "correct horse" {
				return nil, agent.DesktopInfo{}, errors.New("logon failure: wrong password")
			}
			desk.mu.Lock()
			desk.password = open.Password
			desk.mu.Unlock()
			return desk, agent.DesktopInfo{Protocol: rdp.ProtocolHybrid}, nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go agent.RunTunnel(ctx, agent.TunnelConfig{Dial: e.dial(cred, host), Services: services, Log: log})
	e.waitTunnel(t, host, "")
	return th
}

func (e *tunnelEnv) openDesktop(t *testing.T, target, principal, ticket string) *websocket.Conn {
	t.Helper()
	u, _ := url.Parse(e.srv.URL)
	u.Scheme = "wss"
	u.Path = "/api/v1/desktop"
	u.RawQuery = url.Values{"target": {target}, "principal": {principal}, "ticket": {ticket}}.Encode()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		HTTPClient: e.srv.Client(), HTTPHeader: http.Header{"Origin": {e.srv.URL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// nextBinary returns the next display frame, skipping text messages into msgs.
func nextBinary(ch <-chan wsEvent, msgs *[]terminalMessage, until time.Duration) []byte {
	deadline := time.After(until)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			if ev.binary != nil {
				return ev.binary
			}
			*msgs = append(*msgs, ev.msg)
		case <-deadline:
			return nil
		}
	}
}

func TestARemoteDesktopOpensThroughTheAgentAndCanBeWatchedAndEnded(t *testing.T) {
	e := newTunnelEnv(t)
	signer := e.withTerminal(t)
	host := unique("desk")
	cred, _ := enrolled(t, e.store, e.h, host)
	desk := newFakeDesktop()
	th := e.runDesktopAgent(t, cred, host, desk)

	logon := func(c *websocket.Conn, password string) {
		b, _ := json.Marshal(map[string]any{"type": "logon", "password": password,
			"width": 1024, "height": 768})
		if err := c.Write(context.Background(), websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
	}

	// A wrong password is the agent's refusal, said to the browser.
	ticket, _ := signer.IssueTicket("ana@example.com", host, "ana", time.Minute)
	c := e.openDesktop(t, host, "ana", ticket)
	logon(c, "wrong")
	var msgs []terminalMessage
	frame := nextBinary(pump(c), &msgs, 10*time.Second)
	if len(frame) == 0 || frame[0] != rdp.FrameClosed || len(msgs) == 0 ||
		!strings.Contains(msgs[0].Data, "logon failure") {
		t.Fatalf("wrong password: %v %+v", frame, msgs)
	}
	c.CloseNow()

	// A terminal's grant does not open a desktop.
	termGrant, _ := tunnel.SignGrant(e.api.Tunnels.GrantKey,
		tunnel.NewGrant("ana@example.com", host, "ana", ""))
	octx, ocancel := context.WithTimeout(context.Background(), 10*time.Second)
	st, _, err := e.api.Tunnels.OpenWith(octx, host, tunnel.Header{Service: agent.DesktopService,
		Grant: termGrant, Desktop: &tunnel.DesktopOpen{Password: "correct horse"}})
	ocancel()
	if st != nil {
		_ = st.Close()
	}
	if !tunnel.IsRefused(err) {
		t.Fatalf("a terminal grant opened a desktop: %v", err)
	}

	ticket, _ = signer.IssueTicket("ana@example.com", host, "ana", time.Minute)
	c = e.openDesktop(t, host, "ana", ticket)
	defer c.CloseNow()
	logon(c, "correct horse")
	cev := pump(c)
	msgs = nil
	frame = nextBinary(cev, &msgs, 10*time.Second)
	if len(frame) < rdp.FrameHeaderSize || frame[0] != rdp.FrameReady ||
		binary.LittleEndian.Uint16(frame[6:8]) != 1024 {
		t.Fatalf("not ready: %v %+v", frame, msgs)
	}

	desk.frames <- []rdp.Bitmap{{Width: 4, Height: 1, BPP: 16, Data: []byte("pixels!!")}}
	frame = nextBinary(cev, &msgs, 10*time.Second)
	if len(frame) < rdp.FrameHeaderSize || frame[0] != rdp.FrameEncoded ||
		!strings.Contains(string(frame), "pixels!!") {
		t.Fatalf("screen update: %v", frame)
	}
	key, _ := json.Marshal(map[string]any{"type": "key", "code": "KeyA", "down": true})
	_ = c.Write(context.Background(), websocket.MessageText, key)

	// The session id is the agent's; find it the way the console would.
	var id string
	deadline := time.Now().Add(5 * time.Second)
	for id == "" && time.Now().Before(deadline) {
		e.api.live.mu.Lock()
		for k, s := range e.api.live.m {
			if s.protocol == "rdp" {
				id = k
			}
		}
		e.api.live.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	if id == "" {
		t.Fatal("the desktop is not listed as live")
	}

	watch, _ := signer.IssueSessionScopedTicket("aud@example.com", auth.ScopeShadow, id, time.Minute)
	v, _, err := e.openWatch(t, id, watch)
	if err != nil {
		t.Fatal(err)
	}
	defer v.CloseNow()
	vev := pump(v)
	frame = nextBinary(vev, &msgs, 10*time.Second)
	if len(frame) < rdp.FrameHeaderSize || frame[0] != rdp.FrameReady {
		t.Fatalf("viewer not ready: %v", frame)
	}
	// A viewer arriving late gets the screen as it stands, not deltas.
	frame = nextBinary(vev, &msgs, 10*time.Second)
	if !strings.Contains(string(frame), "repaint!") {
		t.Fatalf("viewer did not get a repaint: %v", frame)
	}
	// Nothing the viewer sends reaches the desktop.
	_ = v.Write(context.Background(), websocket.MessageText, key)

	kill, _ := signer.IssueSessionScopedTicket("admin@example.com", auth.ScopeTerminate, id, time.Minute)
	r, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/api/v1/sessions/"+id+"/terminate",
		strings.NewReader(`{"reason":"not during the freeze"}`))
	r.Header.Set("Authorization", "Bearer "+kill)
	var resp *http.Response
	resp, err = e.srv.Client().Do(r)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("terminate: %v %v", err, resp)
	}
	resp.Body.Close()

	var rec agent.SessionRecord
	select {
	case rec = <-th.sessions:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent sealed no recording")
	}
	if rec.Protocol != "rdp" || rec.State != "terminated" || rec.TerminatedBy != "admin@example.com" ||
		rec.Actor != "ana@example.com" || rec.ID != id {
		t.Errorf("sealed as %+v", rec)
	}
	desk.mu.Lock()
	inputs := len(desk.inputs)
	desk.mu.Unlock()
	if inputs != 1 {
		t.Errorf("the desktop got %d inputs; want the person's one key and none of the viewer's", inputs)
	}
	f, err := os.Open(rec.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if !strings.HasSuffix(rec.Path, rdp.Extension) {
		t.Errorf("recorded as %s", rec.Path)
	}
	if v, err := recorder.Verify(f, rec.ChainHead); err != nil || !v.OK {
		t.Errorf("the recording does not verify: %v %+v", err, v)
	}
	b, _ := os.ReadFile(rec.Path)
	if strings.Contains(string(b), "correct horse") {
		t.Error("the password is in the recording")
	}
	if !strings.Contains(string(b), "cGR1OnBpeGVscyEh") { // base64 of "pdu:pixels!!"
		t.Error("what was shown is not in the recording")
	}
}
