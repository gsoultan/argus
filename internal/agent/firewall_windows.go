//go:build windows

package agent

// hostSystemd is false on Windows: Argus changes ufw and firewalld only.
func hostSystemd() bool { return false }
