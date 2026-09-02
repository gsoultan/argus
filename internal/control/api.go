package control

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/auth"
	"github.com/gsoultan/argus/internal/storage"
	"github.com/gsoultan/argus/internal/tlsconfig"
)

// API serves the console and receives reports from gateways and agents.
//
// Two distinct audiences with different trust: browsers hold a user session,
// while gateways and agents hold a machine token. They are separated at the
// route level rather than by a role check inside a handler, because a browser
// must never be able to forge a session report.
type API struct {
	store *Store
	log   *slog.Logger

	// UserTokens maps a console token to a user email. Stands in for the OIDC
	// session the control plane will issue.
	UserTokens map[string]string
	// ReporterToken authenticates gateways and agents.
	ReporterToken string
	// AllowedOrigins for browser CORS.
	AllowedOrigins []string

	// throttles bounds the authentication surface. Nil disables throttling.
	throttles *Throttles

	// RequirePeerCert makes the reporter routes demand a verified client
	// certificate in addition to the token.
	//
	// Only the reporter routes: the same listener serves browsers, which have
	// no client certificate. Splitting it this way means a leaked reporter
	// token is not enough on its own — an attacker also needs a key signed by
	// the internal CA.
	RequirePeerCert bool

	storage *storage.Client

	oidc          *auth.OIDC
	signer        *auth.Signer
	consoleURL    string
	secureCookies bool

	// SessionTTL and TicketTTL default sensibly; see sessionTTL/ticketTTL.
	SessionTTL time.Duration
	TicketTTL  time.Duration
}

// SetThrottles enables per-client rate limiting.
func (a *API) SetThrottles(t *Throttles) { a.throttles = t }

// NewAPI builds the HTTP surface.
func NewAPI(store *Store, log *slog.Logger) *API {
	if log == nil {
		log = slog.Default()
	}
	return &API{store: store, log: log, UserTokens: map[string]string{}}
}

// Handler returns the router.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()

	// Authentication endpoints are throttled per client. The callback matters
	// most: it burns an IdP round trip on every call.
	mux.HandleFunc("GET /auth/login", a.limitLogin(a.handleLogin))
	mux.HandleFunc("GET /auth/callback", a.limitLogin(a.handleCallback))
	mux.HandleFunc("POST /auth/logout", a.handleLogout)
	mux.HandleFunc("GET /auth/me", a.handleMe)
	mux.HandleFunc("POST /api/v1/terminal/ticket", a.limitTickets(a.handleTicket))
	mux.HandleFunc("POST /api/v1/sessions/{id}/shadow/ticket", a.limitTickets(a.handleShadowTicket))
	mux.HandleFunc("POST /api/v1/sessions/{id}/terminate/ticket", a.limitTickets(a.handleTerminateTicket))
	mux.HandleFunc("GET /api/v1/requests", a.user(a.getRequests))
	mux.HandleFunc("POST /api/v1/requests", a.postRequest)
	mux.HandleFunc("POST /api/v1/requests/{id}/decision", a.postDecision)
	mux.HandleFunc("GET /api/v1/grant", a.user(a.getGrant))

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Console read surface. Paths match what web/src/lib/api.ts already calls.
	mux.HandleFunc("GET /api/v1/stats", a.user(a.getStats))
	mux.HandleFunc("GET /api/v1/assets", a.user(a.getAssets))
	mux.HandleFunc("GET /api/v1/sessions", a.user(a.getSessions))
	mux.HandleFunc("GET /api/v1/sessions/{id}", a.user(a.getSession))
	mux.HandleFunc("GET /api/v1/audit", a.user(a.getAudit))
	mux.HandleFunc("GET /api/v1/sessions/{id}/recording", a.user(a.getRecording))
	mux.HandleFunc("GET /api/v1/sessions/{id}/recording/link", a.user(a.presignRecording))

	// Reporter surface. Machine token only.
	mux.HandleFunc("POST /api/v1/report/session", a.reporter(a.postSession))
	mux.HandleFunc("POST /api/v1/report/heartbeat", a.reporter(a.postHeartbeat))
	mux.HandleFunc("POST /api/v1/report/asset", a.reporter(a.postAsset))
	mux.HandleFunc("POST /api/v1/report/audit", a.reporter(a.postAudit))

	// Shared state, so replay protection and host-key pins hold across every
	// gateway rather than per instance.
	mux.HandleFunc("POST /api/v1/terminal/redeem", a.reporter(a.postRedeem))
	mux.HandleFunc("GET /api/v1/hostkeys/pin", a.reporter(a.getHostKeyPin))
	mux.HandleFunc("POST /api/v1/hostkeys/pin", a.reporter(a.postHostKeyPin))

	return a.securityHeaders(a.cors(mux))
}

// securityHeaders sets the response headers a browser needs to protect the
// console, on every response including errors.
func (a *API) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil {
			// Only over TLS: sending HSTS on a plaintext response is
			// meaningless, and setting it during a local HTTP session would
			// pin the browser to HTTPS for a host that cannot serve it.
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		// The API serves JSON, never markup, so nothing needs to execute.
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

/* ── Auth ────────────────────────────────────────────────────────────────── */

// ctxKey namespaces values Argus puts on a request context.
type ctxKey string

// peerKey carries the client-certificate identity of a reporter call.
const peerKey ctxKey = "argus.peer"

func bearer(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return r.URL.Query().Get("token")
}

// user gates a console route.
func (a *API) user(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := a.authenticate(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r, sess.Email)
	}
}

// reporter gates an ingest route.
//
// Deliberately a different credential from the console's: a stolen browser
// token must not let anyone write fabricated sessions into the audit record.
func (a *API) reporter(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.ReporterToken == "" || subtle.ConstantTimeCompare(
			[]byte(bearer(r)), []byte(a.ReporterToken)) != 1 {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if a.RequirePeerCert {
			peer := tlsconfig.PeerIdentity(r.TLS)
			if peer == "" {
				a.log.Warn("reporter call without a client certificate",
					"remote", r.RemoteAddr, "path", r.URL.Path)
				writeErr(w, http.StatusUnauthorized,
					"a client certificate is required for reporter endpoints")
				return
			}
			// Attribute the call to a host rather than to "whoever holds the
			// token", which is what makes a reported session traceable.
			r = r.WithContext(context.WithValue(r.Context(), peerKey, peer))
		}
		next(w, r)
	}
}

// PeerName returns the client-certificate identity of a reporter call, if any.
func PeerName(r *http.Request) string {
	if v, ok := r.Context().Value(peerKey).(string); ok {
		return v
	}
	return ""
}

func (a *API) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		for _, allowed := range a.AllowedOrigins {
			if allowed == origin && origin != "" {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				// Without this the browser silently hides the header and the
				// console cannot tell an intact recording from a tampered one —
				// the verdict is sent, read as null, and shown as nothing.
				w.Header().Set("Access-Control-Expose-Headers", "X-Argus-Chain-Verified")
				// The console runs on a different origin in development, and a
				// session cookie will not be sent without this.
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				w.Header().Set("Vary", "Origin")
				break
			}
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

/* ── Console handlers ────────────────────────────────────────────────────── */

func (a *API) getStats(w http.ResponseWriter, r *http.Request, _ string) {
	st, err := a.store.Stats(r.Context())
	if err != nil {
		a.fail(w, "stats", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (a *API) getAssets(w http.ResponseWriter, r *http.Request, _ string) {
	assets, err := a.store.Assets(r.Context())
	if err != nil {
		a.fail(w, "assets", err)
		return
	}
	writeJSON(w, http.StatusOK, assets)
}

func (a *API) getSessions(w http.ResponseWriter, r *http.Request, _ string) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	sessions, err := a.store.Sessions(r.Context(), SessionFilter{
		State:  r.URL.Query().Get("state"),
		Origin: r.URL.Query().Get("origin"),
		Limit:  limit,
	})
	if err != nil {
		a.fail(w, "sessions", err)
		return
	}
	writeJSON(w, http.StatusOK, sessions)
}

func (a *API) getSession(w http.ResponseWriter, r *http.Request, _ string) {
	sess, err := a.store.Session(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, "session", err)
		return
	}
	if sess == nil {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (a *API) getAudit(w http.ResponseWriter, r *http.Request, _ string) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := a.store.AuditEvents(r.Context(), limit)
	if err != nil {
		a.fail(w, "audit", err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

/* ── Reporter handlers ───────────────────────────────────────────────────── */

func (a *API) postSession(w http.ResponseWriter, r *http.Request) {
	var in Session
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.ID == "" || in.AssetHostname == "" {
		writeErr(w, http.StatusBadRequest, "id and assetHostname are required")
		return
	}
	if in.StartedAt.IsZero() {
		in.StartedAt = time.Now().UTC()
	}
	if err := a.store.UpsertSession(r.Context(), in); err != nil {
		a.fail(w, "report session", err)
		return
	}

	// A bypass is the event the whole agent exists to catch, so it goes into
	// the audit chain rather than only the session table — the audit log is
	// what an auditor reads, and it is the tamper-evident one.
	if in.Origin == "direct" {
		if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
			Action:     "session.direct_detected",
			Severity:   "critical",
			ActorEmail: in.UserEmail,
			Target:     in.AssetHostname,
			Detail: "Session reached the host without passing through the gateway. " +
				"It was recorded, but no approval, time window or principal check was applied.",
		}); err != nil {
			a.log.Error("audit append failed", "error", err)
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
}

func (a *API) postHeartbeat(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Hostname       string `json:"hostname"`
		Version        string `json:"version"`
		ActiveSessions int    `json:"active_sessions"`
		Posture        any    `json:"posture"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Hostname == "" {
		writeErr(w, http.StatusBadRequest, "hostname is required")
		return
	}
	err := a.store.Heartbeat(r.Context(), in.Hostname, in.Version,
		in.ActiveSessions, in.Posture)
	if errors.Is(err, ErrAgentUnmatched) {
		a.log.Warn("agent reports from a host the inventory does not know",
			"hostname", in.Hostname,
			"detail", "coverage is not being tracked for this host; add it to the "+
				"inventory or set its agent_hostname")
		writeJSON(w, http.StatusOK, map[string]string{
			"status": "ok", "warning": "no matching asset; coverage not tracked",
		})
		return
	}
	if err != nil {
		a.fail(w, "heartbeat", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) postAsset(w http.ResponseWriter, r *http.Request) {
	var in Asset
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Hostname == "" {
		writeErr(w, http.StatusBadRequest, "hostname is required")
		return
	}
	if err := a.store.UpsertAsset(r.Context(), in); err != nil {
		a.fail(w, "report asset", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) postAudit(w http.ResponseWriter, r *http.Request) {
	var in AuditEvent
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Action == "" {
		writeErr(w, http.StatusBadRequest, "action is required")
		return
	}
	out, err := a.store.AppendAudit(r.Context(), in)
	if err != nil {
		a.fail(w, "append audit", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

/* ── Helpers ─────────────────────────────────────────────────────────────── */

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	// Reject unknown fields: a reporter sending something this build does not
	// understand should fail loudly rather than have data silently dropped.
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errors.New("invalid JSON: " + err.Error())
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// fail logs the real error and returns a generic one.
//
// Database errors carry schema details and sometimes row contents; those belong
// in the operator's log, not in a response to a browser.
func (a *API) fail(w http.ResponseWriter, op string, err error) {
	a.log.Error("request failed", "op", op, "error", err)
	writeErr(w, http.StatusInternalServerError, "internal error")
}
