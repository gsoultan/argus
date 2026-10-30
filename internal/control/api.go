package control

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/auth"
	"github.com/gsoultan/argus/internal/notify"
	"github.com/gsoultan/argus/internal/storage"
	"github.com/gsoultan/argus/internal/tlsconfig"
	"github.com/gsoultan/argus/internal/tunnel"
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

	// UserTokens maps a console token to a user email. A development stand-in
	// for a real sign-in, and never a substitute for one.
	UserTokens map[string]string
	// StaticTokensDisabled turns UserTokens off. Set when the deployment has a
	// real way to sign in, so a leftover dev token in a config file cannot walk
	// past a password and a second factor.
	StaticTokensDisabled bool
	// ReporterToken authenticates gateways. Agents enrolled since per-host
	// credentials existed report with their own; see enrollment.go.
	ReporterToken string
	// AllowedOrigins for browser CORS.
	AllowedOrigins []string
	// BehindTLSProxy says browsers reach this process over HTTPS that
	// something in front terminates, so it sends what an HTTPS response
	// should even though the connection it sees is plain.
	BehindTLSProxy bool

	// startedAt backs the uptime this process reports about itself.
	startedAt time.Time

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
	// storeRecording replaces storage.Upload for a shipped recording. Nil
	// uses storage; set by tests that have no object store.
	storeRecording recordingUploader

	signer        *auth.Signer
	consoleURL    string
	secureCookies bool

	// SessionTTL and TicketTTL default sensibly; see sessionTTL/ticketTTL.
	SessionTTL time.Duration
	TicketTTL  time.Duration

	// Alerts tells administrators when a session starts. Nil sends nothing.
	Alerts *notify.Dispatcher

	// Enroll is what an agent is handed to trust this control plane when it
	// enrolls. Zero means it relies on the system trust store.
	Enroll EnrollTrust
	// agentCreds caches per-host agent credential lookups.
	agentCreds agentCredCache

	// Tunnels holds the agents' outbound connections on this replica. Nil
	// when the process was built without them, as tests do; the upgrade
	// route then says so rather than failing.
	Tunnels *TunnelHub

	// Console serves the built web console on the console surface. Nil serves
	// the API alone, as before it existed. See console.go.
	Console http.Handler

	// Settings reads and writes the configuration file for the console's
	// Settings page. Nil leaves the page read-only; see settings_routes.go.
	Settings SettingsBackend

	// live is every session this replica relays; see live_sessions.go.
	live liveRegistry

	// setup is the first-run page's one-time token; see setup.go.
	setup setupGate
	// proxyNotice logs an ignored proxy once; see noticeUntrustedProxy.
	proxyNotice sync.Once
}

// SetThrottles enables per-client rate limiting.
func (a *API) SetThrottles(t *Throttles) { a.throttles = t }

// NewAPI builds the HTTP surface.
func NewAPI(store *Store, log *slog.Logger) *API {
	if log == nil {
		log = slog.Default()
	}
	return &API{store: store, log: log, UserTokens: map[string]string{},
		startedAt: time.Now()}
}

// Handler returns the router.
// Handler serves both surfaces on one listener, which is the default.
//
// See ConsoleHandler and FleetHandler for the split: the routes are identical
// either way, and which listener carries them changes nothing about what any
// of them check.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	a.registerCommon(mux)
	a.registerConsole(mux)
	a.registerFleet(mux)
	return a.securityHeaders(a.cors(mux))
}

// ConsoleHandler serves the surface a person reaches: sign-in, the console's
// reads, tickets, access requests.
//
// Separable from the fleet surface so an operator can bind it somewhere
// narrower -- a VPN address, an internal interface -- while gateways and agents
// go on reporting to an address they can actually reach. Nothing here is a
// substitute for the authentication on these routes; it only decides who can
// knock.
func (a *API) ConsoleHandler() http.Handler {
	mux := http.NewServeMux()
	a.registerCommon(mux)
	a.registerConsole(mux)
	return a.securityHeaders(a.cors(mux))
}

// FleetHandler serves the surface gateways and agents reach.
//
// Every route on it is behind the reporter token and, when a client CA is
// configured, a verified client certificate as well. This is the half that has
// to stay reachable: a control plane the fleet cannot post to keeps no
// recordings, no chain heads and no audit events, and the console goes on
// showing a fleet it has stopped hearing from.
func (a *API) FleetHandler() http.Handler {
	mux := http.NewServeMux()
	a.registerCommon(mux)
	a.registerFleet(mux)
	return a.securityHeaders(a.cors(mux))
}

// registerCommon adds what both surfaces need: liveness, and the process's own
// stats, which refuse anything that is not loopback regardless of listener.
func (a *API) registerCommon(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// The process, not the fleet. Loopback only; see process_stats.go.
	mux.HandleFunc("GET /stats", a.handleProcessStats)
}

// registerConsole adds the routes a person reaches.
func (a *API) registerConsole(mux *http.ServeMux) {
	// Authentication endpoints are throttled per client.
	mux.HandleFunc("POST /auth/logout", a.handleLogout)
	// Local accounts: the surface a password-guessing attempt actually reaches,
	// and the only one left.
	mux.HandleFunc("POST /auth/password", a.limitLogin(a.handlePasswordLogin))
	mux.HandleFunc("POST /auth/mfa", a.limitLogin(a.handleMFAVerify))
	mux.HandleFunc("POST /auth/mfa/enrol", a.handleMFABegin)
	mux.HandleFunc("POST /auth/mfa/confirm", a.handleMFAConfirm)
	mux.HandleFunc("POST /auth/password/change", a.handleChangePassword)
	mux.HandleFunc("GET /auth/me", a.handleMe)
	// First run: the first account, once, with a token read from the host.
	mux.HandleFunc("GET /auth/setup", a.handleSetupStatus)
	mux.HandleFunc("POST /auth/setup", a.limitLogin(a.handleSetup))
	mux.HandleFunc("POST /api/v1/terminal/ticket", a.limitTickets(a.handleTicket))
	// The browser terminal through an agent's tunnel; see terminal.go.
	mux.HandleFunc("GET /api/v1/terminal", a.handleTerminal)
	mux.HandleFunc("GET /api/v1/files", a.handleFiles)
	mux.HandleFunc("GET /api/v1/desktop", a.handleDesktop)
	mux.HandleFunc("POST /api/v1/sessions/{id}/shadow/ticket", a.limitTickets(a.handleShadowTicket))
	mux.HandleFunc("POST /api/v1/sessions/{id}/terminate/ticket", a.limitTickets(a.handleTerminateTicket))
	mux.HandleFunc("GET /api/v1/sessions/{id}/watch", a.handleWatch)
	mux.HandleFunc("POST /api/v1/sessions/{id}/terminate", a.handleTerminate)
	mux.HandleFunc("GET /api/v1/requests", a.getRequests)
	mux.HandleFunc("POST /api/v1/requests", a.postRequest)
	mux.HandleFunc("POST /api/v1/requests/{id}/decision", a.postDecision)
	mux.HandleFunc("GET /api/v1/direct-logins", a.user(a.getDirectLogins))
	mux.HandleFunc("POST /api/v1/direct-logins/{id}/decision", a.postDirectLoginDecision)
	mux.HandleFunc("GET /api/v1/grant", a.user(a.getGrant))

	// Console read surface. Paths match what web/src/lib/api.ts already calls.
	mux.HandleFunc("GET /api/v1/stats", a.getStats)

	// The inventory. Reading it is filtered by role rather than gated on one:
	// an ordinary account sees the assets assigned to it. Every write is
	// admin-or-owner and lands in the audit chain. See asset_routes.go.
	mux.HandleFunc("GET /api/v1/assets", a.getAssets)
	mux.HandleFunc("POST /api/v1/assets", a.postAssetCreate)
	mux.HandleFunc("PATCH /api/v1/assets/{id}", a.patchAsset)
	mux.HandleFunc("DELETE /api/v1/assets/{id}", a.deleteAsset)
	mux.HandleFunc("PUT /api/v1/assets/{id}/direct-ssh", a.putAssetDirectSSH)
	mux.HandleFunc("GET /api/v1/users", a.getUsers)
	mux.HandleFunc("POST /api/v1/users", a.postAccount)
	mux.HandleFunc("POST /api/v1/users/{email}/disable", a.postAccountDisabled(true))
	mux.HandleFunc("POST /api/v1/users/{email}/enable", a.postAccountDisabled(false))
	mux.HandleFunc("GET /api/v1/settings", a.getSettings)
	mux.HandleFunc("PUT /api/v1/settings", a.putSettings)
	mux.HandleFunc("POST /api/v1/settings/test-database", a.postTestDatabase)
	mux.HandleFunc("GET /api/v1/assets/{id}/assignments", a.getAssignments)
	mux.HandleFunc("PUT /api/v1/assets/{id}/assignments", a.putAssignment)
	mux.HandleFunc("DELETE /api/v1/assets/{id}/assignments/{email}", a.deleteAssignment)
	mux.HandleFunc("GET /api/v1/sessions", a.getSessions)
	mux.HandleFunc("GET /api/v1/sessions/{id}", a.getSession)
	mux.HandleFunc("GET /api/v1/audit", a.user(a.getAudit))
	mux.HandleFunc("GET /api/v1/coverage", a.user(a.getCoverage))
	mux.HandleFunc("GET /api/v1/discovered", a.user(a.getDiscovered))
	mux.HandleFunc("POST /api/v1/discovered/{hostname}/enrol", a.postEnrol)
	mux.HandleFunc("POST /api/v1/discovered/{hostname}/ignore", a.postIgnoreHost)
	mux.HandleFunc("GET /api/v1/sessions/{id}/recording", a.user(a.getRecording))
	mux.HandleFunc("GET /api/v1/sessions/{id}/rdp-replay", a.user(a.getRDPReplay))
	mux.HandleFunc("GET /api/v1/sessions/{id}/recording/link", a.user(a.presignRecording))
	mux.HandleFunc("GET /api/v1/policy", a.user(a.getPolicy))
	mux.HandleFunc("POST /api/v1/policy", a.postPolicy)

	// The console itself, for every GET no route above claims.
	if a.Console != nil {
		mux.Handle("GET /", a.Console)
	}

	// Agent enrollment. Minting a token and revoking an agent are admin
	// decisions and land in the audit chain; see enrollment_routes.go.
	mux.HandleFunc("POST /api/v1/agents/enroll-tokens", a.postEnrollToken)
	mux.HandleFunc("GET /api/v1/agents", a.getAgentCredentials)
	mux.HandleFunc("POST /api/v1/agents/{id}/revoke", a.postRevokeAgent)

}

// registerFleet adds the routes an agent reaches.
func (a *API) registerFleet(mux *http.ServeMux) {
	// Reporter surface. Machine credentials only.
	//
	// Every route here is an agent's. It takes the per-host agent credential,
	// bound to the host it reports for.
	mux.HandleFunc("POST /api/v1/report/session", a.agentReporter(a.postSession))
	// An agent shipping a sealed recording; see recording_upload.go.
	mux.HandleFunc("PUT /api/v1/report/recording/{id}", a.agentReporter(a.putRecording))
	mux.HandleFunc("POST /api/v1/report/heartbeat", a.agentReporter(a.postHeartbeat))
	mux.HandleFunc("POST /api/v1/report/login", a.agentReporter(a.postLogin))
	mux.HandleFunc("POST /api/v1/report/direct-login", a.agentReporter(a.postDirectLogin))
	mux.HandleFunc("POST /api/v1/report/direct-login/{id}/wait", a.agentReporter(a.postDirectLoginWait))
	mux.HandleFunc("GET "+tunnel.UpgradePath, a.handleTunnelUpgrade)
	mux.HandleFunc("POST /api/v1/report/facts", a.agentReporter(a.postFacts))

	// The exchange that gives an agent its credential. Throttled like a
	// sign-in: the token is the only thing it checks.
	mux.HandleFunc("POST /api/v1/agents/enroll", a.limitLogin(a.postEnroll))
}

// securityHeaders sets the response headers a browser needs to protect the
// console, on every response including errors.
func (a *API) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil || a.BehindTLSProxy {
			// Only over TLS: sending HSTS on a plaintext response is
			// meaningless, and setting it during a local HTTP session would
			// pin the browser to HTTPS for a host that cannot serve it.
			// Behind a proxy that terminates TLS -- a Cloudflare Tunnel, a
			// load balancer -- this process sees plain HTTP while the browser
			// is on HTTPS, which is exactly where the header belongs.
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
				w.Header().Set("Access-Control-Allow-Methods",
					"GET, POST, PATCH, PUT, DELETE, OPTIONS")
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

// getStats serves the Overview's counters, about whatever the caller can see.
//
// Scoped rather than gated: an operator gets the same page about their own
// hosts and sessions. Every figure on it is then true of something they can act
// on, and it agrees with the asset list and the session list below it -- which
// are filtered by the same rule.
func (a *API) getStats(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var (
		st  FleetStats
		err error
	)
	if seesWholeFleet(sess.Role) {
		st, err = a.store.Stats(r.Context())
	} else {
		st, err = a.store.StatsFor(r.Context(), sess.Email)
	}
	if err != nil {
		a.fail(w, "stats", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (a *API) getSessions(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	filter := SessionFilter{
		State:  r.URL.Query().Get("state"),
		Origin: r.URL.Query().Get("origin"),
		Limit:  limit,
	}
	// Who else is on which host and when is the same reconnaissance the asset
	// list is filtered to prevent, and it is not less sensitive for being in a
	// different table.
	if !seesWholeFleet(sess.Role) {
		filter.UserEmail = sess.Email
	}

	sessions, err := a.store.Sessions(r.Context(), filter)
	if err != nil {
		a.fail(w, "sessions", err)
		return
	}
	writeJSON(w, http.StatusOK, sessions)
}

func (a *API) getSession(w http.ResponseWriter, r *http.Request) {
	auth, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	sess, err := a.store.Session(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, "session", err)
		return
	}
	// Not found, rather than forbidden, for someone else's session. A 403 would
	// confirm the id names a real session and who it belongs to, which is most
	// of what the recording would have told them.
	if sess == nil || (!seesWholeFleet(auth.Role) && !strings.EqualFold(sess.UserEmail, auth.Email)) {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (a *API) getAudit(w http.ResponseWriter, r *http.Request, _ string) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	// Paging backwards through the log, so the console can fetch all of it
	// rather than verifying a window and calling the verdict the log's.
	before, _ := strconv.Atoi(r.URL.Query().Get("before"))
	events, err := a.store.AuditEvents(r.Context(), limit, before)
	if err != nil {
		a.fail(w, "audit", err)
		return
	}
	// How much of the log this is. A caller that verifies what it received and
	// reports "intact" is describing a window, and without this it has no way
	// to know that -- the response is just a shorter array.
	//
	// A header rather than an envelope: the body is a bare array everywhere
	// this is consumed, and X-Argus-Chain-Verified already establishes the
	// pattern for saying something *about* a response here.
	if total, cerr := a.store.CountAuditEvents(r.Context()); cerr == nil {
		w.Header().Set("X-Argus-Audit-Total", strconv.Itoa(total))
	} else {
		a.log.Warn("could not count audit events", "error", cerr,
			"detail", "the console cannot tell whether it received the whole log")
	}
	writeJSON(w, http.StatusOK, events)
}

/* ── Reporter handlers ───────────────────────────────────────────────────── */

func (a *API) postSession(w http.ResponseWriter, r *http.Request) {
	var report struct {
		Session
		// DirectLoginID is the approval a direct login was let in on, and
		// DirectApproval is "approved" for one, or "allowed" for a login let
		// in because the server is set to allow and record. Checked against
		// what the control plane holds before either is believed.
		DirectLoginID  string `json:"directLoginId"`
		DirectApproval string `json:"directApproval"`
	}
	if err := decodeReport(r, &report, a.log, "report/session"); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	in := report.Session
	if in.ID == "" || in.AssetHostname == "" {
		writeErr(w, http.StatusBadRequest, "id and assetHostname are required")
		return
	}
	// The column is a 32-bit integer. Out of range, the insert failed with
	// a 500 the agent retried for ever, and the session never appeared.
	if in.ExitCode != nil && (*in.ExitCode < math.MinInt32 || *in.ExitCode > math.MaxInt32) {
		writeErr(w, http.StatusBadRequest, "exitCode must fit in 32 bits; "+
			"report a Windows status such as 0xC0000142 as the signed value it is")
		return
	}
	if !a.mayReportFor(w, r, in.AssetHostname) {
		return
	}
	if host := agentHost(r); host != "" {
		// Said by the control plane, not the reporter: which credential
		// vouched for this row is the part of the record a reporter must not
		// be able to choose.
		in.ReportedBy = "agent:" + host
	}
	if in.StartedAt.IsZero() {
		in.StartedAt = time.Now().UTC()
	}
	created, err := a.store.RecordSession(r.Context(), in)
	if err != nil {
		a.fail(w, "report session", err)
		return
	}
	// Only on the report that created the row: the gateway and the agent both
	// report every session, and keep reporting it while it runs. Queued, not
	// sent, so a slow mail server cannot hold up the reporter.
	if created {
		a.Alerts.SessionStarted(notify.SessionStarted{
			ID:        in.ID,
			UserEmail: in.UserEmail,
			Host:      in.AssetHostname,
			Principal: in.Principal,
			Protocol:  in.Protocol,
			Origin:    in.Origin,
			ClientIP:  in.ClientIP,
			StartedAt: in.StartedAt,
		})
	}

	// A recording that cannot support the fidelity it claims.
	//
	// eBPF fidelity means kernel-observed execve events were captured. A
	// finished session that produced real output and reported not one command
	// is a contradiction: either the probe stopped part-way and the agent kept
	// claiming the higher tier, or the report was altered in flight. Neither is
	// something to discover months later when an auditor asks what ran.
	//
	// The reported value is stored as sent rather than quietly downgraded --
	// rewriting what an agent said would itself put a fiction in the record.
	// What goes in the chain is that the claim and the evidence disagree.
	if fidelityUnsupported(in) {
		a.log.Warn("session claims kernel evidence it did not report",
			"session", in.ID, "host", in.AssetHostname, "bytes", in.RecordingBytes)
		if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
			Action:     "session.fidelity_unsupported",
			Severity:   "warning",
			ActorEmail: in.UserEmail,
			Target:     in.AssetHostname,
			Detail: fmt.Sprintf(
				"Session %s reported eBPF fidelity but recorded %d bytes of output "+
					"with no kernel-observed commands. Its command timeline is not "+
					"kernel evidence; treat it as PTY capture until the agent on this "+
					"host is shown to be working.", in.ID, in.RecordingBytes),
		}); err != nil {
			a.log.Error("audit append failed", "error", err)
		}
	}

	// A bypass is the event the whole agent exists to catch, so it goes into
	// the audit chain rather than only the session table — the audit log is
	// what an auditor reads, and it is the tamper-evident one.
	if in.Origin == "direct" {
		e := AuditEvent{
			Action:     "session.direct_detected",
			Severity:   "critical",
			ActorEmail: in.UserEmail,
			Target:     in.AssetHostname,
			Detail: "Session reached the host without passing through Argus. " +
				"It was recorded, but no approval, time window or principal check was applied.",
		}
		if ok, detail := a.directSessionAdmitted(r.Context(), in, report.DirectLoginID,
			report.DirectApproval); ok {
			e.Action, e.Severity, e.Detail = "session.direct_recorded", "notice", detail
		}
		if _, err := a.store.AppendAudit(r.Context(), e); err != nil {
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
		// What the agent has always sent and this struct did not accept, so
		// every heartbeat carrying it was rejected whole. ExecTracing is
		// whether the kernel probe is loaded; ExecReason says why not.
		ExecTracing bool   `json:"exec_tracing"`
		ExecReason  string `json:"exec_reason"`
		// ConsoleOnly is whether the agent is refusing direct logins now.
		// Absent from an agent older than console-only.
		ConsoleOnly *bool `json:"console_only"`
		// DirectSSH is the direct SSH mode the agent is applying now.
		// Absent from an agent older than the setting.
		DirectSSH *string `json:"direct_ssh"`
	}
	if err := decodeReport(r, &in, a.log, "report/heartbeat"); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Hostname == "" {
		writeErr(w, http.StatusBadRequest, "hostname is required")
		return
	}
	if !a.mayReportFor(w, r, in.Hostname) {
		return
	}
	if in.DirectSSH != nil && !validDirectSSH(*in.DirectSSH) {
		in.DirectSSH = nil
	}
	err := a.store.Heartbeat(r.Context(), in.Hostname, in.Version,
		in.ActiveSessions, in.Posture, in.ExecTracing, in.ExecReason, in.ConsoleOnly,
		in.DirectSSH)
	// The direct SSH mode rides the reply. console_only is what an agent
	// older than the setting understands, and it is on unless the mode is
	// "allow": such an agent cannot hold a login for approval, so under
	// "approve" it refuses, the closed side of what was asked. An agent that
	// knows direct_ssh reads that instead.
	//
	// A setting that cannot be read is left out, so the agent keeps what it
	// last heard rather than a value chosen by an outage; console_only stays
	// on regardless.
	reply := map[string]any{"status": "ok", "console_only": true}
	if mode, merr := a.store.EffectiveDirectSSH(r.Context(), in.Hostname); merr == nil {
		reply["direct_ssh"] = mode
		reply["console_only"] = mode != DirectSSHAllow
	} else {
		a.log.Error("heartbeat: cannot read the direct ssh setting", "error", merr)
	}
	if errors.Is(err, ErrAgentUnmatched) {
		a.log.Warn("agent reports from a host the inventory does not know",
			"hostname", in.Hostname,
			"detail", "coverage is not being tracked for this host; add it to the "+
				"inventory or set its agent_hostname")
		reply["warning"] = "no matching asset; coverage not tracked"
		writeJSON(w, http.StatusOK, reply)
		return
	}
	if err != nil {
		a.fail(w, "heartbeat", err)
		return
	}
	writeJSON(w, http.StatusOK, reply)
}

/* ── Helpers ─────────────────────────────────────────────────────────────── */

// minOutputForCommandEvidence is how much output a session must have produced
// before reporting no commands is treated as a contradiction.
//
// A login that fails, or a session closed before the shell drew a prompt,
// legitimately runs nothing. A few kilobytes of output did not happen without
// something executing.
const minOutputForCommandEvidence = 4096

// fidelityUnsupported reports a finished session whose evidence cannot support
// the fidelity it claims.
func fidelityUnsupported(s Session) bool {
	if s.Fidelity != "ebpf" {
		return false
	}
	// Only once it is over. A session still in flight has legitimately observed
	// nothing yet, and flagging every start would train people to ignore this.
	if s.State == "active" || s.EndedAt == nil {
		return false
	}
	if s.RecordingBytes < minOutputForCommandEvidence {
		return false
	}
	return s.CommandCount == nil || *s.CommandCount == 0
}

// decode reads a request body from the console, strictly.
//
// Unknown fields are refused here because the console ships with this build:
// there is no version skew between them, so an unrecognised field is a typo or
// a stale client and should fail where someone will see it.
//
// Reporters are a different matter entirely -- see decodeReport.
func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
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
