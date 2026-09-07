package agent

import (
	"os"
	"path/filepath"
	"testing"
)

/*
The SFTP subsystem, under a ForceCommand.

sshd applies ForceCommand to subsystem requests as well as commands, and for
the near-universal `Subsystem sftp internal-sftp` it hands the ForceCommand
SSH_ORIGINAL_COMMAND="internal-sftp". That is compiled into sshd; it is not a
program. Running it through the shell fails, the connection closes with no
message, and every SFTP and SCP transfer to the host stops working the moment
the agent is installed.

Measured on a real target before the fix: sshd passed "internal-sftp", the shim
ran `sh -c internal-sftp`, and sftp(1) reported only "Connection closed".
*/

// The two names sshd uses, and nothing else.
func TestInternalSFTPIsRecognised(t *testing.T) {
	for _, cmd := range []string{"internal-sftp", "sftp-server", "  internal-sftp  "} {
		if !isInternalSFTP(cmd) {
			t.Errorf("%q was not recognised as the sftp subsystem", cmd)
		}
	}
}

// A user command that merely mentions sftp must not be turned into a file
// server. A prefix or substring match here would be a way to reach one.
func TestOrdinaryCommandsAreNotMistakenForTheSubsystem(t *testing.T) {
	for _, cmd := range []string{
		"internal-sftp --help",
		"echo internal-sftp",
		"/usr/bin/sftp-server-wrapper",
		"sftp",
		"cat internal-sftp.log",
		"",
	} {
		if isInternalSFTP(cmd) {
			t.Errorf("%q was treated as the sftp subsystem", cmd)
		}
	}
}

// An executable in a known location is found.
func TestFindsAnSFTPServerWhereDistributionsPutIt(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "sftp-server")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	saved := sftpServerPaths
	t.Cleanup(func() { sftpServerPaths = saved })
	sftpServerPaths = []string{filepath.Join(dir, "absent"), fake}

	got, err := findSFTPServer()
	if err != nil {
		t.Fatalf("findSFTPServer: %v", err)
	}
	if got != fake {
		t.Errorf("found %q, want %q", got, fake)
	}
}

// A directory or a non-executable file is not a server.
func TestANonExecutableIsNotAnSFTPServer(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "sftp-server")
	if err := os.WriteFile(plain, []byte("not a program"), 0o644); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(dir, "adir")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}

	saved := sftpServerPaths
	t.Cleanup(func() { sftpServerPaths = saved })
	sftpServerPaths = []string{subdir, plain}

	if got, err := findSFTPServer(); err == nil {
		// PATH may still hold one on a developer machine; only a hit on the
		// list under test is a failure.
		if got == plain || got == subdir {
			t.Errorf("accepted %q, which is not an executable file", got)
		}
	}
}

// With none present the operator is told what to install, not left with a
// closed connection.
func TestAMissingSFTPServerSaysWhatToInstall(t *testing.T) {
	saved := sftpServerPaths
	t.Cleanup(func() { sftpServerPaths = saved })
	sftpServerPaths = []string{filepath.Join(t.TempDir(), "nothing-here")}

	t.Setenv("PATH", t.TempDir())
	_, err := findSFTPServer()
	if err == nil {
		t.Fatal("expected a refusal when no sftp-server exists")
	}
	for _, want := range []string{"openssh-sftp-server", "ForceCommand"} {
		if !contains(err.Error(), want) {
			t.Errorf("the message should mention %q: %v", want, err)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
