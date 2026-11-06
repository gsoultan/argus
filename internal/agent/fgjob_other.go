//go:build !linux && !windows

package agent

import "golang.org/x/sys/unix"

// foregroundBusy reports whether something other than the login shell is in
// the terminal's foreground. Without TIOCGSID the shell's own process group
// stands in for the session's: a direct child's pid is the shell's.
func foregroundBusy(term terminalIO, shellPid int) (busy, ok bool) {
	p, isPty := term.(ptyTerminal)
	if !isPty || shellPid <= 0 {
		return false, false
	}
	pgrp, err := unix.IoctlGetInt(int(p.Fd()), unix.TIOCGPGRP)
	if err != nil || pgrp <= 0 {
		return false, false
	}
	return pgrp != shellPid, true
}
