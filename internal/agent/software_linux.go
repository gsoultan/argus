//go:build linux

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/gsoultan/argus/internal/tunnel"
)

// hostSoftwareAsk runs `argus-agent software-query` as root outside the
// agent's sandbox, which cannot read every package manager's cache nor
// refresh the lists, in a unit of its own, the question on stdin.
func hostSoftwareAsk(executable string) func(context.Context, tunnel.GrantSoftware) tunnel.SoftwareResult {
	return func(ctx context.Context, q tunnel.GrantSoftware) tunnel.SoftwareResult {
		switch {
		case os.Geteuid() != 0:
			return tunnel.SoftwareResult{Error: "the agent is not running as root, so it cannot install software"}
		case !systemdRunning():
			return tunnel.SoftwareResult{Error: "this server does not run systemd, which installing software from the console needs"}
		case executable == "":
			return tunnel.SoftwareResult{Error: "the agent does not know where its own program is"}
		}
		in, err := json.Marshal(q)
		if err != nil {
			return tunnel.SoftwareResult{Error: err.Error()}
		}
		cmd := exec.CommandContext(ctx, "systemd-run", "--pipe", "--wait", "--quiet", "--collect",
			"--service-type=exec", "--description=Argus software question", executable, "software-query")
		var stdout, stderr bytes.Buffer
		cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(in), &stdout, &stderr
		if err := cmd.Run(); err != nil && stdout.Len() == 0 {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = err.Error()
			}
			return tunnel.SoftwareResult{Error: fmt.Sprintf("the package manager could not be asked: %s", tail([]byte(msg), 400))}
		}
		var res tunnel.SoftwareResult
		if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
			return tunnel.SoftwareResult{Error: "the package manager's answer could not be read"}
		}
		return res
	}
}
