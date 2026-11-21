package agent

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
)

// ShimPeer is who is at the other end of a shim connection, as the kernel
// says. Known is false where the platform cannot say and the socket is not
// open to other accounts anyway (Windows, where only SYSTEM and
// Administrators reach it).
type ShimPeer struct {
	UID, PID int
	Known    bool
}

// errNotUnixSocket is a connection the kernel cannot be asked about.
var errNotUnixSocket = errors.New("not a unix socket connection")

// shimPeer asks the kernel who connected, or the test's stand-in.
func (c *Collector) shimPeer(conn net.Conn) (ShimPeer, error) {
	if c.PeerOf != nil {
		return c.PeerOf(conn)
	}
	return socketPeer(conn)
}

// checkShimClaim refuses a start whose account or process is not the one
// that connected.
//
// The socket is open to every local account, because every login's shim has
// to reach it, and the start message is written by that account. Taken at
// its word, any local user could open a recording as root or as someone
// else's personal account -- a session in another person's name, which is
// exactly the claim the recording exists to prove -- or point the kernel
// trace at another user's process. The kernel's answer for the socket is the
// one the shim cannot choose.
//
// A peer running as the agent's own account is not checked: it could write
// the recordings directly, so there is no boundary to hold. Nor is a claim
// about an account the local passwd does not list (a directory user), unless
// the connecting uid is a local account's: that is a local user claiming to
// be somebody else.
func (c *Collector) checkShimClaim(conn net.Conn, m SessionStart) error {
	p, err := c.shimPeer(conn)
	if err != nil {
		return fmt.Errorf("cannot tell which account connected: %w", err)
	}
	if !p.Known || p.UID == os.Geteuid() {
		return nil
	}
	// A pid the kernel could not map into this namespace reads as 0.
	if p.PID > 0 && m.PID != p.PID {
		return fmt.Errorf("the login says it is process %d, and process %d connected", m.PID, p.PID)
	}
	if m.Principal == "uid:"+strconv.Itoa(p.UID) {
		return nil
	}
	entries, err := readPasswdEntries(c.passwdFile())
	if err != nil {
		return fmt.Errorf("cannot read the account list to check the login: %w", err)
	}
	mismatch := fmt.Errorf("the login says it is %q, and uid %d connected", m.Principal, p.UID)
	if e, ok := entries[m.Principal]; ok {
		if e.uid == p.UID {
			return nil
		}
		return mismatch
	}
	for _, e := range entries {
		if e.uid == p.UID {
			return mismatch
		}
	}
	return nil
}
