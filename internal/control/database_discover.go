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
	// Server is "valkey" when a Valkey answered on the redis engine.
	Server string `json:"server,omitempty"`
	// Databases are what the agent found; Added marks the ones this
	// server already has an entry for, by name.
	Databases  []DiscoveredDB `json:"databases"`
	Configured int            `json:"configured,omitempty"`
	Truncated  bool           `json:"truncated,omitempty"`
	// Host and Port are where the agent found Postgres when it was not
	// where the form asked; the form adds the entry there.
	Host string `json:"host,omitempty"`
	Port int    `json:"port,omitempty"`
	// Places are the other Postgres servers the agent found on this server.
	Places []tunnel.DiscoverPlace `json:"places,omitempty"`
	// Poolers are the PgBouncers configured on this server, each database
	// they send on matched to the server in the inventory it goes to.
	Poolers []DiscoveredPooler `json:"poolers,omitempty"`
	// Tried is every way in the agent tried, when none answered.
	Tried []string `json:"tried,omitempty"`
}

// DiscoveredPooler is one PgBouncer and where its databases go.
type DiscoveredPooler struct {
	Config  string            `json:"config"`
	Port    int               `json:"port"`
	Targets []PoolerTargetRef `json:"targets"`
}

// PoolerTargetRef is a PgBouncer [databases] line and, when its Postgres is
// a server in the inventory, that server.
type PoolerTargetRef struct {
	tunnel.PoolerTarget
	AssetID   string `json:"assetId,omitempty"`
	AssetName string `json:"assetName,omitempty"`
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
	port := d.Port
	if ans.Port != 0 {
		port = ans.Port
	}
	if len(ans.Poolers) > 0 {
		a.matchPoolerTargets(r.Context(), asset.ID, ans.Poolers)
	}
	if existing, err := a.store.Databases(r.Context(), asset.ID); err == nil {
		have := map[string]bool{}
		for _, e := range existing {
			if e.Engine == d.Engine && e.Port == port {
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
	case tunnel.EngineMySQL, tunnel.EngineMongoDB:
		return "argus_probe"
	case tunnel.EngineKafka:
		return tunnel.KafkaAllTopics
	}
	return "postgres"
}

func (a *API) discoverDatabases(ctx context.Context, actor, hostname string, d tunnel.GrantDiscover) DiscoverAnswer {
	ans := DiscoverAnswer{Databases: []DiscoveredDB{}}
	if a.Tunnels == nil || a.Tunnels.GrantKey == nil {
		ans.State, ans.Error = "failed", "this control plane cannot reach agents: set signing_secret"
		return ans
	}
	agentHost, err := a.agentOf(ctx, hostname)
	if err != nil {
		ans.State, ans.Error = "failed", "could not find the agent on "+hostname+": "+err.Error()
		return ans
	}
	g := tunnel.NewGrant(actor, agentHost, "discover", "")
	g.Service = tunnel.DiscoverService
	g.Discover = &d
	tok, err := tunnel.SignGrant(a.Tunnels.grantKeyFor(ctx), g)
	if err != nil {
		ans.State, ans.Error = "failed", err.Error()
		return ans
	}
	octx, cancel := context.WithTimeout(ctx, discoverWait)
	defer cancel()
	st, open, err := a.Tunnels.OpenWith(octx, agentHost, tunnel.Header{Service: tunnel.DiscoverService, Grant: tok})
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
		ans.Version, ans.Server, ans.Configured, ans.Truncated = res.Version, res.Server, res.Configured, res.Truncated
		ans.Host, ans.Port, ans.Places, ans.Tried = res.Host, res.Port, res.Places, res.Tried
		for _, p := range res.Poolers {
			dp := DiscoveredPooler{Config: p.Config, Port: p.Port, Targets: []PoolerTargetRef{}}
			for _, t := range p.Targets {
				dp.Targets = append(dp.Targets, PoolerTargetRef{PoolerTarget: t})
			}
			ans.Poolers = append(ans.Poolers, dp)
		}
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

// matchPoolerTargets names the server in the inventory each PgBouncer target
// on another server goes to: by its address, its hostname (or the first
// label of it) or an address its agent reported. A target on the PgBouncer's
// own server, or one nobody matches, is left as it is.
func (a *API) matchPoolerTargets(ctx context.Context, self string, ps []DiscoveredPooler) {
	assets, err := a.store.Assets(ctx)
	if err != nil {
		return
	}
	addrs, _ := a.store.AgentAddresses(ctx)
	byKey := map[string]Asset{}
	put := func(k string, as Asset) {
		k = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(k), "."))
		if k == "" {
			return
		}
		if _, ok := byKey[k]; !ok {
			byKey[k] = as
		}
	}
	for _, as := range assets {
		if as.ID == self {
			continue
		}
		put(as.Address, as)
		put(as.Hostname, as)
		put(as.AgentHostname, as)
		for _, ad := range addrs[as.ID] {
			ip, _, _ := strings.Cut(ad.Address, "/")
			put(ip, as)
		}
	}
	for _, as := range assets {
		if as.ID == self {
			continue
		}
		short, _, _ := strings.Cut(as.Hostname, ".")
		put(short, as)
	}
	for i := range ps {
		for j := range ps[i].Targets {
			t := &ps[i].Targets[j]
			if t.Local() {
				continue
			}
			h := strings.ToLower(strings.TrimSuffix(t.Host, "."))
			as, ok := byKey[h]
			if !ok {
				short, _, _ := strings.Cut(h, ".")
				as, ok = byKey[short]
			}
			if ok {
				t.AssetID, t.AssetName = as.ID, as.Hostname
				if as.Alias != "" {
					t.AssetName = as.Alias
				}
			}
		}
	}
}
