//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// shimSupported says whether this platform can record direct SSH logins.
const shimSupported = true

func isPrivileged() bool { return os.Geteuid() == 0 }

const privilegedName = "root"

// stopRequests delivers the request to shut down.
func stopRequests() <-chan struct{} {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	stop := make(chan struct{})
	go func() { <-sig; close(stop) }()
	return stop
}

// runAsService runs the daemon under the platform's service manager when
// it started us; on Unix systemd and launchd run the daemon directly.
func runAsService() (int, bool) { return 0, false }

func runService(args []string) int {
	fmt.Fprintln(os.Stderr, "argus-agent service: the service is managed with "+startHint)
	return 2
}
