package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

/*
Starting the shell.

The agent's own unit is a sandbox: a read-only system and home, a private
/tmp, no new privileges, a handful of capabilities. A child inherits all of
it, so a shell started as the agent's child is not a login -- an
administrator's root session could not install a package, and sudo would not
work. On a systemd host the shell therefore runs in a transient unit of its
own, started by systemd as the account, with a PAM session like any login,
on the terminal the agent allocated. The agent keeps the terminal, so it
still sees, and records, every byte.

Either way the shell is held before it runs anything: the process exists, so
kernel tracing can attach to its pid, but it waits for the agent to say go,
which the agent says only once the recording is open.
*/

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

type loginAccount struct {
	name, home, shell string
	uid, gid          int
	groups            []uint32
}

func lookupAccount(name string) (loginAccount, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return loginAccount{}, fmt.Errorf("this host has no account named %s", name)
	}
	a := loginAccount{name: u.Username, home: u.HomeDir, shell: shellFromPasswd(u.Username)}
	a.uid, _ = strconv.Atoi(u.Uid)
	a.gid, _ = strconv.Atoi(u.Gid)
	if a.shell == "" {
		a.shell = "/bin/sh"
	}
	if ids, gerr := u.GroupIds(); gerr == nil {
		for _, s := range ids {
			if n, perr := strconv.Atoi(s); perr == nil {
				a.groups = append(a.groups, uint32(n))
			}
		}
	}
	return a, nil
}

func spawnShell(mode SpawnMode, a loginAccount, tty *os.File, actor string) (shellProc, error) {
	if mode == SpawnAuto {
		mode = SpawnDirect
		if os.Geteuid() == 0 && systemdRunning() {
			mode = SpawnSystemd
		}
	}
	if a.uid != os.Geteuid() && os.Geteuid() != 0 {
		return nil, fmt.Errorf("the agent is not running as root, so it cannot "+
			"open a session as %s", a.name)
	}
	if mode == SpawnSystemd {
		return spawnSystemd(a, tty, actor)
	}
	return spawnDirect(a, tty, actor)
}

func systemdRunning() bool {
	if fi, err := os.Stat("/run/systemd/system"); err != nil || !fi.IsDir() {
		return false
	}
	_, err := exec.LookPath("systemd-run")
	return err == nil
}

// launcher holds the shell until released. $1 is what it waits on; "$0" is
// the login shell, and -l makes it read the profile as an ssh login would.
const launcher = `read -r _ < "$1" || exit 1; exec "$0" -l`

/* ── A direct child ──────────────────────────────────────────────────────── */

type directProc struct {
	cmd  *exec.Cmd
	hold *os.File

	waitOnce sync.Once
	code     int
	done     chan struct{}
}

func spawnDirect(a loginAccount, tty *os.File, actor string) (shellProc, error) {
	release, hold, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer release.Close()
	cmd := exec.Command("/bin/sh", "-c", launcher, a.shell, "/dev/fd/3")
	cmd.Dir = a.home
	cmd.Env = []string{
		"HOME=" + a.home, "USER=" + a.name, "LOGNAME=" + a.name, "SHELL=" + a.shell,
		"TERM=xterm-256color", "LANG=C.UTF-8",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"ARGUS_SESSION_FOR=" + actor,
	}
	cmd.ExtraFiles = []*os.File{release}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if a.uid != os.Geteuid() {
		cmd.SysProcAttr.Credential = &syscall.Credential{
			Uid: uint32(a.uid), Gid: uint32(a.gid), Groups: a.groups}
	}
	if err := cmd.Start(); err != nil {
		_ = hold.Close()
		return nil, fmt.Errorf("cannot start a shell for %s: %w", a.name, err)
	}
	return &directProc{cmd: cmd, hold: hold, done: make(chan struct{})}, nil
}

func (p *directProc) Pid() int        { return p.cmd.Process.Pid }
func (p *directProc) Summary() string { return "" }

func (p *directProc) Release() error {
	_, err := p.hold.Write([]byte("\n"))
	_ = p.hold.Close()
	return err
}

// Abort closes the pipe unwritten: the launcher's read fails and it exits.
func (p *directProc) Abort() { _ = p.hold.Close() }

func (p *directProc) Wait() int {
	p.waitOnce.Do(func() {
		p.code = exitCode(p.cmd.Wait())
		close(p.done)
	})
	return p.code
}

func (p *directProc) Hangup() {
	pid := p.cmd.Process.Pid
	// The whole session, not only the shell: its children run under this
	// recording too.
	_ = syscall.Kill(-pid, syscall.SIGHUP)
	go func() {
		select {
		case <-p.done:
		case <-time.After(hangupWait):
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	}()
}

/* ── A transient systemd unit ────────────────────────────────────────────── */

// sessionFifoDir holds each held shell's release fifo.
const sessionFifoDir = "/run/argus/sessions"

type systemdProc struct {
	unit, fifo string
	pid        int
	// summary is what systemd-run said, for the log.
	summary string

	code int
	done chan struct{}
}

func spawnSystemd(a loginAccount, tty *os.File, actor string) (shellProc, error) {
	id := make([]byte, 8)
	_, _ = rand.Read(id)
	unit := "argus-session-" + hex.EncodeToString(id) + ".service"

	if err := os.MkdirAll(sessionFifoDir, 0o711); err != nil {
		return nil, err
	}
	fifo := filepath.Join(sessionFifoDir, hex.EncodeToString(id))
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		return nil, fmt.Errorf("cannot hold the shell: %w", err)
	}
	// The shell's account must be able to open it, and the agent holds no
	// CAP_CHOWN to give it away. Nobody else can find it: the directory is
	// not listable and the name is random. Someone who did could only hold
	// a session up until it timed out, never start one.
	if err := os.Chmod(fifo, 0o666); err != nil {
		_ = os.Remove(fifo)
		return nil, err
	}

	// Not --collect: a unit that failed stays loaded until reset-failed,
	// which is what lets its exit status be read once systemd-run returns.
	// systemd-run's own exit code does not carry it on every version (255
	// returned 1 for a shell that exited 7).
	run := exec.Command("systemd-run", "--wait",
		"--service-type=exec", "--unit="+unit,
		"--uid="+a.name,
		// A login: PAM account and session, limits, a logind session.
		"-p", "PAMName=login",
		"-p", "TTYPath="+tty.Name(),
		"-p", "StandardInput=tty", "-p", "StandardOutput=tty", "-p", "StandardError=tty",
		"-p", "WorkingDirectory=~",
		"-p", "SendSIGHUP=yes",
		"-p", "TimeoutStopSec="+strconv.Itoa(int(hangupWait.Seconds()))+"s",
		"--setenv=TERM=xterm-256color",
		"--setenv=ARGUS_SESSION_FOR="+actor,
		"/bin/sh", "-c", launcher, a.shell, fifo)
	var stderr bytes.Buffer
	run.Stdout, run.Stderr = &stderr, &stderr
	if err := run.Start(); err != nil {
		_ = os.Remove(fifo)
		return nil, fmt.Errorf("cannot start a session unit: %w", err)
	}
	p := &systemdProc{unit: unit, fifo: fifo, done: make(chan struct{})}
	go func() {
		err := run.Wait()
		p.summary = strings.TrimSpace(stderr.String())
		p.code = exitCode(err)
		if code, ok := unitExitStatus(p.summary); ok {
			p.code = code
		}
		out, _ := exec.Command("systemctl", "show", "--property=ExecMainCode,ExecMainStatus",
			unit).Output()
		if code, ok := execMainStatus(string(out)); ok {
			p.code = code
		}
		// A failed unit stays until this; a successful one is gone already.
		_ = exec.Command("systemctl", "reset-failed", unit).Run()
		_ = os.Remove(fifo)
		close(p.done)
	}()

	// The unit's main process is the held launcher. Its pid is what kernel
	// tracing attaches to, so the session cannot start without it.
	deadline := time.Now().Add(10 * time.Second)
	for p.pid == 0 && time.Now().Before(deadline) {
		out, _ := exec.Command("systemctl", "show", "--property=MainPID", "--value", unit).Output()
		if n, _ := strconv.Atoi(strings.TrimSpace(string(out))); n > 0 {
			p.pid = n
			break
		}
		select {
		case <-p.done:
			deadline = time.Time{}
		case <-time.After(10 * time.Millisecond):
		}
	}
	if p.pid == 0 {
		p.Abort()
		p.Wait()
		return nil, fmt.Errorf("the session unit did not start: %s", strings.TrimSpace(stderr.String()))
	}
	return p, nil
}

func (p *systemdProc) Pid() int { return p.pid }

func (p *systemdProc) Release() error {
	// Opening a fifo for writing waits for its reader; the launcher is one,
	// or is about to be.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	opened := make(chan error, 1)
	go func() {
		f, err := os.OpenFile(p.fifo, os.O_WRONLY, 0)
		if err == nil {
			_, err = f.Write([]byte("\n"))
			_ = f.Close()
		}
		opened <- err
	}()
	select {
	case err := <-opened:
		_ = os.Remove(p.fifo)
		return err
	case <-ctx.Done():
		_ = os.Remove(p.fifo)
		return errors.New("the held shell never asked to be released")
	}
}

// Abort stops the unit; its launcher never ran anything.
func (p *systemdProc) Abort() {
	_ = os.Remove(p.fifo)
	_ = exec.Command("systemctl", "stop", "--no-block", p.unit).Run()
}

func (p *systemdProc) Wait() int {
	<-p.done
	return p.code
}

func (p *systemdProc) Summary() string {
	<-p.done
	return p.summary
}

// Hangup stops the unit: SIGTERM and SIGHUP to everything in it, then
// SIGKILL after its stop timeout, which is hangupWait.
func (p *systemdProc) Hangup() {
	_ = exec.Command("systemctl", "stop", "--no-block", p.unit).Run()
}

// unitExitStatus reads the main process's status from the summary
// systemd-run --wait prints: "Main processes terminated with:
// code=exited/status=7", or code=killed/status=HUP for a signal.
func unitExitStatus(summary string) (int, bool) {
	_, after, ok := strings.Cut(summary, "code=")
	if !ok {
		return 0, false
	}
	how, rest, ok := strings.Cut(after, "/status=")
	if !ok {
		return 0, false
	}
	status := rest
	if i := strings.IndexAny(rest, " \n\r,"); i >= 0 {
		status = rest[:i]
	}
	if how == "exited" {
		n, err := strconv.Atoi(status)
		return n, err == nil
	}
	// killed or dumped: by a signal, reported by name.
	if sig := unix.SignalNum("SIG" + status); sig != 0 {
		return 128 + int(sig), true
	}
	return 0, false
}

// execMainStatus reads `systemctl show -p ExecMainCode,ExecMainStatus`.
// The code is the waitid one: 1 exited, 2 killed, 3 dumped; the status is
// the exit status or the signal. A unit already collected shows code 0,
// which only a successful one is.
func execMainStatus(show string) (int, bool) {
	var code, status = -1, -1
	for _, line := range strings.Split(show, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			continue
		}
		switch k {
		case "ExecMainCode":
			code = n
		case "ExecMainStatus":
			status = n
		}
	}
	switch {
	case code == 1 && status >= 0:
		return status, true
	case (code == 2 || code == 3) && status > 0:
		return 128 + status, true
	}
	return 0, false
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	return 255
}
