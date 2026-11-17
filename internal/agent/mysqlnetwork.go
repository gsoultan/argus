package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Which machines can connect to a MySQL or MariaDB on this server (see
tunnel/mysqlnetwork.go). The agent reads and changes bind_address as the
server's MySQL administrator, keeps it for the next start (SET PERSIST_ONLY
on MySQL, an option file of its own on MariaDB), changes the firewall, and
restarts the service when asked. Opening narrows the firewall before mysqld
listens anew; closing stops it listening before the firewall's rules go.
*/

// MySQLNetwork is which machines can connect to this MySQL, and why.
type MySQLNetwork struct {
	// Bind is bind_address as mysqld runs now; Next what it reads at its
	// next start, and PendingRestart says they differ.
	Bind           string `json:"bind"`
	Next           string `json:"next"`
	PendingRestart bool   `json:"pendingRestart"`
	Port           int    `json:"port"`
	SkipNetworking bool   `json:"skipNetworking,omitempty"`
	Version        string `json:"version"`
	MariaDB        bool   `json:"mariadb"`
	// ListOK says bind_address takes a list of addresses (MySQL 8.0.13,
	// MariaDB 10.11); before that it is one address, or every one.
	ListOK bool `json:"listOK"`
	SSL    bool `json:"ssl"`
	// RemoteUsers may sign in from another machine ('user'@'host');
	// NoPassRemote are those of them that need no password.
	RemoteUsers  []string `json:"remoteUsers,omitempty"`
	NoPassRemote []string `json:"noPassRemote,omitempty"`
	// KeptIn is where the setting is kept for the next start.
	KeptIn  string `json:"keptIn,omitempty"`
	Clients int    `json:"clients"`
	Remote  int    `json:"remote"`

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

// MySQLNetworkResult answers network.set.
type MySQLNetworkResult struct {
	Network      MySQLNetwork `json:"network"`
	FirewallRan  []string     `json:"firewallRan,omitempty"`
	Restarted    bool         `json:"restarted,omitempty"`
	RestartError string       `json:"restartError,omitempty"`
}

// mysqlArgusCnf is MariaDB's option file of Argus's own, read after the
// server's ("zz" sorts last in a conf.d directory).
const mysqlArgusCnf = "zz-argus-network.cnf"

// mysqlConfDirs are where a server reads option files from, in the order
// Argus writes to the first that exists.
var mysqlConfDirs = []string{"/etc/mysql/mariadb.conf.d", "/etc/my.cnf.d", "/etc/mysql/conf.d"}

func (c MySQLConfig) root() string {
	if c.ConfRoot != "" {
		return c.ConfRoot
	}
	return "/"
}

func (c MySQLConfig) restart() pgRestarter {
	if c.Restart != nil {
		return c.Restart
	}
	return systemctlRestart
}

// mysqlConfDir is the directory Argus's option file goes in, or "".
func (c MySQLConfig) confDir() string {
	for _, d := range mysqlConfDirs {
		p := filepath.Join(c.root(), d)
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			return p
		}
	}
	return ""
}

var cnfBind = regexp.MustCompile(`(?m)^\s*bind[-_]address\s*=\s*(\S+)\s*$`)

// mysqlListOK says this server's bind_address takes a list.
func mysqlListOK(version string, maria bool) bool {
	v := version
	if i := strings.IndexAny(v, "-+ "); i >= 0 {
		v = v[:i]
	}
	p := strings.Split(v, ".")
	n := func(i int) int {
		if i >= len(p) {
			return 0
		}
		x, _ := strconv.Atoi(p[i])
		return x
	}
	major, minor, patch := n(0), n(1), n(2)
	if maria {
		return major > 10 || major == 10 && minor >= 11
	}
	return major > 8 || major == 8 && (minor > 0 || patch >= 13)
}

func bindKey(b string) string {
	parts := strings.Split(b, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return strings.Join(parts, ",")
}

func mysqlLocalHost(h string) bool {
	switch h {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// mysqlState reads what mysqld says about connections from other machines.
func (s *mysqlSession) mysqlState(ctx context.Context, a *mysqlAdmin) (MySQLNetwork, error) {
	n := MySQLNetwork{Version: a.version, MariaDB: a.mariaDB(), Host: s.g.Host}
	var pidFile string
	var skip int
	if err := a.db.QueryRowContext(ctx, "SELECT @@bind_address, @@port, @@skip_networking, @@pid_file").
		Scan(&n.Bind, &n.Port, &skip, &pidFile); err != nil {
		return n, err
	}
	n.SkipNetworking = skip != 0
	n.ListOK = mysqlListOK(n.Version, n.MariaDB)
	var ssl sql.NullString
	if err := a.db.QueryRowContext(ctx, "SELECT VARIABLE_VALUE FROM information_schema.GLOBAL_VARIABLES WHERE VARIABLE_NAME = 'HAVE_SSL'").Scan(&ssl); err != nil {
		_ = a.db.QueryRowContext(ctx, "SELECT VARIABLE_VALUE FROM performance_schema.global_variables WHERE VARIABLE_NAME = 'have_ssl'").Scan(&ssl)
	}
	n.SSL = strings.EqualFold(ssl.String, "YES")
	rows, err := a.db.QueryContext(ctx, "SELECT user, host, COALESCE(authentication_string, ''), COALESCE(plugin, '') FROM mysql.user")
	if err != nil {
		return n, err
	}
	for rows.Next() {
		var user, host, auth, plugin string
		if err := rows.Scan(&user, &host, &auth, &plugin); err != nil {
			rows.Close()
			return n, err
		}
		if mysqlLocalHost(host) {
			continue
		}
		who := "'" + user + "'@'" + host + "'"
		n.RemoteUsers = append(n.RemoteUsers, who)
		if auth == "" && plugin != "unix_socket" && plugin != "auth_socket" {
			n.NoPassRemote = append(n.NoPassRemote, who)
		}
	}
	rows.Close()
	_ = a.db.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(SUM(host <> '' AND host NOT LIKE 'localhost%' AND host NOT LIKE '127.0.0.1%' AND host NOT LIKE '::1%'), 0) "+
		"FROM information_schema.PROCESSLIST WHERE command <> 'Daemon'").Scan(&n.Clients, &n.Remote)

	n.Next = n.Bind
	if n.MariaDB {
		if d := s.f.cfg.confDir(); d != "" {
			n.KeptIn = filepath.Join(d, mysqlArgusCnf)
			if b, err := os.ReadFile(n.KeptIn); err == nil {
				if m := cnfBind.FindSubmatch(b); m != nil {
					n.Next = string(m[1])
				}
			}
		}
	} else {
		var v string
		if err := a.db.QueryRowContext(ctx, "SELECT VARIABLE_VALUE FROM performance_schema.persisted_variables WHERE VARIABLE_NAME = 'bind_address'").Scan(&v); err == nil {
			n.Next = v
		}
		n.KeptIn = "mysqld-auto.cnf (SET PERSIST_ONLY)"
	}
	n.PendingRestart = bindKey(n.Next) != bindKey(n.Bind)
	if b, err := os.ReadFile(filepath.Join(s.f.cfg.root(), pidFile)); err == nil {
		n.pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	return n, nil
}

func (s *mysqlSession) networkView(ctx context.Context, n MySQLNetwork) MySQLNetwork {
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
	n.Service = serviceOf(n.pid, "mysql", "mariadb")
	any, lan := listenReach(n.Bind)
	switch {
	case n.SkipNetworking || !any && !lan:
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

// keepBind keeps bind_address for mysqld's next start.
func (s *mysqlSession) keepBind(ctx context.Context, a *mysqlAdmin, bind string) error {
	if !a.mariaDB() {
		return a.exec(ctx, "SET PERSIST_ONLY bind_address = '"+strings.ReplaceAll(bind, "'", "")+"'")
	}
	d := s.f.cfg.confDir()
	if d == "" {
		return errors.New("Argus found no option file directory (" + strings.Join(mysqlConfDirs, ", ") + ") to keep bind-address in")
	}
	text := "# Which machines can connect, set in Argus, which rewrites this file.\n[mysqld]\nbind-address = " + bind + "\n"
	p := filepath.Join(d, mysqlArgusCnf)
	if err := os.WriteFile(p+".argus-new", []byte(text), 0o644); err != nil {
		return fmt.Errorf("could not write %s: %w", p, err)
	}
	return os.Rename(p+".argus-new", p)
}

// network answers network and network.set.
func (s *mysqlSession) network(ctx context.Context, req tunnel.PGRequest) error {
	op := req.Admin
	fail := func(msg string) error { return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: msg}) }
	if s.db.Level != tunnel.LevelDBA {
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Code: "42501",
			Error: "changing which machines can connect is DBA work; this session is at " + s.db.Level})
	}
	if !strings.HasPrefix(s.db.Host, "/") && !tunnel.LocalPGHost(s.db.Host) {
		return fail("this MySQL is on another server; change who can connect there, on its own agent")
	}
	actx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	admin, err := adminConn(actx, s.f.cfg.Sockets, s.db)
	if err != nil {
		return fail(err.Error())
	}
	defer admin.close()
	st, err := s.mysqlState(actx, admin)
	if err != nil {
		return fail("could not read MySQL's settings: " + err.Error())
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
		b, _ := json.Marshal(PGAdminDone{SQL: tunnel.MySQLNetworkText(*op)})
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "admin", Admin: b})
	}
	if tunnel.PGAlwaysApproval(op) && req.Approval == "" {
		return fail("opening MySQL to anywhere runs once someone else approves it")
	}
	if op.Reach == tunnel.ReachAny && !strings.EqualFold(strings.TrimSpace(op.Confirm), s.g.Host) {
		return fail("type the server's name, " + s.g.Host + ", to open MySQL to anywhere")
	}
	if op.Reach == tunnel.ReachAny && len(n.NoPassRemote) > 0 {
		return fail(strings.Join(n.NoPassRemote, ", ") + " can sign in from another machine with no password; give " +
			oneOr(len(n.NoPassRemote), "it", "each") + " a password first")
	}
	if n.SkipNetworking && op.Reach != tunnel.ReachLocal {
		return fail("MySQL runs with skip-networking, so it listens on no address; turn that off on the server first")
	}
	if (op.Reach == tunnel.ReachSelected || op.Reach == tunnel.ReachNetwork) && !n.ListOK {
		return fail("MySQL " + n.Version + " listens on one address or on every one, so it cannot keep the loopback and add another; choose anywhere with a firewall, or upgrade")
	}
	if n.FirewallError != "" && op.Reach != tunnel.ReachLocal {
		return fail("the firewall could not be read: " + n.FirewallError)
	}
	if op.Reach == tunnel.ReachSelected {
		if !n.FirewallActive {
			return fail("no firewall is on here, so MySQL cannot be limited to selected machines; choose the private network, or turn a firewall on first")
		}
		if n.FirewallOpenToAll {
			return fail("a firewall rule Argus did not add already lets everyone reach port " + strconv.Itoa(n.Port) + "; remove it on the server first")
		}
	}
	text := tunnel.MySQLNetworkText(*op)
	// Recorded before anything runs. No record, no change.
	if err := s.rec.write(tunnel.PGRecord{Kind: "admin", Request: req.ID, SQL: text, Approval: req.Approval}); err != nil {
		return fmt.Errorf("the recording failed: %w", err)
	}
	start := time.Now()
	failed := func(msg string) error {
		if rerr := s.rec.write(tunnel.PGRecord{Kind: "error", Request: req.ID, Error: msg, MS: time.Since(start).Milliseconds()}); rerr != nil {
			return rerr
		}
		return fail(msg)
	}
	var out MySQLNetworkResult
	fwDo := func() error {
		if !n.FirewallActive {
			return nil
		}
		fw := s.f.cfg.firewall()(actx, tunnel.FirewallRequest{Port: n.Port, Reach: op.Reach, Sources: op.Sources})
		out.FirewallRan = fw.Ran
		if fw.Error != "" {
			return errors.New("the firewall change failed: " + fw.Error)
		}
		return nil
	}
	bindDo := func() error {
		if err := s.keepBind(actx, admin, tunnel.MySQLBindFor(*op)); err != nil {
			return err
		}
		if !op.Restart {
			return nil
		}
		unit := serviceOf(st.pid, "mysql", "mariadb")
		if unit == "" {
			out.RestartError = "Argus found no systemd service for MySQL here; restart it on the server"
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
	if op.Reach == tunnel.ReachLocal {
		steps = []func() error{bindDo, fwDo}
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return failed(err.Error())
		}
	}
	// A restart ended this session's own connection and the admin's; the
	// view is read again once MySQL answers.
	after := st
	for i := 0; i < 20; i++ {
		a2, err := adminConn(actx, s.f.cfg.Sockets, s.db)
		if err == nil {
			after, err = s.mysqlState(actx, a2)
			a2.close()
			if err == nil {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	out.Network = s.networkView(actx, after)
	if rerr := s.rec.write(tunnel.PGRecord{Kind: "done", Request: req.ID, Tag: "NETWORK", MS: time.Since(start).Milliseconds()}); rerr != nil {
		return rerr
	}
	b, _ := json.Marshal(out)
	return s.send(tunnel.PGAnswer{ID: req.ID, Type: "admin", Admin: b, Tag: "NETWORK", MS: time.Since(start).Milliseconds()})
}
