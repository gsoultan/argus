package agent

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/curve25519"
)

const testNet = "11111111-2222-4333-8444-555555555555"

// testAvoid is where the control plane is, for the tests.
var testAvoid = []netip.Addr{netip.MustParseAddr("10.20.1.5")}

func testKey(t *testing.T) string {
	t.Helper()
	_, pub, err := wgKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

func testSpec(t *testing.T) WGNetSpec {
	return WGNetSpec{ID: testNet, Name: "db mesh", Iface: "argus111111", Address: "10.88.0.1/24", ListenPort: 51820, OpenPort: true,
		Peers: []WGNetPeer{{PublicKey: testKey(t), Endpoint: "203.0.113.5:51820", AllowedIPs: []string{"10.88.0.2/32"}, Keepalive: 25}}}
}

func TestWGKeyPairIsWireGuards(t *testing.T) {
	priv, pub, err := wgKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	k, _ := base64.StdEncoding.DecodeString(priv)
	if len(k) != 32 || k[0]&7 != 0 || k[31]&128 != 0 || k[31]&64 == 0 {
		t.Fatalf("private key not clamped: %x", k)
	}
	want, _ := curve25519.X25519(k, curve25519.Basepoint)
	if pub != base64.StdEncoding.EncodeToString(want) {
		t.Fatal("public key does not match")
	}
}

// Anything that could reach outside the network, or into the file's
// syntax, is refused.
func TestWGNetSpecRefuses(t *testing.T) {
	for name, change := range map[string]func(*WGNetSpec){
		"foreign interface":   func(s *WGNetSpec) { s.Iface = "wg0" },
		"default route":       func(s *WGNetSpec) { s.Peers[0].AllowedIPs = []string{"0.0.0.0/0"} },
		"public range":        func(s *WGNetSpec) { s.Peers[0].AllowedIPs = []string{"203.0.113.0/24"} },
		"control plane":       func(s *WGNetSpec) { s.Peers[0].AllowedIPs = []string{"10.20.0.0/16"} },
		"wider than private":  func(s *WGNetSpec) { s.Peers[0].AllowedIPs = []string{"10.0.0.0/7"} },
		"newline in endpoint": func(s *WGNetSpec) { s.Peers[0].Endpoint = "1.2.3.4:51820\nPostUp = rm -rf /" },
		"newline in name":     func(s *WGNetSpec) { s.Name = "x\nPostUp = id" },
		"bad key":             func(s *WGNetSpec) { s.Peers[0].PublicKey = "abc" },
		"low port":            func(s *WGNetSpec) { s.ListenPort = 22 },
		"ipv6":                func(s *WGNetSpec) { s.Address = "fd00::1/64" },
	} {
		s := testSpec(t)
		change(&s)
		if validWGNetSpec(s, testAvoid) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := validWGNetSpec(testSpec(t), testAvoid); err != nil {
		t.Fatalf("a good spec refused: %v", err)
	}
	// A site link: a peer carries the office LAN behind it.
	s := testSpec(t)
	s.Peers[0].AllowedIPs = append(s.Peers[0].AllowedIPs, "192.168.10.0/24")
	if err := validWGNetSpec(s, testAvoid); err != nil {
		t.Fatalf("a site's LAN refused: %v", err)
	}
}

func TestWGConfig(t *testing.T) {
	s := testSpec(t)
	s.Forward = true
	c := wgConfig(s, "PRIV")
	for _, want := range []string{"Address = 10.88.0.1/24", "ListenPort = 51820", "PrivateKey = PRIV", "net.ipv4.ip_forward=1",
		"Endpoint = 203.0.113.5:51820", "AllowedIPs = 10.88.0.2/32", "PersistentKeepalive = 25"} {
		if !strings.Contains(c, want) {
			t.Errorf("missing %q in\n%s", want, c)
		}
	}
}

type fakeWGOps struct {
	tools  bool
	confs  map[string]string
	upIf   map[string]bool
	opened map[int]bool
	failUp string
	log    []string
	// IPsec
	swanTools  bool
	swan       map[string]string
	reloads    int
	failReload bool
}

func newFakeWGOps() *fakeWGOps {
	return &fakeWGOps{tools: true, swanTools: true, confs: map[string]string{}, upIf: map[string]bool{}, opened: map[int]bool{},
		swan: map[string]string{}}
}

func (f *fakeWGOps) haveTools() bool { return f.tools }
func (f *fakeWGOps) install(context.Context) error {
	f.tools = true
	f.log = append(f.log, "install")
	return nil
}
func (f *fakeWGOps) existing() ([]string, error) {
	var out []string
	for k := range f.confs {
		out = append(out, k)
	}
	return out, nil
}
func (f *fakeWGOps) readConf(i string) (string, bool, error) { c, ok := f.confs[i]; return c, ok, nil }
func (f *fakeWGOps) writeConf(i, t string) error             { f.confs[i] = t; return nil }
func (f *fakeWGOps) removeConf(i string) error               { delete(f.confs, i); return nil }
func (f *fakeWGOps) up(_ context.Context, i string) error {
	if i == f.failUp {
		return errors.New("RTNETLINK answers: Address already in use")
	}
	f.upIf[i] = true
	return nil
}
func (f *fakeWGOps) down(_ context.Context, i string) error { delete(f.upIf, i); return nil }
func (f *fakeWGOps) openPort(_ context.Context, p int) (bool, error) {
	f.opened[p] = true
	return true, nil
}
func (f *fakeWGOps) closePort(_ context.Context, p int) error { delete(f.opened, p); return nil }
func (f *fakeWGOps) readKey(string, string) (string, error)   { return "PRIV", nil }

func (f *fakeWGOps) haveSwan() bool { return f.swanTools }
func (f *fakeWGOps) installSwan(context.Context) error {
	f.swanTools = true
	f.log = append(f.log, "install strongswan")
	return nil
}
func (f *fakeWGOps) swanExisting() ([]string, error) {
	var out []string
	for k := range f.swan {
		out = append(out, k)
	}
	return out, nil
}
func (f *fakeWGOps) readSwan(c string) (string, bool, error) { t, ok := f.swan[c]; return t, ok, nil }
func (f *fakeWGOps) writeSwan(c, t string) error             { f.swan[c] = t; return nil }
func (f *fakeWGOps) removeSwan(c string) error               { delete(f.swan, c); return nil }
func (f *fakeWGOps) swanReload(context.Context) error {
	f.reloads++
	if f.failReload {
		return errors.New("loading connection 'argus-0a1b2c3d' failed")
	}
	return nil
}

func testPlan(t *testing.T, specs ...WGNetSpec) WGNetPlan {
	return WGNetPlan{Run: WGNetRun{ID: "run-1"}, StateDir: t.TempDir(), Networks: specs}
}

func TestWGNetApplyKeepsAConfirmedChange(t *testing.T) {
	f := newFakeWGOps()
	f.tools = false
	f.confs["argus999999"] = "old network"
	f.upIf["argus999999"] = true
	f.confs["wg0"] = "someone else's" // never touched
	state, msg := applyWGNet(context.Background(), f, testPlan(t, testSpec(t)), func(context.Context) bool { return true })
	if state != WGNetApplied {
		t.Fatalf("%s: %s", state, msg)
	}
	if !f.upIf["argus111111"] || !f.opened[51820] || f.log[0] != "install" {
		t.Errorf("not applied: %+v", f)
	}
	if _, ok := f.confs["argus999999"]; ok || f.upIf["argus999999"] {
		t.Error("a network no longer wanted is still there")
	}
	if f.confs["wg0"] != "someone else's" {
		t.Error("touched an interface Argus did not make")
	}
}

// A change that leaves the server unable to reach Argus undoes itself:
// the old file back, the old interface up, the port closed again.
func TestWGNetApplyRollsBackWithoutConfirmation(t *testing.T) {
	f := newFakeWGOps()
	f.confs["argus111111"] = "before"
	f.upIf["argus111111"] = true
	f.confs["argus999999"] = "removed one"
	f.upIf["argus999999"] = true
	state, msg := applyWGNet(context.Background(), f, testPlan(t, testSpec(t)), func(context.Context) bool { return false })
	if state != WGNetRolledBack || !strings.Contains(msg, "did not reach Argus") {
		t.Fatalf("%s: %s", state, msg)
	}
	if f.confs["argus111111"] != "before" || !f.upIf["argus111111"] {
		t.Errorf("not put back: %+v", f.confs)
	}
	if f.confs["argus999999"] != "removed one" || !f.upIf["argus999999"] {
		t.Errorf("a removed network was not put back: %+v", f)
	}
	if f.opened[51820] {
		t.Error("the port it opened is still open")
	}
}

func TestWGNetApplyUndoesAFailedStep(t *testing.T) {
	f := newFakeWGOps()
	f.failUp = "argus111111"
	state, msg := applyWGNet(context.Background(), f, testPlan(t, testSpec(t)), func(context.Context) bool { return true })
	if state != WGNetRolledBack || !strings.Contains(msg, "Address already in use") {
		t.Fatalf("%s: %s", state, msg)
	}
	if _, ok := f.confs["argus111111"]; ok {
		t.Error("the new file was left behind")
	}
}

type fakeWGPlatform struct{ launched []string }

func (p *fakeWGPlatform) supported() string { return "" }
func (p *fakeWGPlatform) launch(_ context.Context, plan, unit string) error {
	p.launched = append(p.launched, plan)
	return nil
}
func (p *fakeWGPlatform) active(string) bool { return true }
func (p *fakeWGPlatform) listSA(_ context.Context, conn string) (string, error) {
	return "argus-0a1b2c3d: #1, ESTABLISHED, IKEv2\n  argus-0a1b2c3d: #1, reqid 1, INSTALLED, TUNNEL", nil
}

func TestWGNetsMakeKeysAndStartOnce(t *testing.T) {
	dir := t.TempDir()
	p := &fakeWGPlatform{}
	w := &WGNets{StateDir: dir, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), platform: p, now: time.Now}
	spec := testSpec(t)
	w.Handle(context.Background(), &WGNetOrders{Networks: []WGNetSpec{spec}})
	w.Handle(context.Background(), &WGNetOrders{Networks: []WGNetSpec{spec}})
	if len(p.launched) != 1 {
		t.Fatalf("launched %d times", len(p.launched))
	}
	b := w.Beat()
	if !validWGKey(b.Keys[testNet]) || b.Run == nil || b.Run.State != WGNetApplying {
		t.Fatalf("beat %+v", b)
	}
	// The private key stays on the server, readable by its owner alone,
	// and is in neither the beat nor the plan.
	fi, err := os.Stat(filepath.Join(dir, wgnetDir, testNet+".key"))
	// Windows has no Unix modes; the networks are for Linux servers anyway.
	if err != nil || runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", fi, err)
	}
	priv, _ := os.ReadFile(filepath.Join(dir, wgnetDir, testNet+".key"))
	plan, _ := os.ReadFile(p.launched[0])
	if strings.Contains(string(plan), strings.TrimSpace(string(priv))) || strings.Contains(string(mustJSON(b)), strings.TrimSpace(string(priv))) {
		t.Fatal("the private key left its file")
	}

	// A heartbeat delivered during the trial confirms the change.
	run := *b.Run
	run.State = WGNetTrial
	w.Delivered(WGNetBeat{Run: &run})
	if got, err := os.ReadFile(w.confirmPath(run.ID)); err != nil || string(got) != run.ID {
		t.Fatalf("no confirmation: %v", err)
	}
}

func TestWGNetsRefuseABadSpec(t *testing.T) {
	p := &fakeWGPlatform{}
	w := &WGNets{StateDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), platform: p, now: time.Now}
	s := testSpec(t)
	s.Peers[0].AllowedIPs = []string{"0.0.0.0/0"}
	w.Handle(context.Background(), &WGNetOrders{Networks: []WGNetSpec{s}})
	if len(p.launched) != 0 || len(w.Beat().Keys) != 0 {
		t.Fatal("acted on a spec that routes everything")
	}
}

func TestControlAddrs(t *testing.T) {
	if got := ControlAddrs("https://10.20.1.5:8443")(); len(got) != 1 || got[0] != netip.MustParseAddr("10.20.1.5") {
		t.Fatalf("%v", got)
	}
	if got := ControlAddrs("")(); got != nil {
		t.Fatalf("%v", got)
	}
}

func testLink() IPsecSpec {
	return IPsecSpec{ID: "22222222-2222-4333-8444-555555555555", Name: "office to aws", Conn: "argus-0a1b2c3d", Peer: "52.1.2.3",
		LocalRanges: []string{"192.168.10.0/24"}, RemoteRanges: []string{"10.30.0.0/16"}, Proposals: "strong", PSK: "Abcdefghijklmnop1234._"}
}

func TestIPsecSpecRefuses(t *testing.T) {
	for name, change := range map[string]func(*IPsecSpec){
		"foreign conn":    func(s *IPsecSpec) { s.Conn = "office" },
		"public remote":   func(s *IPsecSpec) { s.RemoteRanges = []string{"0.0.0.0/0"} },
		"control plane":   func(s *IPsecSpec) { s.RemoteRanges = []string{"10.20.0.0/16"} },
		"public local":    func(s *IPsecSpec) { s.LocalRanges = []string{"52.0.0.0/8"} },
		"quote in key":    func(s *IPsecSpec) { s.PSK = `abcdefghijklmnop"; }` },
		"short key":       func(s *IPsecSpec) { s.PSK = "short" },
		"newline in peer": func(s *IPsecSpec) { s.Peer = "52.1.2.3\n}" },
		"weak proposals":  func(s *IPsecSpec) { s.Proposals = "3des-md5-modp1024" },
		"no ranges":       func(s *IPsecSpec) { s.LocalRanges = nil },
	} {
		l := testLink()
		change(&l)
		if validIPsecSpec(l, testAvoid) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := validIPsecSpec(testLink(), testAvoid); err != nil {
		t.Fatal(err)
	}
	c := swanConfig(testLink())
	for _, want := range []string{"remote_addrs = 52.1.2.3", "proposals = aes256gcm16-prfsha384-ecp384", "local_ts = 192.168.10.0/24",
		"remote_ts = 10.30.0.0/16", "start_action = start", `secret = "Abcdefghijklmnop1234._"`} {
		if !strings.Contains(c, want) {
			t.Errorf("missing %q in\n%s", want, c)
		}
	}
}

func TestWGNetApplyIPsec(t *testing.T) {
	f := newFakeWGOps()
	f.swanTools = false
	f.swan["argus-99999999"] = "old link"
	plan := testPlan(t)
	plan.IPsec = []IPsecSpec{testLink()}
	state, msg := applyWGNet(context.Background(), f, plan, func(context.Context) bool { return true })
	if state != WGNetApplied || !strings.Contains(f.swan["argus-0a1b2c3d"], "52.1.2.3") || f.reloads != 1 {
		t.Fatalf("%s: %s %+v", state, msg, f)
	}
	if _, ok := f.swan["argus-99999999"]; ok {
		t.Error("a link no longer wanted is still there")
	}

	// strongSwan refusing the file puts everything back.
	f = newFakeWGOps()
	f.swan["argus-0a1b2c3d"] = "before"
	f.failReload = true
	state, msg = applyWGNet(context.Background(), f, plan, func(context.Context) bool { return true })
	if state != WGNetRolledBack || f.swan["argus-0a1b2c3d"] != "before" || !strings.Contains(msg, "would not load") {
		t.Fatalf("%s: %s %+v", state, msg, f.swan)
	}
	// So does a trial nobody confirms.
	f = newFakeWGOps()
	state, _ = applyWGNet(context.Background(), f, plan, func(context.Context) bool { return false })
	if _, ok := f.swan["argus-0a1b2c3d"]; state != WGNetRolledBack || ok || f.reloads != 2 {
		t.Fatalf("%s %+v", state, f)
	}
}

func TestWGNetsIPsecBeat(t *testing.T) {
	p := &fakeWGPlatform{}
	w := &WGNets{StateDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), platform: p, now: time.Now}
	w.Handle(context.Background(), &WGNetOrders{IPsec: []IPsecSpec{testLink()}})
	if len(p.launched) != 1 {
		t.Fatal("not started")
	}
	b := w.Beat()
	if b.IPsec[testLink().ID] != IPsecUp {
		t.Fatalf("%+v", b.IPsec)
	}
	if strings.Contains(string(mustJSON(b)), testLink().PSK) {
		t.Fatal("the key is in the beat")
	}
}
