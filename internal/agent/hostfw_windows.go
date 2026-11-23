//go:build windows

package agent

import (
	"context"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

// HostFirewallReader reads Windows Firewall with PowerShell; the agent runs
// as Local System, which may read it.
func HostFirewallReader(string, string) func(context.Context) tunnel.HostFirewall {
	return func(ctx context.Context) tunnel.HostFirewall {
		cctx, cancel := context.WithTimeout(ctx, firewallReadTimeout)
		defer cancel()
		out, err := runPowerShell(cctx, winFirewallScript)
		if err != nil {
			return tunnel.HostFirewall{Kind: tunnel.FirewallWindows, Rules: []tunnel.FirewallRule{}, ReadAt: time.Now().UTC(),
				Error: "Windows Firewall could not be read: " + tail([]byte(strings.TrimSpace(string(out))), 300)}
		}
		fw, err := parseWinFirewall(out)
		if err != nil {
			fw.Error = "Windows Firewall's answer could not be read"
		}
		return fw.Bounded()
	}
}
