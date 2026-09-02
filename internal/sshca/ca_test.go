package sshca

import (
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func newCA(t *testing.T) *CA {
	t.Helper()
	ca, err := Generate(filepath.Join(t.TempDir(), "ca"))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return ca
}

func TestMintedCertificateIsUsable(t *testing.T) {
	ca := newCA(t)
	signer, cert, err := ca.Mint(Identity{
		SessionID: "abc123", User: "dewi.p@northwind.id",
		Principal: "ops", Target: "pay-01",
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if signer == nil {
		t.Fatal("no signer returned")
	}
	if cert.CertType != ssh.UserCert {
		t.Errorf("CertType = %d, want UserCert", cert.CertType)
	}
	if got := cert.ValidPrincipals; len(got) != 1 || got[0] != "ops" {
		t.Errorf("ValidPrincipals = %v, want [ops]", got)
	}
}

// The target's own sshd logs the key id, which is how a host records who
// connected without having to trust or query Argus.
func TestKeyIdCarriesAttribution(t *testing.T) {
	ca := newCA(t)
	_, cert, err := ca.Mint(Identity{
		SessionID: "sess-99", User: "kevin.tan@northwind.id", Principal: "root",
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	for _, want := range []string{"argus", "kevin.tan@northwind.id", "sess-99"} {
		if !strings.Contains(cert.KeyId, want) {
			t.Errorf("KeyId %q does not contain %q", cert.KeyId, want)
		}
	}
}

// A certificate that grants port forwarding turns the bastion into a tunnel
// into the private network — the exact thing it exists to prevent.
func TestDangerousExtensionsAreNotGranted(t *testing.T) {
	ca := newCA(t)
	_, cert, err := ca.Mint(Identity{SessionID: "s", User: "u", Principal: "ops"})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	for _, forbidden := range []string{
		"permit-port-forwarding",
		"permit-agent-forwarding",
		"permit-X11-forwarding",
		"permit-user-rc",
	} {
		if _, present := cert.Extensions[forbidden]; present {
			t.Errorf("certificate grants %s; it must not", forbidden)
		}
	}
	if _, ok := cert.Extensions["permit-pty"]; !ok {
		t.Error("certificate does not grant permit-pty, so no interactive shell is possible")
	}
}

func TestCertificateIsShortLived(t *testing.T) {
	ca := newCA(t)
	ca.Validity = 2 * time.Minute

	_, cert, err := ca.Mint(Identity{SessionID: "s", User: "u", Principal: "ops"})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	window := time.Unix(int64(cert.ValidBefore), 0).Sub(time.Unix(int64(cert.ValidAfter), 0))
	// Validity plus the deliberate one-minute backdate for clock skew.
	if window > 5*time.Minute {
		t.Errorf("validity window is %s; a long-lived certificate defeats the point", window)
	}
	if time.Now().After(time.Unix(int64(cert.ValidBefore), 0)) {
		t.Error("certificate is already expired on issue")
	}
	// The backdate must exist, or a host running a minute slow rejects it.
	if !time.Now().After(time.Unix(int64(cert.ValidAfter), 0)) {
		t.Error("certificate is not yet valid; clock skew will break real hosts")
	}
}

// Every session gets its own keypair, so a leaked one is worth a single session
// rather than standing access.
func TestEachSessionGetsAFreshKey(t *testing.T) {
	ca := newCA(t)
	seen := map[string]bool{}
	serials := map[uint64]bool{}

	for i := 0; i < 5; i++ {
		_, cert, err := ca.Mint(Identity{SessionID: "s", User: "u", Principal: "ops"})
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		fp := ssh.FingerprintSHA256(cert.Key)
		if seen[fp] {
			t.Fatal("session key was reused between mints")
		}
		seen[fp] = true

		if serials[cert.Serial] {
			t.Fatal("certificate serial was reused")
		}
		serials[cert.Serial] = true
	}
}

// The whole scheme rests on a real SSH server accepting the certificate, so
// this performs an actual handshake rather than inspecting struct fields.
//
// Worth doing the hard way: ssh.CertChecker.CheckCert does NOT verify the
// signature or consult IsUserAuthority — it only checks principals, validity
// and critical options. Trusting it as a validity check would accept a
// certificate signed by any CA at all.
func handshake(t *testing.T, trustedCA *CA, certSigner ssh.Signer, principal string) error {
	t.Helper()

	trustedPub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(trustedCA.PublicKey()))
	if err != nil {
		t.Fatalf("parse trusted CA: %v", err)
	}
	checker := &ssh.CertChecker{
		IsUserAuthority: func(k ssh.PublicKey) bool {
			return string(k.Marshal()) == string(trustedPub.Marshal())
		},
	}

	serverCfg := &ssh.ServerConfig{PublicKeyCallback: checker.Authenticate}
	serverCfg.AddHostKey(newCA(t).signer) // the host key is not what is under test

	// A real listener rather than net.Pipe: the pipe is unbuffered and
	// synchronous, so a rejected handshake leaves both sides blocked on each
	// other and the test hangs instead of failing.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	serverErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		_, _, _, err = ssh.NewServerConn(conn, serverCfg)
		serverErr <- err
	}()

	clientCfg := &ssh.ClientConfig{
		User:            principal,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(certSigner)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}
	client, err := ssh.Dial("tcp", ln.Addr().String(), clientCfg)
	if client != nil {
		_ = client.Close()
	}

	// The server's verdict is the authoritative one; the client only sees
	// "unable to authenticate".
	if serr := <-serverErr; serr != nil {
		return serr
	}
	return err
}

func TestRealServerAcceptsTheCertificate(t *testing.T) {
	ca := newCA(t)
	signer, _, err := ca.Mint(Identity{SessionID: "s", User: "u", Principal: "ops"})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if err := handshake(t, ca, signer, "ops"); err != nil {
		t.Errorf("a host trusting this CA rejected the certificate: %v", err)
	}
}

func TestRealServerRejectsWrongPrincipal(t *testing.T) {
	ca := newCA(t)
	signer, _, err := ca.Mint(Identity{SessionID: "s", User: "u", Principal: "ops"})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// Minted for ops, presented as root: the principal restriction is the only
	// thing standing between an operator and every account on the box.
	if err := handshake(t, ca, signer, "root"); err == nil {
		t.Error("server accepted a certificate for a principal it was not issued for")
	}
}

func TestRealServerRejectsForeignCA(t *testing.T) {
	mine := newCA(t)
	theirs := newCA(t)

	signer, _, err := theirs.Mint(Identity{SessionID: "s", User: "u", Principal: "ops"})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if err := handshake(t, mine, signer, "ops"); err == nil {
		t.Error("server accepted a certificate signed by an untrusted CA")
	}
}

func TestSourceAddressPinning(t *testing.T) {
	ca := newCA(t)
	ca.SourceAddress = "10.0.0.5/32"

	_, cert, err := ca.Mint(Identity{SessionID: "s", User: "u", Principal: "ops"})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if got := cert.CriticalOptions["source-address"]; got != "10.0.0.5/32" {
		t.Errorf("source-address = %q, want 10.0.0.5/32", got)
	}
}

func TestGenerateWritesUsablePublicKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ca")
	ca, err := Generate(path)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// The .pub file is what an operator pastes into TrustedUserCAKeys, so it
	// has to parse as an authorized-keys line.
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(ca.PublicKey())); err != nil {
		t.Fatalf("public key is not a valid authorized_keys line: %v", err)
	}

	// And the private key must reload, or a gateway restart loses the CA.
	reloaded, err := Load(path, time.Minute)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reloaded.Fingerprint() != ca.Fingerprint() {
		t.Error("reloaded CA has a different fingerprint")
	}
}
