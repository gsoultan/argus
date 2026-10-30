//go:build !darwin && !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const agentUnit = "argus-agent.service"

// stopAgent stops the unit and waits: systemctl stop returns once the daemon
// has sealed its recordings and exited, within TimeoutStopSec.
func stopAgent() error {
	if !haveUnit() {
		return nil
	}
	return systemctl("stop", agentUnit)
}

func startAgent() error {
	if !haveUnit() {
		return errors.New("the argus-agent unit is not installed")
	}
	return systemctl("start", agentUnit)
}

// removeAgentService disables the unit. The unit file and the binary belong to
// the package, so removing them is the package manager's job.
func removeAgentService() ([]string, error) {
	var left []string
	if haveUnit() {
		if err := systemctl("disable", agentUnit); err != nil {
			return nil, err
		}
	}
	switch {
	case quiet("dpkg-query", "-W", "-f=${Status}", "argus-agent"):
		left = append(left, "Remove the package: apt purge argus-agent "+
			"(purge also removes /etc/argus/agent.env)")
	case quiet("rpm", "-q", "argus-agent"):
		left = append(left, "Remove the package: dnf remove argus-agent")
	case quiet("apk", "info", "-e", "argus-agent"):
		left = append(left, "Remove the package: apk del argus-agent")
	default:
		if exe, err := os.Executable(); err == nil {
			left = append(left, "Delete the binary: rm "+exe)
		}
	}
	return left, nil
}

func reloadSSHD() (string, error) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return "", errors.New("systemctl is not available")
	}
	for _, unit := range []string{"ssh.service", "sshd.service"} {
		if quiet("systemctl", "reload", unit) {
			return "reloaded " + unit, nil
		}
	}
	return "", errors.New("neither ssh.service nor sshd.service reloaded")
}

func haveUnit() bool {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	return quiet("systemctl", "cat", agentUnit)
}

func systemctl(args ...string) error {
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err,
			strings.TrimSpace(string(out)))
	}
	return nil
}

// quiet runs a command for its exit status alone.
func quiet(name string, args ...string) bool {
	return exec.Command(name, args...).Run() == nil
}
