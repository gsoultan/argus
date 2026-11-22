package control

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

func testGrantHub(t *testing.T) *TunnelHub {
	t.Helper()
	key, err := tunnel.GrantKeyFromSecret("a-test-signing-secret-that-is-long-enough-0123")
	if err != nil {
		t.Fatal(err)
	}
	return &TunnelHub{GrantKey: key}
}

// deviceEnv is one device read from one server, whose agent answers what
// the test says.
type deviceEnv struct {
	api    *API
	ctx    context.Context
	device NetworkDevice
	host   string

	mu     sync.Mutex
	answer tunnel.AttemptResult
	err    string
	asked  []tunnel.SNMPQuery
}

func newDeviceEnv(t *testing.T) *deviceEnv {
	t.Helper()
	s := testStore(t)
	tn, owner, _ := newTenant(t, s)
	e := &deviceEnv{ctx: WithTenant(context.Background(), tn)}
	e.api = NewAPI(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.api.Tunnels = testGrantHub(t)
	e.host = unique("edge") + ".example.com"
	asset, err := s.CreateAsset(e.ctx, AssetInput{Hostname: e.host})
	if err != nil {
		t.Fatal(err)
	}
	e.api.deviceAsk = func(_ context.Context, host string, g tunnel.GrantCheck) deviceOpinion {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.asked = append(e.asked, *g.SNMP)
		return deviceOpinion{host: host, res: e.answer, err: e.err}
	}
	in := NetworkDeviceInput{Name: "core-sw1", Host: "10.20.0.2", User: "argus", AuthProto: "SHA", AuthPass: "auth-secret-1",
		PrivProto: "AES", PrivPass: "priv-secret-1", CheckFrom: []string{asset.ID}, Watch: []WatchedInterface{{Index: 2, Name: "uplink"}}}
	if err := e.api.prepareDevice(e.ctx, &in, nil); err != nil {
		t.Fatal(err)
	}
	if e.device, err = s.CreateNetworkDevice(e.ctx, in, owner); err != nil {
		t.Fatal(err)
	}
	return e
}

// says sets what the device answers: its uptime, and whether the uplink is up.
func (e *deviceEnv) says(uptime int64, uplinkUp bool, inOctets uint64) {
	ans := &tunnel.SNMPAnswer{Name: "core-sw1", Description: "Cisco IOS", UptimeSec: uptime, Interfaces: []tunnel.SNMPInterface{
		{Index: 1, Name: "Gi0/1", Up: true, AdminUp: true, SpeedMbps: 1000, InOctets: inOctets},
		{Index: 2, Name: "Gi0/2", Alias: "uplink", Up: uplinkUp, AdminUp: true, SpeedMbps: 10000},
	}}
	ok, detail := tunnel.SNMPVerdict(tunnel.SNMPQuery{Watch: []int{2}}, ans)
	res := tunnel.AttemptResult{Kind: tunnel.CheckKindSNMP, OK: ok, Detail: detail, MS: 40, SNMP: ans}
	if !ok {
		res.Fail = tunnel.SNMPFailInterface
	}
	e.mu.Lock()
	e.answer, e.err = res, ""
	e.mu.Unlock()
}

func (e *deviceEnv) round(t *testing.T, at time.Time) NetworkDevice {
	t.Helper()
	d, err := e.api.store.NetworkDevice(e.ctx, e.device.ID)
	if err != nil {
		t.Fatal(err)
	}
	r, ans := e.api.deviceRound(e.ctx, d)
	r.At = at
	e.api.recordDeviceRound(e.ctx, d, r, ans)
	if d, err = e.api.store.NetworkDevice(e.ctx, e.device.ID); err != nil {
		t.Fatal(err)
	}
	return d
}

func (e *deviceEnv) alerts(t *testing.T) []ConsoleAlert {
	t.Helper()
	rows, err := e.api.store.db(e.ctx).Query(e.ctx, `SELECT `+alertColumns+`
		FROM console_alerts a WHERE a.kind = 'website' AND a.audience = 'admins' ORDER BY a.seq`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := collectAlerts(rows)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A device is down when a watched interface stays down two rounds, is back
// when it comes up, and a restart is told but is not an outage.
func TestNetworkDeviceDownBackAndRestart(t *testing.T) {
	e := newDeviceEnv(t)
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

	e.says(100_000, true, 1_000)
	d := e.round(t, base)
	if d.State != "up" || d.Device == nil || len(d.Device.Interfaces) != 2 || d.Device.Name != "core-sw1" {
		t.Fatalf("first answer: %s %+v", d.State, d.Device)
	}
	if q := e.asked[0]; q.AuthPass != "auth-secret-1" || q.PrivPass != "priv-secret-1" || len(q.Watch) != 1 || q.Watch[0] != 2 {
		t.Errorf("the agent was sent %+v", q)
	}
	// Traffic since the last answer: 7,500 bytes in 60 seconds is 1,000 bits a second.
	e.says(100_060, true, 8_500)
	d = e.round(t, base.Add(time.Minute))
	if in := d.Device.Interfaces[0]; in.InBps == nil || *in.InBps != 1000 || !d.Device.Interfaces[1].Watched {
		t.Errorf("rate = %v, watched = %v", in.InBps, d.Device.Interfaces[1].Watched)
	}

	// The uplink goes down: one round is a blip, two is down.
	e.says(100_120, false, 9_000)
	if d = e.round(t, base.Add(2*time.Minute)); d.State != "up" || d.FailingRounds != 1 {
		t.Fatalf("one failed round: %s %d", d.State, d.FailingRounds)
	}
	d = e.round(t, base.Add(3*time.Minute))
	if d.State != "down" || !strings.Contains(d.LastDetail, "Gi0/2 (uplink) is down") {
		t.Fatalf("two failed rounds: %s %q", d.State, d.LastDetail)
	}
	al := e.alerts(t)
	if len(al) != 1 || al[0].Severity != "critical" || al[0].Subject != deviceSubject(d.ID) || !strings.Contains(al[0].Link, "/network-devices") {
		t.Fatalf("down alerts = %+v", al)
	}

	// Back up, and the down alert closes.
	e.says(100_240, true, 9_500)
	if d = e.round(t, base.Add(4*time.Minute)); d.State != "up" {
		t.Fatalf("back: %s", d.State)
	}
	for _, a := range e.alerts(t) {
		if a.Subject != "" && a.ClosedAt == nil {
			t.Errorf("the down alert is still open: %+v", a)
		}
	}

	// It restarts: uptime goes back. Told, not an outage.
	e.says(30, true, 10)
	d = e.round(t, base.Add(5*time.Minute))
	if d.State != "up" || d.Device.RestartedAt == nil {
		t.Fatalf("after a restart: %s %+v", d.State, d.Device)
	}
	if in := d.Device.Interfaces[0]; in.InBps != nil {
		t.Errorf("a rate across a restart: %v", *in.InBps)
	}
	al = e.alerts(t)
	if last := al[len(al)-1]; !strings.HasSuffix(last.Title, "restarted") || last.Severity != "warning" {
		t.Errorf("restart alert = %+v", last)
	}
	outages, _ := e.api.store.WebsiteOutages(e.ctx, d.ID, 10)
	if len(outages) != 1 {
		t.Errorf("outages = %d, want only the uplink's", len(outages))
	}
}

// No answer is down; an agent that cannot be asked, or is too old, is
// unknown, not down.
func TestNetworkDeviceNoAnswerAndUnaskableAgent(t *testing.T) {
	e := newDeviceEnv(t)
	base := time.Now().UTC().Add(-time.Hour)
	e.mu.Lock()
	e.answer = tunnel.AttemptResult{Kind: tunnel.CheckKindSNMP, Fail: tunnel.SNMPFailNoAnswer, Detail: "10.20.0.2 did not answer SNMP on port 161", MS: 10000}
	e.mu.Unlock()
	e.round(t, base)
	d := e.round(t, base.Add(time.Minute))
	if d.State != "down" || !strings.Contains(d.LastDetail, "did not answer") || d.Device != nil {
		t.Fatalf("no answer: %s %q", d.State, d.LastDetail)
	}

	f := newDeviceEnv(t)
	f.mu.Lock()
	f.err = "its agent needs updating to check network devices; update it from its Settings tab"
	f.mu.Unlock()
	d = f.round(t, base)
	d = f.round(t, base.Add(time.Minute))
	if d.State == "down" || d.LastOutcome != RoundUnknown || !strings.Contains(d.LastDetail, "needs updating") {
		t.Fatalf("too old an agent: %s %s %q", d.State, d.LastOutcome, d.LastDetail)
	}
}

// Over the API: credentials are never shown back, an empty field keeps the
// saved one, Test finds the interfaces, and someone given the device sees
// it without how it is read.
func TestNetworkDevicesOverTheAPI(t *testing.T) {
	h := newTenantHarness(t)
	h.api.Tunnels = testGrantHub(t)
	tn, owner, pw := newTenant(t, h.store)
	ctx := WithTenant(context.Background(), tn)
	admin := h.signIn(t, tn.Slug, owner, pw)
	asset, err := h.store.CreateAsset(ctx, AssetInput{Hostname: unique("edge") + ".example.com"})
	if err != nil {
		t.Fatal(err)
	}
	var sent []tunnel.SNMPQuery
	var mu sync.Mutex
	h.api.deviceAsk = func(_ context.Context, host string, g tunnel.GrantCheck) deviceOpinion {
		mu.Lock()
		sent = append(sent, *g.SNMP)
		mu.Unlock()
		return deviceOpinion{host: host, res: tunnel.AttemptResult{Kind: "snmp", OK: true, Detail: "fw1 answered", SNMP: &tunnel.SNMPAnswer{
			Name: "fw1", UptimeSec: 50, Interfaces: []tunnel.SNMPInterface{{Index: 1, Name: "port1", Up: true}, {Index: 4, Name: "wan", Up: true}}}}}
	}

	// Test before saving: the interfaces to choose from.
	form := `{"name":"Firewall","host":"192.168.1.1","user":"argus","authProto":"SHA256","authPass":"auth-pass-99","privProto":"AES","privPass":"priv-pass-99","checkFrom":["` + asset.ID + `"]`
	code, body := h.call(t, admin, "", http.MethodPost, "/api/v1/network-devices/test", form+`}`)
	if code != http.StatusOK || !strings.Contains(body, `"name":"wan"`) || !strings.Contains(body, `"from":"`+asset.Hostname) {
		t.Fatalf("test = %d %s", code, body)
	}
	if code, body := h.call(t, admin, "", http.MethodPost, "/api/v1/network-devices", `{"host":"192.168.1.1","version":"v2c","checkFrom":["`+asset.ID+`"]}`); code != http.StatusBadRequest || !strings.Contains(body, "community") {
		t.Errorf("v2c without a community = %d %s", code, body)
	}
	code, body = h.call(t, admin, "", http.MethodPost, "/api/v1/network-devices", form+`,"watch":[{"index":4,"name":"wan"}]}`)
	if code != http.StatusCreated {
		t.Fatalf("add = %d %s", code, body)
	}
	var d NetworkDevice
	_ = json.Unmarshal([]byte(body), &d)
	for _, b := range []string{body, func() string { _, b := h.call(t, admin, "", http.MethodGet, "/api/v1/network-devices", ""); return b }()} {
		if strings.Contains(b, "auth-pass-99") || strings.Contains(b, "priv-pass-99") || !strings.Contains(b, `"hasSecret":true`) {
			t.Errorf("credentials shown back, or not saved: %s", b)
		}
	}
	var sealed []byte
	_ = h.store.db(ctx).QueryRow(ctx, `SELECT secrets_sealed FROM checks WHERE id::text = $1`, d.ID).Scan(&sealed)
	if len(sealed) == 0 || strings.Contains(string(sealed), "auth-pass") {
		t.Errorf("credentials not sealed: %q", sealed)
	}

	// An edit leaving the passwords empty keeps them.
	code, body = h.call(t, admin, "", http.MethodPut, "/api/v1/network-devices/"+d.ID,
		`{"name":"Edge firewall","host":"192.168.1.1","user":"argus","authProto":"SHA256","privProto":"AES","everySeconds":300,"checkFrom":["`+asset.ID+`"]}`)
	if code != http.StatusOK {
		t.Fatalf("edit = %d %s", code, body)
	}
	saved, _ := h.store.NetworkDevice(ctx, d.ID)
	if q, err := h.api.deviceQuery(ctx, saved); err != nil || q.AuthPass != "auth-pass-99" || q.PrivPass != "priv-pass-99" || saved.EverySeconds != 300 {
		t.Errorf("after an edit: %+v %v", q, err)
	}
	// A different user does not inherit the old one's passwords.
	if code, body := h.call(t, admin, "", http.MethodPut, "/api/v1/network-devices/"+d.ID,
		`{"host":"192.168.1.1","user":"other","authProto":"SHA256","privProto":"AES","checkFrom":["`+asset.ID+`"]}`); code != http.StatusBadRequest {
		t.Errorf("new user, no password = %d %s", code, body)
	}

	// Someone given it sees it, limited, and cannot change it.
	email := unique("netops") + "@northwind.id"
	if _, err := h.store.CreateAccount(ctx, email, "Net", "operator", "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	if code, body := h.call(t, admin, "", http.MethodPut, "/api/v1/checks/"+d.ID+"/viewers", `[{"email":"`+email+`","canOperate":true}]`); code != http.StatusOK {
		t.Fatalf("give = %d %s", code, body)
	}
	netops := h.signIn(t, tn.Slug, email, "correct horse battery staple")
	code, body = h.call(t, netops, "", http.MethodGet, "/api/v1/network-devices", "")
	if code != http.StatusOK || !strings.Contains(body, "Edge firewall") || !strings.Contains(body, `"limited":true`) || strings.Contains(body, `"user":"argus"`) || strings.Contains(body, asset.Hostname) {
		t.Errorf("given list = %d %s", code, body)
	}
	if code, _ := h.call(t, netops, "", http.MethodPost, "/api/v1/network-devices/"+d.ID+"/pause", `{"paused":true}`); code != http.StatusNoContent {
		t.Errorf("operator pausing = %d", code)
	}
	if code, _ := h.call(t, netops, "", http.MethodDelete, "/api/v1/network-devices/"+d.ID, ""); code != http.StatusForbidden {
		t.Errorf("operator removing = %d, want 403", code)
	}
	// Websites stay websites.
	if _, body := h.call(t, admin, "", http.MethodGet, "/api/v1/websites", ""); strings.Contains(body, "Edge firewall") {
		t.Errorf("a device in the websites list: %s", body)
	}
	evs, _ := h.store.AuditEvents(ctx, 50, 0)
	var audit []string
	for _, ev := range evs {
		audit = append(audit, ev.Action+": "+ev.Detail)
		if strings.Contains(ev.Detail, "pass-99") {
			t.Errorf("a credential in the audit log: %s", ev.Detail)
		}
	}
	if j := strings.Join(audit, "\n"); !strings.Contains(j, "device.add") || !strings.Contains(j, "device.change") || !strings.Contains(j, "device.pause") {
		t.Errorf("audit = %s", j)
	}
	if code, _ := h.call(t, admin, "", http.MethodDelete, "/api/v1/network-devices/"+d.ID, ""); code != http.StatusNoContent {
		t.Errorf("remove = %d", code)
	}
}

// A 32-bit counter going back is no rate; uptime wrapping is no restart.
func TestNextDeviceInfo(t *testing.T) {
	at := time.Now()
	prev := &DeviceInfo{At: at.Add(-time.Minute), UptimeSec: 1000, Interfaces: []DeviceInterface{{SNMPInterface: tunnel.SNMPInterface{Index: 1, InOctets: 500, InErrors: 1}}}}
	info, restarted := nextDeviceInfo(prev, &tunnel.SNMPAnswer{UptimeSec: 1060, Interfaces: []tunnel.SNMPInterface{{Index: 1, InOctets: 100, InErrors: 4}}}, at, nil)
	if restarted || info.Interfaces[0].InBps != nil || info.Interfaces[0].ErrorsUp != 3 {
		t.Errorf("%v %+v", restarted, info.Interfaces[0])
	}
	if _, restarted := nextDeviceInfo(prev, &tunnel.SNMPAnswer{UptimeSec: 1030}, at, nil); restarted {
		t.Error("a small step back is a clock, not a restart")
	}
}
