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

// ownLike gives path the owner of dir, so SQL Server, which owns its backup
// folder, can read a file the agent wrote there; the file stays 0600.
func ownLike(path, dir string) error {
	var st unix.Stat_t
	if err := unix.Stat(dir, &st); err != nil {
		return err
	}
	return unix.Chown(path, int(st.Uid), int(st.Gid))
}
