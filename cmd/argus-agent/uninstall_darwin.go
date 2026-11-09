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
	// pkgID is the identifier packaging/macos/build-pkg.sh gives the .pkg.
	pkgID = "com.argus.agent"
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

// removeAgentService deletes the launchd job and the binary, and the
// installer's receipt when the .pkg put them there, so the Mac no longer
// lists the agent as installed and a later install starts clean. The .pkg
// has no remove step of its own, so fromPackage changes nothing here.
func removeAgentService(bool) ([]string, error) {
	var left []string
	if err := os.Remove(launchdPlist); err != nil && !os.IsNotExist(err) {
		left = append(left, "Delete "+launchdPlist+".")
	}
	if exe, err := os.Executable(); err == nil {
		if err := os.Remove(exe); err != nil {
			left = append(left, "Delete the binary: rm "+exe)
		}
	}
	if exec.Command("pkgutil", "--pkg-info", pkgID).Run() == nil {
		if out, err := exec.Command("pkgutil", "--forget", pkgID).CombinedOutput(); err != nil {
			left = append(left, "Forget the installer's receipt: sudo pkgutil --forget "+pkgID+
				" ("+strings.TrimSpace(string(out))+")")
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

// stopSelfForGood removes the launchd job, so it does not start at boot, and
// unloads it, which stops this daemon the way a shutdown does.
func stopSelfForGood() error {
	if err := os.Remove(launchdPlist); err != nil && !os.IsNotExist(err) {
		return err
	}
	// Started, not waited on: it stops the process that would be waiting.
	return exec.Command("launchctl", "bootout", launchdLabel).Start()
}
