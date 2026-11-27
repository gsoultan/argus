package control

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/jackc/pgx/v5"

	"github.com/gsoultan/argus/internal/vault"
)

/*
Secrets servers that dial argus-control (mode "tunnel").

An admin adds the server in the console and gets a one-time token, good for an
hour. `argus-secret enroll` sends it to POST /api/v1/secrets/enroll and gets
the server's own credential back, with this organization's management key.
From then on argus-secret holds GET /api/v1/secrets/tunnel open, upgraded to
yamux, and argus-control opens a stream per request: its own signed
management calls, and agents' and apps' reads, relayed from
/api/v1/secrets/relay/{id}/v1/... unchanged. The value in a read is sealed to
the reader's key, so argus-control relays bytes it cannot open, as it does for
the console.

A tunnel lands on one replica. A replica without it answers that the server is
connected elsewhere; the minute's sync on the replica that has it carries on.
*/

const (
	secretsEnrollTTL   = time.Hour
	maxSecretsRelayIn  = 64 << 10
	secretsRelayPrefix = "/api/v1/secrets/relay/"
)

// secretsEnrollView is a new tunnel server with its token, shown once.
type secretsEnrollView struct {
	SecretsServer
	EnrollToken string `json:"enrollToken"`
}

/* ── Store ──────────────────────────────────────────────────────────────── */

// secretsEnrollToken gives a tunnel server a new one-time token. Its current
// credential, if any, keeps working until the token is used.
func (s *Store) secretsEnrollToken(ctx context.Context, id string) (string, error) {
	token, err := newSecret(vault.EnrollTokenPrefix + secretPrefixFor(tenantIDOf(ctx)))
	if err != nil {
		return "", err
	}
	tag, err := s.db(ctx).Exec(ctx, `UPDATE secrets_servers SET enroll_token_hash = $2,
		enroll_expires_at = now() + $3::interval, updated_at = now() WHERE id = $1 AND mode = 'tunnel'`,
		id, hashSecret(token), fmt.Sprintf("%d seconds", int(secretsEnrollTTL.Seconds())))
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return "", ErrNoSecretsServer
	}
	return token, nil
}

// secretsEnroll spends a token: the server's credential becomes cred.
func (s *Store) secretsEnroll(ctx context.Context, token, cred, version string) (SecretsServer, error) {
	srv, err := scanSecretsServer(s.db(ctx).QueryRow(ctx, `UPDATE secrets_servers SET credential_hash = $2,
		enroll_token_hash = NULL, enroll_expires_at = NULL, version = $3, updated_at = now()
		WHERE enroll_token_hash = $1 AND enroll_expires_at > now() AND mode = 'tunnel'
		RETURNING `+secretsServerColumns, hashSecret(token), hashSecret(cred), version))
	if errors.Is(err, pgx.ErrNoRows) {
		return srv, ErrNoSecretsServer
	}
	return srv, err
}

func (s *Store) secretsServerByCredential(ctx context.Context, cred string) (SecretsServer, error) {
	srv, err := scanSecretsServer(s.db(ctx).QueryRow(ctx, `SELECT `+secretsServerColumns+` FROM secrets_servers
		WHERE credential_hash = $1 AND mode = 'tunnel'`, hashSecret(cred)))
	if errors.Is(err, pgx.ErrNoRows) {
		return srv, ErrNoSecretsServer
	}
	return srv, err
}

func (s *Store) secretsTunnelSeen(ctx context.Context, id string) error {
	_, err := s.db(ctx).Exec(ctx, `UPDATE secrets_servers SET tunnel_seen_at = now() WHERE id = $1`, id)
	return err
}

// secretsRelayCaller is whether cred is a live agent credential or app token
// in this organization: what a relayed read may carry. argus-secret decides
// the read; this only keeps junk off the tunnel.
func (s *Store) secretsRelayCaller(ctx context.Context, cred string) bool {
	var ok bool
	switch {
	case strings.HasPrefix(cred, agentCredentialPrefix):
		_ = s.db(ctx).QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_credentials
			WHERE secret_hash = $1 AND revoked_at IS NULL)`, hashSecret(cred)).Scan(&ok)
	case strings.HasPrefix(cred, vault.AppTokenPrefix):
		_ = s.db(ctx).QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM secret_apps
			WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > now())`, hashSecret(cred)).Scan(&ok)
	}
	return ok
}

/* ── Tunnels held by this replica ───────────────────────────────────────── */

func secretsTunnelKey(ctx context.Context, id string) string { return tenantIDOf(ctx) + "/" + id }

func (a *API) secretsTunnelOf(ctx context.Context, id string) *yamux.Session {
	v, ok := a.secretsTunnels.Load(secretsTunnelKey(ctx, id))
	if !ok {
		return nil
	}
	sess := v.(*yamux.Session)
	if sess.IsClosed() {
		a.secretsTunnels.CompareAndDelete(secretsTunnelKey(ctx, id), sess)
		return nil
	}
	return sess
}

// secretsTransport is how argus-control reaches srv: over its tunnel, or at
// its address.
func (a *API) secretsTransport(ctx context.Context, srv SecretsServer) (*http.Client, string, error) {
	if srv.Mode != secretsTunnel {
		cl, err := secretsHTTPClient(srv.TLSPin)
		return cl, srv.URL, err
	}
	sess := a.secretsTunnelOf(ctx, srv.ID)
	if sess == nil {
		if !srv.Enrolled {
			return nil, "", ErrInvalidSecrets{srv.Name + " has not enrolled yet: run argus-secret enroll with its token"}
		}
		return nil, "", fmt.Errorf("%s is not connected to this argus-control", srv.Name)
	}
	tr := &http.Transport{
		DialContext:           func(context.Context, string, string) (net.Conn, error) { return sess.Open() },
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: secretsTimeout,
	}
	return &http.Client{Transport: tr, Timeout: secretsTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, "http://argus-secret", nil
}

/* ── Routes ─────────────────────────────────────────────────────────────── */

func (a *API) secretsTunnelRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST "+vault.EnrollPath, a.limitLogin(a.postSecretsEnroll))
	mux.HandleFunc("GET "+vault.TunnelPath, a.getSecretsTunnel)
	mux.HandleFunc("POST /api/v1/secrets/servers/{id}/enroll-token", a.postSecretsEnrollToken)
	mux.HandleFunc("POST "+secretsRelayPrefix+"{id}/v1/read", a.relaySecretsRead)
	mux.HandleFunc("GET "+secretsRelayPrefix+"{id}/v1/list", a.relaySecretsRead)
	mux.HandleFunc("GET "+secretsRelayPrefix+"{id}/v1/health", a.relaySecretsRead)
}

// postSecretsEnrollToken gives a tunnel server a fresh token: it was never
// used in time, or the server is being rebuilt.
func (a *API) postSecretsEnrollToken(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.secretsAdmin(w, r)
	if !ok {
		return
	}
	srv, err := a.store.SecretsServer(r.Context(), r.PathValue("id"))
	if err != nil {
		a.secretsErr(w, "enrolment token", err)
		return
	}
	if srv.Mode != secretsTunnel {
		a.secretsErr(w, "enrolment token", ErrInvalidSecrets{srv.Name + " is reached at its address; only a tunnel server enrolls"})
		return
	}
	token, err := a.store.secretsEnrollToken(r.Context(), srv.ID)
	if err != nil {
		a.secretsErr(w, "enrolment token", err)
		return
	}
	srv, _ = a.store.SecretsServer(r.Context(), srv.ID)
	a.audit(r, "secrets.server.enroll_token", actor, "secrets server "+srv.Name,
		fmt.Sprintf("Made a new enrolment token for %s, good for an hour. Its current credential works until the token is used.", srv.Name))
	writeJSON(w, http.StatusOK, secretsEnrollView{SecretsServer: srv, EnrollToken: token})
}

// postSecretsEnroll is argus-secret enroll: a token for a credential.
func (a *API) postSecretsEnroll(w http.ResponseWriter, r *http.Request) {
	var in vault.EnrollRequest
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !strings.HasPrefix(in.Token, vault.EnrollTokenPrefix) || len(in.Version) > 64 {
		a.RecordAuthFailure(r)
		writeErr(w, http.StatusUnauthorized, "that is not an enrolment token, or it was used or has expired")
		return
	}
	cred, err := newSecret(vault.ServerCredPrefix + secretPrefixFor(tenantIDOf(r.Context())))
	if err != nil {
		a.fail(w, "enrol secrets server", err)
		return
	}
	srv, err := a.store.secretsEnroll(r.Context(), in.Token, cred, in.Version)
	if errors.Is(err, ErrNoSecretsServer) {
		a.RecordAuthFailure(r)
		writeErr(w, http.StatusUnauthorized, "that is not an enrolment token, or it was used or has expired")
		return
	}
	if err != nil {
		a.fail(w, "enrol secrets server", err)
		return
	}
	key, err := a.SecretsPublicKey(r.Context())
	if err != nil {
		a.fail(w, "enrol secrets server", err)
		return
	}
	// A server enrolled again is a new connection: the old one, if it is
	// still up, holds a credential that no longer works.
	if old := a.secretsTunnelOf(r.Context(), srv.ID); old != nil {
		_ = old.Close()
	}
	a.audit(r, "secrets.server.enroll", "argus-secret", "secrets server "+srv.Name,
		fmt.Sprintf("Secrets server %s enrolled from %s.", srv.Name, a.clientAddr(r)))
	writeJSON(w, http.StatusOK, vault.EnrollResponse{ServerID: srv.ID, Org: tenantIDOf(r.Context()), ControlKey: key, Credential: cred})
}

// bufferedConn is a hijacked connection with what the server already read.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// getSecretsTunnel takes a tunnel server's connection and holds it.
func (a *API) getSecretsTunnel(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), vault.TunnelUpgrade) {
		writeErr(w, http.StatusBadRequest, "expected Upgrade: "+vault.TunnelUpgrade)
		return
	}
	cred := bearer(r)
	if !strings.HasPrefix(cred, vault.ServerCredPrefix) {
		writeErr(w, http.StatusUnauthorized, "a tunnel needs the credential argus-secret enroll saved")
		return
	}
	srv, err := a.store.secretsServerByCredential(r.Context(), cred)
	if err != nil {
		a.RecordAuthFailure(r)
		writeErr(w, http.StatusUnauthorized, "a tunnel needs the credential argus-secret enroll saved")
		return
	}
	conn, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		a.log.Error("secrets tunnel: cannot take over the connection", "error", err)
		return
	}
	_ = conn.SetDeadline(time.Time{})
	if _, err := buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " +
		vault.TunnelUpgrade + "\r\n\r\n"); err != nil || buf.Flush() != nil {
		_ = conn.Close()
		return
	}
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	// argus-control opens every stream; argus-secret never needs to.
	cfg.AcceptBacklog = 1
	sess, err := yamux.Server(bufferedConn{Conn: conn, r: buf.Reader}, cfg)
	if err != nil {
		_ = conn.Close()
		return
	}
	ctx := context.WithoutCancel(r.Context())
	key := secretsTunnelKey(ctx, srv.ID)
	if err := a.store.secretsTunnelSeen(ctx, srv.ID); err != nil {
		a.log.Warn("secrets tunnel: cannot record contact", "error", err)
	}
	// Counted before the tunnel is visible, so whoever sees it connected
	// can wait for the sync it starts.
	a.secretsBusy.Add(1)
	if old, loaded := a.secretsTunnels.Swap(key, sess); loaded {
		_ = old.(*yamux.Session).Close()
	}
	a.log.Info("secrets tunnel open", "server", srv.Name, "from", a.clientAddr(r))
	go func() {
		defer a.secretsBusy.Done()
		_ = a.SyncSecretsServer(ctx, srv.ID)
	}()
	// Streams opened by argus-secret are refused: it only answers.
	go func() {
		for {
			c, err := sess.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	<-sess.CloseChan()
	a.secretsTunnels.CompareAndDelete(key, sess)
	a.log.Info("secrets tunnel closed", "server", srv.Name)
}

// relaySecretsRead passes an agent's or app's read to a tunnel server. The
// caller's credential goes through untouched and argus-secret decides; the
// answer comes back untouched, sealed to the caller.
func (a *API) relaySecretsRead(w http.ResponseWriter, r *http.Request) {
	scheme, cred, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if (scheme != "Agent" && scheme != "App") || !a.store.secretsRelayCaller(r.Context(), cred) {
		writeErr(w, http.StatusUnauthorized, "send the server's agent credential or an app token in Authorization")
		return
	}
	srv, err := a.store.SecretsServer(r.Context(), r.PathValue("id"))
	if err != nil || srv.Mode != secretsTunnel {
		writeErr(w, http.StatusNotFound, "no such secrets server")
		return
	}
	cl, base, err := a.secretsTransport(r.Context(), srv)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxSecretsRelayIn+1))
	if err != nil || len(body) > maxSecretsRelayIn {
		writeErr(w, http.StatusRequestEntityTooLarge, "a read is at most 64 KiB")
		return
	}
	uri := strings.TrimPrefix(r.URL.Path, secretsRelayPrefix+srv.ID)
	if r.URL.RawQuery != "" {
		uri += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, base+uri, bytes.NewReader(body))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	req.Header.Set("Authorization", scheme+" "+cred)
	req.Header.Set(vault.RemoteHeader, a.clientAddr(r))
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	resp, err := cl.Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, srv.Name+" did not answer")
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "Retry-After"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, maxSecretsResponse))
}
