package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/curve25519"
)

/*
WireGuard networks between servers, set up from the console.

The console says which Argus networks this server belongs to: for each, the
interface, its address, the port it listens on and its peers (their public
keys, where to reach them and what they carry). The agent does the rest, in
two halves like log cleanup (logclean.go):

  - The daemon makes this server's key pair for each network the first time
    it hears of it. The private key is written to the agent's state folder,
    readable by root alone, and never leaves the server; the heartbeat
    carries the public key. When what the console asks for differs from what
    was last applied, the daemon writes a plan and starts the helper.

  - The helper, `argus-agent wgnet-apply`, runs as root in a transient
    systemd unit (wgnet_linux.go). It writes each interface's file under
    /etc/wireguard, brings it up, opens its UDP port in ufw or firewalld when
    asked, and then waits: the change is a trial until the daemon's next
    heartbeat reaches the control plane and the daemon says so. If that does
    not happen within two minutes, the helper puts back the files it
    replaced, brings the old interfaces back, closes the ports it opened,
    and reports that it rolled back. A change that cut this server off from
    Argus undoes itself.

What it will not do:

  - Touch an interface it did not make. Its interfaces are named argus and
    six hex digits, and only files of that name are written or removed.
  - Route everything through a network. A peer's allowed addresses must be
    inside the network's own range, or a private range behind a site's
    gateway (a site link: an office LAN, a cloud VPC). Never the default
    route, never a public range, and never a range holding the control
    plane's address, so the agent's own connection to Argus never moves
    into a network it manages.
  - Read or report a private key, its own or another's.
*/

// WGNetSpec is one Argus network as this server should have it.
type WGNetSpec struct {
	// ID is the network's id.
	ID string `json:"id"`
	// Name is what the console calls it, for the file's comment.
	Name string `json:"name"`
	// Iface is argus and six hex digits.
	Iface string `json:"iface"`
	// Address is this server's address in the network, with the network's
	// prefix length: 10.88.0.3/24.
	Address    string `json:"address"`
	ListenPort int    `json:"listen_port"`
	// Forward is set on a hub, which passes traffic between spokes.
	Forward bool `json:"forward,omitempty"`
	// OpenPort asks for ListenPort/udp to be opened in ufw or firewalld.
	OpenPort bool        `json:"open_port,omitempty"`
	Peers    []WGNetPeer `json:"peers"`
}

// WGNetPeer is another member of the network.
type WGNetPeer struct {
	PublicKey  string   `json:"public_key"`
	Endpoint   string   `json:"endpoint,omitempty"`
	AllowedIPs []string `json:"allowed_ips"`
	Keepalive  int      `json:"keepalive,omitempty"`
}

// WGNetOrders is what the console says on a heartbeat reply. Networks is
// the whole set: a network missing from it is taken down.
type WGNetOrders struct {
	Networks []WGNetSpec `json:"networks"`
}

// WGNet run states.
const (
	WGNetApplying   = "applying"
	WGNetTrial      = "trial"
	WGNetApplied    = "applied"
	WGNetFailed     = "failed"
	WGNetRolledBack = "rolled-back"
)

// WGNetRun is how the last change went.
type WGNetRun struct {
	ID string `json:"id"`
	// Want is the fingerprint of the set it applies.
	Want    string    `json:"want"`
	State   string    `json:"state"`
	Message string    `json:"message,omitempty"`
	Unit    string    `json:"unit,omitempty"`
	At      time.Time `json:"at"`
	// Networks is each network the run applied, by id.
	Networks []string `json:"networks,omitempty"`
}

// Finished reports whether the helper is done with the run.
func (r WGNetRun) Finished() bool {
	switch r.State {
	case WGNetApplied, WGNetFailed, WGNetRolledBack:
		return true
	}
	return false
}

// WGNetBeat is what rides the heartbeat.
type WGNetBeat struct {
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
	// Keys is this server's public key for each network it was told of.
	Keys map[string]string `json:"keys,omitempty"`
	Run  *WGNetRun         `json:"run,omitempty"`
}

// WGNetPlan is what the helper is handed.
type WGNetPlan struct {
	Run WGNetRun `json:"run"`
	// Avoid is the control plane's addresses, which no peer may carry.
	Avoid    []string    `json:"avoid,omitempty"`
	StateDir string      `json:"state_dir"`
	Networks []WGNetSpec `json:"networks"`
	// TrialSeconds is how long the helper waits to hear the change did not
	// cut the server off from Argus.
	TrialSeconds int `json:"trial_seconds"`
}

const (
	wgnetDir      = "wgnet"
	wgnetRunFile  = "wgnet-run.json"
	wgnetTrial    = 2 * time.Minute
	wgnetMaxNets  = 16
	wgnetMaxPeers = 256
)

var (
	reWGIface = regexp.MustCompile(`^argus[0-9a-f]{6}$`)
	reWGName  = regexp.MustCompile(`^[\p{L}\p{N} ._()-]{1,80}$`)
	reWGHost  = regexp.MustCompile(`^[A-Za-z0-9.-]{1,253}$`)
)

type wgnetPlatform interface {
	// supported says why networks cannot be set up here, or "".
	supported() string
	launch(ctx context.Context, planPath, unit string) error
	active(unit string) bool
}

// WGNets is the daemon's half.
type WGNets struct {
	StateDir string
	Log      *slog.Logger
	// Changed is called when there is something new to report.
	Changed func()
	// Avoid is the control plane's addresses: a peer carrying one would
	// move the agent's own connection into the network. Nil when unknown.
	Avoid func() []netip.Addr

	platform wgnetPlatform
	now      func() time.Time

	mu     sync.Mutex
	loaded bool
	keys   map[string]string
	want   []WGNetSpec
	run    WGNetRun
	// confirmed is the run a heartbeat sent during its trial confirmed.
	confirmed string
}

// NewWGNets builds the daemon's half for this host.
func NewWGNets(stateDir, executable string, log *slog.Logger) *WGNets {
	if log == nil {
		log = slog.Default()
	}
	return &WGNets{StateDir: stateDir, Log: log, platform: hostWGNetPlatform(executable), now: time.Now}
}

// Beat is what to put on the next heartbeat.
func (w *WGNets) Beat() WGNetBeat {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.loadLocked()
	b := WGNetBeat{}
	if reason := w.platform.supported(); reason != "" {
		b.Reason = reason
	} else {
		b.Supported = true
	}
	if len(w.keys) > 0 {
		b.Keys = map[string]string{}
		for id, k := range w.keys {
			b.Keys[id] = k
		}
	}
	if w.run.ID != "" {
		r := w.run
		b.Run = &r
	}
	return b
}

// Delivered says a heartbeat carrying b reached the control plane. One
// sent while a change was on trial proves the change left this server
// able to reach Argus, so the helper may keep it.
func (w *WGNets) Delivered(b WGNetBeat) {
	if b.Run == nil || b.Run.State != WGNetTrial {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.confirmed == b.Run.ID {
		return
	}
	if err := writeFileAtomic(w.confirmPath(b.Run.ID), []byte(b.Run.ID), 0o600); err != nil {
		w.Log.Error("wireguard: cannot confirm the change", "error", err)
		return
	}
	w.confirmed = b.Run.ID
}

func (w *WGNets) confirmPath(id string) string {
	return filepath.Join(w.StateDir, wgnetDir, "confirm-"+id)
}

// Handle acts on the console's orders. o is nil from a control plane older
// than server networks, which leaves everything as it is.
func (w *WGNets) Handle(ctx context.Context, o *WGNetOrders) {
	if o == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.loadLocked()
	specs := o.Networks
	if len(specs) > wgnetMaxNets {
		w.Log.Error("wireguard: refusing more networks than an agent sets up", "count", len(specs))
		return
	}
	avoid := w.avoid()
	for _, s := range specs {
		if err := validWGNetSpec(s, avoid); err != nil {
			w.Log.Error("wireguard: refusing the console's networks", "network", s.ID, "error", err)
			return
		}
	}
	// A key for every network first: the others cannot add this server as a
	// peer until its public key has been reported.
	newKey := false
	for _, s := range specs {
		if _, ok := w.keys[s.ID]; ok {
			continue
		}
		pub, err := w.makeKeyLocked(s.ID)
		if err != nil {
			w.Log.Error("wireguard: cannot make a key", "network", s.ID, "error", err)
			return
		}
		w.keys[s.ID] = pub
		newKey = true
	}
	if newKey {
		defer w.changedAsync()
	}
	w.want = specs
	want := wgnetFingerprint(specs)
	// A change that failed or rolled back is not tried again until the
	// console asks for something different.
	if want == w.run.Want {
		return
	}
	if w.run.ID != "" && !w.run.Finished() {
		return
	}
	if len(specs) == 0 && w.run.ID == "" {
		return
	}
	w.startLocked(ctx, specs, want)
}

func (w *WGNets) avoid() []netip.Addr {
	if w.Avoid == nil {
		return nil
	}
	return w.Avoid()
}

func (w *WGNets) changedAsync() {
	if w.Changed != nil {
		go w.Changed()
	}
}

func (w *WGNets) startLocked(ctx context.Context, specs []WGNetSpec, want string) {
	now := w.now().UTC()
	id := now.Format("20060102T150405") + "-" + want[:8]
	run := WGNetRun{ID: id, Want: want, State: WGNetApplying, Message: "starting", At: now}
	for _, s := range specs {
		run.Networks = append(run.Networks, s.ID)
	}
	fail := func(msg string) {
		run.State, run.Message, run.Unit = WGNetFailed, msg, ""
		w.setRunLocked(run)
	}
	if reason := w.platform.supported(); reason != "" {
		fail(reason)
		return
	}
	dir := filepath.Join(w.StateDir, wgnetDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fail("cannot prepare the change: " + err.Error())
		return
	}
	run.Unit = "argus-wgnet-" + strings.ReplaceAll(id, "-", "")[:20]
	planPath := filepath.Join(dir, "plan-"+id+".json")
	plan := WGNetPlan{Run: run, StateDir: w.StateDir, Networks: specs, TrialSeconds: int(wgnetTrial / time.Second)}
	for _, a := range w.avoid() {
		plan.Avoid = append(plan.Avoid, a.String())
	}
	if err := writeFileAtomic(planPath, mustJSON(plan), 0o600); err != nil {
		fail("cannot prepare the change: " + err.Error())
		return
	}
	w.setRunLocked(run)
	if err := w.platform.launch(ctx, planPath, run.Unit); err != nil {
		fail("cannot start the change: " + err.Error())
	}
}

func (w *WGNets) setRunLocked(r WGNetRun) {
	w.run = r
	if err := writeFileAtomic(filepath.Join(w.StateDir, wgnetRunFile), mustJSON(r), 0o600); err != nil {
		w.Log.Error("wireguard: cannot keep the change's state", "error", err)
	}
}

func (w *WGNets) loadLocked() {
	if w.loaded {
		return
	}
	w.loaded = true
	w.keys = map[string]string{}
	if b, err := os.ReadFile(filepath.Join(w.StateDir, wgnetRunFile)); err == nil {
		_ = json.Unmarshal(b, &w.run)
	}
	entries, _ := os.ReadDir(filepath.Join(w.StateDir, wgnetDir))
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".pub")
		if !ok {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(w.StateDir, wgnetDir, e.Name())); err == nil {
			w.keys[id] = strings.TrimSpace(string(b))
		}
	}
}

// makeKeyLocked makes this server's key pair for a network. The private
// key goes to a file only root and the agent can read; the public key is
// kept beside it to report.
func (w *WGNets) makeKeyLocked(network string) (string, error) {
	if !reUUIDish.MatchString(network) {
		return "", errors.New("malformed network id")
	}
	dir := filepath.Join(w.StateDir, wgnetDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	priv, pub, err := wgKeyPair()
	if err != nil {
		return "", err
	}
	if err := writeFileAtomic(filepath.Join(dir, network+".key"), []byte(priv+"\n"), 0o600); err != nil {
		return "", err
	}
	if err := writeFileAtomic(filepath.Join(dir, network+".pub"), []byte(pub+"\n"), 0o600); err != nil {
		return "", err
	}
	return pub, nil
}

var reUUIDish = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// wgKeyPair makes a WireGuard key pair, as `wg genkey | wg pubkey` would.
func wgKeyPair() (priv, pub string, err error) {
	var k [32]byte
	if _, err := rand.Read(k[:]); err != nil {
		return "", "", err
	}
	k[0] &= 248
	k[31] = (k[31] & 127) | 64
	p, err := curve25519.X25519(k[:], curve25519.Basepoint)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(k[:]), base64.StdEncoding.EncodeToString(p), nil
}

// Watch follows a change the helper is making. It returns when ctx ends.
func (w *WGNets) Watch(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		if w.follow() && w.Changed != nil {
			w.Changed()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// follow reads the helper's state and settles a run whose helper is gone.
// It reports whether anything changed.
func (w *WGNets) follow() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.loadLocked()
	cur := w.run
	if cur.ID == "" || cur.Finished() {
		return false
	}
	b, err := os.ReadFile(filepath.Join(w.StateDir, wgnetRunFile))
	if err != nil {
		return false
	}
	var disk WGNetRun
	if json.Unmarshal(b, &disk) != nil || disk.ID != cur.ID {
		return false
	}
	if !disk.Finished() && disk.Unit != "" && !w.platform.active(disk.Unit) && w.now().Sub(disk.At) > 30*time.Second {
		disk.State, disk.Message, disk.At = WGNetFailed, "the change stopped before it finished; check the server's WireGuard interfaces", w.now().UTC()
		w.setRunLocked(disk)
		return true
	}
	if disk.State == cur.State && disk.Message == cur.Message {
		return false
	}
	w.run = disk
	return true
}

/* ── Checks the daemon and the helper both make ─────────────────────────── */

// validWGNetSpec refuses anything that could reach outside the network or
// into the file's syntax.
func validWGNetSpec(s WGNetSpec, avoid []netip.Addr) error {
	if !reUUIDish.MatchString(s.ID) {
		return errors.New("malformed network id")
	}
	if !reWGIface.MatchString(s.Iface) {
		return fmt.Errorf("interface %q is not one Argus makes", s.Iface)
	}
	if s.Name != "" && !reWGName.MatchString(s.Name) {
		return errors.New("malformed network name")
	}
	addr, err := netip.ParsePrefix(s.Address)
	if err != nil || !addr.Addr().Is4() || addr.Bits() < 8 || addr.Bits() > 30 {
		return fmt.Errorf("address %q is not an IPv4 address with a /8 to /30 range", s.Address)
	}
	if s.ListenPort < 1024 || s.ListenPort > 65535 {
		return fmt.Errorf("port %d is out of range", s.ListenPort)
	}
	if len(s.Peers) > wgnetMaxPeers {
		return errors.New("too many peers")
	}
	network := addr.Masked()
	for _, p := range s.Peers {
		if !validWGKey(p.PublicKey) {
			return errors.New("malformed peer key")
		}
		if p.Endpoint != "" {
			host, port, err := net.SplitHostPort(p.Endpoint)
			n, perr := strconv.Atoi(port)
			if err != nil || perr != nil || n < 1 || n > 65535 || !reWGHost.MatchString(host) && net.ParseIP(host) == nil {
				return fmt.Errorf("peer endpoint %q is not host:port", p.Endpoint)
			}
		}
		if p.Keepalive < 0 || p.Keepalive > 3600 {
			return errors.New("peer keepalive out of range")
		}
		if len(p.AllowedIPs) == 0 || len(p.AllowedIPs) > 64 {
			return errors.New("a peer needs between 1 and 64 allowed ranges")
		}
		for _, a := range p.AllowedIPs {
			pfx, err := netip.ParsePrefix(a)
			if err != nil || !pfx.Addr().Is4() {
				return fmt.Errorf("allowed range %q is not IPv4", a)
			}
			// Inside the network's own range, or a private range a site
			// gateway routes: never the default route, never public.
			inside := pfx.Bits() >= network.Bits() && network.Contains(pfx.Addr())
			if !inside && !wgPrivateRange(pfx) {
				return fmt.Errorf("allowed range %s is outside the network %s and not a private range", a, network)
			}
			for _, c := range avoid {
				if pfx.Contains(c) {
					return fmt.Errorf("allowed range %s holds the control plane's address %s", a, c)
				}
			}
		}
	}
	return nil
}

var wgPrivateRanges = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("100.64.0.0/10"),
}

// wgPrivateRange is a range wholly inside one private block.
func wgPrivateRange(p netip.Prefix) bool {
	for _, b := range wgPrivateRanges {
		if p.Bits() >= b.Bits() && b.Contains(p.Addr()) {
			return true
		}
	}
	return false
}

// ControlAddrs resolves the control plane's host from its URL, at most
// every ten minutes, for WGNets.Avoid. On a lookup that fails it keeps the
// addresses it last had.
func ControlAddrs(controlURL string) func() []netip.Addr {
	var (
		mu   sync.Mutex
		at   time.Time
		last []netip.Addr
	)
	host := controlURL
	if u, err := url.Parse(controlURL); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	return func() []netip.Addr {
		mu.Lock()
		defer mu.Unlock()
		if host == "" || time.Since(at) < 10*time.Minute && last != nil {
			return last
		}
		if a, err := netip.ParseAddr(host); err == nil {
			last, at = []netip.Addr{a.Unmap()}, time.Now()
			return last
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
		if err == nil && len(ips) > 0 {
			last = last[:0:0]
			for _, ip := range ips {
				last = append(last, ip.Unmap())
			}
			at = time.Now()
		}
		return last
	}
}

func validWGKey(k string) bool {
	b, err := base64.StdEncoding.DecodeString(k)
	return err == nil && len(b) == 32
}

// wgnetFingerprint names a set of specs, the same however it is ordered.
func wgnetFingerprint(specs []WGNetSpec) string {
	s := append([]WGNetSpec(nil), specs...)
	sort.Slice(s, func(i, j int) bool { return s[i].ID < s[j].ID })
	sum := sha256.Sum256(mustJSON(s))
	return hex.EncodeToString(sum[:])
}

// wgConfig is an interface's file. The private key is read from the
// agent's state folder by the helper; it is never in a plan.
func wgConfig(s WGNetSpec, privateKey string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Made by Argus for the network %q. Changes here are replaced.\n", s.Name)
	b.WriteString("[Interface]\n")
	fmt.Fprintf(&b, "Address = %s\n", s.Address)
	fmt.Fprintf(&b, "ListenPort = %d\n", s.ListenPort)
	fmt.Fprintf(&b, "PrivateKey = %s\n", privateKey)
	if s.Forward {
		b.WriteString("PostUp = sysctl -q -w net.ipv4.ip_forward=1\n")
	}
	for _, p := range s.Peers {
		b.WriteString("\n[Peer]\n")
		fmt.Fprintf(&b, "PublicKey = %s\n", p.PublicKey)
		if p.Endpoint != "" {
			fmt.Fprintf(&b, "Endpoint = %s\n", p.Endpoint)
		}
		fmt.Fprintf(&b, "AllowedIPs = %s\n", strings.Join(p.AllowedIPs, ", "))
		if p.Keepalive > 0 {
			fmt.Fprintf(&b, "PersistentKeepalive = %d\n", p.Keepalive)
		}
	}
	return b.String()
}
