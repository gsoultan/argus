//go:build !windows

package agent

import "syscall"

// hangUp sends SIGHUP: PgBouncer's RELOAD.
func hangUp(pid int) error { return syscall.Kill(pid, syscall.SIGHUP) }
