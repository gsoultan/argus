//go:build !windows

package agent

import (
	"fmt"
	"os"

	"github.com/creack/pty"

	"github.com/gsoultan/argus/internal/tunnel"
)

// ptyTerminal is a pty master.
type ptyTerminal struct{ *os.File }

func (p ptyTerminal) Resize(cols, rows int) error {
	return pty.Setsize(p.File, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

// openShell allocates a pty and starts the account's login shell on it,
// held until the recording is open.
func openShell(cfg TerminalConfig, g tunnel.Grant) (terminalIO, shellProc, string, error) {
	acct, err := lookupAccount(g.Principal)
	if err != nil {
		return nil, nil, "", err
	}
	ptmx, tty, err := pty.Open()
	if err != nil {
		return nil, nil, "", fmt.Errorf("cannot allocate a terminal: %w", err)
	}
	// The account owns its terminal, as it would after an sshd login, so
	// programs that reopen it by name work.
	_ = tty.Chown(acct.uid, acct.gid)
	_ = pty.Setsize(ptmx, &pty.Winsize{Cols: startCols, Rows: startRows})
	proc, err := spawnShell(cfg.Spawn, acct, tty, g.Email)
	_ = tty.Close()
	if err != nil {
		_ = ptmx.Close()
		return nil, nil, "", err
	}
	return ptyTerminal{ptmx}, proc, acct.name, nil
}
