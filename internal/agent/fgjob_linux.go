package agent

import (
	"os"

	"golang.org/x/sys/unix"
)

// foregroundBusy reports whether something other than the login shell is in
// the terminal's foreground -- a command the person started and has not got
// back from. ok is false when it cannot tell.
//
// The shell leads the terminal's session, so at its prompt the foreground
// process group is the session's own. Asked of the pty master, which the
// agent holds; Linux answers both questions for the slave's side.
func foregroundBusy(term terminalIO, _ int) (busy, ok bool) {
	p, isPty := term.(ptyTerminal)
	if !isPty {
		return false, false
	}
	return foregroundBusyFile(p.File)
}

func foregroundBusyFile(f *os.File) (busy, ok bool) {
	fd := int(f.Fd())
	pgrp, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
	if err != nil || pgrp <= 0 {
		return false, false
	}
	sid, err := unix.IoctlGetInt(fd, unix.TIOCGSID)
	if err != nil || sid <= 0 {
		return false, false
	}
	return pgrp != sid, true
}
