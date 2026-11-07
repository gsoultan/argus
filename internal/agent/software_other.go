//go:build !linux

package agent

import (
	"context"

	"github.com/gsoultan/argus/internal/tunnel"
)

// Installing software from the console is for Linux servers so far.
func hostSoftwareAsk(string) func(context.Context, tunnel.GrantSoftware) tunnel.SoftwareResult {
	return func(context.Context, tunnel.GrantSoftware) tunnel.SoftwareResult {
		return tunnel.SoftwareResult{Error: "installing software from the console is for Linux servers so far"}
	}
}
