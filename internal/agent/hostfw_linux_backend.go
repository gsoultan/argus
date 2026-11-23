package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
The helper's changes to ufw, firewalld and nftables (hostfw_apply.go).

  - ufw: one rule per address, each with the comment "argus fw-<key>".
    Removed by number, highest first, after reading `ufw status numbered`.
  - firewalld: rich rules in the default zone for traffic coming in, direct
    rules for traffic going out, each for now and after a restart. firewalld
    keeps no comment on a rich rule, so the commands Argus ran for each line
    are kept in firewall-lines.json in the agent's state folder; removing a
    line runs their opposites.
  - nftables: Argus's own table, "inet argus", written whole from a file
    that a small unit loads at every boot. Nothing else in the ruleset is
    touched.

The saved copy is the firewall's own files (ufw's user rules and settings,
firewalld's zones and direct rules, Argus's nftables file), so putting it
back is writing them and reloading.
*/

type linuxFW struct {
	run      runCmd
	has      func(string) bool
	stateDir string
	// files, for tests.
	readFile   func(string) ([]byte, error)
	writeFile  func(string, []byte) error
	removeFile func(string) error
	glob       func(string) ([]string, error)
}

func newLinuxFW(stateDir string) *linuxFW {
	return &linuxFW{run: execRun, has: lookPath, stateDir: stateDir,
		readFile: os.ReadFile,
		writeFile: func(p string, b []byte) error {
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			return writeFileAtomic(p, b, 0o640)
		},
		removeFile: os.Remove, glob: filepath.Glob}
}

var ufwFiles = []string{"/etc/ufw/user.rules", "/etc/ufw/user6.rules", "/etc/ufw/ufw.conf", "/etc/default/ufw"}

const (
	firewalldZoneGlob = "/etc/firewalld/zones/*.xml"
	firewalldDirect   = "/etc/firewalld/direct.xml"
	nftUnit           = "/etc/systemd/system/argus-firewall.service"
)

func (f *linuxFW) linesPath() string { return filepath.Join(f.stateDir, firewallLines) }
func (f *linuxFW) nftPath() string   { return filepath.Join(f.stateDir, firewallDir, "argus.nft") }

func (f *linuxFW) read(ctx context.Context) tunnel.HostFirewall {
	state := ""
	if f.stateDir != "" {
		state = filepath.Join(f.stateDir, firewallStateName)
	}
	return readLinuxFirewall(ctx, f.run, f.has, state)
}

func (f *linuxFW) snapshot(ctx context.Context) (fwSnapshot, error) {
	cur := f.read(ctx)
	s := fwSnapshot{Kind: cur.Kind, Active: cur.Active, Files: map[string]*string{}}
	keep := func(p string) error {
		b, err := f.readFile(p)
		switch {
		case err == nil:
			v := string(b)
			s.Files[p] = &v
		case errors.Is(err, os.ErrNotExist):
			s.Files[p] = nil
		default:
			return err
		}
		return nil
	}
	paths := []string{f.linesPath(), f.nftPath(), filepath.Join(f.stateDir, firewallDir, "argus-nft-policy.json")}
	if f.has("ufw") {
		paths = append(paths, ufwFiles...)
	}
	if f.has("firewall-cmd") {
		zones, _ := f.glob(firewalldZoneGlob)
		paths = append(paths, zones...)
		paths = append(paths, firewalldDirect)
	}
	for _, p := range paths {
		if err := keep(p); err != nil {
			return s, fmt.Errorf("cannot read %s: %w", p, err)
		}
	}
	if v := s.Files[f.nftPath()]; v != nil {
		s.Nft = *v
	}
	return s, nil
}

func (f *linuxFW) restore(ctx context.Context, s fwSnapshot) error {
	var errs []string
	for p, v := range s.Files {
		var err error
		if v == nil {
			if err = f.removeFile(p); errors.Is(err, os.ErrNotExist) {
				err = nil
			}
		} else {
			err = f.writeFile(p, []byte(*v))
		}
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	// firewalld zones made since the copy are taken away again.
	if f.has("firewall-cmd") {
		zones, _ := f.glob(firewalldZoneGlob)
		for _, z := range zones {
			if _, ok := s.Files[z]; !ok {
				_ = f.removeFile(z)
			}
		}
	}
	cur := f.read(ctx)
	do := func(name string, args ...string) {
		if _, err := f.run(ctx, name, args...); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if f.has("ufw") {
		switch {
		case s.Kind == tunnel.FirewallUFW && s.Active:
			if cur.Kind == tunnel.FirewallUFW && cur.Active {
				do("ufw", "reload")
			} else {
				do("ufw", "--force", "enable")
			}
		case cur.Kind == tunnel.FirewallUFW && cur.Active:
			do("ufw", "--force", "disable")
		}
	}
	if f.has("firewall-cmd") {
		running := cur.Kind == tunnel.FirewallFirewalld && cur.Active
		switch {
		case s.Kind == tunnel.FirewallFirewalld && s.Active:
			if !running {
				do("systemctl", "start", "firewalld")
			}
			do("firewall-cmd", "--reload")
		case running:
			do("systemctl", "disable", "--now", "firewalld")
		}
	}
	if f.has("nft") {
		if s.Nft != "" {
			do("nft", "-f", f.nftPath())
		} else if _, err := f.run(ctx, "nft", "list", "table", "inet", "argus"); err == nil {
			do("nft", "delete", "table", "inet", "argus")
			_, _ = f.run(ctx, "systemctl", "disable", "argus-firewall.service")
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func (f *linuxFW) apply(ctx context.Context, cur tunnel.HostFirewall, spec tunnel.FirewallChangeSpec, always []tunnel.FirewallLine, ran *[]tunnel.FirewallRan) error {
	kind := cur.Kind
	if spec.Install != "" && (kind == "" || !slices.Contains(cur.Installed, kind)) {
		if err := ranCmd(ctx, f.run, ran, installFor(spec.Install, f.has)); err != nil {
			return err
		}
		kind = spec.Install
		cur = f.read(ctx)
		if cur.Kind != "" {
			kind = cur.Kind
		}
	}
	want := append(append(slices.Clone(spec.Lines), spec.Protect...), always...)
	switch kind {
	case tunnel.FirewallUFW:
		return f.applyUFW(ctx, cur, spec, want, ran)
	case tunnel.FirewallFirewalld:
		return f.applyFirewalld(ctx, cur, spec, want, ran)
	case tunnel.FirewallNftables:
		if len(spec.TakeOver) > 0 {
			return errors.New("Argus does not change nftables rules outside its own table")
		}
		return f.applyNft(ctx, spec, want, ran)
	case "":
		return errors.New("no firewall is installed; turn one on first")
	}
	return fmt.Errorf("Argus reads %s but does not change it", kind)
}

// installFor installs fw with the package manager the server has.
func installFor(fw string, has func(string) bool) tunnel.FirewallCmd {
	switch {
	case has("apt-get"):
		return tunnel.FirewallCmd{"env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "-y", "install", fw}
	case has("dnf"):
		return tunnel.FirewallCmd{"dnf", "-y", "install", fw}
	case has("yum"):
		return tunnel.FirewallCmd{"yum", "-y", "install", fw}
	case has("zypper"):
		return tunnel.FirewallCmd{"zypper", "--non-interactive", "install", fw}
	}
	return tunnel.InstallCmd(fw)
}

func wantMap(lines []tunnel.FirewallLine) (map[string]tunnel.FirewallLine, []string) {
	m := map[string]tunnel.FirewallLine{}
	var keys []string
	for _, l := range lines {
		if _, ok := m[l.Key]; !ok {
			keys = append(keys, l.Key)
		}
		m[l.Key] = l
	}
	slices.Sort(keys)
	return m, keys
}

/* ── ufw ─────────────────────────────────────────────────────────────────── */

func (f *linuxFW) applyUFW(ctx context.Context, cur tunnel.HostFirewall, spec tunnel.FirewallChangeSpec, want []tunnel.FirewallLine, ran *[]tunnel.FirewallRan) error {
	wm, keys := wantMap(want)
	have := cur.ArgusLines()
	var gone []string
	for k, h := range have {
		if w, ok := wm[k]; !ok || !tunnel.SameLine(w, h) {
			gone = append(gone, tunnel.FirewallLineMark(k))
		}
	}
	slices.Sort(gone)
	for _, mark := range gone {
		if err := f.ufwDeleteWhere(ctx, ran, func(r ufwRule) bool { return r.comment == mark }); err != nil {
			return err
		}
	}
	for _, raw := range spec.TakeOver {
		n := 0
		norm := strings.Join(strings.Fields(raw), " ")
		if err := f.ufwDeleteWhere(ctx, ran, func(r ufwRule) bool {
			if r.whole == norm {
				n++
				return true
			}
			return false
		}); err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("the rule %q is no longer there", raw)
		}
	}
	for _, k := range keys {
		if h, ok := have[k]; ok && tunnel.SameLine(h, wm[k]) {
			continue
		}
		for _, c := range tunnel.UFWAdd(wm[k]) {
			if err := ranCmd(ctx, f.run, ran, c); err != nil {
				return err
			}
		}
	}
	if spec.DefaultIn != "" {
		if err := ranCmd(ctx, f.run, ran, tunnel.UFWDefault("in", spec.DefaultIn)); err != nil {
			return err
		}
	}
	if spec.DefaultOut != "" {
		if err := ranCmd(ctx, f.run, ran, tunnel.UFWDefault("out", spec.DefaultOut)); err != nil {
			return err
		}
	}
	if spec.TurnOn && !cur.Active {
		return ranCmd(ctx, f.run, ran, tunnel.UFWEnable())
	}
	return nil
}

// ufwDeleteWhere deletes every numbered rule match picks, highest number
// first, reading the list again after each since the numbers move.
func (f *linuxFW) ufwDeleteWhere(ctx context.Context, ran *[]tunnel.FirewallRan, match func(ufwRule) bool) error {
	for i := 0; i < tunnel.MaxHostFirewallRules; i++ {
		out, err := f.run(ctx, "ufw", "status", "numbered")
		if err != nil {
			return fmt.Errorf("ufw status numbered: %v", err)
		}
		_, rules := parseUFW(out)
		pick := 0
		for _, r := range rules {
			if match(r) && r.num > pick {
				pick = r.num
			}
		}
		if pick == 0 {
			return nil
		}
		if err := ranCmd(ctx, f.run, ran, tunnel.UFWDelete(pick)); err != nil {
			return err
		}
	}
	return errors.New("ufw kept listing the rule after deleting it")
}

/* ── firewalld ───────────────────────────────────────────────────────────── */

// fwdLine is a line as firewalld was given it: the commands that added it.
type fwdLine struct {
	Line tunnel.FirewallLine  `json:"line"`
	Cmds []tunnel.FirewallCmd `json:"cmds"`
}

func (f *linuxFW) loadFwdLines() map[string]fwdLine {
	m := map[string]fwdLine{}
	if b, err := f.readFile(f.linesPath()); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func (f *linuxFW) saveFwdLines(m map[string]fwdLine) error {
	return f.writeFile(f.linesPath(), mustJSON(m))
}

func (f *linuxFW) applyFirewalld(ctx context.Context, cur tunnel.HostFirewall, spec tunnel.FirewallChangeSpec, want []tunnel.FirewallLine, ran *[]tunnel.FirewallRan) error {
	if !cur.Active {
		if !spec.TurnOn {
			return errors.New("firewalld is not running, so nothing can be added to it; turn it on first")
		}
		if err := ranCmd(ctx, f.run, ran, tunnel.FirewalldEnable()); err != nil {
			return err
		}
	}
	z, err := f.run(ctx, "firewall-cmd", "--get-default-zone")
	zone := strings.TrimSpace(z)
	if err != nil || zone == "" {
		return errors.New("firewalld's default zone could not be read")
	}
	wm, keys := wantMap(want)
	state := f.loadFwdLines()
	save := func() error {
		if err := f.saveFwdLines(state); err != nil {
			return fmt.Errorf("cannot keep the list of Argus's firewalld rules: %w", err)
		}
		return nil
	}
	quiet := func(c tunnel.FirewallCmd) error {
		// A rule already gone is not a failure when taking it away.
		out, err := f.run(ctx, c[0], c[1:]...)
		ok := err == nil || strings.Contains(out, "NOT_ENABLED") || strings.Contains(out, "not in zone")
		*ran = append(*ran, tunnel.FirewallRan{Command: c.String(), OK: ok, Output: strings.TrimSpace(tail([]byte(out), 400))})
		if !ok {
			return fmt.Errorf("%s: %v", c.String(), err)
		}
		return nil
	}
	var old []string
	for k := range state {
		if k == "outdrop" {
			continue
		}
		if w, ok := wm[k]; !ok || !tunnel.SameLine(w, state[k].Line) {
			old = append(old, k)
		}
	}
	slices.Sort(old)
	for _, k := range old {
		for _, c := range tunnel.FirewalldRemove(state[k].Cmds) {
			if err := quiet(c); err != nil {
				return err
			}
		}
		delete(state, k)
		if err := save(); err != nil {
			return err
		}
	}
	for _, raw := range spec.TakeOver {
		cs, err := tunnel.FirewalldTakeOver(raw)
		if err != nil {
			return err
		}
		for _, c := range cs {
			if err := ranCmd(ctx, f.run, ran, c); err != nil {
				return err
			}
		}
	}
	for _, k := range keys {
		if s, ok := state[k]; ok && tunnel.SameLine(s.Line, wm[k]) {
			continue
		}
		cmds := tunnel.FirewalldAdd(zone, wm[k])
		state[k] = fwdLine{Line: wm[k], Cmds: cmds}
		if err := save(); err != nil {
			return err
		}
		for _, c := range cmds {
			if err := ranCmd(ctx, f.run, ran, c); err != nil {
				return err
			}
		}
	}
	switch _, dropping := state["outdrop"]; {
	case spec.DefaultOut == "deny" && !dropping:
		cmds := tunnel.FirewalldOutBase()
		state["outdrop"] = fwdLine{Cmds: cmds}
		if err := save(); err != nil {
			return err
		}
		for _, c := range cmds {
			if err := ranCmd(ctx, f.run, ran, c); err != nil {
				return err
			}
		}
	case spec.DefaultOut == "allow" && dropping:
		for _, c := range tunnel.FirewalldRemove(state["outdrop"].Cmds) {
			if err := quiet(c); err != nil {
				return err
			}
		}
		delete(state, "outdrop")
		if err := save(); err != nil {
			return err
		}
	}
	if spec.DefaultIn != "" {
		for _, c := range []tunnel.FirewallCmd{tunnel.FirewalldTarget(zone, spec.DefaultIn), tunnel.FirewalldReload()} {
			if err := ranCmd(ctx, f.run, ran, c); err != nil {
				return err
			}
		}
	}
	return nil
}

/* ── nftables ────────────────────────────────────────────────────────────── */

type nftPolicy struct{ In, Out string }

func (f *linuxFW) applyNft(ctx context.Context, spec tunnel.FirewallChangeSpec, want []tunnel.FirewallLine, ran *[]tunnel.FirewallRan) error {
	polPath := filepath.Join(f.stateDir, firewallDir, "argus-nft-policy.json")
	pol := nftPolicy{In: "accept", Out: "accept"}
	if b, err := f.readFile(polPath); err == nil {
		_ = json.Unmarshal(b, &pol)
	}
	switch {
	case spec.DefaultIn == "deny" || spec.TurnOn:
		pol.In = "drop"
	case spec.DefaultIn == "allow":
		pol.In = "accept"
	}
	switch spec.DefaultOut {
	case "deny":
		pol.Out = "drop"
	case "allow":
		pol.Out = "accept"
	}
	text := tunnel.NftTable(want, pol.In, pol.Out)
	if err := f.writeFile(f.nftPath(), []byte(text)); err != nil {
		return err
	}
	if err := f.writeFile(polPath, mustJSON(pol)); err != nil {
		return err
	}
	if err := ranCmd(ctx, f.run, ran, tunnel.FirewallCmd{"nft", "-f", f.nftPath()}); err != nil {
		return err
	}
	// Loaded again at every boot, before the network comes up.
	if _, err := f.readFile(nftUnit); err != nil {
		unit := "[Unit]\nDescription=Argus firewall lines (nftables table inet argus)\nBefore=network-pre.target\nWants=network-pre.target\n\n" +
			"[Service]\nType=oneshot\nRemainAfterExit=yes\nExecStart=/usr/sbin/nft -f " + f.nftPath() + "\n\n[Install]\nWantedBy=multi-user.target\n"
		if err := f.writeFile(nftUnit, []byte(unit)); err != nil {
			return err
		}
		_, _ = f.run(ctx, "systemctl", "daemon-reload")
	}
	return ranCmd(ctx, f.run, ran, tunnel.FirewallCmd{"systemctl", "enable", "argus-firewall.service"})
}
