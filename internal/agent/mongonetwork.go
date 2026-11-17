package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Which machines can connect to a MongoDB on this server (see
tunnel/mongonetwork.go). The agent reads how mongod runs as MongoDB's
administrator (getCmdLineOpts), changes net.bindIp in its configuration file
for the next start, changes the firewall, and restarts the service when
asked. Opening narrows the firewall before mongod listens anew; closing stops
it listening before the firewall's rules go.

Only the bindIp and bindIpAll lines under net: are touched; every other line
of mongod.conf, comments included, stays as it was. The file is written
beside the old one, given its owner and mode, checked by reading it back,
and renamed into place.
*/

// MongoNetwork is which machines can connect to this MongoDB, and why.
type MongoNetwork struct {
	// Bind is net.bindIp as mongod runs now ("*" for bindIpAll); Next what
	// its configuration file says for the next start.
	Bind           string `json:"bind"`
	Next           string `json:"next"`
	PendingRestart bool   `json:"pendingRestart"`
	Port           int    `json:"port"`
	Version        string `json:"version"`
	// ConfigFile is the file mongod was started with; empty when none.
	ConfigFile string `json:"configFile,omitempty"`
	// CmdLine says mongod was started with --bind_ip or --bind_ip_all,
	// which its configuration file does not override.
	CmdLine bool `json:"cmdLine,omitempty"`
	// IPv6 says net.ipv6 is on, which an IPv6 address in bindIp needs.
	IPv6    bool `json:"ipv6,omitempty"`
	TLS     bool `json:"tls"`
	Clients int  `json:"clients"`
	Remote  int  `json:"remote"`

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

// MongoNetworkResult answers network_set.
type MongoNetworkResult struct {
	Network      MongoNetwork `json:"network"`
	FirewallRan  []string     `json:"firewallRan,omitempty"`
	Restarted    bool         `json:"restarted,omitempty"`
	RestartError string       `json:"restartError,omitempty"`
}

func (c StoreConfig) restart() pgRestarter {
	if c.Restart != nil {
		return c.Restart
	}
	return systemctlRestart
}

// mongoConfBind is what a configuration file says mongod listens on: "*"
// for bindIpAll, and the loopback when it says nothing, as mongod does.
// It reads the net: block's own bindIp and bindIpAll lines, the only ones
// Argus writes; the agent carries no YAML library.
func mongoConfBind(conf []byte) (string, error) {
	lines := strings.Split(strings.ReplaceAll(string(conf), "\r\n", "\n"), "\n")
	bind, all := "", false
	in, indent := false, ""
	for _, l := range lines {
		if mongoNetFlow.MatchString(l) {
			return "", errors.New("net: is written on one line")
		}
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if l[0] != ' ' && l[0] != '\t' {
			in = mongoNetLine.MatchString(l)
			indent = ""
			continue
		}
		if !in {
			continue
		}
		lead := l[:len(l)-len(strings.TrimLeft(l, " \t"))]
		if indent == "" {
			indent = lead
		}
		if lead != indent {
			continue
		}
		k, v, ok := strings.Cut(t, ":")
		if !ok {
			continue
		}
		v = yamlScalar(v)
		switch strings.TrimSpace(k) {
		case "bindIp":
			bind = v
		case "bindIpAll":
			all = v == "true"
		}
	}
	switch {
	case all:
		return "*", nil
	case bind == "":
		return "127.0.0.1", nil
	}
	return bindKey(bind), nil
}

// yamlScalar is a plain or quoted YAML value, its comment removed.
func yamlScalar(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > 0 && (v[0] == '"' || v[0] == '\'') {
		if end := strings.IndexByte(v[1:], v[0]); end >= 0 {
			return v[1 : end+1]
		}
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

var (
	mongoNetLine  = regexp.MustCompile(`^net:\s*(#.*)?$`)
	mongoNetFlow  = regexp.MustCompile(`^net:\s*[^\s#]`)
	mongoBindLine = regexp.MustCompile(`^(\s+)(bindIp|bindIpAll)\s*:`)
)

// setMongoBind is a configuration file with net.bindIp (or bindIpAll, for
// "*") set to bind, and every other line as it was.
func setMongoBind(conf []byte, bind string) ([]byte, error) {
	text := strings.ReplaceAll(string(conf), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	set := func(indent string) string {
		if bind == "*" {
			return indent + "bindIpAll: true"
		}
		return indent + "bindIp: " + bind
	}
	at := -1
	for i, l := range lines {
		if mongoNetFlow.MatchString(l) {
			return nil, errors.New("net: is written on one line in the configuration file; Argus changes only the block form, so change bindIp there by hand")
		}
		if mongoNetLine.MatchString(l) {
			if at >= 0 {
				return nil, errors.New("the configuration file has net: twice")
			}
			at = i
		}
	}
	var out []string
	if at < 0 {
		out = append(lines, "")
		for len(out) > 0 && out[len(out)-1] == "" {
			out = out[:len(out)-1]
		}
		out = append(out, "net:", set("  "), "")
	} else {
		end := len(lines)
		indent := ""
		for j := at + 1; j < len(lines); j++ {
			l := lines[j]
			t := strings.TrimSpace(l)
			if t == "" || strings.HasPrefix(t, "#") {
				continue
			}
			if l[0] != ' ' && l[0] != '\t' {
				end = j
				break
			}
			if indent == "" {
				indent = l[:len(l)-len(strings.TrimLeft(l, " \t"))]
			}
		}
		if indent == "" {
			indent = "  "
		}
		out = append(out, lines[:at+1]...)
		out = append(out, set(indent))
		for j := at + 1; j < end; j++ {
			if m := mongoBindLine.FindStringSubmatch(lines[j]); m != nil && m[1] == indent {
				continue
			}
			out = append(out, lines[j])
		}
		out = append(out, lines[end:]...)
	}
	b := []byte(strings.Join(out, "\n"))
	got, err := mongoConfBind(b)
	if err != nil {
		return nil, fmt.Errorf("the changed configuration file would not read back: %w", err)
	}
	if got != bindKey(bind) {
		return nil, fmt.Errorf("the changed configuration file would say %s, not %s; change bindIp there by hand", got, bind)
	}
	return b, nil
}

// writeMongoConf puts a new configuration file in place of the old, with
// its owner and mode.
func writeMongoConf(path string, b []byte) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".argus-new")
	if err := os.WriteFile(tmp, b, fi.Mode().Perm()); err != nil {
		return fmt.Errorf("could not write beside %s: %w", path, err)
	}
	if err := chownLike(tmp, fi); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// cmdLineBind says mongod's command line names where it listens.
func cmdLineBind(argv []string) bool {
	for _, a := range argv {
		if a == "--bind_ip" || a == "--bind_ip_all" || strings.HasPrefix(a, "--bind_ip=") {
			return true
		}
	}
	return false
}

// mongoState reads how mongod runs, as its administrator.
func (s *mongoSession) mongoState(ctx context.Context, admin *mongo.Client) (MongoNetwork, error) {
	n := MongoNetwork{Version: s.version, Host: s.g.Host, Port: s.port}
	var opts struct {
		Argv   []string `bson:"argv"`
		Parsed struct {
			Config string `bson:"config"`
			Net    struct {
				BindIP    string `bson:"bindIp"`
				BindIPAll bool   `bson:"bindIpAll"`
				Port      int    `bson:"port"`
				IPv6      bool   `bson:"ipv6"`
				TLS       struct {
					Mode string `bson:"mode"`
				} `bson:"tls"`
			} `bson:"net"`
		} `bson:"parsed"`
	}
	if err := admin.Database("admin").RunCommand(ctx, bson.D{{Key: "getCmdLineOpts", Value: 1}}).Decode(&opts); err != nil {
		return n, fmt.Errorf("could not read how mongod runs: %w", err)
	}
	p := opts.Parsed
	if p.Net.Port > 0 {
		n.Port = p.Net.Port
	}
	n.Bind = bindKey(p.Net.BindIP)
	if p.Net.BindIPAll {
		n.Bind = "*"
	} else if n.Bind == "" {
		n.Bind = "127.0.0.1"
	}
	n.IPv6 = p.Net.IPv6
	n.TLS = p.Net.TLS.Mode != "" && p.Net.TLS.Mode != "disabled"
	n.CmdLine = cmdLineBind(opts.Argv)
	n.ConfigFile = p.Config
	n.Next = n.Bind
	if n.ConfigFile != "" && !n.CmdLine {
		b, err := os.ReadFile(n.ConfigFile)
		if err != nil {
			return n, fmt.Errorf("could not read %s: %w", n.ConfigFile, err)
		}
		// A file Argus cannot read the bind from is one it will not
		// change either (setMongoBind says why); the view goes on.
		if next, err := mongoConfBind(b); err == nil {
			n.Next = next
		}
	}
	n.PendingRestart = n.Next != n.Bind

	var st struct {
		PID         int64 `bson:"pid"`
		Connections struct {
			Current int `bson:"current"`
		} `bson:"connections"`
	}
	if admin.Database("admin").RunCommand(ctx, bson.D{{Key: "serverStatus", Value: 1}}).Decode(&st) == nil {
		n.pid, n.Clients = int(st.PID), st.Connections.Current
	}
	cur, err := admin.Database("admin").Aggregate(ctx, mongo.Pipeline{
		{{Key: "$currentOp", Value: bson.D{{Key: "allUsers", Value: true}, {Key: "idleConnections", Value: true}}}},
		{{Key: "$project", Value: bson.D{{Key: "client", Value: 1}}}},
	})
	if err == nil {
		var ops []struct {
			Client string `bson:"client"`
		}
		if cur.All(ctx, &ops) == nil {
			for _, o := range ops {
				if o.Client == "" {
					continue
				}
				h := o.Client
				if ap, err := netip.ParseAddrPort(h); err == nil {
					if !ap.Addr().IsLoopback() {
						n.Remote++
					}
				}
			}
		}
	}
	return n, nil
}

func (s *mongoSession) networkView(ctx context.Context, n MongoNetwork) MongoNetwork {
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
	n.Service = serviceOf(n.pid, "mongod", "mongodb")
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

// readNetwork signs in as the administrator and reads the state; the
// factory's netState stands in for it in tests.
func (s *mongoSession) readNetwork(ctx context.Context) (MongoNetwork, error) {
	if s.f.netState != nil {
		return s.f.netState(ctx)
	}
	admin, err := s.adminClient(ctx)
	if err != nil {
		return MongoNetwork{}, err
	}
	defer func() { _ = admin.Disconnect(context.WithoutCancel(ctx)) }()
	return s.mongoState(ctx, admin)
}

// network answers network and network_set.
func (s *mongoSession) network(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if s.db.Level != tunnel.LevelDBA {
		return errAnswer("changing which machines can connect is DBA work; this session is at %s", s.db.Level)
	}
	if !strings.HasPrefix(s.host, "/") && !tunnel.LocalPGHost(s.host) {
		return errAnswer("this MongoDB is on another server; change who can connect there, on its own agent")
	}
	actx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	st, err := s.readNetwork(actx)
	if err != nil {
		return errAnswer("%s", err)
	}
	n := s.networkView(actx, st)
	if req.Op == tunnel.MongoOpNetwork {
		return tunnel.StoreAnswer{Type: "result", Data: n}, nil
	}

	op := tunnel.MongoNetOp(req)
	if err := tunnel.ValidatePGNetwork(&op); err != nil {
		return errAnswer("%s", err)
	}
	req.Sources = op.Sources
	for _, a := range req.Addresses {
		if strings.Contains(a, ":") && !n.IPv6 {
			return errAnswer("%s is an IPv6 address, which mongod listens on only with net.ipv6 on; choose an IPv4 address", a)
		}
	}
	text := tunnel.MongoNetworkText(req)
	if (s.db.ChangesNeedApproval || req.Reach == tunnel.ReachAny) && req.Approval == "" {
		why := "on " + s.g.Host + " this change runs once someone else approves it"
		if req.Reach == tunnel.ReachAny {
			why = "opening MongoDB to anywhere runs once someone else approves it"
		}
		if err := rec.refused(req.ID, text, why); err != nil {
			return tunnel.StoreAnswer{}, err
		}
		return tunnel.StoreAnswer{Type: "error", Error: why, NeedsApproval: &tunnel.RedisApprovalNeed{Text: text}}, nil
	}
	if req.Reach == tunnel.ReachAny && !strings.EqualFold(strings.TrimSpace(req.Confirm), s.g.Host) {
		return errAnswer("type the server's name, %s, to open MongoDB to anywhere", s.g.Host)
	}
	if n.CmdLine {
		return errAnswer("mongod was started with --bind_ip on its command line, which its configuration file does not override; change it in the service's command line on the server")
	}
	if n.ConfigFile == "" {
		return errAnswer("mongod runs with no configuration file, so Argus has nowhere to keep bindIp; start it with --config on the server first")
	}
	if n.FirewallError != "" && req.Reach != tunnel.ReachLocal {
		return errAnswer("the firewall could not be read: %s", n.FirewallError)
	}
	if req.Reach == tunnel.ReachSelected {
		if !n.FirewallActive {
			return errAnswer("no firewall is on here, so MongoDB cannot be limited to selected machines; choose the private network, or turn a firewall on first")
		}
		if n.FirewallOpenToAll {
			return errAnswer("a firewall rule Argus did not add already lets everyone reach port %d; remove it on the server first", n.Port)
		}
	}
	bind := tunnel.MongoBindFor(req.Reach, req.Addresses)
	old, err := os.ReadFile(n.ConfigFile)
	if err != nil {
		return errAnswer("could not read %s: %s", n.ConfigFile, err)
	}
	conf, err := setMongoBind(old, bind)
	if err != nil {
		return errAnswer("%s", err)
	}

	// Recorded before anything runs. No record, no change.
	if err := rec.write(tunnel.PGRecord{Kind: "stmt", Request: req.ID, SQL: text, Approval: req.Approval}); err != nil {
		return tunnel.StoreAnswer{}, fmt.Errorf("the recording failed: %w", err)
	}
	start := time.Now()
	var out MongoNetworkResult
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
			if err := writeMongoConf(n.ConfigFile, conf); err != nil {
				return err
			}
		}
		if !req.Restart {
			return nil
		}
		unit := serviceOf(st.pid, "mongod", "mongodb")
		if unit == "" {
			out.RestartError = "Argus found no systemd service for mongod here; restart it on the server"
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
	// A restart ends the administrator's connections; the state is read
	// again once mongod answers.
	after := st
	for i := 0; i < 20; i++ {
		if a, err := s.readNetwork(actx); err == nil {
			after = a
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	out.Network = s.networkView(actx, after)
	if err := rec.done(req.ID, "NETWORK", 1, time.Since(start).Milliseconds(), out, false); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	return tunnel.StoreAnswer{Type: "result", Data: out, MS: time.Since(start).Milliseconds(), Tag: "NETWORK"}, nil
}
