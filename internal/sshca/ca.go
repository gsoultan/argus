// Package sshca mints short-lived SSH user certificates.
//
// This is what makes "zero standing privilege" true rather than aspirational.
// With certificate auth there is no key in an authorized_keys file to steal, no
// secret in a vault to encrypt, and nothing on the host that outlives the
// session. The target trusts one CA public key; everything else is minted for a
// single session and expires in minutes.
//
// The private key that never exists for long is the strongest kind. Argus
// generates a fresh keypair per session, signs it, uses it once, and drops it —
// so a compromised gateway leaks at most the sessions currently open, not a
// credential that works tomorrow.
package sshca

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"os"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/gsoultan/argus/internal/secrets"
)

// CA signs user certificates.
//
// The gateway holds this rather than the control plane, because native ssh(1)
// sessions must work when the control plane is down — putting the signer on the
// critical path would make an outage an outage for everyone. The cost is that a
// compromised gateway can mint certificates, which is why validity is measured
// in minutes and why production should back this with a KMS or PKCS#11 signer
// rather than a file.
type CA struct {
	signer ssh.Signer

	// Validity is how long a minted certificate is good for. Short: it only
	// has to survive the handshake, not the session. An established connection
	// is unaffected by its certificate expiring.
	Validity time.Duration

	// SourceAddress, when set, pins certificates to the gateway's egress
	// address so a leaked certificate cannot be used from anywhere else.
	SourceAddress string
}

// Load reads a CA private key from disk.
func Load(path string, validity time.Duration) (*CA, error) {
	// Mode-checked before anything else. This is the single most sensitive
	// file in the deployment: whoever reads it mints a certificate for any
	// principal on any target, and no host-key pin or policy will stop them.
	data, err := secrets.ReadPrivate(path)
	if err != nil {
		return nil, fmt.Errorf("read CA key %s: %w", path, err)
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("parse CA key: %w", err)
	}
	if validity <= 0 {
		validity = 2 * time.Minute
	}
	return &CA{signer: signer, Validity: validity}, nil
}

// PublicKey returns the authorized-keys line for TrustedUserCAKeys.
func (c *CA) PublicKey() string {
	return string(ssh.MarshalAuthorizedKey(c.signer.PublicKey()))
}

// Fingerprint identifies the CA for the console and for out-of-band checks.
func (c *CA) Fingerprint() string {
	return ssh.FingerprintSHA256(c.signer.PublicKey())
}

// Identity is who a certificate is being minted for.
type Identity struct {
	// SessionID ties the certificate to one Argus session.
	SessionID string
	// User is the human the session is attributed to.
	User string
	// Principal is the account on the target.
	Principal string
	// Target is the host, recorded in the key id for context.
	Target string
}

// Mint generates an ephemeral keypair and returns a signer holding a
// certificate for it.
//
// The private half never touches disk and is discarded when the session ends,
// so there is no artefact to exfiltrate afterwards.
func (c *CA) Mint(id Identity) (ssh.Signer, *ssh.Certificate, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate session key: %w", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, nil, err
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}

	now := time.Now()
	cert := &ssh.Certificate{
		Key:      sshPub,
		Serial:   serial,
		CertType: ssh.UserCert,
		// KeyId lands in the target's own sshd logs. That is the point: the
		// host records who connected without having to trust or query Argus,
		// so attribution survives even if Argus is unavailable or disbelieved.
		KeyId:           fmt.Sprintf("argus:%s:%s", id.User, id.SessionID),
		ValidPrincipals: []string{id.Principal},
		// Backdate slightly: hosts whose clocks run behind would otherwise
		// reject a certificate that is valid from the gateway's point of view.
		ValidAfter:  uint64(now.Add(-1 * time.Minute).Unix()),
		ValidBefore: uint64(now.Add(c.Validity).Unix()),
		Permissions: ssh.Permissions{
			CriticalOptions: map[string]string{},
			// Deliberately minimal. An OpenSSH certificate grants nothing by
			// default, so this is an allowlist rather than a set of removals.
			//
			// permit-pty is here because an interactive shell needs one.
			// Everything else is omitted on purpose:
			//
			//   permit-port-forwarding  would turn the certificate into a
			//                           tunnel into the private network
			//   permit-agent-forwarding would expose the user's other keys
			//   permit-X11-forwarding   broad surface, never needed on a server
			//   permit-user-rc          runs ~/.ssh/rc, a persistence foothold
			Extensions: map[string]string{
				"permit-pty": "",
			},
		},
	}

	if c.SourceAddress != "" {
		// Binds the certificate to the gateway's address, so even a leaked
		// certificate within its validity window is unusable elsewhere.
		cert.CriticalOptions["source-address"] = c.SourceAddress
	}

	if err := cert.SignCert(rand.Reader, c.signer); err != nil {
		return nil, nil, fmt.Errorf("sign certificate: %w", err)
	}

	sessionSigner, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, nil, err
	}
	certSigner, err := ssh.NewCertSigner(cert, sessionSigner)
	if err != nil {
		return nil, nil, fmt.Errorf("build cert signer: %w", err)
	}
	return certSigner, cert, nil
}

// Generate creates a new CA key at path.
//
// Ed25519 rather than RSA: smaller, faster, and no key-size decision to get
// wrong. Written 0600 because anyone who reads this file can mint credentials
// for every host that trusts it.
func Generate(path string) (*CA, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, "argus-ca")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return nil, fmt.Errorf("write CA key: %w", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, err
	}
	ca := &CA{signer: signer, Validity: 2 * time.Minute}

	if err := os.WriteFile(path+".pub", []byte(ca.PublicKey()), 0o644); err != nil {
		return nil, fmt.Errorf("write CA public key: %w", err)
	}
	return ca, nil
}

func randomSerial() (uint64, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(b), nil
}
