//go:build !windows

package agent

import (
	"io"
	"os"
	"testing"
	"time"

	"github.com/creack/pty"
)

// systemd 255's systemd-run --wait exits 1 for any failing unit, so a shell's
// own exit status has to come from the summary it prints. Seen in the install
// drill: `exit 7` reported as 1.
func TestTheShellsExitStatusIsReadFromTheUnitSummary(t *testing.T) {
	for _, c := range []struct {
		summary string
		want    int
		ok      bool
	}{
		{"Running as unit: argus-session-1.service\nFinished with result: exit-code\n" +
			"Main processes terminated with: code=exited/status=7\nService runtime: 41ms\n", 7, true},
		{"Finished with result: success\nMain processes terminated with: code=exited/status=0\n", 0, true},
		{"Finished with result: signal\nMain processes terminated with: code=killed/status=HUP\n", 129, true},
		{"Failed to start transient service unit: Access denied\n", 0, false},
	} {
		got, ok := unitExitStatus(c.summary)
		if got != c.want || ok != c.ok {
			t.Errorf("%q: got %d %v, want %d %v", c.summary, got, ok, c.want, c.ok)
		}
	}
}

func TestTheShellsExitStatusIsReadFromTheUnit(t *testing.T) {
	for _, c := range []struct {
		show string
		want int
		ok   bool
	}{
		{"ExecMainCode=1\nExecMainStatus=7\n", 7, true},
		{"ExecMainCode=2\nExecMainStatus=1\n", 129, true},
		// Collected: only a successful unit is, and nothing can be said.
		{"ExecMainCode=0\nExecMainStatus=0\n", 0, false},
		{"", 0, false},
	} {
		got, ok := execMainStatus(c.show)
		if got != c.want || ok != c.ok {
			t.Errorf("%q: got %d %v, want %d %v", c.show, got, ok, c.want, c.ok)
		}
	}
}

// A direct child for another account waits on a pipe the agent made. Reopened
// through /dev/fd/3 it was refused once the shell had switched to that
// account, and the session ended before anyone could type.
func TestADirectShellForAnotherAccountStarts(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("switching to another account needs root")
	}
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer ptmx.Close()
	defer tty.Close()
	go func() { _, _ = io.Copy(io.Discard, ptmx) }()

	p, err := spawnDirect(loginAccount{name: "nobody", home: "/", shell: "/bin/sh",
		uid: 65534, gid: 65534}, tty, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	_, _ = ptmx.Write([]byte("exit 5\n"))
	done := make(chan int, 1)
	go func() { done <- p.Wait() }()
	select {
	case code := <-done:
		if code != 5 {
			t.Errorf("the shell exited %d, want 5", code)
		}
	case <-time.After(10 * time.Second):
		p.Hangup()
		t.Fatal("the shell never ran")
	}
}

// A login unit started as Type=exec never finished starting on Ubuntu's
// systemd with PAMName and a TTY: its start job timed out after the default
// 90 seconds and systemd killed the shell, so every session ended at 1m30s
// with exit 129. The unit must not wait on a start it cannot confirm.
func TestASessionUnitIsNotKilledBySystemdsStartTimeout(t *testing.T) {
	args := sessionUnitArgs("argus-session-1.service", loginAccount{name: "ana", shell: "/bin/bash"},
		"/dev/pts/3", "ana@example.com", "/run/argus/sessions/1")
	has := func(want string) bool {
		for _, a := range args {
			if a == want {
				return true
			}
		}
		return false
	}
	if has("--service-type=exec") || !has("--service-type=simple") {
		t.Errorf("the unit must start as Type=simple: %q", args)
	}
	if !has("TimeoutStartSec=infinity") {
		t.Errorf("the unit must not inherit DefaultTimeoutStartSec: %q", args)
	}
	if args[len(args)-1] != "/run/argus/sessions/1" || !has("PAMName=login") || !has("TTYPath=/dev/pts/3") {
		t.Errorf("the login unit lost its shape: %q", args)
	}
}
