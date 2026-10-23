package rdp

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/credssp"
)

// CertFingerprint renders an X.509 certificate the way host pins are stored.
//
// The same SHA256:base64 shape OpenSSH uses for keys, so one trust store holds
// both and an operator reading it sees one kind of thing.
func CertFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return "SHA256:" + strings.TrimRight(
		base64.StdEncoding.EncodeToString(sum[:]), "=")
}

// Pinner verifies a target's identity.
type Pinner interface {
	VerifyFingerprint(host, presented, keyType string) error
}

// DialTarget completes the negotiation with the target and verifies its
// certificate against the pin.
//
// Argus terminates TLS itself: it is the only way to record a session rather
// than a stream of ciphertext, and it is what makes the target's certificate
// checkable at all.
func DialTarget(address, hostname string, protocol uint32, pins Pinner,
	timeout time.Duration) (net.Conn, error) {
	conn, _, err := DialTargetWithAuth(address, hostname, protocol, pins, timeout, nil)
	return conn, err
}

// DialTargetWithAuth dials a target and, when auth is supplied and the
// negotiated protocol calls for it, authenticates on the user's behalf.
//
// This is what gives RDP the property SSH already has: the credential reaching
// the host is one Argus holds, not one the user knows. Without it a user still
// needs a password of their own on every machine, which is the standing access
// the product exists to remove.
func DialTargetWithAuth(address, hostname string, protocol uint32, pins Pinner,
	timeout time.Duration, auth *credssp.Authenticator) (net.Conn, *credssp.Result, error) {

	raw, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return nil, nil, fmt.Errorf("dial %s: %w", address, err)
	}

	if _, err := raw.Write(buildConnectionRequest(protocol)); err != nil {
		raw.Close()
		return nil, nil, fmt.Errorf("send connection request: %w", err)
	}
	frame, err := ReadPDU(raw)
	if err != nil {
		raw.Close()
		return nil, nil, fmt.Errorf("read connection confirm: %w", err)
	}
	selected, failure, err := ParseConnectionConfirm(frame)
	if err != nil {
		raw.Close()
		return nil, nil, err
	}
	if failure != 0 {
		raw.Close()
		return nil, nil, fmt.Errorf("target refused: %s", FailureName(failure))
	}
	if selected == ProtocolRDP {
		raw.Close()
		return nil, nil, errors.New(
			"target selected standard RDP security, which Argus will not broker")
	}

	// InsecureSkipVerify with an explicit pin check, not instead of one. RDP
	// hosts overwhelmingly present self-signed certificates, so PKI validation
	// would reject every real target; the pin is the actual control, and it is
	// stricter than PKI here because it detects a certificate that changed even
	// when the new one is perfectly well signed.
	conn := tls.Client(raw, &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         hostname,
		MinVersion:         tls.VersionTLS12,
		// CredSSP runs inside this channel, and Windows' server side of it
		// answers the first NTLM message with an internal-error alert when the
		// channel is TLS 1.3. The Windows client stays on 1.2 for the same
		// reason.
		MaxVersion: tls.VersionTLS12,
	})
	if err := conn.Handshake(); err != nil {
		raw.Close()
		return nil, nil, fmt.Errorf("tls handshake with %s: %w", address, err)
	}

	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		conn.Close()
		return nil, nil, errors.New("target presented no certificate")
	}
	presented := CertFingerprint(certs[0].Raw)
	if pins != nil {
		if err := pins.VerifyFingerprint(hostname, presented, "x509"); err != nil {
			conn.Close()
			return nil, nil, err
		}
	}

	// Authentication happens after pinning, never before. Delegating a
	// credential to a host whose certificate does not match its pin would hand
	// the password to whoever is actually answering.
	if selected != ProtocolHybrid && selected != ProtocolHybridEx {
		return conn, nil, nil
	}
	// The server chose network level authentication and there is nobody to
	// perform it. Returning the connection would hand back something that
	// negotiated NLA and then skipped it -- Connect's contract is that
	// everything establishing trust has already run, and here it has not.
	//
	// Found by pointing the integration test at a real Windows host with nil
	// and getting a usable connection back with no error.
	if auth == nil {
		conn.Close()
		// hostname is empty for a dial by address; naming something is the
		// difference between a usable error and a sentence starting with a
		// space.
		who := hostname
		if who == "" {
			who = address
		}
		return nil, nil, fmt.Errorf(
			"%s negotiated %s but no authenticator was supplied",
			who, ProtocolName(selected))
	}

	// The binding covers the SubjectPublicKeyInfo of the certificate on this
	// connection, which is what ties the authentication to this channel.
	auth.PublicKey = certs[0].RawSubjectPublicKeyInfo
	result, err := auth.Authenticate(conn)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("authenticate to %s: %w", hostname, err)
	}
	return conn, &result, nil
}

// buildConnectionRequest builds the client-side opening PDU Argus sends to a
// target. No cookie: the target has no use for Argus's routing information, and
// forwarding the user's would leak the internal naming scheme onto the host.
func buildConnectionRequest(protocol uint32) []byte {
	neg := make([]byte, negLength)
	neg[0] = negTypeRequest
	putU16(neg[2:4], negLength)
	putU32(neg[4:8], protocol)

	x := make([]byte, 7+len(neg))
	x[0] = byte(len(x) - 1)
	x[1] = tpduConnectionRequest
	copy(x[7:], neg)

	out := make([]byte, tpktHeader+len(x))
	out[0] = tpktVersion
	out[2] = byte(len(out) >> 8)
	out[3] = byte(len(out))
	copy(out[tpktHeader:], x)
	return out
}

func putU16(b []byte, v uint16) { b[0] = byte(v); b[1] = byte(v >> 8) }
func putU32(b []byte, v uint32) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
}
