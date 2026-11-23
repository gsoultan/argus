//go:build windows

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
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

/*
Changing Windows Firewall (hostfw_apply.go): Argus's rules go in the rule
group "Argus", each named "argus fw-<key>". A rule made by hand that someone
takes over is switched off, never deleted. The saved copy is Windows's own
export (`netsh advfirewall export`), put back with import.

The helper runs in the agent's process: Windows has no transient units, and
the agent runs as Local System, which may change the firewall.
*/

func hostFirewallBackend(stateDir string) fwBackend {
	return winFW{stateDir: stateDir, ps: runPowerShell}
}

type winFW struct {
	stateDir string
	ps       func(ctx context.Context, script string) ([]byte, error)
}

func (w winFW) read(ctx context.Context) tunnel.HostFirewall { return HostFirewallReader("", "")(ctx) }

func (w winFW) snapshot(ctx context.Context) (fwSnapshot, error) {
	cur := w.read(ctx)
	dir := filepath.Join(w.stateDir, firewallDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fwSnapshot{}, err
	}
	path := filepath.Join(dir, "export-"+strconv.FormatInt(time.Now().UnixNano(), 36)+".wfw")
	if out, err := w.ps(ctx, "netsh advfirewall export "+psQuote(path)); err != nil {
		return fwSnapshot{}, errors.New("netsh advfirewall export: " + strings.TrimSpace(string(out)))
	}
	return fwSnapshot{Kind: tunnel.FirewallWindows, Active: cur.Active, Export: path}, nil
}

func (w winFW) restore(ctx context.Context, s fwSnapshot) error {
	if s.Export == "" {
		return errors.New("the copy holds no Windows Firewall export")
	}
	if out, err := w.ps(ctx, "netsh advfirewall import "+psQuote(s.Export)); err != nil {
		return errors.New("netsh advfirewall import: " + strings.TrimSpace(string(out)))
	}
	return nil
}

func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func (w winFW) apply(ctx context.Context, cur tunnel.HostFirewall, spec tunnel.FirewallChangeSpec, always []tunnel.FirewallLine, ran *[]tunnel.FirewallRan) error {
	do := func(script string) error {
		out, err := w.ps(ctx, "$ErrorActionPreference='Stop'; "+script)
		*ran = append(*ran, tunnel.FirewallRan{Command: script, OK: err == nil, Output: strings.TrimSpace(tail(out, 400))})
		if err != nil {
			return errors.New(script + ": " + strings.TrimSpace(tail(out, 300)))
		}
		return nil
	}
	want := append(append(append([]tunnel.FirewallLine{}, spec.Lines...), spec.Protect...), always...)
	wm, keys := wantMap(want)
	have := cur.ArgusLines()
	for k, h := range have {
		if l, ok := wm[k]; !ok || !tunnel.SameLine(l, h) {
			if err := do(tunnel.WinRemove(k)); err != nil {
				return err
			}
		}
	}
	for _, raw := range spec.TakeOver {
		name := ""
		for _, r := range cur.Rules {
			if r.Raw == raw {
				name = r.Name
			}
		}
		if name == "" {
			return errors.New("the rule " + raw + " is no longer there")
		}
		if err := do(tunnel.WinTakeOver(name)); err != nil {
			return err
		}
	}
	for _, k := range keys {
		if h, ok := have[k]; ok && tunnel.SameLine(h, wm[k]) {
			continue
		}
		cmds := tunnel.WinAdd(wm[k])
		if k == "awupd" {
			// Windows Update and the transfer service it uses, by program.
			cmds = nil
			for _, svc := range []string{"wuauserv", "bits"} {
				cmds = append(cmds, "New-NetFirewallRule -DisplayName "+psQuote(tunnel.FirewallLineMark(k))+" -Group "+
					psQuote(tunnel.WinGroup)+` -Direction Outbound -Action Allow -Program "$env:SystemRoot\System32\svchost.exe" -Service `+
					svc+" -Profile Any | Out-Null")
			}
		}
		for _, c := range cmds {
			if err := do(c); err != nil {
				return err
			}
		}
	}
	if spec.DefaultIn != "" {
		if err := do(tunnel.WinDefault("in", spec.DefaultIn)); err != nil {
			return err
		}
	}
	if spec.DefaultOut != "" {
		if err := do(tunnel.WinDefault("out", spec.DefaultOut)); err != nil {
			return err
		}
	}
	if spec.TurnOn {
		return do(tunnel.WinEnable())
	}
	return nil
}

type winFWPlatform struct{}

func hostFirewallPlatform(string, string) fwPlatform { return winFWPlatform{} }

func (winFWPlatform) supported() string { return "" }

func (winFWPlatform) launch(_ context.Context, planPath, _ string) error {
	go RunFirewallApply(planPath)
	return nil
}

func (winFWPlatform) active(string) bool { return true }
