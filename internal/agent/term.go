package agent

import (
	"os"

	xterm "golang.org/x/term"
)

// term wraps the handful of terminal operations the shim needs, so call sites
// read cleanly and the raw-mode restore is hard to forget.
var term terminal

type terminal struct{}

// IsTerminal reports whether stdin is a tty. sshd allocates one only when the
// client asked for it, so this is what distinguishes an interactive shell from
// `ssh host cmd`, scp and sftp.
func (terminal) IsTerminal() bool {
	return xterm.IsTerminal(int(os.Stdin.Fd()))
}

// MakeRaw puts the terminal into raw mode and returns a restore function.
//
// Without raw mode the local terminal line-buffers and echoes, so the remote
// shell never sees a keystroke until Enter, and everything appears twice.
// The returned function must run on every exit path or the user is left with a
// terminal that no longer echoes.
func (terminal) MakeRaw(fd int) (restore func(), err error) {
	state, err := xterm.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	return func() { _ = xterm.Restore(fd, state) }, nil
}
