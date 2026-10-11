package tunnel

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/quic-go/quic-go"
)

/* ── QUIC ────────────────────────────────────────────────────────────────── */

// quicConfig is shared by both ends. Keep-alives every 15s hold the path
// open through NAT and stateful firewalls, which typically forget an idle
// UDP flow after 30s; 45s of silence ends the session so the agent
// reconnects rather than waiting on a dead path.
func quicConfig(maxIncoming int64) *quic.Config {
	return &quic.Config{
		HandshakeIdleTimeout: 10 * time.Second,
		MaxIdleTimeout:       45 * time.Second,
		KeepAlivePeriod:      15 * time.Second,
		MaxIncomingStreams:   maxIncoming,
	}
}

// ServerQUICConfig bounds what an agent may open toward the control plane:
// the hello and nothing else.
func ServerQUICConfig() *quic.Config { return quicConfig(2) }

// clientQUICConfig bounds what the control plane may open toward an agent.
// Generous, because every session is a stream, and still a bound.
func clientQUICConfig() *quic.Config { return quicConfig(256) }

type quicSession struct {
	c        *quic.Conn
	grantKey string
}

// NewQUICSession wraps an established QUIC connection.
func NewQUICSession(c *quic.Conn) Session { return quicSession{c: c} }

func (s quicSession) OpenStream(ctx context.Context) (Stream, error) {
	return s.c.OpenStreamSync(ctx)
}

func (s quicSession) AcceptStream(ctx context.Context) (Stream, error) {
	return s.c.AcceptStream(ctx)
}

func (s quicSession) Done() <-chan struct{} { return s.c.Context().Done() }
func (s quicSession) Close() error          { return s.c.CloseWithError(0, "closed") }
func (s quicSession) Transport() Transport  { return TransportQUIC }
func (s quicSession) RemoteAddr() net.Addr  { return s.c.RemoteAddr() }
func (s quicSession) GrantKey() string      { return s.grantKey }

/* ── yamux over an HTTP upgrade ──────────────────────────────────────────── */

func yamuxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	c.EnableKeepAlive = true
	c.KeepAliveInterval = 15 * time.Second
	c.ConnectionWriteTimeout = 10 * time.Second
	c.LogOutput = io.Discard
	return c
}

type muxSession struct {
	s        *yamux.Session
	conn     net.Conn
	grantKey string
}

// NewServerMux runs the control plane's side of a hijacked upgrade.
func NewServerMux(conn net.Conn, buffered *bufio.Reader) (Session, error) {
	var c net.Conn = conn
	if buffered != nil && buffered.Buffered() > 0 {
		c = bufferedConn{Conn: conn, r: buffered}
	}
	cfg := yamuxConfig()
	// The agent opens nothing toward the control plane over this transport;
	// the backlog only has to hold what a misbehaving agent sends before it
	// is refused.
	cfg.AcceptBacklog = 4
	s, err := yamux.Server(c, cfg)
	if err != nil {
		return nil, err
	}
	return muxSession{s: s, conn: conn}, nil
}

func (m muxSession) OpenStream(context.Context) (Stream, error) { return m.s.OpenStream() }

func (m muxSession) AcceptStream(ctx context.Context) (Stream, error) {
	return m.s.AcceptStreamWithContext(ctx)
}

func (m muxSession) Done() <-chan struct{} { return m.s.CloseChan() }
func (m muxSession) Close() error          { return m.s.Close() }
func (m muxSession) Transport() Transport  { return TransportUpgrade }
func (m muxSession) RemoteAddr() net.Addr  { return m.conn.RemoteAddr() }
func (m muxSession) GrantKey() string      { return m.grantKey }

/* ── The agent's side ────────────────────────────────────────────────────── */

// DialOptions are what an agent needs to reach the control plane.
type DialOptions struct {
	// ControlURL is the control plane's https:// address.
	ControlURL string
	Credential string
	Hostname   string
	Version    string
	// TLS verifies the control plane. Nil means the system trust store.
	TLS *tls.Config
}

// DialQUIC connects over QUIC to port on the control plane's host and says
// hello. It fails fast when UDP is blocked, so the caller can fall back.
func DialQUIC(ctx context.Context, o DialOptions, port int) (Session, error) {
	u, err := url.Parse(o.ControlURL)
	if err != nil {
		return nil, err
	}
	tlsCfg := clientTLS(o.TLS, u.Hostname())
	tlsCfg.NextProtos = []string{ALPN}

	c, err := quic.DialAddr(ctx, net.JoinHostPort(u.Hostname(), strconv.Itoa(port)),
		tlsCfg, clientQUICConfig())
	if err != nil {
		return nil, err
	}
	s := quicSession{c: c}
	key, err := hello(ctx, s, o)
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	s.grantKey = key
	return s, nil
}

// hello says who this agent is and returns the grant key the control plane
// answers with.
func hello(ctx context.Context, s Session, o DialOptions) (string, error) {
	st, err := s.OpenStream(ctx)
	if err != nil {
		return "", err
	}
	defer st.Close()
	_ = st.SetDeadline(time.Now().Add(HeaderTimeout))
	if err := WriteJSON(st, Hello{Credential: o.Credential, Hostname: o.Hostname,
		Version: o.Version}); err != nil {
		return "", err
	}
	var a Answer
	if err := ReadJSON(st, &a); err != nil {
		return "", err
	}
	if !a.OK {
		return "", ErrRefused{Reason: a.Error}
	}
	return a.GrantKey, nil
}

// DialUpgrade connects through the control plane's HTTPS listener. It
// returns the QUIC port the control plane offers, or 0.
func DialUpgrade(ctx context.Context, o DialOptions) (Session, int, error) {
	u, err := url.Parse(o.ControlURL)
	if err != nil {
		return nil, 0, err
	}
	host := u.Host
	if u.Port() == "" {
		port := "443"
		if u.Scheme == "http" {
			port = "80"
		}
		host = net.JoinHostPort(u.Hostname(), port)
	}

	var d net.Dialer
	raw, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, 0, err
	}
	conn := raw
	if u.Scheme == "https" {
		tlsCfg := clientTLS(o.TLS, u.Hostname())
		// HTTP/1.1 only: an upgrade cannot happen inside HTTP/2.
		tlsCfg.NextProtos = []string{"http/1.1"}
		tc := tls.Client(raw, tlsCfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = raw.Close()
			return nil, 0, err
		}
		conn = tc
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	req, _ := http.NewRequest(http.MethodGet, (&url.URL{Scheme: u.Scheme, Host: u.Host,
		Path: UpgradePath}).String(), nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", UpgradeToken)
	req.Header.Set("Authorization", "Bearer "+o.Credential)
	req.Header.Set("Argus-Hostname", o.Hostname)
	req.Header.Set("Argus-Version", o.Version)
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, 0, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		_ = conn.Close()
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = conn.Close()
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, 0, ErrRefused{Reason: fmt.Sprintf("%d %s", resp.StatusCode, body)}
		}
		return nil, 0, fmt.Errorf("tunnel: control plane answered %d: %s", resp.StatusCode, body)
	}
	_ = conn.SetDeadline(time.Time{})
	quicPort, _ := strconv.Atoi(resp.Header.Get(QUICHeader))

	var c net.Conn = conn
	if br.Buffered() > 0 {
		c = bufferedConn{Conn: conn, r: br}
	}
	s, err := yamux.Client(c, yamuxConfig())
	if err != nil {
		_ = conn.Close()
		return nil, 0, err
	}
	return muxSession{s: s, conn: conn, grantKey: resp.Header.Get(GrantKeyHeader)}, quicPort, nil
}

func clientTLS(base *tls.Config, serverName string) *tls.Config {
	var c *tls.Config
	if base != nil {
		c = base.Clone()
	} else {
		c = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	if c.ServerName == "" {
		c.ServerName = serverName
	}
	return c
}

// IsRefused reports whether err is the control plane refusing this agent,
// as opposed to the network failing. A refusal will not cure itself by
// retrying quickly.
func IsRefused(err error) bool {
	var r ErrRefused
	return errors.As(err, &r)
}
