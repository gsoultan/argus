//go:build linux

package agent

import (
	"os"

	"golang.org/x/sys/unix"
)

// growPipe makes a pipe hold screenPipeBuffer: the default 64 KB made one
// batch of tiles several trips between the two processes.
func growPipe(f *os.File) {
	_, _ = unix.FcntlInt(f.Fd(), unix.F_SETPIPE_SZ, screenPipeBuffer)
}
