package credssp

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"time"
)

// ErrAuthFailed reports that the target refused the credentials.
//
// Distinguished from a protocol error because the two need different responses:
// a refusal means the vaulted credential is wrong or the account is locked, and
// a protocol error means Argus and the host disagree about the exchange itself.
var ErrAuthFailed = errors.New("the target refused the credentials")

// maxTokenSize bounds a single CredSSP message.
//
// The exchange carries NTLM tokens and one set of credentials, none of which
// approach this. The bound exists because the length is read from the wire
// before anything has been authenticated, and an unbounded read is an
// invitation to allocate on a stranger's say-so.
const maxTokenSize = 64 << 10

// Authenticator runs the CredSSP exchange against a target.
//
// The connection must already be a completed TLS session: CredSSP is defined to
// run inside one, and the public key binding is over that channel's
// certificate. Passing a plain socket would produce an exchange that appears to
// succeed and binds to nothing.
type Authenticator struct {
	// PublicKey is the DER SubjectPublicKeyInfo of the target's certificate,
	// which the binding is computed over.
	PublicKey []byte
	// Credentials are what Argus delegates on the user's behalf.
	Credentials Credentials
	// Now is overridable for tests.
	Now func() time.Time
}

// Result reports how the exchange concluded.
type Result struct {
	// Version the target agreed to.
	Version int
	// LegacyBinding records that the target used the pre-version-5 public key
	// binding, which is not nonce-bound. Worth carrying onto the session record
	// so an auditor can see which guarantee this session actually had.
	LegacyBinding bool
}

// Authenticate runs the full exchange and delegates the credentials.
//
// The order is not negotiable: the public key binding is verified before the
// credentials are sent, because the binding is what establishes that the
// channel carrying them is the one the target authenticated on. Sending first
// and checking afterwards would hand a password to whoever is actually there.
func (a *Authenticator) Authenticate(conn io.ReadWriter) (Result, error) {
	if len(a.PublicKey) == 0 {
		return Result{}, errors.New("no target public key: the binding would cover nothing")
	}
	now := a.Now
	if now == nil {
		now = time.Now
	}

	// 1. NEGOTIATE
	if err := writeRequest(conn, Request{Version: Version, NegoToken: Negotiate()}); err != nil {
		return Result{}, fmt.Errorf("send negotiate: %w", err)
	}

	// 2. CHALLENGE
	resp, err := readRequest(conn)
	if err != nil {
		return Result{}, fmt.Errorf("read challenge: %w", err)
	}
	if resp.ErrorCode != 0 {
		return Result{}, fmt.Errorf("%w: target reported 0x%08x", ErrAuthFailed, uint32(resp.ErrorCode))
	}
	if len(resp.NegoToken) == 0 {
		return Result{}, fmt.Errorf("%w: challenge carried no token", ErrMalformed)
	}
	challenge, err := ParseChallenge(resp.NegoToken)
	if err != nil {
		return Result{}, err
	}

	result := Result{Version: resp.Version}
	// The nonce binding exists from version 5. A target offering less gets the
	// legacy check rather than a refusal, and the session says which it was.
	result.LegacyBinding = resp.Version < 5

	// 3. AUTHENTICATE, with the public key binding attached
	clientChallenge := make([]byte, 8)
	if _, err := readRandom(clientChallenge); err != nil {
		return Result{}, err
	}
	auth, sessionKey, err := BuildAuthenticate(challenge, a.Credentials,
		clientChallenge, Timestamp(now()))
	if err != nil {
		return Result{}, err
	}

	sealer, err := NewSealer(sessionKey, challenge.Flags, true)
	if err != nil {
		return Result{}, err
	}

	var nonce []byte
	req := Request{Version: Version, NegoToken: auth}
	if result.LegacyBinding {
		req.PubKeyAuth = sealer.Seal(a.PublicKey)
	} else {
		if nonce, err = NewNonce(); err != nil {
			return Result{}, err
		}
		req.ClientNonce = nonce
		req.PubKeyAuth = PubKeyAuth(sealer, a.PublicKey, nonce)
	}
	if err := writeRequest(conn, req); err != nil {
		return Result{}, fmt.Errorf("send authenticate: %w", err)
	}

	// 4. The target's binding answer, which decides whether the credentials go
	resp, err = readRequest(conn)
	if err != nil {
		return Result{}, fmt.Errorf("read public key answer: %w", err)
	}
	if resp.ErrorCode != 0 {
		return Result{}, fmt.Errorf("%w: target reported 0x%08x", ErrAuthFailed, uint32(resp.ErrorCode))
	}
	if len(resp.PubKeyAuth) == 0 {
		return Result{}, fmt.Errorf("%w: target sent no public key binding", ErrMalformed)
	}

	if result.LegacyBinding {
		err = VerifyLegacyPubKeyAuth(sealer, resp.PubKeyAuth, a.PublicKey)
	} else {
		err = VerifyPubKeyAuth(sealer, resp.PubKeyAuth, a.PublicKey, nonce)
	}
	if err != nil {
		// The credentials are still unsent, which is the entire point of
		// checking here rather than after.
		return result, err
	}

	// 5. Only now: the credentials themselves
	creds, err := EncodeCredentials(a.Credentials)
	if err != nil {
		return result, err
	}
	if err := writeRequest(conn, Request{
		Version:  Version,
		AuthInfo: sealer.Seal(creds),
	}); err != nil {
		return result, fmt.Errorf("send credentials: %w", err)
	}
	return result, nil
}

// writeRequest sends one DER-encoded TSRequest.
func writeRequest(w io.Writer, r Request) error {
	der, err := r.Encode()
	if err != nil {
		return err
	}
	_, err = w.Write(der)
	return err
}

// readRequest reads one DER-encoded TSRequest.
//
// TSRequests are self-delimiting: the SEQUENCE header carries the length, so
// exactly that many bytes are read rather than draining the connection. Reading
// more would consume the RDP traffic that follows the exchange.
func readRequest(r io.Reader) (Request, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(r, header); err != nil {
		return Request{}, err
	}
	if header[0] != 0x30 {
		return Request{}, fmt.Errorf("%w: expected a DER SEQUENCE, got 0x%02x",
			ErrMalformed, header[0])
	}

	var bodyLen int
	var lengthBytes []byte
	if header[1] < 0x80 {
		bodyLen = int(header[1])
	} else {
		n := int(header[1] & 0x7F)
		if n == 0 || n > 4 {
			return Request{}, fmt.Errorf("%w: %d-byte DER length", ErrMalformed, n)
		}
		lengthBytes = make([]byte, n)
		if _, err := io.ReadFull(r, lengthBytes); err != nil {
			return Request{}, err
		}
		for _, b := range lengthBytes {
			bodyLen = bodyLen<<8 | int(b)
		}
	}
	if bodyLen > maxTokenSize {
		return Request{}, fmt.Errorf("%w: TSRequest claims %d bytes, over the %d cap",
			ErrMalformed, bodyLen, maxTokenSize)
	}

	full := make([]byte, 0, 2+len(lengthBytes)+bodyLen)
	full = append(full, header...)
	full = append(full, lengthBytes...)
	body := make([]byte, bodyLen)
	if _, err := io.ReadFull(r, body); err != nil {
		return Request{}, err
	}
	full = append(full, body...)
	return ParseRequest(full)
}

// readRandom is a seam so tests can make the exchange deterministic.
var readRandom = rand.Read
