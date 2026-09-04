package gateway

import (
	"sync"
	"testing"
)

// The whole point of the default is that it is closed. If this ever inverts,
// every gateway that cannot reach its control plane becomes an open tunnel.
func TestDefaultPolicyIsClosed(t *testing.T) {
	p := DefaultPolicy()
	for name, allowed := range map[string]bool{
		"direct-tcpip":                   p.channelAllowed("direct-tcpip"),
		"direct-streamlocal@openssh.com": p.channelAllowed("direct-streamlocal@openssh.com"),
		"x11":                            p.channelAllowed("x11"),
		"auth-agent@openssh.com":         p.channelAllowed("auth-agent@openssh.com"),
		"tcpip-forward":                  p.globalRequestAllowed("tcpip-forward"),
	} {
		if allowed {
			t.Errorf("%s is permitted by the default policy; it must be closed", name)
		}
	}
	if !p.channelAllowed("session") {
		t.Error("session channels must always be permitted — that is the shell itself")
	}
}

// A zero Policy denies forwarding but also switches off the recording
// guarantees. Nothing may construct one by accident.
func TestZeroPolicyLosesRecordingGuarantees(t *testing.T) {
	var zero Policy
	if zero.ProxySftpSubsystem || zero.FailClosedOnRecordingLoss {
		t.Fatal("test states the premise wrongly")
	}
	d := DefaultPolicy()
	if !d.ProxySftpSubsystem || !d.FailClosedOnRecordingLoss ||
		!d.RequireEbpfForRoot || !d.EncryptRecordingsSeparateKey {
		t.Error("DefaultPolicy must keep every recording guarantee on")
	}
}

func TestChannelAllowedFollowsPolicy(t *testing.T) {
	open := Policy{AllowLocalForward: true, AllowX11Forward: true, AllowAgentForward: true}
	for _, kind := range []string{"direct-tcpip", "x11", "auth-agent@openssh.com"} {
		if !open.channelAllowed(kind) {
			t.Errorf("%s should be permitted once policy opens it", kind)
		}
	}
	// An allowlist, so a channel type nobody anticipated stays shut.
	if open.channelAllowed("some-future-channel@openssh.com") {
		t.Error("unknown channel types must stay refused")
	}
}

func TestChannelRequestGoverning(t *testing.T) {
	p := Policy{AllowAgentForward: true}
	if governed, allowed := p.channelRequestAllowed("auth-agent-req@openssh.com"); !governed || !allowed {
		t.Error("agent forwarding request should be governed and allowed")
	}
	if governed, allowed := p.channelRequestAllowed("x11-req"); !governed || allowed {
		t.Error("x11 request should be governed and refused")
	}
	// pty-req, shell, exec and friends are the session handler's business.
	if governed, _ := p.channelRequestAllowed("pty-req"); governed {
		t.Error("pty-req must not be treated as policy-governed")
	}
}

// A nil holder is what a Server built without policy has. It must read closed
// rather than panic or return a zero Policy.
func TestNilHolderReadsClosed(t *testing.T) {
	var h *PolicyHolder
	if h.Get() != DefaultPolicy() {
		t.Error("a nil holder must read as the default closed policy")
	}
	h.Set(Policy{AllowLocalForward: true}) // must not panic
}

func TestHolderSwapsAtomically(t *testing.T) {
	h := NewPolicyHolder()
	if h.Get().AllowLocalForward {
		t.Fatal("holder must start closed")
	}

	// Readers must only ever see a whole policy, never a mixture of two.
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				p := h.Get()
				// Both fields are set together, so seeing one without the
				// other would mean a torn read.
				if p.AllowLocalForward != p.AllowRemoteForward {
					t.Error("observed a torn policy")
					return
				}
			}
		}()
	}
	for i := range 200 {
		on := i%2 == 0
		h.Set(Policy{AllowLocalForward: on, AllowRemoteForward: on})
	}
	close(stop)
	wg.Wait()
}
