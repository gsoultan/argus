package control

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

/*
What runs on a server, as its agent last reported it.

The agent scans read-only and collects no secrets (see
internal/agent/apps.go). Here the report is bounded on the way in -- an
agent is a credentialed caller, but it is still input -- and served to the
server page, where a Postgres it found can be added as a database in one
step.
*/

// AppInventory mirrors agent.AppInventory, in the agent's spelling.
type AppInventory struct {
	CheckedAt    time.Time       `json:"checked_at"`
	Applications []DiscoveredApp `json:"applications"`
	Listeners    []AppListener   `json:"listeners"`
	Containers   []AppContainer  `json:"containers"`
	Services     []string        `json:"services"`
	Sources      []string        `json:"sources"`
}

// DiscoveredApp is one known application on a server.
type DiscoveredApp struct {
	Kind      string       `json:"kind"`
	Name      string       `json:"name"`
	Version   string       `json:"version,omitempty"`
	Running   bool         `json:"running"`
	Listen    []AppAddress `json:"listen,omitempty"`
	Process   string       `json:"process,omitempty"`
	Unit      string       `json:"unit,omitempty"`
	Package   string       `json:"package,omitempty"`
	Container string       `json:"container,omitempty"`
	Image     string       `json:"image,omitempty"`
	Evidence  []string     `json:"evidence"`
}

// AppAddress is where an application accepts connections.
type AppAddress struct {
	Address string `json:"address"`
	Port    int    `json:"port"`
}

// AppListener is one listening TCP socket on a server.
type AppListener struct {
	Address string `json:"address"`
	Port    int    `json:"port"`
	Process string `json:"process,omitempty"`
	Unit    string `json:"unit,omitempty"`
	Kind    string `json:"kind,omitempty"`
}

// AppContainer is one container on a server.
type AppContainer struct {
	Name    string       `json:"name"`
	Image   string       `json:"image"`
	State   string       `json:"state"`
	Runtime string       `json:"runtime"`
	Ports   []AppAddress `json:"ports,omitempty"`
	Kind    string       `json:"kind,omitempty"`
}

// Bounds on what one report may hold. A busy host has a few dozen listeners
// and a few hundred services; anything past these is cut, not refused, so a
// host that really is that large still reports what it can.
const (
	maxApps       = 200
	maxListeners  = 512
	maxContainers = 512
	maxServices   = 1024
	maxAppField   = 256
	maxAppAddrs   = 32
)

func cut(s string) string {
	if len(s) > maxAppField {
		return s[:maxAppField]
	}
	return s
}

func cutStrings(in []string, n int) []string {
	if len(in) > n {
		in = in[:n]
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, cut(s))
	}
	return out
}

func cutAddrs(in []AppAddress) []AppAddress {
	if len(in) > maxAppAddrs {
		in = in[:maxAppAddrs]
	}
	out := make([]AppAddress, 0, len(in))
	for _, a := range in {
		if a.Port < 1 || a.Port > 65535 {
			continue
		}
		out = append(out, AppAddress{Address: cut(a.Address), Port: a.Port})
	}
	return out
}

// bounded returns the inventory with every list and string trimmed to what a
// row holds, and every list non-nil.
func (inv *AppInventory) bounded() *AppInventory {
	if inv == nil {
		return nil
	}
	out := &AppInventory{
		CheckedAt:    inv.CheckedAt,
		Applications: []DiscoveredApp{},
		Listeners:    []AppListener{},
		Containers:   []AppContainer{},
		Services:     cutStrings(inv.Services, maxServices),
		Sources:      cutStrings(inv.Sources, 16),
	}
	for i, a := range inv.Applications {
		if i == maxApps {
			break
		}
		out.Applications = append(out.Applications, DiscoveredApp{
			Kind: cut(a.Kind), Name: cut(a.Name), Version: cut(a.Version), Running: a.Running,
			Listen: cutAddrs(a.Listen), Process: cut(a.Process), Unit: cut(a.Unit),
			Package: cut(a.Package), Container: cut(a.Container), Image: cut(a.Image),
			Evidence: cutStrings(a.Evidence, 8),
		})
	}
	for i, l := range inv.Listeners {
		if i == maxListeners {
			break
		}
		if l.Port < 1 || l.Port > 65535 {
			continue
		}
		out.Listeners = append(out.Listeners, AppListener{
			Address: cut(l.Address), Port: l.Port, Process: cut(l.Process), Unit: cut(l.Unit), Kind: cut(l.Kind),
		})
	}
	for i, c := range inv.Containers {
		if i == maxContainers {
			break
		}
		out.Containers = append(out.Containers, AppContainer{
			Name: cut(c.Name), Image: cut(c.Image), State: cut(c.State), Runtime: cut(c.Runtime),
			Ports: cutAddrs(c.Ports), Kind: cut(c.Kind),
		})
	}
	return out
}

// ServerApplications is what the server page shows.
type ServerApplications struct {
	// Reported is false when the agent has not sent an inventory: it is
	// older than application discovery, or has not checked in.
	Reported     bool              `json:"reported"`
	CheckedAt    *time.Time        `json:"checkedAt"`
	Applications []ServerApp       `json:"applications"`
	Listeners    []ServerAppListen `json:"listeners"`
	Containers   []ServerAppBox    `json:"containers"`
	Services     []string          `json:"services"`
	Sources      []string          `json:"sources"`
}

// ServerApp is one application on the server page.
type ServerApp struct {
	Kind      string            `json:"kind"`
	Name      string            `json:"name"`
	Version   string            `json:"version"`
	Running   bool              `json:"running"`
	Listen    []ServerAppListen `json:"listen"`
	Process   string            `json:"process"`
	Unit      string            `json:"unit"`
	Package   string            `json:"package"`
	Container string            `json:"container"`
	Image     string            `json:"image"`
	Evidence  []string          `json:"evidence"`
}

// ServerAppListen is an address something listens on, and whether it is
// reachable from other machines or only from the server itself.
type ServerAppListen struct {
	Address string `json:"address"`
	Port    int    `json:"port"`
	// LocalOnly is a loopback address: nothing off the server can connect.
	LocalOnly bool   `json:"localOnly"`
	Process   string `json:"process,omitempty"`
	Unit      string `json:"unit,omitempty"`
	Kind      string `json:"kind,omitempty"`
}

// ServerAppBox is one container on the server page.
type ServerAppBox struct {
	Name    string            `json:"name"`
	Image   string            `json:"image"`
	State   string            `json:"state"`
	Runtime string            `json:"runtime"`
	Ports   []ServerAppListen `json:"ports"`
	Kind    string            `json:"kind"`
}

func listenOf(a AppAddress) ServerAppListen {
	ip := net.ParseIP(a.Address)
	return ServerAppListen{Address: a.Address, Port: a.Port, LocalOnly: ip != nil && ip.IsLoopback()}
}

func listensOf(in []AppAddress) []ServerAppListen {
	out := make([]ServerAppListen, 0, len(in))
	for _, a := range in {
		out = append(out, listenOf(a))
	}
	return out
}

// ApplicationsFor reads what the agent on asset id reported running.
func (s *Store) ApplicationsFor(ctx context.Context, id string) (ServerApplications, error) {
	var raw []byte
	err := s.db(ctx).QueryRow(ctx, `
		SELECT ag.applications
		  FROM assets a
		  LEFT JOIN LATERAL (
		       SELECT applications FROM agents g
		        WHERE g.hostname = a.hostname OR g.hostname = a.agent_hostname
		           OR g.hostname = split_part(a.hostname, '.', 1)
		        ORDER BY g.last_seen_at DESC NULLS LAST
		        LIMIT 1) ag ON TRUE
		 WHERE a.id::text = $1 AND a.archived_at IS NULL`, id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return ServerApplications{}, ErrAssetNotFound
	}
	if err != nil {
		return ServerApplications{}, err
	}
	out := ServerApplications{
		Applications: []ServerApp{}, Listeners: []ServerAppListen{},
		Containers: []ServerAppBox{}, Services: []string{}, Sources: []string{},
	}
	var inv AppInventory
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &inv) != nil {
		return out, nil // not reported, or a report that does not parse
	}
	out.Reported = true
	if !inv.CheckedAt.IsZero() {
		at := inv.CheckedAt
		out.CheckedAt = &at
	}
	for _, a := range inv.Applications {
		out.Applications = append(out.Applications, ServerApp{
			Kind: a.Kind, Name: a.Name, Version: a.Version, Running: a.Running,
			Listen: listensOf(a.Listen), Process: a.Process, Unit: a.Unit, Package: a.Package,
			Container: a.Container, Image: a.Image, Evidence: nonNilStrings(a.Evidence),
		})
	}
	for _, l := range inv.Listeners {
		sl := listenOf(AppAddress{Address: l.Address, Port: l.Port})
		sl.Process, sl.Unit, sl.Kind = l.Process, l.Unit, l.Kind
		out.Listeners = append(out.Listeners, sl)
	}
	for _, c := range inv.Containers {
		out.Containers = append(out.Containers, ServerAppBox{
			Name: c.Name, Image: c.Image, State: c.State, Runtime: c.Runtime,
			Ports: listensOf(c.Ports), Kind: c.Kind,
		})
	}
	out.Services = nonNilStrings(inv.Services)
	out.Sources = nonNilStrings(inv.Sources)
	return out, nil
}

// getApplications lists what runs on a server. Readable by whoever sees the
// whole fleet: a map of ports and versions is what an attacker would draw
// first, so an operator who can reach one server does not get it.
func (a *API) getApplications(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden,
			"seeing what runs on a host requires the admin, owner or auditor role")
		return
	}
	out, err := a.store.ApplicationsFor(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failAsset(w, "applications", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
