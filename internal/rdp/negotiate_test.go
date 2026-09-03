package rdp

import (
	"errors"
	"testing"
)

func TestParseConnectionRequestReadsTheCookieAndProtocols(t *testing.T) {
	frame := connectionRequest("ops:pay-01", ProtocolSSL|ProtocolHybrid, true)

	info, err := ParseConnectionRequest(frame)
	if err != nil {
		t.Fatalf("ParseConnectionRequest: %v", err)
	}
	// The cookie is how a stock client tells Argus what it wants to reach,
	// before any session exists — the RDP counterpart of the SSH username.
	if info.Cookie != "ops:pay-01" {
		t.Errorf("Cookie = %q", info.Cookie)
	}
	if !info.HasNegotiation {
		t.Fatal("negotiation structure was not found")
	}
	if !info.Supports(ProtocolHybrid) || !info.Supports(ProtocolSSL) {
		t.Errorf("protocols %#x did not report the offered options", info.RequestedProtocols)
	}
	if info.Supports(ProtocolRDSTLS) {
		t.Error("reported support for a protocol that was not offered")
	}
}

// Standard RDP security is the absence of every other bit rather than a bit of
// its own, so a naive mask test reports it for every client that ever connects.
func TestSupportsStandardRDPOnlyWhenNothingElseIsOffered(t *testing.T) {
	weak, err := ParseConnectionRequest(connectionRequest("ops", ProtocolRDP, true))
	if err != nil {
		t.Fatal(err)
	}
	if !weak.Supports(ProtocolRDP) {
		t.Error("a client offering nothing else was not recognised as standard RDP")
	}

	strong, err := ParseConnectionRequest(connectionRequest("ops", ProtocolHybrid, true))
	if err != nil {
		t.Fatal(err)
	}
	if strong.Supports(ProtocolRDP) {
		t.Error("a client offering CredSSP was also reported as standard RDP")
	}
}

// A client that sends no negotiation structure is asking for standard RDP
// security implicitly. Defaulting quietly would make the weakest option the one
// you get by saying nothing.
func TestMissingNegotiationIsAnError(t *testing.T) {
	_, err := ParseConnectionRequest(connectionRequest("ops", 0, false))
	if !errors.Is(err, ErrNoNegotiation) {
		t.Errorf("err = %v, want ErrNoNegotiation", err)
	}
}

func TestRoutingTokenIsRecognised(t *testing.T) {
	frame := connectionRequestRaw("Cookie: msts=3640205228.15629.0000\r\n", ProtocolSSL)

	info, err := ParseConnectionRequest(frame)
	if err != nil {
		t.Fatalf("ParseConnectionRequest: %v", err)
	}
	if info.RoutingToken != "3640205228.15629.0000" {
		t.Errorf("RoutingToken = %q", info.RoutingToken)
	}
	if info.Cookie != "" {
		t.Errorf("Cookie = %q, want empty when a routing token is present", info.Cookie)
	}
}

// The single most important decision in the connection sequence: which security
// protocol the session runs under. Standard RDP security has no meaningful
// server authentication, so brokering it would mean Argus cannot prove a
// session reached the host it claims — the same failure host-key pinning exists
// to prevent on the SSH side.
func TestSelectProtocolPrefersTheStrongestAndRefusesTheWeakest(t *testing.T) {
	cases := []struct {
		name      string
		requested uint32
		want      uint32
		ok        bool
	}{
		{"CredSSP over TLS", ProtocolSSL | ProtocolHybrid, ProtocolHybrid, true},
		{"CredSSP-EX over CredSSP", ProtocolHybrid | ProtocolHybridEx, ProtocolHybridEx, true},
		{"TLS when it is all that is offered", ProtocolSSL, ProtocolSSL, true},
		{"nothing acceptable", ProtocolRDP, 0, false},
		{"RDSTLS alone is not acceptable", ProtocolRDSTLS, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := SelectProtocol(tc.requested)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Errorf("protocol = %s, want %s", ProtocolName(got), ProtocolName(tc.want))
			}
		})
	}

	if MinimumProtocol == ProtocolRDP {
		t.Error("MinimumProtocol admits standard RDP security")
	}
}

// The response and failure PDUs are built by hand, and a client that cannot
// parse them sees a dropped connection with no explanation.
func TestBuiltResponsesRoundTrip(t *testing.T) {
	confirm := BuildConnectionConfirm(ProtocolHybrid)
	if got := Classify(confirm); got != ConnectionConfirm {
		t.Fatalf("built confirm classifies as %q", got)
	}
	protocol, failure, err := ParseConnectionConfirm(confirm)
	if err != nil {
		t.Fatalf("ParseConnectionConfirm: %v", err)
	}
	if protocol != ProtocolHybrid || failure != 0 {
		t.Errorf("protocol = %s, failure = %#x", ProtocolName(protocol), failure)
	}

	refusal := BuildNegotiationFailure(FailHybridRequiredByServer)
	protocol, failure, err = ParseConnectionConfirm(refusal)
	if err != nil {
		t.Fatalf("ParseConnectionConfirm on a failure: %v", err)
	}
	if failure != FailHybridRequiredByServer {
		t.Errorf("failure = %#x, want %#x", failure, FailHybridRequiredByServer)
	}
	if protocol != 0 {
		t.Errorf("a failure PDU also reported protocol %s", ProtocolName(protocol))
	}
}

func TestBuiltFramesAreWellFormedTPKT(t *testing.T) {
	for name, frame := range map[string][]byte{
		"confirm": BuildConnectionConfirm(ProtocolSSL),
		"failure": BuildNegotiationFailure(FailSSLRequiredByServer),
	} {
		t.Run(name, func(t *testing.T) {
			if frame[0] != tpktVersion {
				t.Errorf("version byte = 0x%02x", frame[0])
			}
			declared := int(frame[2])<<8 | int(frame[3])
			if declared != len(frame) {
				t.Errorf("declared length %d, actual %d", declared, len(frame))
			}
		})
	}
}

func TestNamesAreUsefulInAudit(t *testing.T) {
	if got := ProtocolName(ProtocolHybrid); got != "credssp" {
		t.Errorf("ProtocolName = %q", got)
	}
	if got := FailureName(FailHybridRequiredByServer); got == "" {
		t.Error("FailureName returned nothing for a known code")
	}
	// An unknown code must still render as something an operator can search
	// for, rather than an empty string in an audit record.
	if got := FailureName(0xDEAD); got == "" {
		t.Error("FailureName returned nothing for an unknown code")
	}
}
