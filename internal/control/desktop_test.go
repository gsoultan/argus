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
	// The clipboard, when the session has one: copies stands for
	// something copied on the desktop, pasted for what the console sent.
	clip     *agent.ClipboardSink
	clipRule string
	copies   chan string
	pasted   []string
}

func (f *fakeDesktop) SetClipboard(text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pasted = append(f.pasted, text)
	return nil
}

func newFakeDesktop() *fakeDesktop {
	return &fakeDesktop{frames: make(chan []rdp.Bitmap, 16), closed: make(chan struct{}),
		copies: make(chan string, 4)}
}

func (f *fakeDesktop) NextBitmaps() ([]rdp.Bitmap, error) {
	f.mu.Lock()
	closed := f.closed
	f.mu.Unlock()
	select {
	case b := <-f.frames:
		f.mu.Lock()
		tap := f.tap
		f.mu.Unlock()
		if tap != nil {
			tap([]byte("pdu:" + string(b[0].Data)))
		}
		return b, nil
	case text := <-f.copies:
		f.mu.Lock()
		clip := f.clip
		f.mu.Unlock()
		if clip != nil {
			clip.Set(text)
		}
		return nil, nil
	case <-closed:
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
func (f *fakeDesktop) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.once.Do(func() { close(f.closed) })
	return nil
}

// reopen readies a closed fake for the next session, as a listener would be.
func (f *fakeDesktop) reopen() {
	f.mu.Lock()
	defer f.mu.Unlock()
	select {
	case <-f.closed:
		f.closed, f.once = make(chan struct{}), sync.Once{}
	default:
	}
}

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
		Connect: func(_ agent.DesktopConfig, _ tunnel.Grant, open tunnel.DesktopOpen,
			_ *slog.Logger) (agent.DesktopClient, agent.DesktopInfo, error) {
			if open.Password != "correct horse" {
				return nil, agent.DesktopInfo{}, errors.New("logon failure: wrong password")
			}
			desk.reopen()
			info := agent.DesktopInfo{Protocol: rdp.ProtocolHybrid}
			if tunnel.ClipboardAllows(open.Clipboard, true) || tunnel.ClipboardAllows(open.Clipboard, false) {
				// As connectDesktop does: a sink only for what may leave.
				info.Clipboard, info.ClipboardRule = &agent.ClipboardSink{}, open.Clipboard
			}
			desk.mu.Lock()
			desk.password = open.Password
			desk.clipRule = open.Clipboard
			desk.clip = nil
			if tunnel.ClipboardAllows(open.Clipboard, false) {
				desk.clip = info.Clipboard
			}
			desk.mu.Unlock()
			return desk, info, nil
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
		for _, s := range e.api.live.m {
			if s.protocol == "rdp" {
				id = s.id
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

// Copy and paste cross a desktop's clipboard as the asset type's rule allows,
// each crossing audited by size and never by content, and the control plane
// holds to the rule even if the agent did not.
func TestDesktopClipboardFollowsTheTypeRule(t *testing.T) {
	e := newTunnelEnv(t)
	signer := e.withTerminal(t)
	host := unique("clip")
	cred, _ := enrolled(t, e.store, e.h, host)
	desk := newFakeDesktop()
	e.runDesktopAgent(t, cred, host, desk)
	ctx := context.Background()
	t.Cleanup(func() { _ = e.store.SetConnectRules(context.Background(), DefaultConnectRules(), "test") })

	open := func() (*websocket.Conn, <-chan wsEvent, []terminalMessage) {
		ticket, _ := signer.IssueTicket("ana@example.com", host, "ana", time.Minute)
		c := e.openDesktop(t, host, "ana", ticket)
		b, _ := json.Marshal(map[string]any{"type": "logon", "password": "correct horse", "width": 1024, "height": 768})
		_ = c.Write(ctx, websocket.MessageText, b)
		ev := pump(c)
		var msgs []terminalMessage
		if f := nextBinary(ev, &msgs, 10*time.Second); len(f) == 0 || f[0] != rdp.FrameReady {
			t.Fatalf("not ready: %v %+v", f, msgs)
		}
		return c, ev, msgs
	}
	paste := func(c *websocket.Conn, text string) {
		b, _ := json.Marshal(map[string]any{"type": "clipboard", "text": text})
		_ = c.Write(ctx, websocket.MessageText, b)
		// A key after it, to know the paste was handled before looking.
		k, _ := json.Marshal(map[string]any{"type": "key", "code": "KeyV", "down": true})
		_ = c.Write(ctx, websocket.MessageText, k)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			desk.mu.Lock()
			n := len(desk.inputs)
			desk.mu.Unlock()
			if n > 0 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		desk.mu.Lock()
		t.Fatalf("the key after the paste never arrived; pasted %q, rule %q", desk.pasted, desk.clipRule)
	}
	copied := func(ev <-chan wsEvent, text string) string {
		desk.copies <- text
		// A frame after it, so a clipboard message would have come first.
		desk.frames <- []rdp.Bitmap{{Width: 4, Height: 1, BPP: 16, Data: []byte("after!!!")}}
		var msgs []terminalMessage
		nextBinary(ev, &msgs, 10*time.Second)
		for _, m := range msgs {
			if m.Type == "clipboard" {
				return m.Data
			}
		}
		return ""
	}

	// The default: both ways.
	c, ev, msgs := open()
	if len(msgs) == 0 || msgs[len(msgs)-1].Clipboard != tunnel.ClipboardBoth {
		t.Errorf("ready said %+v; want the clipboard both ways", msgs)
	}
	paste(c, "into the desktop")
	if got := copied(ev, "out of it"); got != "out of it" {
		t.Errorf("a copy on the desktop reached the console as %q", got)
	}
	desk.mu.Lock()
	if len(desk.pasted) != 1 || desk.pasted[0] != "into the desktop" {
		t.Errorf("pasted %q", desk.pasted)
	}
	desk.pasted, desk.inputs = nil, nil
	desk.mu.Unlock()
	if n := auditCount(t, e.store, "desktop.clipboard", host); n != 2 {
		t.Errorf("%d clipboard crossings audited, want 2", n)
	}
	c.CloseNow()

	// Paste only: what is copied on the desktop stays there.
	rules := DefaultConnectRules()
	rules.Server.Clipboard = tunnel.ClipboardToAsset
	if err := e.store.SetConnectRules(ctx, rules, "test"); err != nil {
		t.Fatal(err)
	}
	c, ev, _ = open()
	paste(c, "in again")
	if got := copied(ev, "kept on the desktop"); got != "" {
		t.Errorf("a copy left a paste-only desktop: %q", got)
	}
	c.CloseNow()

	// Off: no clipboard is opened at all, and a paste is dropped here.
	rules.Server.Clipboard = tunnel.ClipboardOff
	_ = e.store.SetConnectRules(ctx, rules, "test")
	c, _, _ = open()
	defer c.CloseNow()
	desk.mu.Lock()
	desk.pasted, desk.inputs = nil, nil
	desk.mu.Unlock()
	paste(c, "not this")
	desk.mu.Lock()
	defer desk.mu.Unlock()
	if desk.clipRule != tunnel.ClipboardOff || len(desk.pasted) != 0 {
		t.Errorf("clipboard off: opened with %q, pasted %q", desk.clipRule, desk.pasted)
	}
}

// runScreenAgent is an agent offering both ways in, with the console and
// the Remote Desktop listener both replaced, counting which one opened.
func (e *tunnelEnv) runScreenAgent(t *testing.T, cred, host string, desk *fakeDesktop,
	opened *[]string, offerScreen bool) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	col := agent.NewCollector(t.TempDir(), log)
	var mu sync.Mutex
	connect := func(kind string, info agent.DesktopInfo) func(agent.DesktopConfig, tunnel.Grant,
		tunnel.DesktopOpen, *slog.Logger) (agent.DesktopClient, agent.DesktopInfo, error) {
		return func(_ agent.DesktopConfig, _ tunnel.Grant, open tunnel.DesktopOpen,
			_ *slog.Logger) (agent.DesktopClient, agent.DesktopInfo, error) {
			mu.Lock()
			entry := kind + ":" + open.Password
			if open.Graphics {
				entry += "+h264"
			}
			*opened = append(*opened, entry)
			mu.Unlock()
			return desk, info, nil
		}
	}
	services := agent.DefaultTunnelServices()
	cfg := agent.DesktopConfig{Collector: col, Verifier: &tunnel.GrantVerifier{Host: host},
		Hostname: host, Log: log}
	rdpCfg := cfg
	rdpCfg.Connect = connect("rdp", agent.DesktopInfo{Protocol: rdp.ProtocolHybrid})
	services[agent.DesktopService] = agent.Desktop(rdpCfg)
	if offerScreen {
		scrCfg := cfg
		scrCfg.Connect = connect("screen", agent.DesktopInfo{Screen: true})
		services[agent.ScreenService] = agent.Screen(scrCfg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go agent.RunTunnel(ctx, agent.TunnelConfig{Dial: e.dial(cred, host), Services: services, Log: log})
	e.waitTunnel(t, host, "")
}

func TestAServerSetToScreenSharingOpensItsConsoleAndNeverPassesThePassword(t *testing.T) {
	e := newTunnelEnv(t)
	signer := e.withTerminal(t)
	host := unique("console")
	cred, _ := enrolled(t, e.store, e.h, host)
	if _, err := e.store.CreateAsset(context.Background(), AssetInput{
		Hostname: host, Protocol: "rdp", DesktopMode: "screen", Principals: []string{"ana"},
	}); err != nil {
		t.Fatal(err)
	}
	desk := newFakeDesktop()
	var opened []string
	e.runScreenAgent(t, cred, host, desk, &opened, true)

	ticket, _ := signer.IssueTicket("ana@example.com", host, "ana", time.Minute)
	c := e.openDesktop(t, host, "ana", ticket)
	defer c.CloseNow()
	// The browser decodes H.264 (video), and does not ask Remote Desktop's
	// graphics pipeline, which is not what opens here.
	b, _ := json.Marshal(map[string]any{"type": "logon", "password": "typed anyway",
		"width": 1024, "height": 768, "video": true})
	if err := c.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
	cev := pump(c)
	var msgs []terminalMessage
	frame := nextBinary(cev, &msgs, 10*time.Second)
	if len(frame) < rdp.FrameHeaderSize || frame[0] != rdp.FrameReady {
		t.Fatalf("not ready: %v %+v", frame, msgs)
	}
	if len(opened) != 1 || opened[0] != "screen:+h264" {
		t.Fatalf("opened %q; want the console, with no password, sending motion as H.264", opened)
	}
	desk.frames <- []rdp.Bitmap{{Width: 4, Height: 1, BPP: 16, Data: []byte("console!")}}
	if frame = nextBinary(cev, &msgs, 10*time.Second); !strings.Contains(string(frame), "console!") {
		t.Fatalf("screen update: %v", frame)
	}

	// A Remote Desktop grant does not open the console.
	g := tunnel.NewGrant("ana@example.com", host, "ana", "")
	g.Service = agent.DesktopService
	grant, _ := tunnel.SignGrant(e.api.Tunnels.GrantKey, g)
	octx, ocancel := context.WithTimeout(context.Background(), 10*time.Second)
	st, _, err := e.api.Tunnels.OpenWith(octx, host, tunnel.Header{Service: agent.ScreenService,
		Grant: grant, Desktop: &tunnel.DesktopOpen{}})
	ocancel()
	if st != nil {
		_ = st.Close()
	}
	if !tunnel.IsRefused(err) {
		t.Fatalf("a Remote Desktop grant opened the console: %v", err)
	}

	events, err := e.store.AuditEvents(context.Background(), 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.Action == "desktop.opened" && ev.Target == host {
			found = strings.Contains(ev.Detail, "whoever is signed in at the console")
		}
	}
	if !found {
		t.Error("the audit does not say the session acts as whoever is at the console")
	}
}

func TestScreenSharingOnAnAgentTooOldForItSaysToUpdate(t *testing.T) {
	e := newTunnelEnv(t)
	signer := e.withTerminal(t)
	host := unique("oldagent")
	cred, _ := enrolled(t, e.store, e.h, host)
	if _, err := e.store.CreateAsset(context.Background(), AssetInput{
		Hostname: host, Protocol: "rdp", DesktopMode: "screen", Principals: []string{"ana"},
	}); err != nil {
		t.Fatal(err)
	}
	var opened []string
	e.runScreenAgent(t, cred, host, newFakeDesktop(), &opened, false)
	ticket, _ := signer.IssueTicket("ana@example.com", host, "ana", time.Minute)
	c := e.openDesktop(t, host, "ana", ticket)
	defer c.CloseNow()
	b, _ := json.Marshal(map[string]any{"type": "logon", "width": 1024, "height": 768})
	_ = c.Write(context.Background(), websocket.MessageText, b)
	var msgs []terminalMessage
	frame := nextBinary(pump(c), &msgs, 10*time.Second)
	if len(frame) == 0 || frame[0] != rdp.FrameClosed || len(msgs) == 0 ||
		!strings.Contains(msgs[0].Data, "too old to share its screen") {
		t.Fatalf("got %v %+v", frame, msgs)
	}
	if len(opened) != 0 {
		t.Fatalf("fell back to %q; the server's setting must decide", opened)
	}
}

func TestDesktopModeIsValidated(t *testing.T) {
	for _, tc := range []struct {
		in   AssetInput
		want string
		bad  bool
	}{
		{AssetInput{Hostname: "w1", Protocol: "rdp"}, "rdp", false},
		{AssetInput{Hostname: "w1", Protocol: "rdp", DesktopMode: "Screen"}, "screen", false},
		// A Linux or Mac server shares its screen beside the terminal.
		{AssetInput{Hostname: "l1", Protocol: "ssh", DesktopMode: "screen"}, "screen", false},
		{AssetInput{Hostname: "l1", Protocol: "ssh"}, "rdp", false},
		{AssetInput{Hostname: "w1", Protocol: "rdp", DesktopMode: "vnc"}, "", true},
	} {
		got, err := tc.in.normalise()
		if (err != nil) != tc.bad || (!tc.bad && got.DesktopMode != tc.want) {
			t.Errorf("%+v: %q %v", tc.in, got.DesktopMode, err)
		}
	}
}
