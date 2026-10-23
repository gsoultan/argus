// Package agent implements the Argus host agent.
//
// The agent exists to close the gap the gateway cannot: a session that never
// goes through Argus. If someone holds a standing key and connects straight to
// sshd on port 22, the gateway sees nothing, because nothing crossed it. Only
// something running on the host can observe that session.
//
// The agent has two halves that run as different users, and the split is the
// whole security argument:
//
//	sshd (as the connecting user)
//	  └─> argus-agent shim      — allocates the PTY, tees the session
//	        │ unix socket
//	        ▼
//	      argus-agent daemon    — runs as root, owns the recording
//
// The shim runs as whoever connected, so it must never own the recording file.
// If it did, a user could edit the record of their own session and the entire
// product would be theatre. The shim only ever writes into a socket; the daemon
// holds the file descriptor, computes the hash chain, and decides what happens
// to the bytes.
package agent

import (
	"encoding/json"
	"fmt"
	"time"
)

// SocketPath is where the collector listens (see paths_*.go). Mode 0666
// because any user with shell access must be able to connect — the daemon's
// ownership of the output, not the socket's permissions, is what protects the
// recording.
const SocketPath = socketPath

// Origin records whether a session was brokered or bypassed the gateway.
type Origin string

const (
	// Brokered means the connection arrived from a known gateway address, so
	// the gateway already recorded it and policy was evaluated beforehand.
	Brokered Origin = "brokered"
	// Direct means someone reached sshd without passing through Argus. The
	// session is recorded, but no approval, time window or principal check ever
	// ran. Every direct session is a control failure, even an authorised one.
	Direct Origin = "direct"
)

// SessionStart is the first message a shim sends. Everything the control plane
// needs to attribute the session travels here.
type SessionStart struct {
	Type string `json:"type"` // always "start"

	// Principal is the local account the session opened as.
	Principal string `json:"principal"`
	// ClientAddr is the peer from SSH_CONNECTION.
	ClientAddr string `json:"client_addr"`
	// Origin distinguishes a brokered session from a bypass.
	Origin Origin `json:"origin"`
	// Command is set for non-interactive `ssh host cmd` invocations, which have
	// no PTY and would otherwise be the least visible path onto the host.
	Command string `json:"command,omitempty"`
	// TTY reports whether a PTY was allocated.
	TTY bool `json:"tty"`
	// Cols and Rows seed the asciicast header.
	Cols int `json:"cols"`
	Rows int `json:"rows"`
	// Hostname of the target, so recordings are attributable without a lookup.
	Hostname string `json:"hostname"`
	// StartedAt is the shim's clock. The daemon stamps its own time too; a
	// large disagreement is itself worth alarming on.
	StartedAt time.Time `json:"started_at"`
	// PID of the shim, for correlating with kernel-observed events later.
	PID int `json:"pid"`
	// OriginReason explains how Origin was decided, so an operator reviewing a
	// flagged session can see whether it rested on proof or on a fallback.
	OriginReason string `json:"origin_reason,omitempty"`
	// Actor is the person a session opened through the tunnel is for, from
	// the control plane's signed grant. Never set by a shim: a shim runs as
	// the user being recorded, and could claim to be anyone.
	Actor string `json:"-"`
}

// Frame is one chunk of session data.
type Frame struct {
	Type string `json:"type"` // "o" stdout, "i" stdin, "r" resize
	// Offset is seconds since the session began.
	Offset float64 `json:"t"`
	Data   string  `json:"d"`
}

// SessionEnd closes a recording.
type SessionEnd struct {
	Type     string `json:"type"` // always "end"
	ExitCode int    `json:"exit_code"`
}

// Ack is the daemon's reply once a recording is sealed, so the shim can log the
// chain head for the operator.
type Ack struct {
	SessionID string `json:"session_id"`
	ChainHead string `json:"chain_head"`
	Error     string `json:"error,omitempty"`
}

// Decision is the daemon's answer to a start, sent before the shim runs
// anything, so the shim knows whether to stream the session or step aside.
//
// A daemon older than this message sends nothing until the session ends; the
// shim treats that silence as "record", which is what that daemon does.
type Decision struct {
	Type   string `json:"type"` // always "decision"
	Record bool   `json:"record"`
	// SessionID is set when Record is true.
	SessionID string `json:"session_id,omitempty"`
	// Reason is said to the user when Record is false.
	Reason string `json:"reason,omitempty"`
	// Refused means the login may not happen at all: the deployment is
	// console-only and this one did not come through Argus. Error carries the
	// same reason, so a shim older than Refused -- which reads any error as a
	// recording that failed to open -- still refuses rather than skipping.
	Refused bool   `json:"refused,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Reply is anything the daemon sends a shim: a Decision, or an Ack, which has
// no type and carries Error when the recording could not be opened.
type Reply struct {
	Type      string `json:"type"`
	Record    bool   `json:"record"`
	SessionID string `json:"session_id"`
	Reason    string `json:"reason"`
	Refused   bool   `json:"refused"`
	ChainHead string `json:"chain_head"`
	Error     string `json:"error"`
}

// envelope is used to peek at a message's type before full decoding.
type envelope struct {
	Type string `json:"type"`
}

// DecodeMessage dispatches one newline-delimited JSON message.
func DecodeMessage(line []byte) (any, error) {
	var e envelope
	if err := json.Unmarshal(line, &e); err != nil {
		return nil, fmt.Errorf("malformed message: %w", err)
	}
	switch e.Type {
	case "start":
		var m SessionStart
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, fmt.Errorf("decode start: %w", err)
		}
		return m, nil
	case "end":
		var m SessionEnd
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, fmt.Errorf("decode end: %w", err)
		}
		return m, nil
	case "o", "i", "r":
		var m Frame
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, fmt.Errorf("decode frame: %w", err)
		}
		return m, nil
	default:
		return nil, fmt.Errorf("unknown message type %q", e.Type)
	}
}
