//go:build !windows

package agent

import (
	"bytes"
	"context"
	"os"
	"os/exec"
)

// SystemdOutside runs `helper personal-apply` as a transient systemd unit,
// so a pass runs as root without the agent's sandbox. It is the daemon's
// Provisioner.Outside on a systemd host when the agent is root, and nil
// otherwise: there the pass runs in the agent, which has no sandbox to leave.
func SystemdOutside(helper string) func(context.Context, []byte) ([]byte, []byte, error) {
	if os.Geteuid() != 0 || !systemdRunning() {
		return nil
	}
	return func(ctx context.Context, request []byte) ([]byte, []byte, error) {
		cmd := exec.CommandContext(ctx, "systemd-run", "--pipe", "--wait", "--quiet", "--collect",
			"--service-type=exec", "--description=Argus personal accounts", helper, "personal-apply")
		var stdout, stderr bytes.Buffer
		cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(request), &stdout, &stderr
		err := cmd.Run()
		return stdout.Bytes(), stderr.Bytes(), err
	}
}
