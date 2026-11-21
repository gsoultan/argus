//go:build !linux && !darwin

package agent

import (
	"errors"
	"net"
	"runtime"
)

// socketPeer has no kernel to ask on these platforms. On Windows the socket
// sits in a directory only SYSTEM and Administrators can enter, so whoever
// connected is already trusted. Anywhere else the agent cannot tell, and
// refuses rather than take the shim's word.
func socketPeer(net.Conn) (ShimPeer, error) {
	if runtime.GOOS == "windows" {
		return ShimPeer{}, nil
	}
	return ShimPeer{}, errors.New("this platform cannot say which account connected")
}
