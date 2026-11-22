//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type linuxWGNets struct{ executable string }

func hostWGNetPlatform(executable string) wgnetPlatform { return linuxWGNets{executable: executable} }

func (p linuxWGNets) supported() string {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return "this server has no systemd, which Argus uses to change its network outside the agent's sandbox"
	}
	return ""
}

func (p linuxWGNets) launch(ctx context.Context, planPath, unit string) error {
	cmd := exec.CommandContext(ctx, "systemd-run", "--quiet", "--collect",
		"--unit="+unit, "--service-type=exec", "--description=Argus WireGuard network change",
		p.executable, "wgnet-apply", "--plan", planPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemd-run: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (p linuxWGNets) listSA(ctx context.Context, conn string) (string, error) {
	if !reIPsecConn.MatchString(conn) {
		return "", errors.New("not an Argus connection")
	}
	out, err := exec.CommandContext(ctx, "swanctl", "--list-sas", "--ike", conn).CombinedOutput()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func (p linuxWGNets) active(unit string) bool {
	return exec.Command("systemctl", "is-active", "--quiet", unit+".service").Run() == nil
}

/* ── The helper's operations, as root ───────────────────────────────────── */

const wgConfDir = "/etc/wireguard"

type linuxWGNetOps struct{}

func hostWGNetOps() (wgnetOps, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("the change must run as root")
	}
	return linuxWGNetOps{}, nil
}

func wgRun(ctx context.Context, name string, args ...string) error {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(cctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, tail(out, 400))
	}
	return nil
}

func (linuxWGNetOps) haveTools() bool {
	_, err1 := exec.LookPath("wg-quick")
	_, err2 := exec.LookPath("wg")
	return err1 == nil && err2 == nil
}

func (linuxWGNetOps) install(ctx context.Context) error {
	switch {
	case wgHave("apt-get"):
		return wgRun(ctx, "env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "install", "-y", "-q", "wireguard-tools")
	case wgHave("dnf"):
		return wgRun(ctx, "dnf", "install", "-y", "-q", "wireguard-tools")
	case wgHave("yum"):
		return wgRun(ctx, "yum", "install", "-y", "-q", "wireguard-tools")
	case wgHave("zypper"):
		return wgRun(ctx, "zypper", "--non-interactive", "install", "wireguard-tools")
	}
	return errors.New("no package manager Argus knows; install wireguard-tools and try again")
}

func wgHave(name string) bool { _, err := exec.LookPath(name); return err == nil }

func (linuxWGNetOps) existing() ([]string, error) {
	entries, err := os.ReadDir(wgConfDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if name, ok := strings.CutSuffix(e.Name(), ".conf"); ok && reWGIface.MatchString(name) && e.Type().IsRegular() {
			out = append(out, name)
		}
	}
	return out, nil
}

func wgConfPath(iface string) (string, error) {
	if !reWGIface.MatchString(iface) {
		return "", errors.New("not an Argus interface")
	}
	return filepath.Join(wgConfDir, iface+".conf"), nil
}

func (linuxWGNetOps) readConf(iface string) (string, bool, error) {
	p, err := wgConfPath(iface)
	if err != nil {
		return "", false, err
	}
	fi, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !fi.Mode().IsRegular() {
		return "", false, errors.New(p + " is not a regular file")
	}
	b, err := os.ReadFile(p)
	return string(b), err == nil, err
}

func (linuxWGNetOps) writeConf(iface, text string) error {
	p, err := wgConfPath(iface)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(wgConfDir, 0o700); err != nil {
		return err
	}
	return writeFileAtomic(p, []byte(text), 0o600)
}

func (linuxWGNetOps) removeConf(iface string) error {
	p, err := wgConfPath(iface)
	if err != nil {
		return err
	}
	_ = exec.Command("systemctl", "disable", "--quiet", "wg-quick@"+iface).Run()
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (linuxWGNetOps) up(ctx context.Context, iface string) error {
	if !reWGIface.MatchString(iface) {
		return errors.New("not an Argus interface")
	}
	if err := wgRun(ctx, "wg-quick", "up", iface); err != nil {
		return err
	}
	// Up again after a restart.
	_ = exec.Command("systemctl", "enable", "--quiet", "wg-quick@"+iface).Run()
	return nil
}

func (linuxWGNetOps) down(ctx context.Context, iface string) error {
	if !reWGIface.MatchString(iface) {
		return errors.New("not an Argus interface")
	}
	if _, err := os.Stat("/sys/class/net/" + iface); err != nil {
		return nil
	}
	return wgRun(ctx, "wg-quick", "down", iface)
}

func ufwActive() bool {
	out, err := exec.Command("ufw", "status").Output()
	return err == nil && strings.Contains(string(out), "Status: active")
}

func firewalldRunning() bool {
	return exec.Command("firewall-cmd", "--state").Run() == nil
}

func (linuxWGNetOps) openPort(ctx context.Context, port int) (bool, error) {
	p := strconv.Itoa(port) + "/udp"
	switch {
	case wgHave("ufw") && ufwActive():
		return true, wgRun(ctx, "ufw", "allow", p, "comment", "argus wireguard "+strconv.Itoa(port))
	case wgHave("firewall-cmd") && firewalldRunning():
		if err := wgRun(ctx, "firewall-cmd", "--permanent", "--add-port="+p); err != nil {
			return false, err
		}
		return true, wgRun(ctx, "firewall-cmd", "--add-port="+p)
	}
	// No host firewall Argus knows is on: nothing to open.
	return false, nil
}

func (linuxWGNetOps) closePort(ctx context.Context, port int) error {
	p := strconv.Itoa(port) + "/udp"
	switch {
	case wgHave("ufw") && ufwActive():
		return wgRun(ctx, "ufw", "delete", "allow", p)
	case wgHave("firewall-cmd") && firewalldRunning():
		_ = wgRun(ctx, "firewall-cmd", "--permanent", "--remove-port="+p)
		return wgRun(ctx, "firewall-cmd", "--remove-port="+p)
	}
	return nil
}

func (linuxWGNetOps) readKey(stateDir, network string) (string, error) {
	if !reUUIDish.MatchString(network) {
		return "", errors.New("malformed network id")
	}
	b, err := os.ReadFile(filepath.Join(stateDir, wgnetDir, network+".key"))
	if err != nil {
		return "", err
	}
	k := strings.TrimSpace(string(b))
	if !validWGKey(k) {
		return "", errors.New("the key file is damaged")
	}
	return k, nil
}

/* ── IPsec (ipsec.go) ───────────────────────────────────────────────────── */

const swanConfDir = "/etc/swanctl/conf.d"

func (linuxWGNetOps) haveSwan() bool { return wgHave("swanctl") }

func (linuxWGNetOps) installSwan(ctx context.Context) error {
	switch {
	case wgHave("apt-get"):
		return wgRun(ctx, "env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "install", "-y", "-q", "strongswan-swanctl", "charon-systemd")
	case wgHave("dnf"):
		return wgRun(ctx, "dnf", "install", "-y", "-q", "strongswan")
	case wgHave("yum"):
		return wgRun(ctx, "yum", "install", "-y", "-q", "strongswan")
	case wgHave("zypper"):
		return wgRun(ctx, "zypper", "--non-interactive", "install", "strongswan")
	}
	return errors.New("no package manager Argus knows; install strongSwan with swanctl and try again")
}

func (linuxWGNetOps) swanExisting() ([]string, error) {
	entries, err := os.ReadDir(swanConfDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if name, ok := strings.CutSuffix(e.Name(), ".conf"); ok && reIPsecConn.MatchString(name) && e.Type().IsRegular() {
			out = append(out, name)
		}
	}
	return out, nil
}

func swanConfPath(conn string) (string, error) {
	if !reIPsecConn.MatchString(conn) {
		return "", errors.New("not an Argus connection")
	}
	return filepath.Join(swanConfDir, conn+".conf"), nil
}

func (linuxWGNetOps) readSwan(conn string) (string, bool, error) {
	p, err := swanConfPath(conn)
	if err != nil {
		return "", false, err
	}
	fi, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !fi.Mode().IsRegular() {
		return "", false, errors.New(p + " is not a regular file")
	}
	b, err := os.ReadFile(p)
	return string(b), err == nil, err
}

func (linuxWGNetOps) writeSwan(conn, text string) error {
	p, err := swanConfPath(conn)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(swanConfDir, 0o750); err != nil {
		return err
	}
	return writeFileAtomic(p, []byte(text), 0o600)
}

func (linuxWGNetOps) removeSwan(conn string) error {
	p, err := swanConfPath(conn)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// swanReload starts strongSwan if it is not running and loads every
// connection and secret; a connection whose file went is unloaded.
func (linuxWGNetOps) swanReload(ctx context.Context) error {
	for _, unit := range []string{"strongswan", "strongswan-swanctl"} {
		if exec.Command("systemctl", "cat", unit+".service").Run() == nil {
			_ = wgRun(ctx, "systemctl", "enable", "--now", unit+".service")
			break
		}
	}
	return wgRun(ctx, "swanctl", "--load-all", "--noprompt")
}
