package agent

import (
	"net"

	"golang.org/x/sys/unix"
)

// socketPeer reads LOCAL_PEERCRED and LOCAL_PEERPID: the uid and pid of the
// process that connected, fixed by the kernel at connect time.
func socketPeer(conn net.Conn) (ShimPeer, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return ShimPeer{}, errNotUnixSocket
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return ShimPeer{}, err
	}
	var (
		cred *unix.Xucred
		pid  int
		gerr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, gerr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if gerr == nil {
			pid, gerr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
		}
	}); err != nil {
		return ShimPeer{}, err
	}
	if gerr != nil {
		return ShimPeer{}, gerr
	}
	return ShimPeer{UID: int(cred.Uid), PID: pid, Known: true}, nil
}
