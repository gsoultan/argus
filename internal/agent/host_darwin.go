//go:build darwin

package agent

import (
	"os/exec"
	"strings"
)

// platformMachineID is the hardware UUID, which survives a rename and a
// reinstall the way /etc/machine-id does on Linux.
func platformMachineID() string {
	out, err := exec.Command("/usr/sbin/ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && strings.Contains(k, `"IOPlatformUUID"`) {
			return strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return ""
}

// platformOSName is "macOS 15.1", from sw_vers.
func platformOSName() string {
	name, err := exec.Command("/usr/bin/sw_vers", "-productName").Output()
	if err != nil {
		return ""
	}
	ver, _ := exec.Command("/usr/bin/sw_vers", "-productVersion").Output()
	return strings.TrimSpace(strings.TrimSpace(string(name)) + " " + strings.TrimSpace(string(ver)))
}
