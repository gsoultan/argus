package rdp

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/credssp"
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

// rdpCredentials returns an authenticator for the target, or skips.
//
// Environment only, never a file. These are somebody's real domain credentials
// and a checked-in fixture is how they end up somewhere they should not be.
//
//	ARGUS_TEST_RDP_DOMAIN=EXAMPLE ARGUS_TEST_RDP_USER=someone \
//	ARGUS_TEST_RDP_PASSWORD=... go test ./internal/rdp/ -run Target -v
func rdpCredentials(t *testing.T) *credssp.Authenticator {
	t.Helper()
	user := os.Getenv("ARGUS_TEST_RDP_USER")
	pass := os.Getenv("ARGUS_TEST_RDP_PASSWORD")
	if user == "" || pass == "" {
		t.Skip("set ARGUS_TEST_RDP_USER and ARGUS_TEST_RDP_PASSWORD to run the authenticated path")
	}
	return &credssp.Authenticator{Credentials: credssp.Credentials{
		Domain:      os.Getenv("ARGUS_TEST_RDP_DOMAIN"),
		User:        user,
		Password:    pass,
		Workstation: "ARGUS",
	}}
}

// CredSSP against a real Windows server, end to end.
//
// Everything in internal/credssp is tested against recorded byte sequences,
// which proves the encoding and proves nothing about whether Windows accepts
// it: NTLM's flag negotiation, the MIC, the version-5 nonce-bound public key
// binding and the TSRequest framing all have to be right simultaneously, and a
// fixture only ever asserts that they match what was captured once.
func TestTargetCompletesCredSSP(t *testing.T) {
	addr := rdpTarget(t)
	auth := rdpCredentials(t)

	conn, result, err := DialTargetWithAuth(addr, "", ProtocolHybrid, nil, 15*time.Second, auth)
	if err != nil {
		t.Fatalf("credssp against a real server: %v", err)
	}
	defer conn.Close()

	if result == nil {
		t.Fatal("authenticated with no result to record")
	}
	t.Logf("credssp version %d, legacy binding %v", result.Version, result.LegacyBinding)
	if result.Version < 2 {
		t.Errorf("version %d is below anything a current Windows should agree to", result.Version)
	}
	// Worth knowing rather than failing on: the gateway already flags it onto
	// the session record, because the pre-version-5 binding is not nonce-bound.
	if result.LegacyBinding {
		t.Logf("NOTE: this target used the pre-version-5 binding")
	}
}

// The whole client sequence on an authenticated connection.
//
// GCC, MCS, licensing, capability exchange and the finalization handshake --
// none of which has ever run against a real server. Each is tested here against
// hand-built frames, which is exactly the kind of test that agrees with its own
// assumptions.
func TestTargetCompletesTheClientSequence(t *testing.T) {
	addr := rdpTarget(t)
	auth := rdpCredentials(t)

	conn, _, err := DialTargetWithAuth(addr, "", ProtocolHybrid, nil, 20*time.Second, auth)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	client, err := Connect(conn, ClientConfig{
		Width: 1024, Height: 768, Depth: 16,
		SelectedProtocol: ProtocolHybrid,
		ReadTimeout:      20 * time.Second,
	})
	if err != nil {
		t.Fatalf("client sequence against a real server: %v", err)
	}
	w, h := client.Size()
	t.Logf("session established: %dx%d", w, h)
	if w <= 0 || h <= 0 {
		t.Errorf("negotiated a %dx%d desktop", w, h)
	}
}
