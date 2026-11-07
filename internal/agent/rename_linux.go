//go:build linux

package agent

import (
	"os"

	"golang.org/x/sys/unix"
)

// renameExclusive moves from to to, failing with EEXIST when to exists.
func renameExclusive(from, to string) error {
	err := unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
	if err == unix.EINVAL || err == unix.ENOSYS {
		// A filesystem without RENAME_NOREPLACE: the caller checked already.
		return renamePlain(from, to)
	}
	if err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	return nil
}
