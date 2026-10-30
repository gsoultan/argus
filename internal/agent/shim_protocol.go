package agent

import (
	"encoding/json"
	"errors"
	"net"
	"time"
)

// ShimConfig controls how a session is wrapped.
type ShimConfig struct {
	// SocketPath is the collector to stream to.
	SocketPath string
	// FailOpen decides what happens when the collector is unreachable.
	//
	// False (the default) refuses the session: if it cannot be recorded, it
	// does not happen. That is what "all privileged sessions are recorded"
	// requires, and it is the correct posture for a PAM product.
	//
	// True lets the session proceed unrecorded. Only sensible during rollout,
	// when locking every admin out of the fleet because a daemon crashed is a
	// worse outcome than a gap in the audit trail.
	FailOpen bool
	// Shell overrides the login shell; empty means look it up.
	Shell string
}

// decisionWait bounds how long a login waits for the daemon's answer.
//
// A current daemon answers at once. One older than the decision message --
// for the moments of an upgrade between the new shim landing and the daemon
// restarting -- says nothing, and the wait is what that costs per login.
const decisionWait = 2 * time.Second

type decision struct {
	// refused: the daemon could not open the recording, or went away.
	refused bool
	// skip: the daemon chose not to record this login, for reason.
	skip   bool
	reason string
	// consoleOnly: the login is not allowed at all, for reason.
	consoleOnly bool
	// pending: the login is held for an admin to approve, and another
	// decision follows; reason says so to the person.
	pending bool
	// silent: nothing arrived before the deadline.
	silent bool
}

// approvalWait bounds how long a held login waits for its decision: the
// daemon's own limit, plus room for its answer to arrive.
const approvalWait = DirectApprovalWait + 15*time.Second

// awaitDecision reads the daemon's answer to a start.
//
// Silence until the deadline is a daemon that predates the answer and is
// recording, so it streams. A closed connection or an error is a recording
// that did not open, and goes to the same fail-closed path as an unreachable
// collector.
func awaitDecision(conn net.Conn, dec *json.Decoder, wait time.Duration) decision {
	_ = conn.SetReadDeadline(time.Now().Add(wait))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()

	var r Reply
	if err := dec.Decode(&r); err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return decision{silent: true}
		}
		return decision{refused: true}
	}
	switch {
	case r.Pending:
		return decision{pending: true, reason: r.Reason}
	case r.Refused:
		reason := r.Reason
		if reason == "" {
			reason = ConsoleOnlyReason
		}
		return decision{consoleOnly: true, reason: reason}
	case r.Error != "":
		return decision{refused: true}
	case r.Type == "decision" && !r.Record:
		reason := r.Reason
		if reason == "" {
			reason = "this login is not recorded"
		}
		return decision{skip: true, reason: reason}
	}
	return decision{}
}
