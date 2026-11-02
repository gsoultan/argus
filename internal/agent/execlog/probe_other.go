//go:build !linux

package execlog

import (
	"context"
	"log/slog"
	"runtime"
)

// Open reports that kernel-observed execution is not available.
//
// Argus targets Linux; this exists so the agent, the control plane and the
// tests all build and run on a developer's machine. The failure is
// ErrUnsupported rather than a plain error so the caller takes the same
// documented fallback it would take on a Linux host with an unsuitable kernel —
// one path, exercised everywhere, instead of one that only runs in production.
func Open(_ context.Context, _ *slog.Logger) (Probe, error) {
	return nil, Unsupported(
		"kernel execution tracing requires Linux; this agent is running on %s", runtime.GOOS)
}
