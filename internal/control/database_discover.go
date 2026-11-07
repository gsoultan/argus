package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
The add-a-database form asks the server's agent what is there, so an admin
picks a database, a Redis db or a vhost instead of typing its name.

	GET /api/v1/assets/{id}/databases/discover?engine=postgres&port=5432

Admins only, like adding one. The agent reads names and nothing else (see
internal/agent/dbdiscover.go). Every way it can fail is an answer with a
reason, never an error page, because the form falls back to typing the name
and must say why: the agent is offline, too old to list, or the engine did
not answer.
*/

const discoverWait = 25 * time.Second

// DiscoverAnswer is what the console's picker shows.
type DiscoverAnswer struct {
	// State is ok, offline, unsupported or failed.
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`
	Version string `json:"version,omitempty"`
	// Databases are what the agent found; Added marks the ones this
	// server already has an entry for, by name.
	Databases  []DiscoveredDB `json:"databases"`
	Configured int            `json:"configured,omitempty"`
	Truncated  bool           `json:"truncated,omitempty"`
}

// DiscoveredDB is one found database, and whether Argus already has it.
type DiscoveredDB struct {
	tunnel.Discovered
	Added bool `json:"added,omitempty"`
}

// GET /api/v1/assets/{id}/databases/discover
func (a *API) getDiscoverDatabases(w http.ResponseWriter, r *http.Request, actor string) {
	asset, err := a.store.AssetByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such server")
		return
	}
	q := r.URL.Query()
	d := tunnel.GrantDiscover{Engine: q.Get("engine"), Host: strings.TrimSpace(q.Get("host")),
		ConfigPath: strings.TrimSpace(q.Get("configPath"))}
	if d.Engine == "" {
		d.Engine = tunnel.EnginePostgres
	}
	d.Port, _ = strconv.Atoi(q.Get("port"))
	// The same defaults an entry gets, so what is listed is what Add opens.
	probe := Database{Engine: d.Engine, Host: d.Host, Port: d.Port, ConfigPath: d.ConfigPath,
		Name: discoverPlaceholder(d.Engine)}
	if err := probe.validate(); err != nil {
		var invalid ErrInvalidDatabase
		if errors.As(err, &invalid) {
			writeErr(w, http.StatusBadRequest, invalid.Reason)
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	d.Host, d.Port, d.ConfigPath, d.AdminUser = probe.Host, probe.Port, probe.ConfigPath, probe.AdminUser
	if err := d.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ans := a.discoverDatabases(r.Context(), actor, asset.Hostname, d)
	if existing, err := a.store.Databases(r.Context(), asset.ID); err == nil {
		have := map[string]bool{}
		for _, e := range existing {
			if e.Engine == d.Engine && e.Port == d.Port {
				have[e.Name] = true
			}
		}
		for i := range ans.Databases {
			ans.Databases[i].Added = have[ans.Databases[i].Name]
		}
	}
	writeJSON(w, http.StatusOK, ans)
}

// discoverPlaceholder is a name that passes an engine's check, so a probe
// gets the entry's defaults without naming a real database.
func discoverPlaceholder(engine string) string {
	switch engine {
	case tunnel.EngineRedis:
		return "0"
	case tunnel.EngineRabbitMQ:
		return "/"
	case tunnel.EngineMySQL:
		return "argus_probe"
	}
	return "postgres"
}

func (a *API) discoverDatabases(ctx context.Context, actor, hostname string, d tunnel.GrantDiscover) DiscoverAnswer {
	ans := DiscoverAnswer{Databases: []DiscoveredDB{}}
	if a.Tunnels == nil || a.Tunnels.GrantKey == nil {
		ans.State, ans.Error = "failed", "this control plane cannot reach agents: set signing_secret"
		return ans
	}
	g := tunnel.NewGrant(actor, hostname, "discover", "")
	g.Service = tunnel.DiscoverService
	g.Discover = &d
	tok, err := tunnel.SignGrant(a.Tunnels.grantKeyFor(ctx), g)
	if err != nil {
		ans.State, ans.Error = "failed", err.Error()
		return ans
	}
	octx, cancel := context.WithTimeout(ctx, discoverWait)
	defer cancel()
	st, open, err := a.Tunnels.OpenWith(octx, hostname, tunnel.Header{Service: tunnel.DiscoverService, Grant: tok})
	switch {
	case errors.Is(err, ErrNoTunnel):
		ans.State, ans.Error = "offline", "the agent on "+hostname+" is not connected"
		return ans
	case tunnel.IsRefused(err) && strings.Contains(open.Error, "unknown service"):
		ans.State, ans.Error = "unsupported", "the agent on "+hostname+" is too old to list databases"
		return ans
	case tunnel.IsRefused(err):
		ans.State, ans.Error = "failed", open.Error
		return ans
	case err != nil:
		ans.State, ans.Error = "failed", "could not reach the agent on "+hostname+": "+err.Error()
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
			ans.State, ans.Error = "failed", "the agent went away before it answered"
			return ans
		}
		if typ != tunnel.FrameDiscoverResult {
			continue
		}
		var res tunnel.DiscoverResult
		if err := json.Unmarshal(p, &res); err != nil {
			ans.State, ans.Error = "failed", "the agent's answer could not be read"
			return ans
		}
		ans.Version, ans.Configured, ans.Truncated = res.Version, res.Configured, res.Truncated
		if !res.OK {
			ans.State, ans.Error = "failed", res.Error
			return ans
		}
		ans.State = "ok"
		for _, f := range res.Databases {
			if len(ans.Databases) == tunnel.MaxDiscovered {
				ans.Truncated = true
				break
			}
			ans.Databases = append(ans.Databases, DiscoveredDB{Discovered: f})
		}
		return ans
	}
}
