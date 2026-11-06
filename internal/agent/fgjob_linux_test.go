package agent

import (
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// At its prompt a shell is not busy; running something in the foreground,
// it is. This is what decides whether a held terminal nobody came back to is
// kept for its command or hung up.
func TestAShellIsBusyOnlyWhileACommandRunsInTheForeground(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	cmd := exec.Command(sh, "-i")
	cmd.Env = []string{"PS1=$ ", "PATH=/usr/bin:/bin"}
	ptmx, err := pty.StartWithAttrs(cmd, nil, &syscall.SysProcAttr{Setsid: true, Setctty: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ptmx.Close()
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := ptmx.Read(buf); err != nil {
				return
			}
		}
	}()

	wait := func(want bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if busy, ok := foregroundBusy(ptyTerminal{ptmx}, cmd.Process.Pid); ok && busy == want {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("never saw busy=%v", want)
	}
	wait(false)
	if _, err := ptmx.Write([]byte("sleep 3\n")); err != nil {
		t.Fatal(err)
	}
	wait(true)
	wait(false)
}
