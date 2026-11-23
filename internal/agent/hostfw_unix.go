//go:build !windows

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

// HostFirewallReader reads the server's firewall for the facts report: as
// root through `argus-agent firewall-read` in a unit of its own, or in
// process on a root agent with no systemd (and so no sandbox to step out of).
func HostFirewallReader(executable, stateDir string) func(context.Context) tunnel.HostFirewall {
	return func(ctx context.Context) tunnel.HostFirewall {
		fail := func(msg string) tunnel.HostFirewall {
			return tunnel.HostFirewall{Rules: []tunnel.FirewallRule{}, ReadAt: time.Now().UTC(), Error: msg}
		}
		if runtime.GOOS != "linux" {
			return fail("Argus reads the firewall on Linux and Windows servers")
		}
		if os.Geteuid() != 0 {
			return fail("the agent is not running as root, so it cannot read the firewall")
		}
		req := firewallReadRequest{}
		if stateDir != "" {
			req.StateFile = filepath.Join(stateDir, firewallStateName)
		}
		if !hostSystemd() || executable == "" {
			cctx, cancel := context.WithTimeout(ctx, firewallReadTimeout)
			defer cancel()
			return readLinuxFirewall(cctx, execRun, lookPath, req.StateFile).Bounded()
		}
		in, _ := json.Marshal(req)
		cctx, cancel := context.WithTimeout(ctx, firewallReadTimeout+15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(cctx, "systemd-run", "--pipe", "--wait", "--quiet", "--collect",
			"--service-type=exec", "--description=Argus firewall read", executable, "firewall-read")
		var stdout, stderr bytes.Buffer
		cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(in), &stdout, &stderr
		if err := cmd.Run(); err != nil && stdout.Len() == 0 {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = err.Error()
			}
			return fail("the firewall could not be read: " + tail([]byte(msg), 300))
		}
		var res tunnel.HostFirewall
		if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
			return fail("the firewall's answer could not be read")
		}
		return res.Bounded()
	}
}

// hostFirewallBackend is the firewall the helper changes: ufw, firewalld or
// Argus's own nftables table.
func hostFirewallBackend(stateDir string) fwBackend { return newLinuxFW(stateDir) }

type unixFWPlatform struct{ executable, stateDir string }

func hostFirewallPlatform(executable, stateDir string) fwPlatform {
	return unixFWPlatform{executable: executable, stateDir: stateDir}
}

func (p unixFWPlatform) supported() string {
	if runtime.GOOS != "linux" {
		return "Argus changes the firewall on Linux and Windows servers"
	}
	if os.Geteuid() != 0 {
		return "the agent is not running as root, so it cannot change the firewall"
	}
	return ""
}

// launch starts the helper in a unit of its own, so a change outlives the
// agent restarting and the undo still runs; with no systemd (and so no
// sandbox) it runs in this process.
func (p unixFWPlatform) launch(ctx context.Context, planPath, unit string) error {
	if !hostSystemd() || p.executable == "" {
		go RunFirewallApply(planPath)
		return nil
	}
	cmd := exec.CommandContext(ctx, "systemd-run", "--quiet", "--collect",
		"--unit="+unit, "--service-type=exec", "--description=Argus firewall change",
		p.executable, "firewall-apply", "--plan", planPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return errors.New("systemd-run: " + err.Error() + ": " + strings.TrimSpace(string(out)))
	}
	return nil
}

func (p unixFWPlatform) active(unit string) bool {
	if !hostSystemd() {
		return true
	}
	return exec.Command("systemctl", "is-active", "--quiet", unit+".service").Run() == nil
}
