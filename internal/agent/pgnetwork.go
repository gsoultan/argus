package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
The agent's half of "which machines can connect" to a Postgres on this
server (the helper's half is pgnetwork_helper.go). The agent adds what the
postgres account cannot: the server's addresses and whether each answers,
the server's firewall, and restarting Postgres's service so a new
listen_addresses takes effect. Opening narrows the firewall before Postgres
listens anew; closing stops it listening before the firewall's rules go.
*/

// PGNetwork is which machines can connect to this Postgres, and why.
type PGNetwork struct {
	PGHBAState
	// Addresses are this server's; State is signin when Postgres answers
	// there, closed when it does not.
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
	// Service is the systemd unit Argus restarts, or empty when it found
	// none and the restart is the server's to do.
	Service string `json:"service,omitempty"`
}

// PGNetworkResult answers network.set.
type PGNetworkResult struct {
	Network     PGNetwork `json:"network"`
	FirewallRan []string  `json:"firewallRan,omitempty"`
	Restarted   bool      `json:"restarted,omitempty"`
	// RestartError says the restart was asked for and failed; Postgres
	// still listens as before.
	RestartError string `json:"restartError,omitempty"`
}

// pgRestarter restarts the service the postmaster pid runs in.
type pgRestarter func(ctx context.Context, unit string) error

func (c PostgresConfig) firewall() firewaller {
	if c.Firewall != nil {
		return c.Firewall
	}
	return hostFirewall(c.Executable, c.StateDir)
}

func (c PostgresConfig) restart() pgRestarter {
	if c.Restart != nil {
		return c.Restart
	}
	return func(ctx context.Context, unit string) error {
		if os.Geteuid() != 0 {
			return errors.New("the agent does not run as root here, so restart Postgres on the server")
		}
		rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		out, err := exec.CommandContext(rctx, "systemctl", "restart", unit).CombinedOutput()
		if err != nil {
			return fmt.Errorf("systemctl restart %s: %s", unit, strings.TrimSpace(string(out)))
		}
		return nil
	}
}

var unitInCgroup = regexp.MustCompile(`/([A-Za-z0-9@._:-]+\.service)(?:/|$)`)

// serviceOf is the systemd unit a process runs in, from its cgroup; a
// variable so a test can name one.
var serviceOf = func(pid int) string {
	if pid <= 0 {
		return ""
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if m := unitInCgroup.FindStringSubmatch(line); m != nil && strings.HasPrefix(m[1], "postgresql") {
			return m[1]
		}
	}
	return ""
}

// probePG says whether Postgres answers on ip:port.
func probePG(ctx context.Context, ip string, port int) (state, detail string) {
	d := net.Dialer{Timeout: 2 * time.Second}
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
	if err != nil {
		return "closed", "nothing listens there"
	}
	_ = c.Close()
	return "signin", ""
}

// listenReach is what listen_addresses lets in: every address, some, or
// only this server.
func listenReach(listen string) (any, lan bool) {
	for _, a := range strings.Split(listen, ",") {
		a = strings.TrimSpace(a)
		switch {
		case a == "*" || a == "0.0.0.0" || a == "::":
			any = true
		case a == "" || a == "localhost" || bindLoopback(a):
		default:
			lan = true
		}
	}
	return any, lan
}

// pgReach is who can connect, from listen_addresses, Argus's pg_hba.conf
// lines and the firewall.
func pgReach(n PGNetwork) string {
	any, lan := listenReach(n.Listen)
	switch {
	case !any && !lan:
		return tunnel.ReachLocal
	case n.FirewallActive && len(n.FirewallSources) > 0 && !n.FirewallAllowAll && !n.FirewallOpenToAll:
		return tunnel.ReachSelected
	case any && (n.OpenToAll || hasEveryone(n.Mine)):
		return tunnel.ReachAny
	}
	return tunnel.ReachNetwork
}

func hasEveryone(lines []string) bool {
	for _, l := range lines {
		if addr, _, ok := hbaLine(l); ok && hbaEveryone(addr) {
			return true
		}
	}
	return false
}

func (s *pgSession) localPG() bool {
	return strings.HasPrefix(s.db.Host, "/") || tunnel.LocalPGHost(s.db.Host)
}

func (s *pgSession) helperNetwork(ctx context.Context, op *tunnel.PGAdminOp) (PGHBAState, string, error) {
	res := s.admin(ctx, PGAdminRequest{Op: "admin", Database: s.db.Name, Socket: socketDir(s.db),
		Port: s.db.Port, AdminUser: s.cfg.AdminUser, Role: s.g.Principal, Owner: s.g.Email, Admin: op})
	if !res.OK {
		if res.Error == "" {
			res.Error = "could not read Postgres's settings"
		}
		return PGHBAState{}, res.SQL, errors.New(res.Error)
	}
	var st PGHBAState
	if err := json.Unmarshal(res.Data, &st); err != nil {
		return st, res.SQL, err
	}
	return st, res.SQL, nil
}

func (s *pgSession) networkView(ctx context.Context, st PGHBAState) PGNetwork {
	n := PGNetwork{PGHBAState: st, Addresses: hostAddresses(), Host: s.g.Host, Firewall: firewallFound()}
	for i := range n.Addresses {
		n.Addresses[i].State, n.Addresses[i].Detail = probePG(ctx, n.Addresses[i].IP, n.Port)
	}
	fw := s.cfg.firewall()(ctx, tunnel.FirewallRequest{Port: n.Port, Check: true})
	if fw.Error != "" {
		n.FirewallError = fw.Error
	} else {
		n.Firewall, n.FirewallActive = fw.Firewall, fw.Active
		n.FirewallSources, n.FirewallAllowAll = fw.Sources, fw.AllowAll
		n.FirewallOthers, n.FirewallOpenToAll = fw.Others, fw.OpenToAll
	}
	n.Service = serviceOf(n.PID)
	n.Reach = pgReach(n)
	return n
}

// network answers network and network.set: a read, or a change recorded
// before any of it runs.
func (s *pgSession) network(ctx context.Context, req tunnel.PGRequest) error {
	op := req.Admin
	fail := func(msg string) error {
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: msg})
	}
	if !s.localPG() || s.pooled {
		return fail(errNotLocalPG.Error())
	}
	actx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	st, _, err := s.helperNetwork(actx, &tunnel.PGAdminOp{Kind: tunnel.AdminNetwork})
	if err != nil {
		return fail(err.Error())
	}
	n := s.networkView(actx, st)
	if op.Kind == tunnel.AdminNetwork {
		b, _ := json.Marshal(n)
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "admin", Admin: b})
	}

	if err := tunnel.ValidatePGNetwork(op); err != nil {
		return fail(err.Error())
	}
	if op.Preview {
		b, _ := json.Marshal(PGAdminDone{SQL: tunnel.PGNetworkText(*op)})
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "admin", Admin: b})
	}
	if tunnel.PGAlwaysApproval(op) && req.Approval == "" {
		return fail("opening Postgres to anywhere runs once someone else approves it")
	}
	if op.Reach == tunnel.ReachAny && !strings.EqualFold(strings.TrimSpace(op.Confirm), s.g.Host) {
		return fail("type the server's name, " + s.g.Host + ", to open Postgres to anywhere")
	}
	if op.Reach == tunnel.ReachAny && len(st.Trust) > 0 {
		return fail("pg_hba.conf lets machines in with no password (" + strings.Join(st.Trust, "; ") + "); change that line first")
	}
	if n.FirewallError != "" && op.Reach != tunnel.ReachLocal {
		return fail("the firewall could not be read: " + n.FirewallError)
	}
	if op.Reach == tunnel.ReachSelected {
		if !n.FirewallActive {
			return fail("no firewall is on here, so Postgres cannot be limited to selected machines; choose the private network, or turn a firewall on first")
		}
		if n.FirewallOpenToAll {
			return fail("a firewall rule Argus did not add already lets everyone reach port " + strconv.Itoa(n.Port) + "; remove it on the server first")
		}
	}
	text := tunnel.PGNetworkText(*op)
	// Recorded before anything runs. No record, no change.
	if err := s.record(tunnel.PGRecord{Kind: "admin", Request: req.ID, SQL: text}); err != nil {
		return fmt.Errorf("the recording failed: %w", err)
	}
	start := time.Now()
	failed := func(msg string) error {
		if rerr := s.record(tunnel.PGRecord{Kind: "error", Request: req.ID, Error: msg, MS: time.Since(start).Milliseconds()}); rerr != nil {
			return rerr
		}
		return fail(msg)
	}
	var out PGNetworkResult
	fwDo := func() error {
		if !n.FirewallActive {
			return nil
		}
		fw := s.cfg.firewall()(actx, tunnel.FirewallRequest{Port: n.Port, Reach: op.Reach, Sources: op.Sources})
		out.FirewallRan = fw.Ran
		if fw.Error != "" {
			return errors.New("the firewall change failed: " + fw.Error)
		}
		return nil
	}
	pgDo := func() error {
		_, _, err := s.helperNetwork(actx, op)
		return err
	}
	steps := []func() error{fwDo, pgDo}
	if op.Reach == tunnel.ReachLocal {
		steps = []func() error{pgDo, fwDo}
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return failed(err.Error())
		}
	}
	after, _, err := s.helperNetwork(actx, &tunnel.PGAdminOp{Kind: tunnel.AdminNetwork})
	if err != nil {
		return failed(err.Error())
	}
	if after.PendingRestart && op.Restart {
		unit := serviceOf(after.PID)
		switch {
		case unit == "":
			out.RestartError = "Argus found no systemd service for Postgres here; restart it on the server"
		default:
			if err := s.cfg.restart()(actx, unit); err != nil {
				out.RestartError = err.Error()
			} else {
				out.Restarted = true
			}
		}
	}
	if out.Restarted {
		// The restart ended this session's own connection; the view is read
		// once Postgres answers again.
		for i := 0; i < 20; i++ {
			if again, _, err := s.helperNetwork(actx, &tunnel.PGAdminOp{Kind: tunnel.AdminNetwork}); err == nil {
				after = again
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	out.Network = s.networkView(actx, after)
	if rerr := s.record(tunnel.PGRecord{Kind: "done", Request: req.ID, Tag: "NETWORK", MS: time.Since(start).Milliseconds()}); rerr != nil {
		return rerr
	}
	b, _ := json.Marshal(out)
	return s.send(tunnel.PGAnswer{ID: req.ID, Type: "admin", Admin: b, Tag: "NETWORK", MS: time.Since(start).Milliseconds()})
}
