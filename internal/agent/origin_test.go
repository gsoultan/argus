package agent

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func keyPair(t *testing.T) (ssh.PublicKey, string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k, ssh.FingerprintSHA256(k)
}

// writeAuthInfo produces the file sshd creates when ExposeAuthInfo is on.
func writeAuthInfo(t *testing.T, key ssh.PublicKey) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "authinfo")
	line := "publickey " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A session authenticated with a gateway key is brokered, and the gateway has
// already recorded it.
func TestBrokeredWhenTheGatewayKeyAuthenticated(t *testing.T) {
	key, fp := keyPair(t)
	t.Setenv("SSH_USER_AUTH", writeAuthInfo(t, key))

	origin, reason := OriginDetector{
		GatewayKeyFingerprints: map[string]bool{fp: true},
	}.Detect()

	if origin != Brokered {
		t.Errorf("origin = %s, want brokered (%s)", origin, reason)
	}
}

// Any other key means someone reached sshd without passing through Argus.
func TestDirectWhenAnotherKeyAuthenticated(t *testing.T) {
	key, _ := keyPair(t)
	_, gatewayFP := keyPair(t)
	t.Setenv("SSH_USER_AUTH", writeAuthInfo(t, key))

	origin, reason := OriginDetector{
		GatewayKeyFingerprints: map[string]bool{gatewayFP: true},
	}.Detect()

	if origin != Direct {
		t.Errorf("origin = %s, want direct", origin)
	}
	if !strings.Contains(reason, "does not use") {
		t.Errorf("reason should say the key is not the gateway's: %q", reason)
	}
}

// The critical property. Behind NAT or a port forward every client shares one
// source address, so treating an address match as proof would let anyone on
// that path suppress their own recording.
func TestSourceAddressIsNeverProofOfBrokering(t *testing.T) {
	os.Unsetenv("SSH_USER_AUTH") // sshd without ExposeAuthInfo
	t.Setenv("SSH_CONNECTION", "10.0.0.5 54321 10.0.0.9 22")

	origin, reason := OriginDetector{
		GatewayAddrs: []string{"10.0.0.5"}, // the client IS at the gateway's address
	}.Detect()

	if origin != Direct {
		t.Fatalf("origin = %s — an address match was treated as proof of brokering", origin)
	}
	if !strings.Contains(reason, "not proof") {
		t.Errorf("reason should explain why the address was not trusted: %q", reason)
	}
}

// No signal at all must mean "record it", never "assume it was brokered".
func TestUnknownOriginDefaultsToDirect(t *testing.T) {
	os.Unsetenv("SSH_USER_AUTH")
	os.Unsetenv("SSH_CONNECTION")

	origin, reason := OriginDetector{}.Detect()
	if origin != Direct {
		t.Errorf("origin = %s, want direct when nothing is known", origin)
	}
	if reason == "" {
		t.Error("no reason given for the classification")
	}
}

func TestUnreadableAuthInfoFallsBackToDirect(t *testing.T) {
	t.Setenv("SSH_USER_AUTH", "/nonexistent/authinfo")
	_, fp := keyPair(t)

	origin, _ := OriginDetector{
		GatewayKeyFingerprints: map[string]bool{fp: true},
	}.Detect()

	// Unable to read the evidence means unable to prove brokering.
	if origin != Direct {
		t.Errorf("origin = %s, want direct when auth info cannot be read", origin)
	}
}

func TestNonPublickeyAuthIsNotBrokered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "authinfo")
	// Password auth: no key, so nothing can prove this came from the gateway.
	if err := os.WriteFile(path, []byte("password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSH_USER_AUTH", path)
	_, fp := keyPair(t)

	origin, _ := OriginDetector{GatewayKeyFingerprints: map[string]bool{fp: true}}.Detect()
	if origin != Direct {
		t.Errorf("origin = %s, want direct for password auth", origin)
	}
}

func TestLoadGatewayFingerprints(t *testing.T) {
	k1, fp1 := keyPair(t)
	k2, fp2 := keyPair(t)

	path := filepath.Join(t.TempDir(), "gateway_keys")
	content := string(ssh.MarshalAuthorizedKey(k1)) + string(ssh.MarshalAuthorizedKey(k2))
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadGatewayFingerprints(path)
	if err != nil {
		t.Fatalf("LoadGatewayFingerprints: %v", err)
	}
	if !got[fp1] || !got[fp2] {
		t.Errorf("loaded %d fingerprints, want both keys", len(got))
	}

	// An empty path means no gateway keys, not an error — that is the standalone
	// configuration, where everything is correctly treated as direct.
	empty, err := LoadGatewayFingerprints("")
	if err != nil || len(empty) != 0 {
		t.Errorf("empty path: got %v, %v", empty, err)
	}
}

func TestPostureRating(t *testing.T) {
	cases := []struct {
		name string
		p    Posture
		want string
	}{
		{
			// Nothing is watching: a direct session here leaves no trace at all.
			name: "no recorder installed",
			p:    Posture{ShimInstalled: false},
			want: "open",
		},
		{
			// No standing credential exists, so there is nothing to bypass with.
			name: "locked down",
			p: Posture{ShimInstalled: true, TrustedCAConfigured: true,
				PasswordAuthEnabled: false},
			want: "enforced",
		},
		{
			// A bypass is possible, but it would be recorded.
			name: "standing keys remain",
			p: Posture{ShimInstalled: true, TrustedCAConfigured: true,
				UnmanagedKeys: []UnmanagedKey{{User: "ops"}}},
			want: "monitored",
		},
		{
			// Password auth is a bypass that needs no key at all.
			name: "passwords enabled",
			p: Posture{ShimInstalled: true, TrustedCAConfigured: true,
				PasswordAuthEnabled: true},
			want: "monitored",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.p.Rating(); got != c.want {
				t.Errorf("Rating() = %q, want %q", got, c.want)
			}
		})
	}
}
