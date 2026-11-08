//go:build !windows

package controlupdate

import (
	"os"
	"syscall"
)

func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

// chownLike gives path the owner of dir, when this process may (root).
func chownLike(path, dir string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	// Lchown: the name was just renamed into place by this process, but the
	// directory is not ours, so nothing there is followed.
	return os.Lchown(path, int(st.Uid), int(st.Gid))
}
