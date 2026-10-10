package rdp

import (
	"os"
	"strings"
	"testing"
	"time"
)

// Everything else in this package is tested against pipes and hand-built
// frames, which proves the parsing and proves nothing about whether a real
// Windows server agrees with any of it. Point this at one and find out.
//
//	ARGUS_TEST_RDP_TARGET=192.168.101.91:3389 go test ./internal/rdp/ -run Target -v
//
// Opt-in, because it needs a host on the network and CI has none.
func rdpTarget(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("ARGUS_TEST_RDP_TARGET")
	if addr == "" {
		t.Skip("set ARGUS_TEST_RDP_TARGET=host:3389 to run against a real server")
	}
	return addr
}

// Asking a real server for network level authentication with nobody to perform
// it must fail, not hand back a connection.
//
// It handed one back. `auth == nil` short-circuited before the check on what
// was actually negotiated, so a caller that asked for CredSSP and passed no
// authenticator got a TLS connection that had negotiated NLA and then skipped
// it -- reported as success. Not reachable from the gateway, which asks for
// ProtocolSSL whenever it has no credential; found by pointing this at a real
// Windows host.
func TestTargetRefusesNLAWithNoAuthenticator(t *testing.T) {
	addr := rdpTarget(t)

	conn, result, err := DialTargetWithAuth(addr, "", ProtocolHybrid, nil, 10*time.Second, nil)
	if conn != nil {
		conn.Close()
	}
	if err == nil {
		t.Fatalf("negotiated NLA with no authenticator and got a usable connection back "+
			"(credssp result %+v); the connection authenticated nobody", result)
	}
	t.Logf("refused as expected: %v", err)
}

// What a correctly hardened Windows host does to a TLS-only dial.
//
// It refuses it: NLA is required, which is the configuration a customer of a
// privileged-access product would have. The property under test is that Argus
// reports that as the specific, actionable thing it is rather than as a generic
// handshake failure -- the remedy is "give this asset a vaulted credential",
// and nothing else will make the host reachable.
func TestTargetRequiringNLARefusesTLSOnlyClearly(t *testing.T) {
	addr := rdpTarget(t)

	conn, _, err := DialTargetWithAuth(addr, "", ProtocolSSL, nil, 10*time.Second, nil)
	if conn != nil {
		conn.Close()
	}
	if err == nil {
		t.Skip("this host allows TLS-only; nothing to assert about an NLA refusal")
	}
	if !strings.Contains(err.Error(), "network level authentication") {
		t.Errorf("a host that requires NLA should say so; got %v", err)
	}
	t.Logf("refused as expected: %v", err)
}

// Argus refuses standard RDP security. On a host that requires NLA the server
// refuses first, so this cannot distinguish the two -- the unit tests in
// proxy_test.go cover Argus's own guard against a server that would accept it.
// What this adds is that a real server never talks us into one.
func TestTargetNeverYieldsLegacyRDPSecurity(t *testing.T) {
	addr := rdpTarget(t)

	conn, _, err := DialTargetWithAuth(addr, "", ProtocolRDP, nil, 10*time.Second, nil)
	if conn != nil {
		conn.Close()
	}
	if err == nil {
		t.Fatal("asking for standard RDP security succeeded; it must always be refused")
	}
	t.Logf("refused as expected: %v", err)
}
