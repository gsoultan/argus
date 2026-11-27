//go:build linux

package vault

import (
	"log/slog"

	"golang.org/x/sys/unix"
)

// hardenProcess keeps keys and values out of reach of other processes of the
// same user and out of core dumps: a process that is not dumpable writes no
// core file, and /proc/<pid>/mem and ptrace are closed to anyone but root.
// It does not call setrlimit: the systemd unit's SystemCallFilter kills a
// process that does (it sets LimitCORE=0 itself).
func hardenProcess(log *slog.Logger) {
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		log.Warn("could not mark the process non-dumpable; a core dump could hold keys", "err", err)
	}
}

// lockMemory keeps b out of swap. Best effort: RLIMIT_MEMLOCK may be too
// small, and the key is still cleared after use either way.
func lockMemory(b []byte) {
	if len(b) > 0 {
		_ = unix.Mlock(b)
	}
}

// dumpable says whether the process may dump core, for tests.
func dumpable() bool {
	v, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
	return err != nil || v != 0
}
