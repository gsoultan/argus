package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
The reach check (tunnel/reachcheck.go): one Argus server connects to a
database port on another and says what a client there gets. It answers the
question a check on the database's own server cannot: does the firewall
between them let this machine in?

POST /api/v1/assets/{id}/reach-check {host, port, engine}, where {id} is the
server to connect from. Admins only, like adding a database, and each check
is in the audit log: it is a connection made from someone's server.
*/

const reachWait = 15 * time.Second

// ReachCheckAnswer is a reach check's result, or why there is none.
type ReachCheckAnswer struct {
	tunnel.ReachResult
	// State is ok, offline, unsupported or failed.
	State string `json:"state"`
	Error string `json:"error,omitempty"`
}

func (a *API) postReachCheck(w http.ResponseWriter, r *http.Request, actor string) {
	asset, err := a.store.AssetByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such server")
		return
	}
	var in tunnel.GrantReach
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	in.Host = strings.TrimSpace(in.Host)
	if err := in.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Engine != tunnel.EngineRedis {
		in.Engine = ""
	}
	ans := a.reachCheck(r.Context(), actor, asset.Hostname, in)
	what := "could not be checked: " + ans.Error
	if ans.State == "ok" {
		what = "connected"
		if !ans.Connected {
			what = "did not connect: " + ans.Detail
		}
	}
	a.auditAsset(r, AuditEvent{Action: "asset.reach_check", Severity: "info", ActorEmail: actor, Target: asset.Hostname,
		Detail: fmt.Sprintf("Checked from %s whether %s:%d can be reached: %s.", asset.Hostname, in.Host, in.Port, what)})
	writeJSON(w, http.StatusOK, ans)
}

func (a *API) reachCheck(ctx context.Context, actor, hostname string, q tunnel.GrantReach) ReachCheckAnswer {
	ans := ReachCheckAnswer{State: "failed"}
	if a.Tunnels == nil || a.Tunnels.GrantKey == nil {
		ans.Error = "this control plane cannot reach agents: set signing_secret"
		return ans
	}
	agentHost, err := a.agentOf(ctx, hostname)
	if err != nil {
		ans.Error = "could not find the agent on " + hostname + ": " + err.Error()
		return ans
	}
	g := tunnel.NewGrant(actor, agentHost, "reachcheck", "")
	g.Service = tunnel.ReachCheckService
	g.Reach = &q
	tok, err := tunnel.SignGrant(a.Tunnels.grantKeyFor(ctx), g)
	if err != nil {
		ans.Error = err.Error()
		return ans
	}
	octx, cancel := context.WithTimeout(ctx, reachWait)
	defer cancel()
	st, open, err := a.Tunnels.OpenWith(octx, agentHost, tunnel.Header{Service: tunnel.ReachCheckService, Grant: tok})
	switch {
	case errors.Is(err, ErrNoTunnel):
		ans.State, ans.Error = "offline", "the agent on "+hostname+" is not connected"
		return ans
	case tunnel.IsRefused(err) && strings.Contains(open.Error, "unknown service"):
		ans.State, ans.Error = "unsupported", "the agent on "+hostname+" is too old to check a connection; update it from its Settings tab"
		return ans
	case tunnel.IsRefused(err):
		ans.Error = open.Error
		return ans
	case err != nil:
		ans.Error = agentUnreachable(hostname, err)
		return ans
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
			ans.Error = "the agent went away before it answered"
			return ans
		}
		if typ != tunnel.FrameReachResult {
			continue
		}
		if err := json.Unmarshal(p, &ans.ReachResult); err != nil {
			ans.Error = "the agent's answer could not be read"
			return ans
		}
		ans.State = "ok"
		return ans
	}
}
