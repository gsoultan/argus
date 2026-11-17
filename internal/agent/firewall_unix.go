//go:build !windows

package agent

// hostSystemd says the server runs systemd, which the firewall helper runs under.
func hostSystemd() bool { return systemdRunning() }
