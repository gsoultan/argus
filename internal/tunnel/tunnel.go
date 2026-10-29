// Package tunnel carries streams between argus-control and the agents.
//
// Each agent holds one outbound connection to the control plane, so a managed
// server needs no inbound port at all. The control plane opens a stream per
// use and names the service it wants in a short header; the agent decides
// what each name means from its own configuration, and refuses any name it
// does not know. The control plane never names an address.
//
// Two transports carry the same streams:
//
//   - QUIC, the fast one. Streams do not wait on each other when a packet is
//     lost, a reconnect is one round trip, and the connection survives the
//     server's address changing. Needs UDP to reach the control plane.
//   - TLS over TCP with yamux, through the control plane's ordinary HTTPS
//     listener as an HTTP upgrade. Slower under loss, but it goes wherever
//     HTTPS goes, including through a proxy that terminates TLS.
//
// The agent prefers QUIC when the control plane offers it and falls back to
// the upgrade when UDP is blocked.
package tunnel

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

const (
	// ALPN names the protocol on the QUIC handshake. A version in the name,
	// so a later incompatible change is a different protocol rather than a
	// silent misreading of this one.
	ALPN = "argus-tunnel/1"
	// UpgradeToken is the Upgrade header value on the HTTPS fallback.
	UpgradeToken = "argus-tunnel/1"
	// UpgradePath is where the HTTPS fallback is served.
	UpgradePath = "/api/v1/agent/tunnel"
	// QUICHeader is set on the upgrade response when the control plane also
	// listens for QUIC, naming the UDP port.
	QUICHeader = "Argus-Tunnel-Quic-Port"

	// maxHeader bounds a stream header. A header is a service name and a
	// little metadata; anything longer is not one, and reading it unbounded
	// would let the other side make this side buffer without limit.
	maxHeader = 4 << 10

	// HeaderTimeout is how long either side waits for a stream's header or
	// its answer.
	HeaderTimeout = 10 * time.Second
)

// Transport names how a session is carried.
type Transport string

const (
	TransportQUIC    Transport = "quic"
	TransportUpgrade Transport = "tcp"
)

// Stream is one bidirectional byte stream in a session.
type Stream interface {
	io.ReadWriteCloser
	SetDeadline(time.Time) error
}

// Session is one agent's connection, whichever transport carries it.
type Session interface {
	OpenStream(ctx context.Context) (Stream, error)
	AcceptStream(ctx context.Context) (Stream, error)
	// Done is closed when the session ends, from either side.
	Done() <-chan struct{}
	Close() error
	Transport() Transport
	RemoteAddr() net.Addr
	// GrantKey is the control plane's grant key as it sent it when the
	// session opened, on the agent's side; empty on the control plane's.
	GrantKey() string
}

// Header opens every stream. Service is the only field the agent acts on.
type Header struct {
	Service string `json:"service"`
	// Grant is the control plane's signed permission, for a service that
	// opens a session. See Grant.
	Grant string `json:"grant,omitempty"`
	// Desktop is how a desktop session should open; see DesktopOpen.
	Desktop *DesktopOpen `json:"desktop,omitempty"`
}

// DesktopOpen is what a desktop session needs beyond its grant.
type DesktopOpen struct {
	// Password is the account's own, typed by the person in the console, for
	// the host's Remote Desktop to authenticate (network level
	// authentication). It crosses the control plane in memory only, is used
	// for this one logon and is never written down or logged on either side.
	// Empty asks for the Windows sign-in screen instead, which a host that
	// requires network level authentication refuses.
	Password string `json:"password,omitempty"`
	// Width and Height are the desktop size the browser asked for.
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
	// Graphics asks for RDP's graphics pipeline, passed through to a browser
	// that decodes it (H.264), instead of bitmap updates.
	Graphics bool `json:"graphics,omitempty"`
}

// Answer is the agent's reply to a Header, before any service bytes.
type Answer struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Session is the recording's id, for a service that opened one.
	Session string `json:"session,omitempty"`
	// GrantKey answers a QUIC hello with the control plane's grant key.
	GrantKey string `json:"grant_key,omitempty"`
}

// Hello is the first message on a QUIC session, from the agent. The HTTPS
// fallback carries the same credential in its Authorization header instead.
type Hello struct {
	Credential string `json:"credential"`
	Hostname   string `json:"hostname"`
	Version    string `json:"version"`
}

// ErrRefused is a stream the agent would not open, with its reason.
type ErrRefused struct{ Reason string }

func (e ErrRefused) Error() string { return "tunnel: refused: " + e.Reason }

// WriteJSON writes one header-sized message as a line.
func WriteJSON(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) >= maxHeader {
		return fmt.Errorf("tunnel: message of %d bytes exceeds %d", len(b), maxHeader)
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// ReadJSON reads one line written by WriteJSON, refusing anything longer
// than a header can be.
//
// It reads byte by byte through a one-byte buffer rather than a bufio.Reader,
// so nothing past the newline is consumed: the bytes that follow belong to
// the service, not to the header.
func ReadJSON(r io.Reader, v any) error {
	buf := make([]byte, 0, 256)
	one := make([]byte, 1)
	for {
		n, err := r.Read(one)
		if n == 1 {
			if one[0] == '\n' {
				return json.Unmarshal(buf, v)
			}
			if len(buf) >= maxHeader {
				return fmt.Errorf("tunnel: header longer than %d bytes", maxHeader)
			}
			buf = append(buf, one[0])
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return io.ErrUnexpectedEOF
			}
			return err
		}
	}
}

// Open opens a stream to service and waits for the agent to accept it.
func Open(ctx context.Context, s Session, service string) (Stream, error) {
	st, _, err := OpenWith(ctx, s, Header{Service: service})
	return st, err
}

// OpenWith opens a stream with a full header and returns the agent's answer.
func OpenWith(ctx context.Context, s Session, h Header) (Stream, Answer, error) {
	st, err := s.OpenStream(ctx)
	if err != nil {
		return nil, Answer{}, err
	}
	// The caller's deadline when it set one: it knows how long what it is
	// opening may take to answer, and a session on a first logon takes longer
	// than a header.
	deadline := time.Now().Add(HeaderTimeout)
	if d, ok := ctx.Deadline(); ok {
		deadline = d
	}
	_ = st.SetDeadline(deadline)
	if err := WriteJSON(st, h); err != nil {
		_ = st.Close()
		return nil, Answer{}, err
	}
	var a Answer
	if err := ReadJSON(st, &a); err != nil {
		_ = st.Close()
		return nil, Answer{}, err
	}
	if !a.OK {
		_ = st.Close()
		return nil, a, ErrRefused{Reason: a.Error}
	}
	_ = st.SetDeadline(time.Time{})
	return st, a, nil
}

// Ping measures the round trip to the agent through its "ping" service.
func Ping(ctx context.Context, s Session) (time.Duration, error) {
	st, err := Open(ctx, s, "ping")
	if err != nil {
		return 0, err
	}
	defer st.Close()
	if d, ok := ctx.Deadline(); ok {
		_ = st.SetDeadline(d)
	}
	payload := []byte("argus-ping-0123456789abcdef")
	start := time.Now()
	if _, err := st.Write(payload); err != nil {
		return 0, err
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(st, got); err != nil {
		return 0, err
	}
	rtt := time.Since(start)
	if string(got) != string(payload) {
		return 0, errors.New("tunnel: ping answered with something else")
	}
	return rtt, nil
}

// ServePing is the agent's "ping" service: it echoes a short payload.
func ServePing(st Stream) {
	defer st.Close()
	_ = st.SetDeadline(time.Now().Add(HeaderTimeout))
	buf := make([]byte, 64)
	n, err := st.Read(buf)
	if err != nil && n == 0 {
		return
	}
	_, _ = st.Write(buf[:n])
}

// bufferedConn is a net.Conn whose first bytes were already read into a
// bufio.Reader, as happens to a hijacked HTTP connection.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
