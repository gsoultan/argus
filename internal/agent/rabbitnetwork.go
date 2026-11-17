package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Which machines can connect to a RabbitMQ on this server (see
tunnel/rabbitnetwork.go). The agent asks the node through rabbitmqctl
(status, the guest user, connections), changes the AMQP listener's
listeners.tcp lines in rabbitmq.conf for the next start, changes the
firewall, and restarts the service when asked. Opening narrows the firewall
before the node listens anew; closing stops it listening before the
firewall's rules go.

Every listeners.tcp line of rabbitmq.conf is replaced by Argus's own, under
a comment that says so; the rest of the file stays as it was. A listener
set in advanced.config overrides rabbitmq.conf, so Argus refuses there.
*/

// RabbitNetwork is which machines can connect to this RabbitMQ, and why.
type RabbitNetwork struct {
	// Bind is what the AMQP listener binds now ("*" for every address);
	// Next what rabbitmq.conf says for the next start.
	Bind           string `json:"bind"`
	Next           string `json:"next"`
	PendingRestart bool   `json:"pendingRestart"`
	Port           int    `json:"port"`
	Version        string `json:"version"`
	ConfigFile     string `json:"configFile,omitempty"`
	// Advanced says advanced.config sets tcp_listeners, which overrides
	// rabbitmq.conf.
	Advanced bool `json:"advanced,omitempty"`
	TLS      bool `json:"tls"`
	// GuestRemote says the guest user signs in with its default password
	// and is not kept to the loopback.
	GuestRemote bool `json:"guestRemote,omitempty"`
	Clients     int  `json:"clients"`
	Remote      int  `json:"remote"`

	Addresses []RedisAddress `json:"addresses"`
	Reach     string         `json:"reach"`
	Host      string         `json:"host"`

	Firewall          string   `json:"firewall"`
	FirewallActive    bool     `json:"firewallActive"`
	FirewallSources   []string `json:"firewallSources,omitempty"`
	FirewallAllowAll  bool     `json:"firewallAllowAll,omitempty"`
	FirewallOthers    []string `json:"firewallOthers,omitempty"`
	FirewallOpenToAll bool     `json:"firewallOpenToAll,omitempty"`
	FirewallError     string   `json:"firewallError,omitempty"`
	Service           string   `json:"service,omitempty"`

	pid int
}

// RabbitNetworkResult answers network_set.
type RabbitNetworkResult struct {
	Network      RabbitNetwork `json:"network"`
	FirewallRan  []string      `json:"firewallRan,omitempty"`
	Restarted    bool          `json:"restarted,omitempty"`
	RestartError string        `json:"restartError,omitempty"`
}

// rabbitConfDefault is where RabbitMQ reads rabbitmq.conf when nothing
// says otherwise.
const rabbitConfDefault = "/etc/rabbitmq/rabbitmq.conf"

// rabbitArgusHeader marks the lines Argus writes in rabbitmq.conf.
const rabbitArgusHeader = "# Which machines can connect, set in Argus, which rewrites the listeners.tcp lines."

var rabbitTCPLine = regexp.MustCompile(`^\s*listeners\.tcp(\.[A-Za-z0-9_]+)?\s*=\s*(.*?)\s*$`)

// rabbitListenAddr is the address of one listeners.tcp value: "*" for a
// bare port, "default" or an unspecified address; "" for "none".
func rabbitListenAddr(v string) string {
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	if v == "none" {
		return ""
	}
	if _, err := strconv.Atoi(v); err == nil {
		return "*"
	}
	if ap, err := netip.ParseAddrPort(v); err == nil {
		if ap.Addr().IsUnspecified() {
			return "*"
		}
		return ap.Addr().String()
	}
	// host:port with a name.
	if h, _, ok := strings.Cut(v, ":"); ok && h != "" {
		if h == "localhost" {
			return "127.0.0.1"
		}
		return h
	}
	return "*"
}

// rabbitConfBind is what rabbitmq.conf says the AMQP listener binds: "*"
// when it names no listener, as RabbitMQ listens on every address then.
func rabbitConfBind(conf []byte) string {
	var addrs []string
	saw := false
	for _, l := range strings.Split(strings.ReplaceAll(string(conf), "\r\n", "\n"), "\n") {
		m := rabbitTCPLine.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		saw = true
		a := rabbitListenAddr(m[2])
		if a == "*" {
			return "*"
		}
		if a != "" && !slices.Contains(addrs, a) {
			addrs = append(addrs, a)
		}
	}
	if !saw {
		return "*"
	}
	return rabbitBindKey(addrs)
}

// rabbitBindKey is a set of addresses in one written form: the loopback
// first, the rest in order.
func rabbitBindKey(addrs []string) string {
	var lo, rest []string
	for _, a := range addrs {
		if a == "127.0.0.1" {
			lo = append(lo, a)
		} else {
			rest = append(rest, a)
		}
	}
	sort.Strings(rest)
	return strings.Join(append(lo, rest...), ",")
}

// setRabbitBind is rabbitmq.conf with every listeners.tcp line replaced by
// Argus's for bind, and every other line as it was.
func setRabbitBind(conf []byte, bind string, port int) []byte {
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(string(conf), "\r\n", "\n"), "\n") {
		if rabbitTCPLine.MatchString(l) || l == rabbitArgusHeader {
			continue
		}
		out = append(out, l)
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	if len(out) > 0 {
		out = append(out, "")
	}
	out = append(out, rabbitArgusHeader)
	if bind == "*" {
		out = append(out, "listeners.tcp.default = "+strconv.Itoa(port))
	} else {
		for i, a := range strings.Split(bind, ",") {
			out = append(out, fmt.Sprintf("listeners.tcp.%d = %s", i+1, netip.AddrPortFrom(netip.MustParseAddr(a), uint16(port))))
		}
	}
	return []byte(strings.Join(out, "\n") + "\n")
}

// rabbitStatus is the part of `rabbitmqctl status --formatter json` Argus
// reads.
type rabbitStatus struct {
	PID         int      `json:"pid"`
	Version     string   `json:"rabbitmq_version"`
	ConfigFiles []string `json:"config_files"`
	Listeners   []struct {
		Interface string `json:"interface"`
		Port      int    `json:"port"`
		Protocol  string `json:"protocol"`
	} `json:"listeners"`
}

// rabbitState reads how the node runs, through rabbitmqctl.
func (s *rabbitSession) rabbitState(ctx context.Context) (RabbitNetwork, error) {
	n := RabbitNetwork{Host: s.g.Host, Version: s.version, Port: 5672}
	ctl := s.f.cfg.Ctl
	out, err := ctl(ctx, []string{"status", "--formatter", "json"}, "")
	if err != nil {
		return n, fmt.Errorf("could not read how RabbitMQ runs: %w", err)
	}
	var st rabbitStatus
	if err := json.Unmarshal(out, &st); err != nil {
		return n, fmt.Errorf("could not read rabbitmqctl status: %w", err)
	}
	n.pid = st.PID
	if st.Version != "" {
		n.Version = st.Version
	}
	var addrs []string
	any := false
	for _, l := range st.Listeners {
		switch l.Protocol {
		case "amqp":
			n.Port = l.Port
			ip := strings.Trim(l.Interface, "[]")
			a, err := netip.ParseAddr(ip)
			switch {
			case err != nil || a.IsUnspecified():
				any = true
			case !slices.Contains(addrs, a.String()):
				addrs = append(addrs, a.String())
			}
		case "amqp/ssl":
			n.TLS = true
		}
	}
	n.Bind = rabbitBindKey(addrs)
	if any {
		n.Bind = "*"
	}
	for _, f := range st.ConfigFiles {
		switch {
		case strings.HasSuffix(f, "advanced.config"):
			if b, err := os.ReadFile(f); err == nil && bytes.Contains(b, []byte("tcp_listeners")) {
				n.Advanced = true
			}
		case strings.HasSuffix(f, ".conf") && n.ConfigFile == "":
			n.ConfigFile = f
		}
	}
	if n.ConfigFile == "" {
		if fi, err := os.Stat(filepath.Dir(s.f.confPath())); err == nil && fi.IsDir() {
			n.ConfigFile = s.f.confPath()
		}
	}
	n.Next = n.Bind
	if n.ConfigFile != "" {
		if b, err := os.ReadFile(n.ConfigFile); err == nil {
			n.Next = rabbitConfBind(b)
		} else if errors.Is(err, os.ErrNotExist) {
			n.Next = "*"
		}
	}
	n.PendingRestart = n.Next != n.Bind

	// The guest user, with its default password, kept to the loopback or not.
	if _, err := ctl(ctx, []string{"authenticate_user", "guest", "guest"}, ""); err == nil {
		lb, err := ctl(ctx, []string{"eval", "application:get_env(rabbit, loopback_users)."}, "")
		n.GuestRemote = err != nil || !bytes.Contains(lb, []byte(`<<"guest">>`))
	}
	if b, err := ctl(ctx, []string{"list_connections", "peer_host", "--silent", "--formatter", "json"}, ""); err == nil {
		var conns []struct {
			Peer string `json:"peer_host"`
		}
		if json.Unmarshal(b, &conns) == nil {
			n.Clients = len(conns)
			for _, c := range conns {
				if a, err := netip.ParseAddr(c.Peer); err == nil && !a.Unmap().IsLoopback() {
					n.Remote++
				}
			}
		}
	}
	return n, nil
}

func (f *rabbitFactory) confPath() string {
	if f.cfg.ConfPath != "" {
		return f.cfg.ConfPath
	}
	return rabbitConfDefault
}

func (s *rabbitSession) networkView(ctx context.Context, n RabbitNetwork) RabbitNetwork {
	n.Addresses = hostAddresses()
	n.Firewall = firewallFound()
	for i := range n.Addresses {
		n.Addresses[i].State, n.Addresses[i].Detail = probePG(ctx, n.Addresses[i].IP, n.Port)
	}
	fw := s.f.cfg.firewall()(ctx, tunnel.FirewallRequest{Port: n.Port, Check: true})
	if fw.Error != "" {
		n.FirewallError = fw.Error
	} else {
		n.Firewall, n.FirewallActive = fw.Firewall, fw.Active
		n.FirewallSources, n.FirewallAllowAll = fw.Sources, fw.AllowAll
		n.FirewallOthers, n.FirewallOpenToAll = fw.Others, fw.OpenToAll
	}
	n.Service = serviceOf(n.pid, "rabbitmq-server", "rabbitmq")
	any, lan := listenReach(n.Bind)
	switch {
	case !any && !lan:
		n.Reach = tunnel.ReachLocal
	case n.FirewallActive && len(n.FirewallSources) > 0 && !n.FirewallAllowAll && !n.FirewallOpenToAll:
		n.Reach = tunnel.ReachSelected
	case any:
		n.Reach = tunnel.ReachAny
	default:
		n.Reach = tunnel.ReachNetwork
	}
	return n
}

// network answers network and network_set.
func (s *rabbitSession) network(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if s.db.Level != tunnel.LevelDBA {
		return errAnswer("changing which machines can connect is DBA work; this session is at %s", s.db.Level)
	}
	actx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	st, err := s.rabbitState(actx)
	if err != nil {
		return errAnswer("%s", err)
	}
	n := s.networkView(actx, st)
	if req.Op == tunnel.RabbitOpNetwork {
		return tunnel.StoreAnswer{Type: "result", Data: n}, nil
	}

	op := tunnel.MongoNetOp(req)
	if err := tunnel.ValidatePGNetwork(&op); err != nil {
		return errAnswer("%s", err)
	}
	req.Sources = op.Sources
	text := tunnel.RabbitNetworkText(req)
	if (s.db.ChangesNeedApproval || req.Reach == tunnel.ReachAny) && req.Approval == "" {
		why := "on " + s.g.Host + " this change runs once someone else approves it"
		if req.Reach == tunnel.ReachAny {
			why = "opening RabbitMQ to anywhere runs once someone else approves it"
		}
		if err := rec.refused(req.ID, text, why); err != nil {
			return tunnel.StoreAnswer{}, err
		}
		return tunnel.StoreAnswer{Type: "error", Error: why, NeedsApproval: &tunnel.RedisApprovalNeed{Text: text}}, nil
	}
	if req.Reach == tunnel.ReachAny && !strings.EqualFold(strings.TrimSpace(req.Confirm), s.g.Host) {
		return errAnswer("type the server's name, %s, to open RabbitMQ to anywhere", s.g.Host)
	}
	if n.Advanced {
		return errAnswer("advanced.config sets RabbitMQ's tcp_listeners, which overrides rabbitmq.conf; change it there on the server")
	}
	if n.ConfigFile == "" {
		return errAnswer("Argus found no rabbitmq.conf, nor %s to write one in", filepath.Dir(s.f.confPath()))
	}
	if req.Reach != tunnel.ReachLocal && n.GuestRemote {
		return errAnswer("the guest user signs in with its default password and is not kept to this server; change its password or delete it first")
	}
	for _, a := range req.Addresses {
		if strings.Contains(a, ":") {
			return errAnswer("%s is an IPv6 address; choose an IPv4 address", a)
		}
	}
	if n.FirewallError != "" && req.Reach != tunnel.ReachLocal {
		return errAnswer("the firewall could not be read: %s", n.FirewallError)
	}
	if req.Reach == tunnel.ReachSelected {
		if !n.FirewallActive {
			return errAnswer("no firewall is on here, so RabbitMQ cannot be limited to selected machines; choose the private network, or turn a firewall on first")
		}
		if n.FirewallOpenToAll {
			return errAnswer("a firewall rule Argus did not add already lets everyone reach port %d; remove it on the server first", n.Port)
		}
	}
	old, err := os.ReadFile(n.ConfigFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errAnswer("could not read %s: %s", n.ConfigFile, err)
	}
	conf := setRabbitBind(old, tunnel.RabbitBindFor(req.Reach, req.Addresses), n.Port)

	// Recorded before anything runs. No record, no change.
	if err := rec.write(tunnel.PGRecord{Kind: "stmt", Request: req.ID, SQL: text, Approval: req.Approval}); err != nil {
		return tunnel.StoreAnswer{}, fmt.Errorf("the recording failed: %w", err)
	}
	start := time.Now()
	var out RabbitNetworkResult
	fwDo := func() error {
		if !n.FirewallActive {
			return nil
		}
		fw := s.f.cfg.firewall()(actx, tunnel.FirewallRequest{Port: n.Port, Reach: req.Reach, Sources: req.Sources})
		out.FirewallRan = fw.Ran
		if fw.Error != "" {
			return errors.New("the firewall change failed: " + fw.Error)
		}
		return nil
	}
	bindDo := func() error {
		if !bytes.Equal(conf, old) {
			if err := writeRabbitConf(n.ConfigFile, conf); err != nil {
				return err
			}
		}
		if !req.Restart {
			return nil
		}
		unit := serviceOf(st.pid, "rabbitmq-server", "rabbitmq")
		if unit == "" {
			out.RestartError = "Argus found no systemd service for RabbitMQ here; restart it on the server"
			return nil
		}
		if err := s.f.cfg.restart()(actx, unit); err != nil {
			out.RestartError = err.Error()
			return nil
		}
		out.Restarted = true
		return nil
	}
	steps := []func() error{fwDo, bindDo}
	if req.Reach == tunnel.ReachLocal {
		steps = []func() error{bindDo, fwDo}
	}
	for _, step := range steps {
		if err := step(); err != nil {
			if rerr := rec.fail(req.ID, err.Error(), time.Since(start).Milliseconds()); rerr != nil {
				return tunnel.StoreAnswer{}, rerr
			}
			return errAnswer("%s", err)
		}
	}
	after := st
	for i := 0; i < 30; i++ {
		if a, err := s.rabbitState(actx); err == nil {
			after = a
			break
		}
		time.Sleep(time.Second)
	}
	out.Network = s.networkView(actx, after)
	if err := rec.done(req.ID, "NETWORK", 1, time.Since(start).Milliseconds(), out, false); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	return tunnel.StoreAnswer{Type: "result", Data: out, MS: time.Since(start).Milliseconds(), Tag: "NETWORK"}, nil
}

// writeRabbitConf puts rabbitmq.conf in place: with the old file's owner
// and mode, or readable by the rabbitmq account's group when it is new.
func writeRabbitConf(path string, b []byte) error {
	fi, err := os.Stat(path)
	mode := os.FileMode(0o644)
	if err == nil {
		mode = fi.Mode().Perm()
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".argus-new")
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return fmt.Errorf("could not write beside %s: %w", path, err)
	}
	if fi != nil {
		if err := chownLike(tmp, fi); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
