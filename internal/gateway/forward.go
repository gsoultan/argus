package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

/*
Channel forwarding.

Every forwarding mode Argus supports is the same shape: a channel opens on one
side of the gateway and has to be joined to a matching channel on the other,
with bytes copied between them until either end closes. Only the direction and
the trigger differ:

  - local (-L)     client opens direct-tcpip           → gateway dials from the target
  - remote (-R)    target accepts on a listener        → gateway opens forwarded-tcpip to the client
  - agent          target opens auth-agent@openssh.com → gateway opens the same to the client
  - X11            target opens x11                    → gateway opens x11 to the client

So there is one relay and four thin callers, rather than four hand-rolled
copies that would drift in their error handling and — more to the point — in
what they record.

## What is recorded, and what is not

A tunnel is not a terminal. Its contents are someone else's protocol, they can
be arbitrarily large, and capturing them would mean the gateway holding
database traffic in a session recording. Argus records the *fact* of the
connection instead: who, from where, to which host and port, when it opened and
closed, and how many bytes moved in each direction. That is what an
investigator actually asks of a tunnel, and it is honest about the rest — the
console says a forwarded connection's payload is not recorded rather than
implying the recording covers it.

Every one of these events goes to the audit chain, not just the session log,
because a tunnel out of a bastion is exactly the sort of thing that must be
tamper-evident.
*/

// Bounds on how much forwarding one session may have open at once.
//
// Both structures below are keyed or driven by what the client asks for, and an
// unbounded one is a way to exhaust the gateway from inside an authorised
// session: every -L channel costs a goroutine pair and a socket on the target,
// every -R costs a listener. The limits are generous enough that no legitimate
// workflow meets them — an operator tunnelling a handful of services will not
// notice — and low enough that a session cannot become a resource attack.
//
// Refusals say which limit was hit, so someone who genuinely needs more asks
// for it rather than diagnosing a mysterious failure.
const (
	// MaxLocalForwards caps concurrent -L channels per session.
	MaxLocalForwards = 32
	// MaxRemoteForwards caps -R listeners per session. Lower, because each one
	// binds a port on the target and is inbound.
	MaxRemoteForwards = 8
)

// ErrTooManyForwards is returned when a session is at its limit.
var ErrTooManyForwards = errors.New("too many forwards open for this session")

// forwardedBytes tracks one tunnel so its close event can state the volume.
type forwardedBytes struct {
	up   atomic.Int64
	down atomic.Int64
}

// relayChannels joins two channels and returns once both directions are done.
//
// Both halves are copied concurrently and each closes the far side's write
// direction when its source ends, so a peer that only ever reads still sees
// EOF. Failing to do that is how a forwarded connection hangs until the whole
// session is torn down.
func relayChannels(a, b ssh.Channel, n *forwardedBytes) {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		written, _ := io.Copy(b, a)
		if n != nil {
			n.up.Add(written)
		}
		_ = b.CloseWrite()
	}()
	go func() {
		defer wg.Done()
		written, _ := io.Copy(a, b)
		if n != nil {
			n.down.Add(written)
		}
		_ = a.CloseWrite()
	}()

	// Out-of-band data carries TCP urgent and exit status on these channels;
	// dropping it silently would be a quiet protocol violation.
	go func() { _, _ = io.Copy(io.Discard, a.Stderr()) }()
	go func() { _, _ = io.Copy(io.Discard, b.Stderr()) }()

	wg.Wait()
	_ = a.Close()
	_ = b.Close()
}

/* ── Local forwarding (-L) ───────────────────────────────────────────────── */

// directTCPIP is the payload of a direct-tcpip channel open.
// RFC 4254 §7.2.
type directTCPIP struct {
	DestHost   string
	DestPort   uint32
	OriginHost string
	OriginPort uint32
}

// handleDirectTCPIP joins a client's -L channel to a connection dialled from
// the target.
//
// The connection originates on the *target*, not on the gateway. That is the
// whole point: `-L` through Argus reaches what the target can reach, which is
// the behaviour an operator expects from a bastion and the one the network
// controls around the target were written for. Dialling from the gateway
// instead would silently grant the gateway's network position to every user.
func (s *Session) handleDirectTCPIP(newChan ssh.NewChannel) {
	var req directTCPIP
	if err := ssh.Unmarshal(newChan.ExtraData(), &req); err != nil {
		_ = newChan.Reject(ssh.ConnectionFailed, "malformed direct-tcpip request")
		return
	}
	dest := net.JoinHostPort(req.DestHost, strconv.FormatUint(uint64(req.DestPort), 10))

	// Counted before anything is allocated, and released however this returns.
	if n := s.localForwards.Add(1); n > MaxLocalForwards {
		s.localForwards.Add(-1)
		s.log.Warn("local forward refused: session at its limit",
			"dest", dest, "limit", MaxLocalForwards)
		s.auditForward("forward.refused", "warning",
			fmt.Sprintf("Refused a local forward to %s: this session already holds %d, "+
				"which is the per-session limit.", dest, MaxLocalForwards))
		_ = newChan.Reject(ssh.ResourceShortage,
			fmt.Sprintf("this session already has %d forwards open", MaxLocalForwards))
		return
	}
	defer s.localForwards.Add(-1)

	client := s.targetClient()
	if client == nil {
		_ = newChan.Reject(ssh.ConnectionFailed, "session is closing")
		return
	}

	// Dial before accepting. Accepting first would leave the client believing
	// the tunnel is up while the far end is refusing, and the failure would
	// surface as a silent hang instead of "connection refused".
	remote, err := client.Dial("tcp", dest)
	if err != nil {
		s.log.Warn("forward refused by target", "dest", dest, "error", err)
		s.auditForward("forward.refused", "warning",
			fmt.Sprintf("Local forward to %s refused by %s: %v", dest, s.Target.Hostname, err))
		_ = newChan.Reject(ssh.ConnectionFailed, err.Error())
		return
	}

	ch, reqs, err := newChan.Accept()
	if err != nil {
		_ = remote.Close()
		return
	}
	go ssh.DiscardRequests(reqs)

	s.log.Info("local forward opened", "dest", dest)
	s.auditForward("forward.open", "notice",
		fmt.Sprintf("Opened a local port forward to %s through %s. "+
			"The tunnel's contents are not recorded; its endpoints and volume are.",
			dest, s.Target.Hostname))

	started := time.Now()
	var n forwardedBytes
	// remote is a net.Conn, not a channel, so this half is copied here rather
	// than through relayChannels.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); w, _ := io.Copy(remote, ch); n.up.Add(w); closeWrite(remote) }()
	go func() { defer wg.Done(); w, _ := io.Copy(ch, remote); n.down.Add(w); _ = ch.CloseWrite() }()
	wg.Wait()
	_ = ch.Close()
	_ = remote.Close()

	s.auditForward("forward.close", "info",
		fmt.Sprintf("Closed the local port forward to %s after %s. %s.",
			dest, time.Since(started).Round(time.Second), describeVolume(&n)))
}

// closeWrite half-closes a connection when the type supports it, so the far
// end sees EOF rather than waiting for the whole session to end.
func closeWrite(c net.Conn) {
	type halfCloser interface{ CloseWrite() error }
	if hc, ok := c.(halfCloser); ok {
		_ = hc.CloseWrite()
		return
	}
	_ = c.Close()
}

/* ── Remote forwarding (-R) ──────────────────────────────────────────────── */

// tcpipForward is the payload of a tcpip-forward global request. RFC 4254 §7.1.
type tcpipForward struct {
	BindAddr string
	BindPort uint32
}

// forwardedTCPIP is the payload of a forwarded-tcpip channel open, which the
// gateway sends to the client for each accepted connection.
type forwardedTCPIP struct {
	ConnectedHost string
	ConnectedPort uint32
	OriginHost    string
	OriginPort    uint32
}

// remoteForwards tracks the listeners a session has opened on its target, so
// they can be cancelled individually and are all closed when the session ends.
type remoteForwards struct {
	mu sync.Mutex
	ln map[string]net.Listener
}

// put registers a listener, refusing once the session is at its limit.
//
// The map is keyed by an address the client chose, so without this a session
// could ask for listeners until the gateway ran out of descriptors.
func (r *remoteForwards) put(key string, l net.Listener) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ln == nil {
		r.ln = map[string]net.Listener{}
	}
	// Replacing an existing key is not a new listener, so it does not count
	// against the limit — otherwise re-binding the same port would leak budget.
	if _, exists := r.ln[key]; !exists && len(r.ln) >= MaxRemoteForwards {
		return ErrTooManyForwards
	}
	if old, exists := r.ln[key]; exists {
		_ = old.Close()
	}
	r.ln[key] = l
	return nil
}

func (r *remoteForwards) take(key string) net.Listener {
	r.mu.Lock()
	defer r.mu.Unlock()
	l := r.ln[key]
	delete(r.ln, key)
	return l
}

// closeAll releases every listener. A session that ends must not leave a port
// open on the target — that would outlive the authorisation that created it.
func (r *remoteForwards) closeAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, l := range r.ln {
		_ = l.Close()
		delete(r.ln, k)
	}
}

// startRemoteForward asks the target to listen and relays each accepted
// connection back to the client.
//
// Returns the bound port, which matters when the client asked for 0 and needs
// to be told what it got.
func (s *Session) startRemoteForward(req tcpipForward) (uint32, error) {
	client := s.targetClient()
	if client == nil {
		return 0, fmt.Errorf("session is closing")
	}
	addr := net.JoinHostPort(req.BindAddr, strconv.FormatUint(uint64(req.BindPort), 10))
	ln, err := client.Listen("tcp", addr)
	if err != nil {
		return 0, err
	}

	bound := req.BindPort
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
		bound = uint32(tcp.Port)
	}
	key := net.JoinHostPort(req.BindAddr, strconv.FormatUint(uint64(bound), 10))
	if err := s.remote.put(key, ln); err != nil {
		// Close the listener we just opened on the target rather than leaving a
		// port bound that nothing is tracking.
		_ = ln.Close()
		s.log.Warn("remote forward refused: session at its limit",
			"addr", key, "limit", MaxRemoteForwards)
		s.auditForward("forward.refused", "warning",
			fmt.Sprintf("Refused a remote forward on %s: this session already holds %d, "+
				"which is the per-session limit.", key, MaxRemoteForwards))
		return 0, err
	}

	s.log.Info("remote forward listening", "addr", key)
	s.auditForward("forward.listen", "warning",
		fmt.Sprintf("Opened a remote port forward: %s is listening on %s and relaying to the client. "+
			"This is an inbound path into the session that outlives no longer than it does.",
			s.Target.Hostname, key))

	go s.acceptRemoteForward(ln, req.BindAddr, bound)
	return bound, nil
}

func (s *Session) acceptRemoteForward(ln net.Listener, bindAddr string, bindPort uint32) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed, by cancel-tcpip-forward or session end
		}
		go s.serveRemoteForward(conn, bindAddr, bindPort)
	}
}

func (s *Session) serveRemoteForward(conn net.Conn, bindAddr string, bindPort uint32) {
	defer conn.Close()

	originHost, originPortStr, _ := net.SplitHostPort(conn.RemoteAddr().String())
	originPort, _ := strconv.ParseUint(originPortStr, 10, 32)

	clientConn := s.clientConn()
	if clientConn == nil {
		return
	}
	ch, reqs, err := clientConn.OpenChannel("forwarded-tcpip", ssh.Marshal(forwardedTCPIP{
		ConnectedHost: bindAddr,
		ConnectedPort: bindPort,
		OriginHost:    originHost,
		OriginPort:    uint32(originPort),
	}))
	if err != nil {
		s.log.Warn("client refused forwarded-tcpip", "error", err)
		return
	}
	go ssh.DiscardRequests(reqs)

	started := time.Now()
	var n forwardedBytes
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); w, _ := io.Copy(conn, ch); n.down.Add(w); closeWrite(conn) }()
	go func() { defer wg.Done(); w, _ := io.Copy(ch, conn); n.up.Add(w); _ = ch.CloseWrite() }()
	wg.Wait()
	_ = ch.Close()

	s.auditForward("forward.close", "info",
		fmt.Sprintf("Closed a remote-forwarded connection from %s after %s. %s.",
			conn.RemoteAddr(), time.Since(started).Round(time.Second), describeVolume(&n)))
}

/* ── Agent and X11 ───────────────────────────────────────────────────────── */

// relayToClient joins a channel the target opened to a matching one on the
// client. Agent and X11 forwarding are both exactly this.
//
// The gateway never interprets what crosses: for agent forwarding in
// particular, it must not be able to — the whole objection to agent forwarding
// is that whoever holds the gateway can sign with the user's keys, and a
// gateway that parsed the agent protocol would be positioned to do it.
func (s *Session) relayToClient(newChan ssh.NewChannel, kind, describe string) {
	clientConn := s.clientConn()
	if clientConn == nil {
		_ = newChan.Reject(ssh.ConnectionFailed, "session is closing")
		return
	}
	clientCh, clientReqs, err := clientConn.OpenChannel(kind, newChan.ExtraData())
	if err != nil {
		_ = newChan.Reject(ssh.ConnectionFailed, "client refused "+kind)
		return
	}
	go ssh.DiscardRequests(clientReqs)

	targetCh, targetReqs, err := newChan.Accept()
	if err != nil {
		_ = clientCh.Close()
		return
	}
	go ssh.DiscardRequests(targetReqs)

	s.log.Info("channel forwarded to client", "type", kind)
	s.auditForward("forward.open", "warning", describe)

	started := time.Now()
	var n forwardedBytes
	relayChannels(targetCh, clientCh, &n)

	s.auditForward("forward.close", "info",
		fmt.Sprintf("Closed a forwarded %s channel after %s. %s.",
			kind, time.Since(started).Round(time.Second), describeVolume(&n)))
}

// handleTargetChannel routes a channel the *target* opened back to the client.
//
// A target opening a channel is unusual and worth being strict about: these are
// the only three types Argus will relay, and anything else is refused rather
// than passed through on the assumption it is harmless.
func (s *Session) handleTargetChannel(newChan ssh.NewChannel) {
	p := s.policy()
	switch newChan.ChannelType() {
	case "auth-agent@openssh.com":
		if !p.AllowAgentForward {
			_ = newChan.Reject(ssh.Prohibited, "agent forwarding is not permitted by policy")
			return
		}
		s.relayToClient(newChan, "auth-agent@openssh.com",
			fmt.Sprintf("Forwarded the SSH agent to %s. For the life of this channel, "+
				"anyone with root on the gateway or the target can sign challenges with "+
				"this user's keys.", s.Target.Hostname))
	case "x11":
		if !p.AllowX11Forward {
			_ = newChan.Reject(ssh.Prohibited, "X11 forwarding is not permitted by policy")
			return
		}
		s.relayToClient(newChan, "x11",
			fmt.Sprintf("Forwarded an X11 channel from %s.", s.Target.Hostname))
	default:
		s.log.Info("target-opened channel refused", "type", newChan.ChannelType())
		_ = newChan.Reject(ssh.Prohibited,
			newChan.ChannelType()+" is not permitted by policy")
	}
}

/* ── Shared ──────────────────────────────────────────────────────────────── */

// targetClient returns the connection to the target, or nil once closed.
func (s *Session) targetClient() *ssh.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	return s.client
}

// clientConn returns the user's connection, or nil if the session has none.
func (s *Session) clientConn() ssh.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	return s.userConn
}

func describeVolume(n *forwardedBytes) string {
	return fmt.Sprintf("%s out, %s back", humanBytes(n.up.Load()), humanBytes(n.down.Load()))
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

// auditForward writes a tunnel event to the audit chain.
//
// The chain rather than only the session log: a forward out of a bastion is
// precisely the thing someone will later need to prove happened, and the
// recording is not tamper-evident in the way the audit log is.
func (s *Session) auditForward(action, severity, detail string) {
	if s.srv == nil || s.srv.cfg.Reporter == nil || !s.srv.cfg.Reporter.Enabled() {
		return
	}
	s.srv.report(func(ctx context.Context) {
		s.srv.cfg.Reporter.Audit(ctx, map[string]any{
			"action":     action,
			"severity":   severity,
			"actorEmail": s.User,
			"target":     s.Target.Hostname,
			"detail":     detail,
		})
	})
}
