package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/auth"
	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Who is signed in on a host, asked of its agent when the Connect page opens,
so the person connecting sees who else is there before a session starts; and
signing out a disconnected Windows session when every Remote Desktop session
the server allows is taken, so that Windows does not ask whoever is in the
other one to make room.

	GET  /api/v1/assets/{id}/logons            anyone who may connect, and admins, owners, auditors
	POST /api/v1/assets/{id}/logons/signout    admins and owners, audited

The agent refuses to sign out a session that is in use, the console, or one
that no longer belongs to the account shown (agent/logons.go); the control
plane checks the role and writes the audit event, whatever the agent answers.
*/

// logonsWait bounds one question to an agent.
const logonsWait = 25 * time.Second

// LogonsAnswer is what the console gets.
type LogonsAnswer struct {
	// State is ok, offline, unsupported or failed.
	State       string         `json:"state"`
	Error       string         `json:"error,omitempty"`
	Platform    string         `json:"platform,omitempty"`
	Edition     string         `json:"edition,omitempty"`
	RemoteLimit int            `json:"remoteLimit,omitempty"`
	Logons      []tunnel.Logon `json:"logons"`
	// CanSignOut is whether this person may sign a session out here.
	CanSignOut bool   `json:"canSignOut"`
	SignedOut  uint32 `json:"signedOut,omitempty"`
}

type signOutRequest struct {
	Session uint32 `json:"session"`
	Account string `json:"account"`
}

func (a *API) registerLogonsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/assets/{id}/logons", a.getLogons)
	mux.HandleFunc("POST /api/v1/assets/{id}/logons/signout", a.postLogonsSignOut)
}

// mayAskLogons is whether a person may see who is signed in on an asset:
// whoever may open a session on it, and those who see the whole fleet.
func (a *API) mayAskLogons(ctx context.Context, sess auth.Session, assetID string) (bool, error) {
	if seesWholeFleet(sess.Role) {
		return true, nil
	}
	granted, err := a.store.grantedPrincipals(ctx, sess.Email)
	if err != nil {
		return false, err
	}
	if len(granted[assetID]) > 0 {
		return true, nil
	}
	personal, err := a.store.personalPrincipals(ctx, sess.Email)
	if err != nil {
		return false, err
	}
	return personal[assetID] != "", nil
}

func maySignOut(sess auth.Session, asset Asset) bool {
	return (sess.Role == "admin" || sess.Role == "owner") && isWindowsAsset(asset)
}

// logonsAsset reads the asset and checks the person may ask about it,
// answering the request itself when not.
func (a *API) logonsAsset(w http.ResponseWriter, r *http.Request) (auth.Session, Asset, string, bool) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return sess, Asset{}, "", false
	}
	asset, err := a.store.AssetByID(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failAsset(w, "asset", err)
		return sess, asset, "", false
	}
	allowed, err := a.mayAskLogons(r.Context(), sess, asset.ID)
	if err != nil {
		a.fail(w, "logons", err)
		return sess, asset, "", false
	}
	if !allowed {
		// The same answer as a host that does not exist: who is signed in
		// on a server is not for someone who may not open it.
		writeErr(w, http.StatusNotFound, "no such asset")
		return sess, asset, "", false
	}
	if asset.Kind == "android" {
		writeErr(w, http.StatusConflict, "a device has no sign-in sessions to list")
		return sess, asset, "", false
	}
	agent, err := a.store.AgentHostname(r.Context(), asset.Hostname)
	if err != nil {
		a.fail(w, "logons", err)
		return sess, asset, "", false
	}
	return sess, asset, agent, true
}

// GET /api/v1/assets/{id}/logons
func (a *API) getLogons(w http.ResponseWriter, r *http.Request) {
	sess, asset, agent, ok := a.logonsAsset(w, r)
	if !ok {
		return
	}
	res, state, msg := a.askLogons(r.Context(), sess.Email, agent, tunnel.GrantLogons{Op: tunnel.LogonsList})
	ans := logonsAnswer(res, state, msg)
	ans.CanSignOut = state == "ok" && maySignOut(sess, asset) && res.Platform == "windows"
	writeJSON(w, http.StatusOK, ans)
}

// POST /api/v1/assets/{id}/logons/signout
func (a *API) postLogonsSignOut(w http.ResponseWriter, r *http.Request) {
	sess, asset, agent, ok := a.logonsAsset(w, r)
	if !ok {
		return
	}
	if !a.privileged(w, sess, "signing out a session", "admin", "owner") {
		return
	}
	if !isWindowsAsset(asset) {
		writeErr(w, http.StatusConflict, "only a Windows session is signed out from the console")
		return
	}
	var req signOutRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	req.Account = strings.TrimSpace(req.Account)
	q := tunnel.GrantLogons{Op: tunnel.LogonsSignOut, Session: req.Session, Account: req.Account}
	if err := q.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res, state, msg := a.askLogons(r.Context(), sess.Email, agent, q)
	ans := logonsAnswer(res, state, msg)
	ans.SignedOut = res.SignedOut
	what := req.Account + "'s disconnected session (" + strconv.FormatUint(uint64(req.Session), 10) + ") on " + asset.Hostname
	if state == "ok" {
		a.auditAsset(r, AuditEvent{
			Action:     "asset.session_signed_out",
			Severity:   "warning",
			ActorEmail: sess.Email,
			Target:     asset.Hostname,
			Detail: "Signed out " + what + " to free a Remote Desktop session. " +
				"Anything left open in it was closed without saving.",
		})
		a.log.Warn("windows session signed out", "hostname", asset.Hostname, "session", req.Session,
			"account", req.Account, "actor", sess.Email)
	} else {
		a.auditAsset(r, AuditEvent{
			Action:     "asset.session_sign_out_refused",
			Severity:   "notice",
			ActorEmail: sess.Email,
			Target:     asset.Hostname,
			Detail:     "Asked to sign out " + what + "; nothing was signed out: " + msg,
		})
	}
	writeJSON(w, http.StatusOK, ans)
}

func logonsAnswer(res tunnel.LogonsResult, state, msg string) LogonsAnswer {
	ans := LogonsAnswer{State: state, Error: msg, Logons: []tunnel.Logon{}}
	if state != "ok" {
		return ans
	}
	ans.Platform, ans.Edition, ans.RemoteLimit = res.Platform, res.Edition, res.RemoteLimit
	if res.Logons != nil {
		ans.Logons = res.Logons
	}
	if len(ans.Logons) > tunnel.MaxLogons {
		ans.Logons = ans.Logons[:tunnel.MaxLogons]
	}
	return ans
}

// askLogons puts one question to a host's agent. state is ok, offline,
// unsupported or failed, with why in msg.
func (a *API) askLogons(ctx context.Context, actor, hostname string, q tunnel.GrantLogons) (
	tunnel.LogonsResult, string, string) {
	var res tunnel.LogonsResult
	if a.Tunnels == nil || a.Tunnels.GrantKey == nil {
		return res, "failed", "this control plane cannot reach agents: set signing_secret"
	}
	if err := q.Validate(); err != nil {
		return res, "failed", err.Error()
	}
	g := tunnel.NewGrant(actor, hostname, "logons", "")
	g.Service = tunnel.LogonsService
	g.Logons = &q
	tok, err := tunnel.SignGrant(a.Tunnels.grantKeyFor(ctx), g)
	if err != nil {
		return res, "failed", err.Error()
	}
	octx, cancel := context.WithTimeout(ctx, logonsWait)
	defer cancel()
	st, open, err := a.Tunnels.OpenWith(octx, hostname, tunnel.Header{Service: tunnel.LogonsService, Grant: tok})
	switch {
	case errors.Is(err, ErrNoTunnel):
		return res, "offline", "the agent on " + hostname + " is not connected"
	case tunnel.IsRefused(err) && strings.Contains(open.Error, "unknown service"):
		return res, "unsupported", "the agent on " + hostname + " is too old to say who is signed in; " +
			"update it from the asset's Agent tab"
	case tunnel.IsRefused(err):
		return res, "failed", open.Error
	case err != nil:
		return res, "failed", "could not reach the agent on " + hostname + ": " + err.Error()
	}
	defer st.Close()
	go func() {
		<-octx.Done()
		_ = st.Close()
	}()
	fr := tunnel.NewFrameReader(st)
	for {
		typ, p, err := fr.Next()
		if err != nil {
			if octx.Err() != nil {
				return res, "failed", "the agent on " + hostname + " did not answer in time"
			}
			return res, "failed", "the agent went away before it answered"
		}
		if typ != tunnel.FrameLogonsResult {
			continue
		}
		if err := json.Unmarshal(p, &res); err != nil {
			return res, "failed", "the agent's answer could not be read"
		}
		if !res.OK {
			return res, "failed", res.Error
		}
		return res, "ok", ""
	}
}
