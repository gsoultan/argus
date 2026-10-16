//go:build darwin

package agent

import (
	"fmt"
	"os/exec"
)

// directoryPasswd lists local accounts from Directory Services.
func directoryPasswd() ([]byte, error) {
	out, err := exec.Command("/usr/bin/dscl", ".", "-readall", "/Users",
		"RecordName", "UniqueID", "PrimaryGroupID", "NFSHomeDirectory", "UserShell").Output()
	if err != nil {
		return nil, fmt.Errorf("cannot list accounts with dscl: %w", err)
	}
	return dsclToPasswd(out), nil
}
