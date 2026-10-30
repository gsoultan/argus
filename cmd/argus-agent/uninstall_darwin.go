package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const (
	launchdLabel = "system/com.argus.agent"
	launchdPlist = "/Library/LaunchDaemons/com.argus.agent.plist"
)

// stopAgent unloads the job, which stops the daemon and waits for it to seal
// its recordings within the plist's ExitTimeOut. Unloading rather than
// stopping, because the job is KeepAlive and launchd would start it again.
func stopAgent() error {
	if exec.Command("launchctl", "print", launchdLabel).Run() != nil {
		return nil
	}
	if out, err := exec.Command("launchctl", "bootout", launchdLabel).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootout: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func startAgent() error {
	if _, err := os.Stat(launchdPlist); err != nil {
		return errors.New(launchdPlist + " is not installed")
	}
	if out, err := exec.Command("launchctl", "bootstrap", "system", launchdPlist).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// removeAgentService deletes the launchd job and the binary, which were
// installed by hand rather than by a package.
func removeAgentService() ([]string, error) {
	var left []string
	if err := os.Remove(launchdPlist); err != nil && !os.IsNotExist(err) {
		left = append(left, "Delete "+launchdPlist+".")
	}
	if exe, err := os.Executable(); err == nil {
		if err := os.Remove(exe); err != nil {
			left = append(left, "Delete the binary: rm "+exe)
		}
	}
	return left, nil
}

// reloadSSHD has nothing to do: macOS starts sshd for each connection, so the
// next login reads the new configuration. Restarting it here would cut off
// whoever is running this over SSH.
func reloadSSHD() (string, error) {
	return "sshd reads the change on the next login", nil
}
