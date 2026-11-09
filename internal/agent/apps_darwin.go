//go:build darwin

package agent

import (
	"context"
	"os/exec"
	"time"
)

// brewCellars are where Homebrew installs: Apple silicon, then Intel.
var brewCellars = []string{"/opt/homebrew/Cellar", "/usr/local/Cellar"}

func platformAppScan(ctx context.Context) hostScan {
	var raw hostScan
	run := func(name string, args ...string) (string, bool) {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		out, err := exec.CommandContext(cctx, name, args...).Output()
		return string(out), err == nil
	}
	// lsof exits 1 when nothing listens; its output is still the answer.
	if out, ok := run("/usr/sbin/lsof", "-nP", "-iTCP", "-sTCP:LISTEN", "-F", "pcn"); ok || out != "" {
		raw.listeners = parseLsofListen(out)
		raw.sources = append(raw.sources, "sockets")
	}
	if out, ok := run("/bin/ps", "-axco", "comm="); ok {
		raw.processes = catalogueNames(out)
	}
	for _, c := range brewCellars {
		if pkgs, ok := brewCellar(c); ok {
			raw.packages = append(raw.packages, pkgs...)
			if len(raw.sources) == 0 || raw.sources[len(raw.sources)-1] != "homebrew" {
				raw.sources = append(raw.sources, "homebrew")
			}
		}
	}
	return raw
}
