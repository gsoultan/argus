//go:build !windows

package agent

import (
	"path/filepath"
	"testing"
)

// A stopped agent must not lock everyone out: by default the login goes
// ahead, unrecorded, and only --fail-closed refuses it.
func TestALoginGoesAheadWhileTheAgentIsDown(t *testing.T) {
	t.Setenv("SSH_ORIGINAL_COMMAND", "exit 3")
	t.Setenv("SSH_CONNECTION", "203.0.113.9 50000 10.0.0.1 22")
	gone := filepath.Join(t.TempDir(), "no-agent.sock")

	if code := RunShim(ShimConfig{SocketPath: gone, Shell: "/bin/sh",
		OpenWhenAgentDown: true}); code != 3 {
		t.Errorf("with the agent down the command should have run and exited 3, got %d", code)
	}
	if code := RunShim(ShimConfig{SocketPath: gone, Shell: "/bin/sh"}); code != 1 {
		t.Errorf("fail-closed should refuse with 1, got %d", code)
	}
}
