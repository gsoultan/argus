package control

import (
	"context"
	"crypto/ed25519"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/quic-go/quic-go"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
The control plane's end of the agent tunnels.

Each enrolled agent holds one outbound connection here, so a managed server
needs no inbound port. The hub admits a connection only for a live per-host
credential -- never the shared reporter token: a tunnel is a way into one
host, and a credential that speaks for the whole fleet must not be able to
open one -- and keeps at most one per credential, the newest.

Every ping (tunnelPingEvery) does three jobs in one round trip each: it
measures the path, proves the agent is still answering, and re-checks the
credential in the database. A revocation on another replica therefore ends a
tunnel held here within one ping, without that replica having to reach this
one.
*/

// tunnelPingEvery is how often each tunnel is measured and re-checked.
const tunnelPingEvery = 30 * time.Second

// tunnelFreshFor is how long a tunnel's last ping still counts. Three missed
// pings: a replica that died without clearing its rows stops being believed.
const tunnelFreshFor = 3 * tunnelPingEvery

// TunnelHub holds the live tunnels on this replica.
type TunnelHub struct {
	store *Store
	log   *slog.Logger
	// Replica names this process in the database, so the console can say
	// which replica holds a tunnel.
	Replica string
	// QUICPort is advertised to agents on the HTTPS fallback; zero when
	// this replica does not listen for QUIC.
	QUICPort int
	// GrantKey signs session grants; see tunnel.Grant. Nil when there is
	// no signing secret, and then no session opens through a tunnel.
	GrantKey ed25519.PrivateKey
	// OnConnect is told of every admitted tunnel, after it can be used.
	// Called on the admitting goroutine; it must not block. Nil is none.
	OnConnect func(ctx context.Context, c ServerConnection)

	mu     sync.Mutex
	byCred map[string]*liveTunnel
	byHost map[string]*liveTunnel
	closed bool

	// grantKeys caches the per-organization keys derived from GrantKey.
	gmu       sync.Mutex
	grantKeys map[string]ed25519.PrivateKey
}

type liveTunnel struct {
	// tenant is the organization the agent enrolled into. Every lookup is by
	// (tenant, host) or (tenant, credential): a host name is unique within an
	// organization, and a console in one organization must never reach a
	// tunnel in another even when the two hold a host by the same name.
	tenant       string
	credID, host string
	sess         tunnel.Session
	since        time.Time
}

// tunnelKey is how the hub's maps are keyed: the organization first.
func tunnelKey(tenant, name string) string { return tenant + "\x00" + strings.ToLower(name) }

// ServerConnection is an agent's tunnel being admitted.
type ServerConnection struct {
	Host string
	// First is the credential's first connection since it enrolled.
	First bool
	// Replaced is set when it took over from a connection still open here,
	// which is the agent redialling rather than coming back.
	Replaced   bool
	Transport  string
	RemoteAddr string
}

// remoteHost is an address without its port.
func remoteHost(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	if h, _, err := net.SplitHostPort(addr.String()); err == nil {
		return h
	}
	return addr.String()
}

// NewTunnelHub returns a hub writing tunnel state through store.
func NewTunnelHub(store *Store, log *slog.Logger, replica string) *TunnelHub {
	if log == nil {
		log = slog.Default()
	}
	return &TunnelHub{store: store, log: log, Replica: replica,
		byCred: map[string]*liveTunnel{}, byHost: map[string]*liveTunnel{}}
}

// admit makes sess the tunnel for a credential, replacing any older one.
func (h *TunnelHub) admit(ctx context.Context, credID, host string, sess tunnel.Session) {
	// Microseconds: this is compared against the stored value, which
	// TIMESTAMPTZ keeps at that precision.
	t := &liveTunnel{tenant: tenantIDOf(ctx), credID: credID, host: strings.ToLower(host),
		sess: sess, since: time.Now().UTC().Truncate(time.Microsecond)}

	h.mu.Lock()
	closed := h.closed
	h.mu.Unlock()
	if closed {
		_ = sess.Close()
		return
	}
	// The database is told before the tunnel can be found here. Published
	// first, the tunnel could be used, and the old one's disconnection
	// clear the row, while the console still said the host was offline.
	first, err := h.store.tunnelConnected(t.ctx(), credID,
		string(sess.Transport()), h.Replica, t.since)
	if err != nil {
		h.log.Error("tunnel: recording the connection failed", "host", host, "error", err)
	}

	h.mu.Lock()
	old := h.byCred[tunnelKey(t.tenant, credID)]
	if h.closed || (old != nil && old.since.After(t.since)) {
		// Closing, or a newer connection from the same agent was admitted
		// while this one was being recorded. The row is cleared only if it
		// is still this connection's.
		h.mu.Unlock()
		_ = sess.Close()
		_ = h.store.tunnelDisconnected(t.ctx(), credID, t.since)
		return
	}
	h.byCred[tunnelKey(t.tenant, credID)] = t
	h.byHost[tunnelKey(t.tenant, t.host)] = t
	h.mu.Unlock()

	if old != nil {
		_ = old.sess.Close()
	}
	h.log.Info("tunnel: agent connected", "host", host,
		"transport", sess.Transport(), "remote", sess.RemoteAddr())
	if h.OnConnect != nil {
		h.OnConnect(t.ctx(), ServerConnection{Host: t.host, First: first,
			Transport: string(sess.Transport()), RemoteAddr: remoteHost(sess.RemoteAddr()),
			Replaced: old != nil})
	}

	go h.refuseAgentStreams(t)
	go h.watch(t)
}

// refuseAgentStreams closes anything the agent opens. The control plane
// opens streams; the agent only answers them.
func (h *TunnelHub) refuseAgentStreams(t *liveTunnel) {
	for {
		st, err := t.sess.AcceptStream(context.Background())
		if err != nil {
			return
		}
		_ = st.Close()
	}
}

// watch pings until the session ends, then clears it.
func (h *TunnelHub) watch(t *liveTunnel) {
	tick := time.NewTicker(tunnelPingEvery)
	defer tick.Stop()
	h.ping(t)
	for {
		select {
		case <-t.sess.Done():
			h.drop(t, "the connection ended")
			return
		case <-tick.C:
			h.ping(t)
		}
	}
}

// ctx is a context scoped to the tunnel's organization, for the store calls
// the hub makes on its own behalf.
func (t *liveTunnel) ctx() context.Context {
	return WithTenant(context.Background(), &Tenant{ID: t.tenant})
}

func (h *TunnelHub) ping(t *liveTunnel) {
	ctx, cancel := context.WithTimeout(t.ctx(), 10*time.Second)
	defer cancel()
	rtt, err := tunnel.Ping(ctx, t.sess)
	if err != nil {
		h.log.Warn("tunnel: agent did not answer a ping; closing", "host", t.host, "error", err)
		_ = t.sess.Close()
		return
	}
	still, err := h.store.tunnelSeen(ctx, t.credID, t.since, rtt)
	if err != nil {
		// The database is unreachable: keep the tunnel. Closing every tunnel
		// in the fleet because Postgres blinked would take every session
		// down with it; the credential is re-checked at the next ping.
		h.log.Error("tunnel: re-checking the credential failed", "host", t.host, "error", err)
		return
	}
	if !still {
		h.log.Warn("tunnel: closing; the credential was revoked or the agent "+
			"reconnected elsewhere", "host", t.host)
		_ = t.sess.Close()
	}
}

func (h *TunnelHub) drop(t *liveTunnel, why string) {
	h.mu.Lock()
	if h.byCred[tunnelKey(t.tenant, t.credID)] == t {
		delete(h.byCred, tunnelKey(t.tenant, t.credID))
	}
	if h.byHost[tunnelKey(t.tenant, t.host)] == t {
		delete(h.byHost, tunnelKey(t.tenant, t.host))
	}
	h.mu.Unlock()
	if err := h.store.tunnelDisconnected(t.ctx(), t.credID, t.since); err != nil {
		h.log.Error("tunnel: recording the disconnection failed", "host", t.host, "error", err)
	}
	h.log.Info("tunnel: agent disconnected", "host", t.host, "why", why,
		"after", time.Since(t.since).Round(time.Second))
}

// Revoked closes the tunnel of a credential this replica just revoked,
// without waiting for the next ping.
func (h *TunnelHub) Revoked(ctx context.Context, credID string) {
	h.mu.Lock()
	t := h.byCred[tunnelKey(tenantIDOf(ctx), credID)]
	h.mu.Unlock()
	if t != nil {
		_ = t.sess.Close()
	}
}

// closeTenant ends every tunnel an organization holds on this replica, when
// the organization is disabled.
func (h *TunnelHub) closeTenant(tenant string) {
	h.mu.Lock()
	var all []*liveTunnel
	for _, t := range h.byCred {
		if t.tenant == tenant {
			all = append(all, t)
		}
	}
	h.mu.Unlock()
	for _, t := range all {
		_ = t.sess.Close()
	}
}

// ErrNoTunnel means the host has no tunnel on this replica.
var ErrNoTunnel = errors.New("the agent on this host is not connected")

// Open opens a stream to a service on host's agent.
func (h *TunnelHub) Open(ctx context.Context, host, service string) (tunnel.Stream, error) {
	h.mu.Lock()
	t := h.byHost[tunnelKey(tenantIDOf(ctx), host)]
	h.mu.Unlock()
	if t == nil {
		return nil, ErrNoTunnel
	}
	return tunnel.Open(ctx, t.sess, service)
}

// Close ends every tunnel, for shutdown. Agents reconnect to whichever
// replica answers next.
func (h *TunnelHub) Close() {
	h.mu.Lock()
	h.closed = true
	all := make([]*liveTunnel, 0, len(h.byCred))
	for _, t := range h.byCred {
		all = append(all, t)
	}
	h.mu.Unlock()
	for _, t := range all {
		_ = t.sess.Close()
	}
}

/* ── Admission ───────────────────────────────────────────────────────────── */

// credentialFor resolves an agent credential. Only the per-host kind: the
// shared reporter token never opens a tunnel.
func (a *API) credentialFor(ctx context.Context, cred string) (id, host string, ok bool) {
	if !strings.HasPrefix(cred, agentCredentialPrefix) {
		return "", "", false
	}
	// Straight to the database rather than through the report cache: a
	// tunnel lasts for hours, so admitting one on a cached answer up to 30s
	// stale is not worth the round trip it saves.
	id, host, err := a.store.agentCredentialHost(ctx, hashSecret(cred))
	if err != nil {
		return "", "", false
	}
	return id, host, true
}

// handleTunnelUpgrade is the HTTPS fallback: an HTTP/1.1 upgrade to yamux.
func (a *API) handleTunnelUpgrade(w http.ResponseWriter, r *http.Request) {
	if a.Tunnels == nil {
		writeErr(w, http.StatusNotFound, "this control plane does not accept agent tunnels")
		return
	}
	ws := tunnel.IsWebSocketUpgrade(r)
	if !ws && !strings.EqualFold(r.Header.Get("Upgrade"), tunnel.UpgradeToken) {
		writeErr(w, http.StatusBadRequest, "expected Upgrade: "+tunnel.UpgradeToken+" or websocket")
		return
	}
	id, host, ok := a.credentialFor(r.Context(), bearer(r))
	if !ok {
		writeErr(w, http.StatusUnauthorized,
			"a tunnel needs this host's own enrolled credential")
		return
	}
	if claimed := r.Header.Get("Argus-Hostname"); claimed != "" && !strings.EqualFold(claimed, host) {
		writeErr(w, http.StatusForbidden,
			"this agent is enrolled as "+host+" and may only connect as that host")
		return
	}

	if ws {
		// Through a proxy that forwards WebSockets only; see
		// tunnel/transport_ws.go. No QUIC port: the proxy will not carry it.
		if k := a.Tunnels.grantKeyPublic(r.Context()); k != "" {
			w.Header().Set(tunnel.GrantKeyHeader, k)
		}
		sess, err := tunnel.AcceptWebSocket(w, r)
		if err != nil {
			a.log.Warn("tunnel: WebSocket handshake failed", "host", host, "error", err)
			return
		}
		a.Tunnels.admit(r.Context(), id, host, sess)
		return
	}

	conn, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		a.log.Error("tunnel: cannot take over the connection", "error", err)
		return
	}
	// The server's read and write deadlines do not apply to a tunnel that
	// lives for hours; yamux keep-alives detect a dead peer instead.
	_ = conn.SetDeadline(time.Time{})
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Connection: Upgrade\r\nUpgrade: " + tunnel.UpgradeToken + "\r\n"
	if a.Tunnels.QUICPort > 0 {
		resp += tunnel.QUICHeader + ": " + strconv.Itoa(a.Tunnels.QUICPort) + "\r\n"
	}
	if k := a.Tunnels.grantKeyPublic(r.Context()); k != "" {
		resp += tunnel.GrantKeyHeader + ": " + k + "\r\n"
	}
	if _, err := buf.WriteString(resp + "\r\n"); err != nil || buf.Flush() != nil {
		_ = conn.Close()
		return
	}
	sess, err := tunnel.NewServerMux(conn, buf.Reader)
	if err != nil {
		_ = conn.Close()
		return
	}
	a.Tunnels.admit(r.Context(), id, host, sess)
}

// ServeQUIC accepts agent tunnels on ln until ctx ends.
func (a *API) ServeQUIC(ctx context.Context, ln *quic.Listener) {
	for {
		c, err := ln.Accept(ctx)
		if err != nil {
			return
		}
		go a.admitQUIC(ctx, tunnel.NewQUICSession(c))
	}
}

// admitQUIC reads the agent's hello and admits or refuses the connection.
// A connection that does not say hello in time is closed: an unauthenticated
// connection must cost this process as little and for as short a time as
// possible.
func (a *API) admitQUIC(ctx context.Context, sess tunnel.Session) {
	hctx, cancel := context.WithTimeout(ctx, tunnel.HeaderTimeout)
	defer cancel()
	st, err := sess.AcceptStream(hctx)
	if err != nil {
		_ = sess.Close()
		return
	}
	_ = st.SetDeadline(time.Now().Add(tunnel.HeaderTimeout))
	var hello tunnel.Hello
	if err := tunnel.ReadJSON(st, &hello); err != nil {
		_ = sess.Close()
		return
	}
	refuse := func(why string) {
		_ = tunnel.WriteJSON(st, tunnel.Answer{Error: why})
		_ = st.Close()
		// Let the answer reach the agent before the connection goes.
		time.AfterFunc(time.Second, func() { _ = sess.Close() })
	}
	// No HTTP request, so no scope middleware: the organization is read off
	// the credential's prefix here and checked against the registry the
	// same way.
	tctx, ok := a.scopeSecret(hctx, hello.Credential)
	if !ok {
		refuse("a tunnel needs this host's own enrolled credential")
		return
	}
	id, host, ok := a.credentialFor(tctx, hello.Credential)
	if !ok {
		refuse("a tunnel needs this host's own enrolled credential")
		return
	}
	if hello.Hostname != "" && !strings.EqualFold(hello.Hostname, host) {
		refuse("this agent is enrolled as " + host + " and may only connect as that host")
		return
	}
	if err := tunnel.WriteJSON(st, tunnel.Answer{OK: true,
		GrantKey: a.Tunnels.grantKeyPublic(tctx)}); err != nil {
		_ = sess.Close()
		return
	}
	_ = st.Close()
	a.Tunnels.admit(tctx, id, host, sess)
}

/* ── Store ───────────────────────────────────────────────────────────────── */

// tunnelConnected records a connection, and reports whether it is the
// credential's first.
func (s *Store) tunnelConnected(ctx context.Context, credID, transport, replica string,
	since time.Time) (first bool, err error) {
	// first_connected_at becomes $4 only when it was unset, so it equals $4
	// afterwards only for the first connection; the row lock the update
	// takes means two replicas admitting the agent at once cannot both see
	// that.
	err = s.db(ctx).QueryRow(ctx, `
		UPDATE agent_credentials SET
			tunnel_transport = $2, tunnel_replica = $3,
			tunnel_connected_at = $4, tunnel_seen_at = now(), tunnel_rtt_us = NULL,
			first_connected_at = COALESCE(first_connected_at, $4)
		 WHERE id::text = $1
		   -- Two connections admitted at once on this replica: the newer
		   -- one is kept, whichever is recorded last.
		   AND (tunnel_replica IS DISTINCT FROM $3 OR tunnel_connected_at IS NULL
		        OR tunnel_connected_at < $4)
		RETURNING first_connected_at = $4`, credID, transport, replica, since).Scan(&first)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return first, err
}

// tunnelSeen records a ping and reports whether the tunnel should stay: the
// credential is live and this is still the connection the database knows.
func (s *Store) tunnelSeen(ctx context.Context, credID string, since time.Time,
	rtt time.Duration) (bool, error) {
	var one int
	err := s.db(ctx).QueryRow(ctx, `
		UPDATE agent_credentials SET tunnel_seen_at = now(), tunnel_rtt_us = $3
		 WHERE id::text = $1 AND tunnel_connected_at = $2 AND revoked_at IS NULL
		RETURNING 1`, credID, since, min(rtt.Microseconds(), 1<<31-1)).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// tunnelDisconnected clears the state, unless a newer connection has
// already replaced it.
func (s *Store) tunnelDisconnected(ctx context.Context, credID string, since time.Time) error {
	_, err := s.db(ctx).Exec(ctx, `
		UPDATE agent_credentials SET
			tunnel_transport = NULL, tunnel_replica = NULL,
			tunnel_connected_at = NULL, tunnel_seen_at = NULL, tunnel_rtt_us = NULL
		 WHERE id::text = $1 AND tunnel_connected_at = $2`, credID, since)
	return err
}

// count is how many agents hold a tunnel to this replica.
func (h *TunnelHub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.byCred)
}

// TunnelState is what the console is told about one agent's tunnel.
type TunnelState struct {
	Connected   bool       `json:"connected"`
	Transport   string     `json:"transport,omitempty"`
	Replica     string     `json:"replica,omitempty"`
	ConnectedAt *time.Time `json:"connectedAt,omitempty"`
	SeenAt      *time.Time `json:"seenAt,omitempty"`
	// RTTMillis is the last measured round trip through the tunnel.
	RTTMillis *float64 `json:"rttMillis,omitempty"`
}

func tunnelStateFrom(transport, replica *string, connectedAt, seenAt *time.Time,
	rttUs *int32, now time.Time) TunnelState {
	var st TunnelState
	if transport == nil || seenAt == nil {
		return st
	}
	if now.Sub(*seenAt) > tunnelFreshFor {
		// Held by a replica that has not reported for three pings. Said as
		// down: the console must not call a connection live on the word of
		// a process that may no longer exist.
		return st
	}
	st.Connected = true
	st.Transport = *transport
	if replica != nil {
		st.Replica = *replica
	}
	st.ConnectedAt, st.SeenAt = connectedAt, seenAt
	if rttUs != nil {
		ms := float64(*rttUs) / 1000
		st.RTTMillis = &ms
	}
	return st
}
