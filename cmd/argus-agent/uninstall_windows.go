package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// stopAgent stops the service and waits for it to have stopped, which is
// after it has sealed its recordings. Not installed is not an error: there is
// nothing running to stop.
func stopAgent() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return nil
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return err
	}
	if st.State != svc.Stopped && st.State != svc.StopPending {
		if _, err := s.Control(svc.Stop); err != nil {
			return err
		}
	}
	// The service asks for 70 seconds to drain and seal; a little more here.
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if st, err = s.Query(); err != nil {
			return err
		}
		if st.State == svc.Stopped {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return errors.New("the service is still stopping after 90 seconds")
}

func startAgent() error { return startService() }

// removeAgentService deletes the service. The binary is the one running this,
// which Windows does not let a process delete, so that is left to the person.
func removeAgentService() ([]string, error) {
	var left []string
	if err := uninstallService(); err != nil && err.Error() != "not installed" {
		return nil, fmt.Errorf("the service could not be removed: %w", err)
	}
	if exe, err := os.Executable(); err == nil {
		left = append(left, fmt.Sprintf("Delete the program: Remove-Item -Recurse '%s'",
			filepath.Dir(exe)))
	}
	return left, nil
}

// removeSSHDHook has nothing to undo: the sshd hook is not ported to Windows,
// so the agent never added one.
func removeSSHDHook() (notes, left []string, err error) {
	return []string{"the agent adds nothing to sshd on Windows; nothing to change"}, nil, nil
}

// remainingSSHWaysIn has nothing to check: the agent never stood between
// Windows' OpenSSH server and its logins.
func remainingSSHWaysIn() ([]string, bool) { return nil, false }
