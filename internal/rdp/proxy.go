package rdp

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"
)

// Separators accepted between principal and target in the mstshash cookie.
//
// A colon reads best and is what the SSH side uses, but some clients are
// awkward about punctuation in a username field, so the same alternatives are
// accepted:
//
//	mstsc /v:argus.example.com   with username  ops:win-01
//	                                            ops+win-01
const cookieSeparators = ":+/"

// Request is a parsed connection intent, derived from the cookie.
type Request struct {
	// Principal is the Windows account to open on the target.
	Principal string
	// Target is the host as the user typed it.
	Target string
}

// ParseCookie splits an mstshash cookie into a connection request.
//
// A cookie with no separator is refused rather than guessed at. Defaulting the
// principal would mean a typo silently opens a session as the wrong account,
// which is the kind of ambiguity a privileged access gateway must not have.
func ParseCookie(cookie string) (Request, error) {
	cookie = strings.TrimSpace(cookie)
	if cookie == "" {
		return Request{}, errors.New(
			"no cookie: connect with a username of principal:host (e.g. ops:win-01)")
	}
	idx := strings.IndexAny(cookie, cookieSeparators)
	if idx < 0 {
		return Request{}, fmt.Errorf(
			"cookie %q has no target: use principal:host (e.g. ops:win-01)", cookie)
	}

	principal := strings.TrimSpace(cookie[:idx])
	target := strings.TrimSpace(cookie[idx+1:])
	if principal == "" {
		return Request{}, fmt.Errorf("cookie %q has an empty principal", cookie)
	}
	if target == "" {
		return Request{}, fmt.Errorf("cookie %q has an empty target", cookie)
	}
	// A second separator means the user typed something ambiguous.
	if strings.ContainsAny(target, cookieSeparators) {
		return Request{}, fmt.Errorf("cookie %q has more than one separator", cookie)
	}
	return Request{Principal: principal, Target: target}, nil
}

// CertFingerprint renders an X.509 certificate the way host pins are stored.
//
// The same SHA256:base64 shape OpenSSH uses for keys, so one trust store holds
// both and an operator reading it sees one kind of thing.
func CertFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return "SHA256:" + strings.TrimRight(
		base64.StdEncoding.EncodeToString(sum[:]), "=")
}

// Pinner verifies a target's identity. Satisfied by hostkey.Store.
type Pinner interface {
	VerifyFingerprint(host, presented, keyType string) error
}

// Session is one brokered RDP connection.
type Session struct {
	ID        string
	User      string
	Principal string
	Target    string
	Address   string
	RemoteIP  string
	StartedAt time.Time
	// Protocol is the security protocol the session was brokered over.
	Protocol uint32

	mu       sync.Mutex
	rec      *Recorder
	closed   bool
	chainEnd string
}

// Handshake performs the RDP negotiation with a client and refuses anything
// Argus will not broker.
//
// Runs before the target is dialled. Everything refusable is refused here, so a
// rejected connection never reaches the point of producing traffic — and a
// refusal is a negotiation failure PDU, which the client renders as a specific
// message, rather than a dropped socket that sends the user to a helpdesk.
func Handshake(client net.Conn, log *slog.Logger) (Request, uint32, error) {
	frame, err := ReadPDU(client)
	if err != nil {
		return Request{}, 0, fmt.Errorf("read connection request: %w", err)
	}
	if Classify(frame) != ConnectionRequest {
		return Request{}, 0, errors.New("first PDU is not a connection request")
	}

	info, err := ParseConnectionRequest(frame)
	if err != nil {
		if errors.Is(err, ErrNoNegotiation) {
			// No negotiation structure means standard RDP security by default.
			// Saying why, in the protocol's own vocabulary, is the difference
			// between a user who reconfigures their client and one who files a
			// ticket.
			_, _ = client.Write(BuildNegotiationFailure(FailSSLRequiredByServer))
			return Request{}, 0, errors.New(
				"client offered only standard RDP security, which Argus will not broker")
		}
		return Request{}, 0, err
	}

	protocol, ok := SelectProtocol(info.RequestedProtocols)
	if !ok {
		_, _ = client.Write(BuildNegotiationFailure(FailSSLRequiredByServer))
		return Request{}, 0, fmt.Errorf(
			"client offered no acceptable security protocol (requested %#08x)",
			info.RequestedProtocols)
	}

	req, err := ParseCookie(info.Cookie)
	if err != nil {
		// The connection is well-formed but says nothing about where it wants
		// to go. Answering with a failure lets the client show the reason.
		_, _ = client.Write(BuildNegotiationFailure(FailInconsistentFlags))
		return Request{}, 0, err
	}

	if log != nil {
		log.Info("rdp connection request",
			"principal", req.Principal, "target", req.Target,
			"protocol", ProtocolName(protocol),
			"offered", fmt.Sprintf("%#08x", info.RequestedProtocols))
	}
	return req, protocol, nil
}

// ConfirmProtocol tells the client which protocol was selected.
//
// Sent only once the target has been resolved and the connection authorised: a
// client that receives a confirm will immediately start a TLS handshake, so
// sending it before Argus is ready to proxy would leave the user staring at a
// connection that is going nowhere.
func ConfirmProtocol(client net.Conn, protocol uint32) error {
	_, err := client.Write(BuildConnectionConfirm(protocol))
	return err
}

// DialTarget completes the negotiation with the target and verifies its
// certificate against the pin.
//
// Argus terminates TLS on both sides: it is the only way to record a session
// rather than a stream of ciphertext, and it is what makes the target's
// certificate checkable at all. That check is the RDP counterpart of host-key
// pinning — without it a gateway would broker privileged sessions it cannot
// prove reached the right machine.
func DialTarget(address, hostname string, protocol uint32, pins Pinner,
	timeout time.Duration) (net.Conn, error) {

	raw, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", address, err)
	}

	// The target gets the same negotiation the client asked for, so the session
	// is no weaker end to end than it is on the near side.
	if _, err := raw.Write(buildConnectionRequest(protocol)); err != nil {
		raw.Close()
		return nil, fmt.Errorf("send connection request: %w", err)
	}
	frame, err := ReadPDU(raw)
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("read connection confirm: %w", err)
	}
	selected, failure, err := ParseConnectionConfirm(frame)
	if err != nil {
		raw.Close()
		return nil, err
	}
	if failure != 0 {
		raw.Close()
		return nil, fmt.Errorf("target refused: %s", FailureName(failure))
	}
	if selected == ProtocolRDP {
		raw.Close()
		return nil, errors.New(
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
	})
	if err := conn.Handshake(); err != nil {
		raw.Close()
		return nil, fmt.Errorf("tls handshake with %s: %w", address, err)
	}

	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		conn.Close()
		return nil, errors.New("target presented no certificate")
	}
	presented := CertFingerprint(certs[0].Raw)
	if pins != nil {
		if err := pins.VerifyFingerprint(hostname, presented, "x509"); err != nil {
			conn.Close()
			return nil, err
		}
	}
	return conn, nil
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

// Relay proxies the two plaintext streams, recording every PDU.
//
// Both directions run until one ends. Recording failures terminate the session
// rather than letting it continue unrecorded: an auditor asking "are all
// privileged sessions recorded?" needs that to be true without an asterisk.
func (s *Session) Relay(client, target net.Conn, log *slog.Logger) error {
	var wg sync.WaitGroup
	errs := make(chan error, 2)

	pump := func(dst, src net.Conn, stream Stream) {
		defer wg.Done()
		// Closing the far side is what unblocks the opposite direction; without
		// it a half-closed connection leaves one goroutine reading forever.
		defer dst.Close()

		for {
			frame, err := ReadPDU(src)
			if err != nil {
				if !errors.Is(err, io.EOF) && !isClosed(err) {
					errs <- fmt.Errorf("read %s: %w", stream, err)
				}
				return
			}
			if err := s.record(stream, frame); err != nil {
				errs <- err
				return
			}
			if _, err := dst.Write(frame); err != nil {
				return
			}
		}
	}

	wg.Add(2)
	go pump(target, client, ClientInput)
	go pump(client, target, ServerOutput)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Session) record(stream Stream, frame []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.rec == nil {
		return nil
	}
	return s.rec.Write(stream, frame)
}

// SetRecorder attaches a recording to the session.
func (s *Session) SetRecorder(r *Recorder) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec = r
}

// Recorder returns the session's recorder, or nil.
func (s *Session) Recorder() *Recorder {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec
}

// Close seals the recording and returns the chain head.
func (s *Session) Close() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.chainEnd, nil
	}
	s.closed = true
	if s.rec == nil {
		return "", nil
	}
	head, err := s.rec.Close()
	s.chainEnd = head
	return head, err
}

// isClosed reports the ordinary "the other end went away" errors, which are not
// worth surfacing as session failures.
func isClosed(err error) bool {
	return errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrUnexpectedEOF)
}
