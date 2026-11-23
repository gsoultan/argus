//go:build !windows

package agent

import (
	"bytes"
	"context"
	"encoding/json"
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
