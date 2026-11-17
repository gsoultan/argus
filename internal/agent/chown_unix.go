//go:build !windows

package agent

import (
	"os"
	"syscall"
)

// chownLike gives path the owner and group of info's file, so a file put in
// place of another stays readable by the account that read the old one.
func chownLike(path string, info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if err := os.Chown(path, int(st.Uid), int(st.Gid)); err != nil && !os.IsPermission(err) {
		return err
	}
	return nil
}
