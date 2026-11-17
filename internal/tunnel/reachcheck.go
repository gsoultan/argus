package tunnel

import (
	"errors"
	"net/netip"
)

/*
The reach check: can one server connect to a database port on another?

	argus-control ──"reachcheck" stream──▶ agent on the client's server ──▶ TCP to host:port

A check from the database's own server cannot see its firewall, since the
connection comes from that machine. When the machine that should connect
is itself an Argus server, its agent makes the connection for real, which
is the only check that would have caught a "connection refused" a client
sees. It sends nothing but a Redis PING, never a password, and reads one
reply.
*/

// ReachCheckService is the tunnel service name.
const ReachCheckService = "reachcheck"

// FrameReachResult carries the one ReachResult.
const FrameReachResult byte = 'z'

// GrantReach is the one connection a reach check grant may make.
type GrantReach struct {
	// Host is an IP address: a name would make the check depend on this
	// server's DNS, which is not the question.
	Host string `json:"host"`
	Port int    `json:"port"`
	// Engine says what to say once connected: redis sends PING; any other
	// only connects.
	Engine string `json:"engine,omitempty"`
}

// Validate refuses a check that is not one connection to one address.
func (g *GrantReach) Validate() error {
	a, err := netip.ParseAddr(g.Host)
	if err != nil || a.Zone() != "" {
		return errors.New("the address to check must be an IP address")
	}
	if a.IsUnspecified() || a.IsMulticast() {
		return errors.New("that address cannot be connected to")
	}
	if g.Port < 1 || g.Port > 65535 {
		return errors.New("the port must be between 1 and 65535")
	}
	return nil
}

// ReachResult is how the connection went.
type ReachResult struct {
	// Connected says the TCP connection was accepted.
	Connected bool `json:"connected"`
	// Answer is what the database said: "signin" (asks for a password),
	// "open" (answers with none), "protected" (Redis's protected mode
	// refused), or "" when it was not asked.
	Answer string `json:"answer,omitempty"`
	// Detail is the error a client would see, in its words.
	Detail string `json:"detail,omitempty"`
	MS     int64  `json:"ms"`
	From   string `json:"from,omitempty"`
}
