//go:build !windows

package main

import (
	"errors"
	"os"
	"syscall"
)

// openNoFollow opens path for reading, refusing a symlink in its last part.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

// checkRequestOwner refuses a request that is not a plain file, or that
// anyone but its owner can write: then someone other than argus-control
// could have put a token there.
func checkRequestOwner(f *os.File) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	if st.Mode().Perm()&0o022 != 0 {
		return errors.New("writable by others than its owner")
	}
	return nil
}
