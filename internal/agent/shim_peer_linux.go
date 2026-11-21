package agent

import (
	"net"

	"golang.org/x/sys/unix"
)

// socketPeer reads SO_PEERCRED: the uid and pid of the process that
// connected, fixed by the kernel at connect time.
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
		cred *unix.Ucred
		gerr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, gerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return ShimPeer{}, err
	}
	if gerr != nil {
		return ShimPeer{}, gerr
	}
	return ShimPeer{UID: int(cred.Uid), PID: int(cred.Pid), Known: true}, nil
}
