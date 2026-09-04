package gateway

import "sync/atomic"

// Policy is what a brokered session may do, as the control plane holds it.
//
// The gateway enforced all of this before it was configurable — by hardcoding
// the closed position at each decision point. That was the right default and it
// still is; what was missing was any way for an owner to deliberately open one
// channel, with the decision recorded against their name. So this type does not
// introduce a policy, it names the one that already existed.
type Policy struct {
	AllowLocalForward  bool `json:"allowLocalForward"`
	AllowRemoteForward bool `json:"allowRemoteForward"`
	// AllowAgentForward is offered despite credential injection making it
	// unnecessary for reaching the target itself.
	//
	// The case against it is strong and stated everywhere it appears: for the
	// life of the channel, anyone with root on the gateway or the target can
	// sign challenges with the operator's keys. The case *for* offering it is
	// that a real workflow needs it — git operations on the target, against a
	// forge that authenticates the person rather than the host — and a gateway
	// that cannot do it sends that operator around the gateway to a direct
	// sshd session instead. An unrecorded bypass is worse than a recorded,
	// audited, deliberately-enabled risk, and coverage is the thing this
	// product is actually defending.
	//
	// So it stays: off by default, owner-or-admin to change, logged at warning
	// severity on every channel, and described in full in the console. If that
	// trade ever stops holding, deleting this field and its column is the
	// whole reversal.
	AllowAgentForward bool `json:"allowAgentForward"`
	AllowX11Forward   bool `json:"allowX11Forward"`

	ProxySftpSubsystem           bool `json:"proxySftpSubsystem"`
	FailClosedOnRecordingLoss    bool `json:"failClosedOnRecordingLoss"`
	RequireEbpfForRoot           bool `json:"requireEbpfForRoot"`
	EncryptRecordingsSeparateKey bool `json:"encryptRecordingsSeparateKey"`
}

// DefaultPolicy is the closed configuration.
//
// Note which way the zero value falls: a `Policy{}` denies every forwarding
// channel but *also* switches off SFTP proxying and fail-closed recording,
// which loses evidence. So a zero Policy is never safe to use directly, and
// nothing in this package constructs one — PolicyHolder starts from here.
func DefaultPolicy() Policy {
	return Policy{
		ProxySftpSubsystem:           true,
		FailClosedOnRecordingLoss:    true,
		RequireEbpfForRoot:           true,
		EncryptRecordingsSeparateKey: true,
	}
}

// PolicyHolder carries the current policy across a refresh.
//
// Sessions are long-lived and policy is fetched on a timer, so the value is
// read on a connection goroutine while being replaced on another. An atomic
// pointer swap means a session sees one coherent policy rather than a mixture
// of the old and new — a session that got agent forwarding from the old policy
// and SFTP proxying from the new one would be a configuration nobody chose.
type PolicyHolder struct {
	v atomic.Pointer[Policy]
}

// NewPolicyHolder starts closed. A gateway that has never reached the control
// plane must not be more permissive than one that has.
func NewPolicyHolder() *PolicyHolder {
	h := &PolicyHolder{}
	p := DefaultPolicy()
	h.v.Store(&p)
	return h
}

// Get returns the policy in force. Never nil.
func (h *PolicyHolder) Get() Policy {
	if h == nil {
		return DefaultPolicy()
	}
	if p := h.v.Load(); p != nil {
		return *p
	}
	return DefaultPolicy()
}

// Set replaces the policy for sessions started from here on.
//
// Sessions already open keep the policy they started under. Revoking a channel
// mid-session would drop a connection the user was told they could open, and
// tightening policy is not an incident response tool — terminating the session
// is, and that is a separate deliberate act with its own audit entry.
func (h *PolicyHolder) Set(p Policy) {
	if h == nil {
		return
	}
	h.v.Store(&p)
}

// channelAllowed reports whether a session may open this channel type.
//
// "session" is always permitted — it is the shell itself, and refusing it would
// mean refusing the connection, which is a different decision made elsewhere.
func (p Policy) channelAllowed(kind string) bool {
	switch kind {
	case "session":
		return true
	case "direct-tcpip", "direct-streamlocal@openssh.com":
		return p.AllowLocalForward
	case "x11":
		return p.AllowX11Forward
	case "auth-agent@openssh.com":
		return p.AllowAgentForward
	default:
		// Unknown channel types stay refused. An allowlist is the only form of
		// this check that stays correct as OpenSSH adds extensions.
		return false
	}
}

// globalRequestAllowed covers requests made against the connection rather than
// a channel — remote forwarding is the one that matters here.
func (p Policy) globalRequestAllowed(kind string) bool {
	switch kind {
	case "tcpip-forward", "cancel-tcpip-forward",
		"streamlocal-forward@openssh.com", "cancel-streamlocal-forward@openssh.com":
		return p.AllowRemoteForward
	default:
		return false
	}
}

// channelRequestAllowed covers requests inside an open session channel that
// policy governs. Anything not named here is decided by the session handler.
func (p Policy) channelRequestAllowed(kind string) (governed, allowed bool) {
	switch kind {
	case "auth-agent-req@openssh.com":
		return true, p.AllowAgentForward
	case "x11-req":
		return true, p.AllowX11Forward
	default:
		return false, false
	}
}
