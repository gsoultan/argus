//go:build !windows

package secrets

import (
	"fmt"
	"os"
)

// ownerOnly refuses a file with any group or other permission bit set.
func ownerOnly(path string, info os.FileInfo) error {
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s is mode %04o; a private key must not be readable "+
			"by group or others (chmod 600 %s)", path, perm, path)
	}
	return nil
}
