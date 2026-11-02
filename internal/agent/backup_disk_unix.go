//go:build !windows

package agent

import "golang.org/x/sys/unix"

// freeBytes is what is free under dir, or 0 when it cannot be measured.
func freeBytes(dir string) int64 {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0
	}
	return int64(st.Bavail) * int64(st.Bsize)
}
