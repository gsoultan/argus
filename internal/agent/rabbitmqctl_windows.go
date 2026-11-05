package agent

import (
	"context"
	"os/exec"
)

// rabbitmqctlCmd runs rabbitmqctl as the agent: Windows has no wrapper that
// switches accounts, and the CLI reads the cookie from the agent's profile.
func rabbitmqctlCmd(ctx context.Context, bin string, args []string) *exec.Cmd {
	return exec.CommandContext(ctx, bin, args...)
}
