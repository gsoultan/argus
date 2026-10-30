package main

import (
	"strings"
	"testing"
)

func TestUninstallCommentsOutOnlyTheAgentsForceCommand(t *testing.T) {
	in := `Port 22
ForceCommand /usr/bin/argus-agent shim
# ForceCommand /usr/bin/argus-agent shim
Match User backup
    ForceCommand=/usr/libexec/argus-shim shim
Match User deploy
	ForceCommand /usr/local/bin/deploy-only
forcecommand "/usr/local/bin/argus-agent" shim
`
	out, n := disableArgusForceCommand([]byte(in))
	if n != 3 {
		t.Fatalf("disabled %d ForceCommand lines, want 3:\n%s", n, out)
	}
	want := `Port 22
# Removed by argus-agent uninstall: ForceCommand /usr/bin/argus-agent shim
# ForceCommand /usr/bin/argus-agent shim
Match User backup
    # Removed by argus-agent uninstall: ForceCommand=/usr/libexec/argus-shim shim
Match User deploy
	ForceCommand /usr/local/bin/deploy-only
# Removed by argus-agent uninstall: forcecommand "/usr/local/bin/argus-agent" shim
`
	if string(out) != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
}

// A config with no hook is left byte for byte, so an uninstall never rewrites
// a file it had no business touching.
func TestUninstallLeavesAConfigWithoutTheHookAlone(t *testing.T) {
	in := "Port 22\nForceCommand /usr/local/bin/deploy-only\nPasswordAuthentication no"
	out, n := disableArgusForceCommand([]byte(in))
	if n != 0 || string(out) != in {
		t.Errorf("changed %d lines:\n%s", n, out)
	}
	if strings.Contains(string(out), sshdHookMarker) {
		t.Error("marked a line it did not change")
	}
}
