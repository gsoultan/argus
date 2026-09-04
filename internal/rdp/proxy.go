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

	"github.com/gsoultan/argus/internal/credssp"
	"github.com/gsoultan/argus/internal/live"
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
	// Injected records that Argus authenticated on the user's behalf rather
	// than passing them through to a logon screen.
	Injected bool
	// LegacyBinding records a target that used the pre-version-5 public key
	// binding, which is not bound to a nonce.
	LegacyBinding bool
	// Width and Height are the desktop dimensions, needed by a viewer
	// attaching to a session already running.
	Width, Height int

	// hub fans display frames out to shadow viewers. Always present, so any
	// session can be watched without having been opened in a special way.
	hub *live.Hub
	// rea decodes relayed updates, but only while someone is watching.
	rea *Reassembler
	// shareID and userID are learned by watching the connection sequence go
	// past. A proxied session is relayed opaquely, so these are the only two
	// values Argus reads out of it — and they are read because without them it
	// cannot ask the target to redraw for a viewer who arrives late.
	shareID uint32
	userID  uint16
	// killedBy and killReason record an administrative termination, so the
	// session's record says who ended it rather than leaving it
	// indistinguishable from a dropped connection.
	killedBy   string
	killReason string

	mu       sync.Mutex
	rec      *Recorder
	closed   bool
	chainEnd string
}

// Refuse sends a negotiation failure and closes the connection cleanly.
//
// Closing a socket that still has unread data queued makes the kernel send RST
// and discard whatever was pending, so a refusal written immediately before
// Close reaches the client as a connection reset — the user sees "the
// connection was lost" instead of the reason. Half-closing and then draining
// gives the peer the chance to read what was sent before the socket goes away.
func Refuse(conn net.Conn, code uint32) {
	_, _ = conn.Write(BuildNegotiationFailure(code))

	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return
	}
	// FIN rather than RST: the client sees an orderly end after the refusal.
	_ = tcp.CloseWrite()
	// Draining is what actually clears the receive queue. Bounded, because a
	// client that keeps talking must not hold the connection open.
	_ = tcp.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _ = io.Copy(io.Discard, io.LimitReader(tcp, 64<<10))
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
			Refuse(client, FailSSLRequiredByServer)
			return Request{}, 0, errors.New(
				"client offered only standard RDP security, which Argus will not broker")
		}
		return Request{}, 0, err
	}

	protocol, ok := SelectProtocol(info.RequestedProtocols)
	if !ok {
		Refuse(client, FailSSLRequiredByServer)
		return Request{}, 0, fmt.Errorf(
			"client offered no acceptable security protocol (requested %#08x)",
			info.RequestedProtocols)
	}

	req, err := ParseCookie(info.Cookie)
	if err != nil {
		// The connection is well-formed but says nothing about where it wants
		// to go. Answering with a failure lets the client show the reason.
		Refuse(client, FailInconsistentFlags)
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

	// The target gets the same negotiation the client asked for, so the session
	// is no weaker end to end than it is on the near side.
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
	if auth == nil || (selected != ProtocolHybrid && selected != ProtocolHybridEx) {
		return conn, nil, nil
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
			if stream == ServerOutput {
				s.observe(frame)
				s.shadow(frame)
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

// observe learns the share and user ids from the relayed stream.
//
// Nothing else is interpreted. A proxy that parsed the whole session would be
// a second RDP implementation running against traffic it does not need to
// understand, and every field it read would be a field it could be wrong about.
func (s *Session) observe(frame []byte) {
	s.mu.Lock()
	known := s.shareID != 0 && s.userID != 0
	s.mu.Unlock()
	if known || len(frame) == 0 || frame[0]&actionMask != actionX224 {
		return
	}

	if id, err := ParseAttachUserConfirm(frame); err == nil {
		s.mu.Lock()
		s.userID = id
		s.mu.Unlock()
		return
	}
	_, payload, err := ParseSendDataIndication(frame)
	if err != nil {
		return
	}
	sc, err := ParseShareControl(payload)
	if err != nil || sc.Type != pduTypeDemandActive {
		return
	}
	if demand, derr := ParseDemandActive(sc.Body); derr == nil {
		s.mu.Lock()
		s.shareID = demand.ShareID
		s.mu.Unlock()
	}
}

// shadow decodes a frame and broadcasts it, but only while someone is watching.
//
// Decoding every update of every proxied session whether or not anyone is
// looking would spend the gateway's CPU on pixels nobody sees, and a privileged
// access gateway should not get slower because a feature exists.
func (s *Session) shadow(frame []byte) {
	s.mu.Lock()
	hub := s.hub
	s.mu.Unlock()
	if hub == nil || hub.Viewers() == 0 {
		return
	}

	s.mu.Lock()
	if s.rea == nil {
		s.rea = NewReassembler()
	}
	rea := s.rea
	s.mu.Unlock()

	rects, err := DecodePDU(rea, frame)
	if err != nil || len(rects) == 0 {
		return
	}
	batch := make([]byte, 0, 4096)
	for _, r := range rects {
		batch = append(batch, EncodeRect(r)...)
	}
	hub.Broadcast(batch)
}

// RefreshFor asks the target to redraw, for a viewer that arrived mid-session.
//
// Injected into the stream the real client is using. A redraw is idempotent, so
// the operator sees nothing unusual — their screen is simply repainted with
// what was already on it.
func (s *Session) RefreshFor(target io.Writer) error {
	s.mu.Lock()
	shareID, userID := s.shareID, s.userID
	s.mu.Unlock()
	if shareID == 0 || userID == 0 {
		return errors.New("the connection sequence has not been observed yet")
	}
	pdu := RefreshRectPDU(shareID, userID, s.Width, s.Height)
	_, err := target.Write(SendDataRequest(userID, ChannelGlobal, pdu))
	return err
}

func (s *Session) record(stream Stream, frame []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.rec == nil {
		return nil
	}
	return s.rec.Write(stream, frame)
}

// Hub returns the session's broadcast hub, creating it on first use.
func (s *Session) Hub() *live.Hub {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hub == nil {
		s.hub = live.NewHub()
	}
	return s.hub
}

// Terminate ends the session on an administrator's instruction.
//
// Returns false if it had already finished, so a caller can say so rather than
// report a kill that did not happen.
func (s *Session) Terminate(by, reason string) bool {
	s.mu.Lock()
	if s.closed || s.killedBy != "" {
		s.mu.Unlock()
		return false
	}
	if reason == "" {
		reason = "no reason given"
	}
	s.killedBy, s.killReason = by, reason
	s.mu.Unlock()
	// The caller closes the connection; there is no equivalent of a terminal
	// notice to write into a desktop, so the record carries the explanation
	// instead.
	return true
}

// Killed reports the administrative termination, if there was one.
func (s *Session) Killed() (by, reason string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.killedBy, s.killReason, s.killedBy != ""
}

// Live reports whether the session is still running.
func (s *Session) Live() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed
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
	// Ends every shadow subscription. A viewer left hanging on a finished
	// session cannot tell it from one that has gone quiet.
	if s.hub != nil {
		s.hub.Close()
	}
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
