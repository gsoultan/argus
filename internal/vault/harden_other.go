//go:build !linux

package vault

import "log/slog"

// hardenProcess has nothing to do here: argus-secret is packaged for Linux,
// where the process is made non-dumpable. Elsewhere it runs as it is.
func hardenProcess(*slog.Logger) {}

func lockMemory([]byte) {}

func dumpable() bool { return true }
