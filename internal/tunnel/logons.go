package tunnel

import (
	"errors"
	"time"
)

/*
The logons service: who is signed in on a host right now, and, on Windows,
signing out a session nobody is using.

	argus-control ──"logons" stream──▶ agent

One stream per question, like software. After the header and the answer the
agent sends one 'z' LogonsResult (JSON) and closes. The console shows the list
before a Remote Desktop session opens, so the person connecting sees who else
is there; when every Remote Desktop session a Windows Server allows is taken,
an administrator may sign out a disconnected one rather than leave Windows to
ask whoever is using it. The agent itself refuses to sign out a session that
is in use, whatever the grant says. An old agent refuses the service as
unknown, which the console turns into "update the agent".
*/

// LogonsService is the tunnel service name.
const LogonsService = "logons"

// FrameLogonsResult carries the one LogonsResult.
const FrameLogonsResult byte = 'z'

// Logons questions.
const (
	LogonsList    = "list"
	LogonsSignOut = "signout"
)

// MaxLogons bounds one answer.
const MaxLogons = 200

// GrantLogons is what a logons grant may ask; nil on every other grant.
type GrantLogons struct {
	Op string `json:"op"`
	// Session is the Windows session to sign out, for LogonsSignOut.
	Session uint32 `json:"session,omitempty"`
	// Account is who the console showed in that session. The agent checks
	// it still is before signing anything out, so a session that changed
	// hands between the list and the press is left alone.
	Account string `json:"account,omitempty"`
}

// Validate refuses a question no console asks.
func (q GrantLogons) Validate() error {
	switch q.Op {
	case LogonsList:
		if q.Session != 0 || q.Account != "" {
			return errors.New("a list names no session")
		}
	case LogonsSignOut:
		if q.Session == 0 {
			// Session 0 is Windows' services session, never a person's.
			return errors.New("name the session to sign out")
		}
		if q.Account == "" || len(q.Account) > 256 {
			return errors.New("name the account the session belongs to")
		}
	default:
		return errors.New("unknown logons question")
	}
	return nil
}

// Logon states, as the console shows them.
const (
	LogonActive       = "active"
	LogonDisconnected = "disconnected"
)

// Logon is one signed-in session.
type Logon struct {
	// Session is Windows' session id; on Linux the login's terminal is in
	// Line and Session is 0.
	Session uint32 `json:"session,omitempty"`
	Account string `json:"account"`
	// State is LogonActive or LogonDisconnected.
	State string `json:"state"`
	// Console is the physical screen's session.
	Console bool `json:"console,omitempty"`
	// Remote is a Remote Desktop session (Windows) or a login from another
	// address (Linux).
	Remote bool   `json:"remote,omitempty"`
	Line   string `json:"line,omitempty"`
	// From is the client's name or address, when the host knows it.
	From    string     `json:"from,omitempty"`
	LogonAt *time.Time `json:"logonAt,omitempty"`
	// IdleSeconds is how long since its last input, when the host says.
	IdleSeconds int64 `json:"idleSeconds,omitempty"`
}

// LogonsResult is the agent's answer.
type LogonsResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Platform is "windows" or "linux".
	Platform string  `json:"platform,omitempty"`
	Logons   []Logon `json:"logons,omitempty"`
	// RemoteLimit is how many Remote Desktop sessions this Windows host
	// takes at once, when the agent can tell: 1 on a desktop edition, 2 on
	// a server without Remote Desktop Services. 0 is unknown or no limit.
	RemoteLimit int `json:"remoteLimit,omitempty"`
	// Edition is "workstation" or "server" on Windows.
	Edition string `json:"edition,omitempty"`
	// SignedOut is the session a sign-out ended.
	SignedOut uint32 `json:"signedOut,omitempty"`
}
