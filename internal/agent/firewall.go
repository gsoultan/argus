package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
The server's firewall for one database port: what it allows now, and the
change to "who can connect" (tunnel/firewall.go).

The daemon cannot change a firewall from its sandbox, so it runs
`argus-agent firewall` as root through systemd-run, the request on stdin and
the answer on stdout, like the software questions. Only ufw and firewalld
are changed, and only the rules Argus added: ufw rules carry the comment
"argus <port>", firewalld rules (which take no comment) are listed in a
file in the agent's state directory. A firewall that is off stays off.
*/

const firewallTimeout = 45 * time.Second

// firewallStateName is the file the firewalld rules Argus added are kept in.
const firewallStateName = "firewall-rules.json"

// runCmd runs one command and returns what it printed; a variable so a test
// stands in for ufw and firewall-cmd.
type runCmd func(ctx context.Context, name string, args ...string) (string, error)

func execRun(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("%s: %v: %s", name, err, strings.TrimSpace(tail(out.Bytes(), 300)))
	}
	return out.String(), nil
}

// lookPath says whether a program is installed; a variable for tests.
var lookPath = func(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// firewallDo answers one firewall request, as root. Only the helper calls it.
func firewallDo(ctx context.Context, run runCmd, req tunnel.FirewallRequest) tunnel.FirewallResult {
	if err := req.Validate(); err != nil {
		return tunnel.FirewallResult{Error: err.Error()}
	}
	switch {
	case lookPath("ufw"):
		return ufwDo(ctx, run, req)
	case lookPath("firewall-cmd"):
		return firewalldDo(ctx, run, req)
	}
	return tunnel.FirewallResult{}
}

// ---------------------------------------------------------------- ufw

// ufwRule is one line of `ufw status numbered`.
type ufwRule struct {
	num            int
	to, act, from  string
	comment, whole string
}

var ufwLine = regexp.MustCompile(`^\[\s*(\d+)\]\s+(.+?)\s{2,}(ALLOW|DENY|REJECT|LIMIT)(?: (?:IN|OUT|FWD))?\s{2,}(.+?)\s*(?:#\s*(.*))?$`)

func parseUFW(out string) (active bool, rules []ufwRule) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, " \r")
		if strings.HasPrefix(line, "Status:") {
			active = strings.TrimSpace(strings.TrimPrefix(line, "Status:")) == "active"
			continue
		}
		m := ufwLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		rules = append(rules, ufwRule{num: n, to: m[2], act: m[3], from: strings.TrimSpace(m[4]),
			comment: strings.TrimSpace(m[5]), whole: strings.Join(strings.Fields(line[strings.Index(line, "]")+1:]), " ")})
	}
	return active, rules
}

// portIn says whether a ufw "To" names port: 6379, 6379/tcp, 6379,6380/tcp,
// 6000:7000/tcp, or Anywhere (every port).
func portIn(to string, port int) bool {
	to = strings.TrimSpace(strings.TrimSuffix(to, " (v6)"))
	if strings.HasPrefix(to, "Anywhere") {
		return true
	}
	f := strings.Fields(to)
	if len(f) == 0 {
		return false
	}
	// "10.0.0.5 6379/tcp" names an address and then the port.
	spec := f[len(f)-1]
	spec, proto, _ := strings.Cut(spec, "/")
	if proto != "" && proto != "tcp" {
		return false
	}
	for _, part := range strings.Split(spec, ",") {
		lo, hi, rng := strings.Cut(part, ":")
		a, err1 := strconv.Atoi(lo)
		if err1 != nil {
			continue
		}
		b := a
		if rng {
			if v, err := strconv.Atoi(hi); err == nil {
				b = v
			}
		}
		if port >= a && port <= b {
			return true
		}
	}
	return false
}

func ufwDo(ctx context.Context, run runCmd, req tunnel.FirewallRequest) tunnel.FirewallResult {
	res := tunnel.FirewallResult{Firewall: "ufw"}
	look := func() []ufwRule {
		out, err := run(ctx, "ufw", "status", "numbered")
		if err != nil {
			res.Error = err.Error()
			return nil
		}
		var rules []ufwRule
		res.Active, rules = parseUFW(out)
		res.Mine, res.Others, res.OpenToAll, res.Sources, res.AllowAll = nil, nil, false, nil, false
		mark := tunnel.FirewallComment(req.Port)
		for _, r := range rules {
			if !portIn(r.to, req.Port) {
				continue
			}
			if r.comment == mark {
				res.Mine = append(res.Mine, r.whole)
				if r.act == "ALLOW" {
					from := strings.TrimSpace(strings.TrimSuffix(r.from, "(v6)"))
					if strings.HasPrefix(from, "Anywhere") {
						res.AllowAll = true
					} else if !slices.Contains(res.Sources, from) {
						res.Sources = append(res.Sources, from)
					}
				}
				continue
			}
			res.Others = append(res.Others, r.whole)
			if r.act == "ALLOW" && strings.HasPrefix(r.from, "Anywhere") {
				res.OpenToAll = true
			}
		}
		return rules
	}
	rules := look()
	if res.Error != "" || req.Check || !res.Active {
		return res
	}
	mark := tunnel.FirewallComment(req.Port)
	// Argus's own rules first, highest number first so the numbers left stay true.
	var mine []int
	for _, r := range rules {
		if r.comment == mark && portIn(r.to, req.Port) {
			mine = append(mine, r.num)
		}
	}
	slices.Sort(mine)
	slices.Reverse(mine)
	step := func(args ...string) bool {
		res.Ran = append(res.Ran, "ufw "+strings.Join(args, " "))
		if _, err := run(ctx, "ufw", args...); err != nil {
			res.Error = err.Error()
			return false
		}
		return true
	}
	for _, n := range mine {
		if !step("--force", "delete", strconv.Itoa(n)) {
			return res
		}
	}
	p := strconv.Itoa(req.Port)
	switch req.Reach {
	case tunnel.ReachSelected, tunnel.ReachNetwork:
		for _, s := range req.Sources {
			if !step("allow", "from", s, "to", "any", "port", p, "proto", "tcp", "comment", mark) {
				return res
			}
		}
		if !step("deny", "proto", "tcp", "to", "any", "port", p, "comment", mark) {
			return res
		}
	case tunnel.ReachAny:
		if !step("allow", "proto", "tcp", "to", "any", "port", p, "comment", mark) {
			return res
		}
	}
	look()
	return res
}

// ---------------------------------------------------------------- firewalld

// firewalldMine is the rules Argus added, by port: a rich rule, or "port"
// for an opened port.
type firewalldMine map[string][]string

func readFirewalldMine(path string) firewalldMine {
	m := firewalldMine{}
	if path == "" {
		return m
	}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func writeFirewalldMine(path string, m firewalldMine) error {
	if path == "" {
		return errors.New("no state file for the firewalld rules Argus adds")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(path, mustJSON(m), 0o600)
}

var richSource = regexp.MustCompile(`source address="([^"]+)"`)

func richRule(src string, port int) string {
	fam := "ipv4"
	if p, err := netip.ParsePrefix(src); err == nil && p.Addr().Is6() {
		fam = "ipv6"
	} else if a, err := netip.ParseAddr(src); err == nil && a.Is6() {
		fam = "ipv6"
	}
	return fmt.Sprintf(`rule family="%s" source address="%s" port port="%d" protocol="tcp" accept`, fam, src, port)
}

func firewalldDo(ctx context.Context, run runCmd, req tunnel.FirewallRequest) tunnel.FirewallResult {
	res := tunnel.FirewallResult{Firewall: "firewalld"}
	state, _ := run(ctx, "firewall-cmd", "--state")
	res.Active = strings.TrimSpace(state) == "running"
	if !res.Active {
		return res
	}
	key := strconv.Itoa(req.Port)
	mine := readFirewalldMine(req.StateFile)
	portSpec := key + "/tcp"
	look := func() {
		res.Others, res.OpenToAll, res.Sources, res.AllowAll = nil, false, nil, false
		res.Mine = slices.Clone(mine[key])
		ours := map[string]bool{}
		for _, r := range mine[key] {
			ours[r] = true
			if r == "port" {
				res.AllowAll = true
			} else if m := richSource.FindStringSubmatch(r); m != nil {
				res.Sources = append(res.Sources, m[1])
			}
		}
		if out, err := run(ctx, "firewall-cmd", "--list-ports"); err == nil {
			for _, p := range strings.Fields(out) {
				if p == portSpec && !ours["port"] {
					res.Others = append(res.Others, "port "+p)
					res.OpenToAll = true
				}
			}
		}
		if out, err := run(ctx, "firewall-cmd", "--list-rich-rules"); err == nil {
			for _, line := range strings.Split(out, "\n") {
				line = strings.TrimSpace(line)
				if line != "" && strings.Contains(line, `port="`+key+`"`) && !ours[line] {
					res.Others = append(res.Others, line)
				}
			}
		}
	}
	look()
	if req.Check {
		return res
	}
	both := func(args ...string) bool {
		for _, a := range [][]string{args, append([]string{"--permanent"}, args...)} {
			res.Ran = append(res.Ran, "firewall-cmd "+strings.Join(a, " "))
			if _, err := run(ctx, "firewall-cmd", a...); err != nil {
				res.Error = err.Error()
				return false
			}
		}
		return true
	}
	for _, r := range slices.Clone(mine[key]) {
		ok := false
		if r == "port" {
			ok = both("--remove-port=" + portSpec)
		} else {
			ok = both("--remove-rich-rule=" + r)
		}
		if !ok {
			_ = writeFirewalldMine(req.StateFile, mine)
			return res
		}
		mine[key] = slices.DeleteFunc(mine[key], func(x string) bool { return x == r })
	}
	var added []string
	switch req.Reach {
	case tunnel.ReachSelected, tunnel.ReachNetwork:
		for _, s := range req.Sources {
			r := richRule(s, req.Port)
			if !both("--add-rich-rule=" + r) {
				break
			}
			added = append(added, r)
		}
	case tunnel.ReachAny:
		if !res.OpenToAll {
			if both("--add-port=" + portSpec) {
				added = append(added, "port")
			}
		}
	}
	mine[key] = append(mine[key], added...)
	if len(mine[key]) == 0 {
		delete(mine, key)
	}
	if err := writeFirewalldMine(req.StateFile, mine); err != nil && res.Error == "" {
		res.Error = "the rules were changed but the list of Argus's rules could not be saved: " + err.Error()
	}
	look()
	return res
}

// ---------------------------------------------------------------- the helper

// RunFirewall is `argus-agent firewall`: one request on stdin, the answer
// on stdout. It runs as root, outside the daemon's sandbox.
func RunFirewall(in io.Reader, out io.Writer) int {
	var req tunnel.FirewallRequest
	if err := json.NewDecoder(io.LimitReader(in, 1<<16)).Decode(&req); err != nil {
		_ = json.NewEncoder(out).Encode(tunnel.FirewallResult{Error: "the request could not be read: " + err.Error()})
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), firewallTimeout)
	defer cancel()
	res := firewallDo(ctx, execRun, req)
	_ = json.NewEncoder(out).Encode(res)
	return 0
}

// firewaller asks the server's firewall; the default runs the helper.
type firewaller func(ctx context.Context, req tunnel.FirewallRequest) tunnel.FirewallResult

// hostFirewall runs `argus-agent firewall` as root in a unit of its own.
func hostFirewall(executable, stateDir string) firewaller {
	return func(ctx context.Context, req tunnel.FirewallRequest) tunnel.FirewallResult {
		switch {
		case os.Geteuid() != 0:
			return tunnel.FirewallResult{Error: "the agent is not running as root, so it cannot read or change the firewall"}
		case !systemdRunning():
			return tunnel.FirewallResult{Error: "this server does not run systemd, which changing the firewall from the console needs"}
		case executable == "":
			return tunnel.FirewallResult{Error: "the agent does not know where its own program is"}
		}
		if stateDir != "" {
			req.StateFile = filepath.Join(stateDir, firewallStateName)
		}
		in, err := json.Marshal(req)
		if err != nil {
			return tunnel.FirewallResult{Error: err.Error()}
		}
		cctx, cancel := context.WithTimeout(ctx, firewallTimeout+15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(cctx, "systemd-run", "--pipe", "--wait", "--quiet", "--collect",
			"--service-type=exec", "--description=Argus firewall", executable, "firewall")
		var stdout, stderr bytes.Buffer
		cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(in), &stdout, &stderr
		if err := cmd.Run(); err != nil && stdout.Len() == 0 {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = err.Error()
			}
			return tunnel.FirewallResult{Error: "the firewall could not be asked: " + tail([]byte(msg), 400)}
		}
		var res tunnel.FirewallResult
		if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
			return tunnel.FirewallResult{Error: "the firewall's answer could not be read"}
		}
		return res
	}
}
