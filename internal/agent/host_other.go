//go:build !darwin && !windows

package agent

// On Linux these come from /etc/machine-id and /etc/os-release.
func platformMachineID() string { return "" }
func platformOSName() string    { return "" }
