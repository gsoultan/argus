package agent

import "io"

// SpawnMode is how a tunnel session's shell is started.
type SpawnMode string

const (
	// SpawnAuto uses systemd when the agent runs as root on a systemd host,
	// and a direct child otherwise.
	SpawnAuto SpawnMode = ""
	// SpawnSystemd starts the shell in a transient unit, outside the agent's
	// sandbox.
	SpawnSystemd SpawnMode = "systemd"
	// SpawnDirect starts the shell as the agent's child. For hosts without
	// systemd, and tests.
	SpawnDirect SpawnMode = "direct"
)

// shellProc is a held shell.
type shellProc interface {
	Pid() int
	// Release lets the shell run.
	Release() error
	// Abort makes a held shell exit without running anything.
	Abort()
	// Wait returns the exit code, once.
	Wait() int
	// Hangup ends the session; a shell still running after hangupWait is
	// killed.
	Hangup()
	// Summary is what started the shell said about how it ended, after
	// Wait; empty when there is nothing to add.
	Summary() string
}

// terminalIO is the session's terminal as the agent holds it: a pty master
// on Unix, a pseudoconsole's pipes on Windows.
type terminalIO interface {
	io.ReadWriteCloser
	Resize(cols, rows int) error
}
