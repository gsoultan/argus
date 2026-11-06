package control

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/gsoultan/argus/internal/agent"
	"github.com/gsoultan/argus/internal/rdp"
	"github.com/gsoultan/argus/internal/recorder"
	"github.com/gsoultan/argus/internal/tunnel"
)

/*
An Android device through the Argus app's tunnel, with the app replaced by
the drawn phone: a device asset opens on the android service with no
password, the console's touches and device keys reach it, "view" reaches
nothing, a desktop grant does not open it, and what was shown is in the
recording. Control needing an approval is decided per device.
*/

// runAndroidAgent is the Argus app, as far as the control plane can tell.
func (e *tunnelEnv) runAndroidAgent(t *testing.T, cred, host string, dev *agent.FakeAndroid) *terminalHost {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	col := agent.NewCollector(dir, log)
	th := &terminalHost{host: host, dir: dir, sessions: make(chan agent.SessionRecord, 4)}
	col.OnSession = func(r agent.SessionRecord) { th.sessions <- r }
	services := agent.DefaultTunnelServices()
	services[agent.AndroidService] = agent.Android(agent.AndroidConfig{
		DesktopConfig: agent.DesktopConfig{Collector: col, Verifier: &tunnel.GrantVerifier{Host: host},
			Hostname: host, Log: log},
		Device: func(agent.AndroidOpen) (agent.AndroidDevice, error) { return dev, nil },
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go agent.RunTunnel(ctx, agent.TunnelConfig{Dial: e.dial(cred, host), Services: services, Log: log})
	e.waitTunnel(t, host, "")
	return th
}

func TestADeviceOpensThroughTheAppAndTakesTouchesAndKeys(t *testing.T) {
	e := newTunnelEnv(t)
	signer := e.withTerminal(t)
	host := unique("pixel")
	cred, _ := enrolled(t, e.store, e.h, host)
	asset, err := e.store.CreateAsset(context.Background(), AssetInput{Hostname: host, Kind: "android"})
	if err != nil {
		t.Fatal(err)
	}
	if asset.Protocol != "rdp" || asset.DesktopMode != "screen" || asset.Kind != "android" ||
		asset.ControlMode != "attended" || strings.Join(asset.Principals, ",") != "control,view" {
		t.Fatalf("a device was stored as %+v", asset)
	}
	dev := agent.NewFakeAndroid(192, 384)
	th := e.runAndroidAgent(t, cred, host, dev)

	// A desktop grant does not open a device.
	g := tunnel.NewGrant("ana@example.com", host, DeviceControl, "")
	g.Service = agentScreenService
	grant, _ := tunnel.SignGrant(e.api.Tunnels.GrantKey, g)
	octx, ocancel := context.WithTimeout(context.Background(), 10*time.Second)
	st, _, err := e.api.Tunnels.OpenWith(octx, host, tunnel.Header{Service: agent.AndroidService,
		Grant: grant, Desktop: &tunnel.DesktopOpen{}})
	ocancel()
	if st != nil {
		_ = st.Close()
	}
	if !tunnel.IsRefused(err) {
		t.Fatalf("a screen-sharing grant opened the device: %v", err)
	}

	ticket, _ := signer.IssueTicket("ana@example.com", host, DeviceControl, time.Minute)
	c := e.openDesktop(t, host, DeviceControl, ticket)
	defer c.CloseNow()
	b, _ := json.Marshal(map[string]any{"type": "logon", "password": "typed anyway",
		"width": 1024, "height": 768, "video": true})
	if err := c.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
	cev := pump(c)
	var msgs []terminalMessage
	frame := nextBinary(cev, &msgs, 10*time.Second)
	if len(frame) < rdp.FrameHeaderSize || frame[0] != rdp.FrameReady ||
		binary.LittleEndian.Uint16(frame[6:8]) != 192 || binary.LittleEndian.Uint16(frame[8:10]) != 384 {
		t.Fatalf("not ready at the device's size: %v %+v", frame, msgs)
	}
	if frame = nextBinary(cev, &msgs, 10*time.Second); len(frame) < rdp.FrameHeaderSize ||
		frame[0] != rdp.FrameEncoded {
		t.Fatalf("the first screen did not arrive as tiles: %v", frame)
	}

	// A tap, a device key and typed text, as the console sends them.
	for _, m := range []map[string]any{
		{"type": "button", "button": 0, "down": true, "x": 50, "y": 300},
		{"type": "button", "button": 0, "down": false, "x": 50, "y": 300},
		{"type": "device", "name": "back"},
		{"type": "text", "text": "hi"},
	} {
		b, _ := json.Marshal(m)
		if err := c.Write(context.Background(), websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(dev.Plays()) < 4 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	plays := dev.Plays()
	if len(plays) != 4 || plays[0].Type != "button" || plays[2].Name != "back" || plays[3].Text != "hi" {
		t.Fatalf("the device played %+v", plays)
	}

	// The session is listed like a desktop, so Watch live and terminate work
	// as they do; ending it seals a recording that says what it was.
	var id string
	deadline = time.Now().Add(5 * time.Second)
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
		t.Fatal("the device is not listed as live")
	}
	c.CloseNow()
	var rec agent.SessionRecord
	select {
	case rec = <-th.sessions:
	case <-time.After(10 * time.Second):
		t.Fatal("the app sealed no recording")
	}
	if rec.ID != id || rec.Actor != "ana@example.com" || rec.Principal != DeviceControl {
		t.Errorf("sealed as %+v", rec)
	}
	f, err := os.Open(rec.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if v, err := recorder.Verify(f, rec.ChainHead); err != nil || !v.OK {
		t.Errorf("the recording does not verify: %v %+v", err, v)
	}
	raw, _ := os.ReadFile(rec.Path)
	if strings.Contains(string(raw), "typed anyway") {
		t.Error("the password the browser typed is in the recording")
	}
	if !strings.Contains(string(raw), `"android"`) {
		t.Error("the recording's header does not say it is a device")
	}

	events, err := e.store.AuditEvents(context.Background(), 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.Action == "desktop.opened" && ev.Target == host {
			found = strings.Contains(ev.Detail, "whoever holds the device") &&
				strings.Contains(ev.Detail, "to drive it")
		}
	}
	if !found {
		t.Error("the audit does not say a device opened, to be driven")
	}
}

func TestViewingADeviceReachesNothingOnIt(t *testing.T) {
	e := newTunnelEnv(t)
	signer := e.withTerminal(t)
	host := unique("tablet")
	cred, _ := enrolled(t, e.store, e.h, host)
	if _, err := e.store.CreateAsset(context.Background(), AssetInput{Hostname: host, Kind: "android"}); err != nil {
		t.Fatal(err)
	}
	dev := agent.NewFakeAndroid(128, 256)
	e.runAndroidAgent(t, cred, host, dev)

	ticket, _ := signer.IssueTicket("aud@example.com", host, DeviceView, time.Minute)
	c := e.openDesktop(t, host, DeviceView, ticket)
	defer c.CloseNow()
	b, _ := json.Marshal(map[string]any{"type": "logon", "width": 1024, "height": 768})
	_ = c.Write(context.Background(), websocket.MessageText, b)
	cev := pump(c)
	var msgs []terminalMessage
	if frame := nextBinary(cev, &msgs, 10*time.Second); len(frame) == 0 || frame[0] != rdp.FrameReady {
		t.Fatalf("not ready: %v %+v", frame, msgs)
	}
	for _, m := range []map[string]any{
		{"type": "button", "button": 0, "down": true, "x": 50, "y": 300},
		{"type": "device", "name": "home"},
	} {
		b, _ := json.Marshal(m)
		_ = c.Write(context.Background(), websocket.MessageText, b)
	}
	// Give the input every chance to arrive, then check that it did not.
	time.Sleep(300 * time.Millisecond)
	if plays := dev.Plays(); len(plays) != 0 {
		t.Errorf("a viewer's input reached the device: %+v", plays)
	}
	events, _ := e.store.AuditEvents(context.Background(), 50, 0)
	for _, ev := range events {
		if ev.Action == "desktop.opened" && ev.Target == host && !strings.Contains(ev.Detail, "no input") {
			t.Errorf("the audit does not say the session only watches: %s", ev.Detail)
		}
	}
}

func TestControlOfADeviceNeedsAnApprovalWhenItsSettingsSaySo(t *testing.T) {
	e := newTunnelEnv(t)
	ctx := context.Background()
	asks := unique("kiosk")
	free := unique("demo")
	for _, in := range []AssetInput{
		{Hostname: asks, Kind: "android", ControlNeedsApproval: true},
		{Hostname: free, Kind: "android"},
	} {
		if _, err := e.store.CreateAsset(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		host, principal string
		want            bool
	}{
		{asks, DeviceControl, true},
		{asks, DeviceView, false},
		{free, DeviceControl, false},
		{free, "root", true},
		{unique("nowhere"), DeviceControl, false},
	} {
		if got := e.api.elevatedOn(ctx, tc.host, tc.principal); got != tc.want {
			t.Errorf("%s as %s: elevated %v, want %v", tc.host, tc.principal, got, tc.want)
		}
	}
	// What the request form is told agrees.
	hosts, err := e.store.RequestableAccounts(ctx, []string{asks, free})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hosts {
		for _, acct := range h.Accounts {
			want := h.Hostname == asks && acct.Name == DeviceControl
			if acct.Elevated != want {
				t.Errorf("%s %s: elevated %v, want %v", h.Hostname, acct.Name, acct.Elevated, want)
			}
		}
	}
}

func TestADeviceIsValidated(t *testing.T) {
	for _, tc := range []struct {
		in  AssetInput
		bad string
	}{
		{AssetInput{Hostname: "p1", Kind: "android", Protocol: "ssh", Principals: []string{"root"}}, "no accounts"},
		{AssetInput{Hostname: "p1", Kind: "android", ControlMode: "sometimes"}, "attended or unattended"},
		{AssetInput{Hostname: "s1", ControlMode: "unattended"}, "not a server"},
		{AssetInput{Hostname: "s1", ControlNeedsApproval: true}, "not a server"},
		{AssetInput{Hostname: "x1", Kind: "ios"}, "server or android"},
	} {
		_, err := tc.in.normalise()
		if err == nil || !strings.Contains(err.Error(), tc.bad) {
			t.Errorf("%+v: %v; want %q", tc.in, err, tc.bad)
		}
	}
	got, err := tc0()
	if err != nil || got.Kind != "android" || got.Protocol != "rdp" || got.DesktopMode != "screen" ||
		got.ControlMode != "unattended" || len(got.Principals) != 2 {
		t.Errorf("an unattended device normalised to %+v, %v", got, err)
	}
	srv, err := AssetInput{Hostname: "s1"}.normalise()
	if err != nil || srv.Kind != "server" || srv.ControlMode != "attended" {
		t.Errorf("a server normalised to %+v, %v", srv, err)
	}
}

func tc0() (AssetInput, error) {
	return AssetInput{Hostname: "p2", Kind: "Android", ControlMode: "Unattended",
		Principals: []string{"view", "control"}}.normalise()
}

// Finishing the Add-a-device wizard enrols the reporting app as a device:
// stored the way a device opens, with the control mode picked, and with only
// view and control to give.
func TestEnrollingADeviceStoresHowItIsControlled(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	cleanHost(t, s, "pixel-disc")
	seedDiscovered(t, s, "pixel-disc")

	if _, err := s.EnrolDevice(ctx, "pixel-disc", "admin@northwind.id", []string{"ops"}, nil, nil,
		DeviceEnrol{}); err == nil {
		t.Fatal("a device was enrolled with a login account, which it has none of")
	}
	id, err := s.EnrolDevice(ctx, "pixel-disc", "admin@northwind.id", []string{"control", "view"}, nil, nil,
		DeviceEnrol{ControlMode: "unattended", ControlNeedsApproval: true})
	if err != nil {
		t.Fatalf("EnrolDevice: %v", err)
	}
	a, err := s.AssetByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != "android" || a.Protocol != "rdp" || a.DesktopMode != "screen" || a.ControlMode != "unattended" ||
		!a.ControlNeedsApproval {
		t.Errorf("enrolled as %+v; want an unattended android device that opens as a shared screen", a)
	}
	if len(a.Principals) != 2 {
		t.Errorf("principals %v; want view and control", a.Principals)
	}
	hosts, _ := s.DiscoveredHosts(ctx, "unreviewed")
	for _, h := range hosts {
		if h.Hostname == "pixel-disc" {
			t.Error("an enrolled device is still listed as unreviewed")
		}
	}
}
