//go:build !windows

package agent

import (
	"fmt"
	"os"
)

// ProtectStateDir creates dir readable by root alone.
func ProtectStateDir(dir string) error {
	return os.MkdirAll(dir, 0o700)
}

// prepareSocket makes the collector's socket reachable by every local
// user: their session is what is being recorded. Confidentiality comes from
// the daemon owning the output, not from who may speak to it.
func prepareSocketDir(dir string) error {
	return os.MkdirAll(dir, 0o755)
}

func openSocket(path string) error {
	if err := os.Chmod(path, 0o666); err != nil {
		return fmt.Errorf("chmod socket: %w", err)
	}
	return nil
}
