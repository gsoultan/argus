package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
WireGuard networks between servers, set up through their agents.

An admin draws a network on the Networks page: a name, a range such as
10.88.0.0/24, a UDP port, a shape and the servers in it.

  - mesh: every member reaches every other directly. Each peer carries only
    the other member's own address (/32).
  - hub: one member, the hub, passes traffic between the others. A spoke
    has one peer, the hub, carrying the whole range; the hub has every
    spoke and forwards between them.

Either shape can link sites: a member may route private ranges behind it
(an office LAN, a cloud VPC), and the others reach them through it. The
member forwards, and the console says what the site's own router or the
cloud's route table needs for the way back.

Nothing reaches a server until a second admin approves it, and every later
change (members, range, port, removing it) waits the same way: the draft is
kept in pending and only an approved network is sent to agents. When nobody
else could approve, the person who asked may, as everywhere else in Argus
(self_approval.go).

Each heartbeat (heartbeatWGNet) carries the member's public key for each of
its networks and how its last change went, and is answered with the whole
set of networks the server should be in (agent/wgnet.go). A peer is added
once its key has been reported, so a network comes together over a few
heartbeats. The agent applies every change on a two-minute trial and undoes
it unless the server still reaches Argus afterwards.

A range is refused when it overlaps another Argus network, a WireGuard
interface Argus did not make, a cloud network or subnet in a connected
account, a Cloudflare private network route, or a member's own addresses:
any of those would send traffic the wrong way.
*/

// WGNetwork is a network as the console shows it.
type WGNetwork struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Shape string `json:"shape"`
	CIDR  string `json:"cidr"`
	Port  int    `json:"port"`
	Iface string `json:"iface"`
	// State is waiting (never approved; on no server) or active.
	State   string     `json:"state"`
	Members []WGMember `json:"members"`
	// Pending is a change waiting for approval.
	Pending     *WGNetworkDraft `json:"pending,omitempty"`
	PendingBy   string          `json:"pendingBy,omitempty"`
	PendingAt   *time.Time      `json:"pendingAt,omitempty"`
	RequestedBy string          `json:"requestedBy"`
	ApprovedBy  string          `json:"approvedBy,omitempty"`
	ApprovedAt  *time.Time      `json:"approvedAt,omitempty"`
	Generation  int             `json:"generation"`
	CreatedAt   time.Time       `json:"createdAt"`
	// ApproveOwn is set for the person who asked when nobody else could
	// approve the change.
	ApproveOwn bool `json:"approveOwn,omitempty"`
}

// WGMember is an approved member and how it is doing.
type WGMember struct {
	AssetID  string `json:"assetId"`
	Hostname string `json:"hostname"`
	Address  string `json:"address"`
	Hub      bool   `json:"hub,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	// Routes are the private ranges behind it that it routes for the others.
	Routes    []string `json:"routes,omitempty"`
	PublicKey string   `json:"publicKey,omitempty"`
	// State is no-agent, unsupported, waiting-key, applying, trial,
	// applied, failed or rolled-back.
	State   string     `json:"state"`
	Message string     `json:"message,omitempty"`
	At      *time.Time `json:"at,omitempty"`
}

// WGNetworkDraft is a network as someone asked for it.
type WGNetworkDraft struct {
	Name    string          `json:"name"`
	Shape   string          `json:"shape"`
	CIDR    string          `json:"cidr"`
	Port    int             `json:"port"`
	Members []WGMemberDraft `json:"members"`
	// Remove asks for the network to be taken off every server.
	Remove bool `json:"remove,omitempty"`
}

// WGMemberDraft is one server in a draft. Address is filled in when empty.
type WGMemberDraft struct {
	AssetID  string   `json:"assetId"`
	Hostname string   `json:"hostname,omitempty"`
	Address  string   `json:"address,omitempty"`
	Hub      bool     `json:"hub,omitempty"`
	Endpoint string   `json:"endpoint,omitempty"`
	Routes   []string `json:"routes,omitempty"`
}

// ErrInvalidWGNetwork is a draft that cannot be set up, with why.
type ErrInvalidWGNetwork struct{ msg string }

func (e ErrInvalidWGNetwork) Error() string { return e.msg }

var (
	ErrNoWGNetwork  = errors.New("no such WireGuard network")
	ErrWGNoPending  = errors.New("there is no change waiting for approval")
	ErrWGPending    = errors.New("a change to this network is already waiting for approval: approve or decline it first")
	ErrWGNameExists = errors.New("another network already has that name")
)

const (
	wgDefaultPort = 51820
	wgMaxMembers  = 64
	wgMaxNetworks = 50
	// wgKeepalive keeps a link through NAT open; WireGuard's own advice.
	wgKeepalive = 25
	wgMaxRoutes = 16
)

var (
	// The same as the agent's (agent/wgnet.go): a name it will write into a
	// file's comment, a host it will put in Endpoint.
	reWGNetName = regexp.MustCompile(`^[\p{L}\p{N} ._()-]{1,80}$`)
	reWGHost    = regexp.MustCompile(`^[A-Za-z0-9.-]{1,253}$`)
	reWGIface   = regexp.MustCompile(`^argus[0-9a-f]{6}$`)

	wgPrivate = []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("100.64.0.0/10"),
	}
)

func invalidWG(format string, args ...any) error {
	return ErrInvalidWGNetwork{fmt.Sprintf(format, args...)}
}

/* ── Checking a draft ────────────────────────────────────────────────────── */

// wgTaken is everything a new range must not overlap, and the ports each
// server already uses for another Argus network.
type wgTaken struct {
	ranges []wgRange
	ports  map[string]map[int]string // asset → port → network name
	// own is each member's own addresses, by asset.
	own map[string][]netip.Prefix
}

type wgRange struct {
	prefix netip.Prefix
	what   string
}

func prefixesOverlap(a, b netip.Prefix) bool {
	return a.Contains(b.Masked().Addr()) || b.Contains(a.Masked().Addr())
}

// parseLoosePrefix reads "10.0.0.0/16", "10.0.0.5/24" or a bare "10.0.0.5".
func parseLoosePrefix(s string) (netip.Prefix, bool) {
	s = strings.TrimSpace(s)
	if p, err := netip.ParsePrefix(s); err == nil && p.Addr().Is4() {
		return p, true
	}
	if a, err := netip.ParseAddr(s); err == nil && a.Is4() {
		return netip.PrefixFrom(a, 32), true
	}
	return netip.Prefix{}, false
}

// checkWGDraft checks d and fills in what it leaves out: the port, each
// member's address and hostname. hosts is the hostname of every live asset.
func checkWGDraft(d *WGNetworkDraft, hosts map[string]string, taken wgTaken) error {
	d.Name = strings.TrimSpace(d.Name)
	if !reWGNetName.MatchString(d.Name) {
		return invalidWG("give the network a name of up to 80 letters, digits, spaces and . _ ( ) -")
	}
	switch d.Shape {
	case "mesh", "hub":
	default:
		return invalidWG("the shape is mesh or hub")
	}
	if d.Port == 0 {
		d.Port = wgDefaultPort
	}
	if d.Port < 1024 || d.Port > 65535 {
		return invalidWG("the port is between 1024 and 65535")
	}
	d.CIDR = strings.TrimSpace(d.CIDR)
	pfx, err := netip.ParsePrefix(d.CIDR)
	if err != nil || !pfx.Addr().Is4() {
		return invalidWG("the range is an IPv4 range such as 10.88.0.0/24")
	}
	if pfx.Bits() < 16 || pfx.Bits() > 29 {
		return invalidWG("the range is between a /16 and a /29")
	}
	if pfx != pfx.Masked() {
		return invalidWG("%s is not the start of its range: did you mean %s?", d.CIDR, pfx.Masked())
	}
	if !slices.ContainsFunc(wgPrivate, func(p netip.Prefix) bool { return p.Contains(pfx.Addr()) && p.Bits() <= pfx.Bits() }) {
		return invalidWG("use a private range (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16 or 100.64.0.0/10)")
	}
	d.CIDR = pfx.String()
	for _, t := range taken.ranges {
		if prefixesOverlap(pfx, t.prefix) {
			return invalidWG("%s overlaps %s (%s): pick a range nothing else uses", d.CIDR, t.what, t.prefix)
		}
	}

	if len(d.Members) < 2 {
		return invalidWG("a network needs at least two servers")
	}
	if len(d.Members) > wgMaxMembers {
		return invalidWG("a network has at most %d servers", wgMaxMembers)
	}
	first, last := pfx.Masked().Addr().Next(), lastAddr(pfx)
	seen, used := map[string]bool{}, map[netip.Addr]bool{}
	hubs := 0
	for i := range d.Members {
		m := &d.Members[i]
		m.AssetID = strings.TrimSpace(m.AssetID)
		host, ok := hosts[m.AssetID]
		if !ok {
			return invalidWG("server %s is not in the inventory", m.AssetID)
		}
		m.Hostname = host
		if seen[m.AssetID] {
			return invalidWG("%s is in the network twice", host)
		}
		seen[m.AssetID] = true
		if d.Shape == "mesh" {
			m.Hub = false
		}
		if m.Hub {
			hubs++
		}
		m.Endpoint = strings.TrimSpace(m.Endpoint)
		if m.Endpoint != "" && (!reWGHost.MatchString(m.Endpoint) || strings.HasPrefix(m.Endpoint, "-")) {
			return invalidWG("%s: the address others reach it on is a hostname or IPv4 address, without the port", host)
		}
		if name, ok := taken.ports[m.AssetID][d.Port]; ok {
			return invalidWG("%s already uses UDP port %d for the network %s", host, d.Port, name)
		}
		for _, own := range taken.own[m.AssetID] {
			if prefixesOverlap(pfx, own) {
				return invalidWG("%s overlaps %s's own address %s", d.CIDR, host, own)
			}
		}
		if m.Address = strings.TrimSpace(m.Address); m.Address != "" {
			a, err := netip.ParseAddr(m.Address)
			if err != nil || !pfx.Contains(a) || a.Less(first) || last.Less(a) {
				return invalidWG("%s: %s is not a usable address in %s", host, m.Address, d.CIDR)
			}
			if used[a] {
				return invalidWG("%s: %s is given to two servers", host, m.Address)
			}
			used[a] = true
		}
	}
	if d.Shape == "hub" {
		if hubs != 1 {
			return invalidWG("a hub network has exactly one hub")
		}
		for _, m := range d.Members {
			if m.Hub && m.Endpoint == "" {
				return invalidWG("the hub, %s, needs an address the others reach it on", m.Hostname)
			}
		}
	} else {
		// Two members that only dial out never meet.
		var quiet []string
		for _, m := range d.Members {
			if m.Endpoint == "" {
				quiet = append(quiet, m.Hostname)
			}
		}
		if len(quiet) > 1 {
			return invalidWG("%s and %s both lack an address the other can reach: give at least one of them one, or make it a hub network",
				quiet[0], quiet[1])
		}
	}
	if err := checkWGRoutes(d, pfx, taken); err != nil {
		return err
	}
	// Addresses for the members that left theirs out, in order.
	next := first
	for i := range d.Members {
		if d.Members[i].Address != "" {
			continue
		}
		for used[next] {
			next = next.Next()
		}
		if !pfx.Contains(next) || last.Less(next) {
			return invalidWG("%s has too few addresses for %d servers", d.CIDR, len(d.Members))
		}
		used[next] = true
		d.Members[i].Address = next.String()
	}
	return nil
}

// checkWGRoutes checks the ranges members route for the others: private,
// not the network's own, not routed by two members, not another Argus
// network, and not holding another member's own address (it would send
// that member's own traffic into the tunnel).
func checkWGRoutes(d *WGNetworkDraft, network netip.Prefix, taken wgTaken) error {
	type owner struct {
		p    netip.Prefix
		host string
	}
	var all []owner
	for i := range d.Members {
		m := &d.Members[i]
		if len(m.Routes) > wgMaxRoutes {
			return invalidWG("%s routes more than %d ranges", m.Hostname, wgMaxRoutes)
		}
		clean := make([]string, 0, len(m.Routes))
		for _, r := range m.Routes {
			r = strings.TrimSpace(r)
			if r == "" {
				continue
			}
			p, err := netip.ParsePrefix(r)
			if err != nil || !p.Addr().Is4() {
				return invalidWG("%s: %q is not an IPv4 range such as 192.168.10.0/24", m.Hostname, r)
			}
			if p != p.Masked() {
				return invalidWG("%s: %s is not the start of its range: did you mean %s?", m.Hostname, r, p.Masked())
			}
			if !slices.ContainsFunc(wgPrivate, func(b netip.Prefix) bool { return b.Bits() <= p.Bits() && b.Contains(p.Addr()) }) {
				return invalidWG("%s: %s is not a private range; a site link carries private ranges only", m.Hostname, r)
			}
			if prefixesOverlap(p, network) {
				return invalidWG("%s: %s overlaps the network's own range %s", m.Hostname, r, network)
			}
			for _, o := range all {
				if prefixesOverlap(p, o.p) {
					return invalidWG("%s and %s both route %s", o.host, m.Hostname, p)
				}
			}
			for _, t := range taken.ranges {
				if strings.HasPrefix(t.what, "the ") && prefixesOverlap(p, t.prefix) {
					return invalidWG("%s: %s overlaps %s (%s)", m.Hostname, r, t.what, t.prefix)
				}
			}
			for _, other := range d.Members {
				if other.AssetID == m.AssetID {
					continue
				}
				for _, own := range taken.own[other.AssetID] {
					if p.Contains(own.Addr()) {
						return invalidWG("%s is inside %s, which %s would route: it reaches that range directly, so leave it out of the network or route a narrower range",
							other.Hostname, p, m.Hostname)
					}
				}
			}
			all = append(all, owner{p, m.Hostname})
			clean = append(clean, p.String())
		}
		m.Routes = clean
	}
	return nil
}

// lastAddr is the last usable address in p, before its broadcast address.
func lastAddr(p netip.Prefix) netip.Addr {
	a := p.Masked().Addr().As4()
	n := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	n |= (1 << (32 - p.Bits())) - 1
	n--
	return netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
}

// wgTakenFor reads what a draft for network id (or "" for a new one) must
// not clash with.
func (s *Store) wgTakenFor(ctx context.Context, id string) (wgTaken, error) {
	t := wgTaken{ports: map[string]map[int]string{}, own: map[string][]netip.Prefix{}}
	// Other Argus networks, as they are and as a waiting change would make them.
	rows, err := s.db(ctx).Query(ctx, `
		SELECT n.name, n.cidr, n.port, n.pending, coalesce(array_agg(m.asset_id::text) FILTER (WHERE m.asset_id IS NOT NULL), '{}')
		  FROM wg_networks n LEFT JOIN wg_members m ON m.network_id = n.id
		 WHERE n.id::text <> $1
		 GROUP BY n.id`, id)
	if err != nil {
		return t, err
	}
	addPort := func(asset string, port int, name string) {
		if t.ports[asset] == nil {
			t.ports[asset] = map[int]string{}
		}
		t.ports[asset][port] = name
	}
	for rows.Next() {
		var (
			name, cidr string
			port       int
			pending    []byte
			members    []string
		)
		if err := rows.Scan(&name, &cidr, &port, &pending, &members); err != nil {
			rows.Close()
			return t, err
		}
		if p, ok := parseLoosePrefix(cidr); ok {
			t.ranges = append(t.ranges, wgRange{p, "the Argus network " + name})
		}
		for _, m := range members {
			addPort(m, port, name)
		}
		var d WGNetworkDraft
		if len(pending) > 0 && json.Unmarshal(pending, &d) == nil && !d.Remove {
			if p, ok := parseLoosePrefix(d.CIDR); ok {
				t.ranges = append(t.ranges, wgRange{p, "the waiting change to the Argus network " + name})
			}
			for _, m := range d.Members {
				addPort(m.AssetID, d.Port, name)
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return t, err
	}

	// WireGuard interfaces Argus did not make, and every server's own
	// addresses, from what the agents last said.
	rows, err = s.db(ctx).Query(ctx, `
		SELECT a.id::text, a.hostname, ag.vpn, ag.addresses
		  FROM assets a
		  JOIN LATERAL (
		       SELECT vpn, addresses FROM agents g
		        WHERE g.hostname = a.hostname OR g.hostname = a.agent_hostname
		           OR g.hostname = split_part(a.hostname, '.', 1)
		        ORDER BY g.last_seen_at DESC NULLS LAST LIMIT 1) ag ON TRUE
		 WHERE a.archived_at IS NULL`)
	if err != nil {
		return t, err
	}
	for rows.Next() {
		var (
			asset, host string
			vpn         []byte
			addrs       []string
		)
		if err := rows.Scan(&asset, &host, &vpn, &addrs); err != nil {
			rows.Close()
			return t, err
		}
		for _, a := range addrs {
			if p, ok := parseLoosePrefix(a); ok {
				t.own[asset] = append(t.own[asset], p)
			}
		}
		var scan VPNScan
		if len(vpn) > 0 && json.Unmarshal(vpn, &scan) == nil {
			for _, wi := range scan.WireGuard {
				if reWGIface.MatchString(wi.Name) {
					continue
				}
				for _, a := range wi.Addresses {
					if p, ok := parseLoosePrefix(a); ok {
						t.ranges = append(t.ranges, wgRange{p.Masked(), "WireGuard " + wi.Name + " on " + host})
					}
				}
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return t, err
	}

	// Cloud networks and subnets: their range is the resource's size.
	for _, k := range []cloud.Kind{cloud.KindNetwork, cloud.KindSubnet} {
		items, err := s.CloudResources(ctx, k)
		if err != nil {
			return t, err
		}
		for _, it := range items {
			if p, ok := parseLoosePrefix(it.Resource.Size); ok && p.Bits() < 32 {
				t.ranges = append(t.ranges, wgRange{p.Masked(), string(k) + " " + it.Resource.Name + " in " + it.AccountName})
			}
		}
	}
	// Cloudflare private network routes.
	cfs, err := s.CloudflareAccounts(ctx)
	if err != nil {
		return t, err
	}
	for _, c := range cfs {
		if c.snap == nil {
			continue
		}
		for _, r := range c.snap.Routes {
			if p, ok := parseLoosePrefix(r.Network); ok {
				t.ranges = append(t.ranges, wgRange{p.Masked(), "a Cloudflare route to " + r.TunnelName})
			}
		}
	}
	return t, nil
}

// wgHosts is every live asset's hostname, by id.
func (s *Store) wgHosts(ctx context.Context) (map[string]string, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT id::text, hostname FROM assets WHERE archived_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, h string
		if err := rows.Scan(&id, &h); err != nil {
			return nil, err
		}
		out[id] = h
	}
	return out, rows.Err()
}

/* ── The store ───────────────────────────────────────────────────────────── */

const wgNetworkColumns = `id::text, name, shape, cidr, port, iface, state, pending, pending_by, pending_at,
	requested_by, approved_by, approved_at, generation, created_at`

func scanWGNetwork(row pgx.Row) (WGNetwork, error) {
	var (
		n       WGNetwork
		pending []byte
	)
	err := row.Scan(&n.ID, &n.Name, &n.Shape, &n.CIDR, &n.Port, &n.Iface, &n.State, &pending, &n.PendingBy, &n.PendingAt,
		&n.RequestedBy, &n.ApprovedBy, &n.ApprovedAt, &n.Generation, &n.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return n, ErrNoWGNetwork
	}
	if err != nil {
		return n, err
	}
	if len(pending) > 0 {
		var d WGNetworkDraft
		if json.Unmarshal(pending, &d) == nil {
			n.Pending = &d
		}
	}
	n.Members = []WGMember{}
	return n, nil
}

// wgAgentState is agents.wgnet_state.
type wgAgentState struct {
	Supported bool      `json:"supported"`
	Reason    string    `json:"reason,omitempty"`
	Run       *WGNetRun `json:"run,omitempty"`
}

// WGNetworks reads every network with its members and how each is doing.
func (s *Store) WGNetworks(ctx context.Context) ([]WGNetwork, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT `+wgNetworkColumns+` FROM wg_networks ORDER BY lower(name)`)
	if err != nil {
		return nil, err
	}
	var out []WGNetwork
	byID := map[string]int{}
	for rows.Next() {
		n, err := scanWGNetwork(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		byID[n.ID] = len(out)
		out = append(out, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if out == nil {
		return []WGNetwork{}, nil
	}
	mrows, err := s.db(ctx).Query(ctx, `
		SELECT m.network_id::text, m.asset_id::text, a.hostname, m.address, m.hub, m.endpoint, m.routes, m.public_key, ag.wgnet_state
		  FROM wg_members m JOIN assets a ON a.id = m.asset_id
		  LEFT JOIN LATERAL (
		       SELECT wgnet_state FROM agents g
		        WHERE g.hostname = a.hostname OR g.hostname = a.agent_hostname
		           OR g.hostname = split_part(a.hostname, '.', 1)
		        ORDER BY g.last_seen_at DESC NULLS LAST LIMIT 1) ag ON TRUE
		 ORDER BY m.network_id, m.address`)
	if err != nil {
		return nil, err
	}
	defer mrows.Close()
	for mrows.Next() {
		var (
			netID string
			m     WGMember
			state []byte
		)
		if err := mrows.Scan(&netID, &m.AssetID, &m.Hostname, &m.Address, &m.Hub, &m.Endpoint, &m.Routes, &m.PublicKey, &state); err != nil {
			return nil, err
		}
		i, ok := byID[netID]
		if !ok {
			continue
		}
		var st *wgAgentState
		if len(state) > 0 {
			st = &wgAgentState{}
			if json.Unmarshal(state, st) != nil {
				st = nil
			}
		}
		memberState(&m, netID, st)
		out[i].Members = append(out[i].Members, m)
	}
	// Addresses sort as text otherwise: 10.0.0.10 before 10.0.0.2.
	for i := range out {
		sort.SliceStable(out[i].Members, func(a, b int) bool {
			x, _ := netip.ParseAddr(out[i].Members[a].Address)
			y, _ := netip.ParseAddr(out[i].Members[b].Address)
			return x.Less(y)
		})
	}
	return out, mrows.Err()
}

// memberState says how a member is doing from what its agent last said.
func memberState(m *WGMember, network string, st *wgAgentState) {
	switch {
	case st == nil:
		m.State, m.Message = "no-agent", "its agent has not reported on WireGuard networks; it may need updating"
	case !st.Supported:
		m.State, m.Message = "unsupported", st.Reason
	case m.PublicKey == "":
		m.State, m.Message = "waiting-key", "waiting for the server to make its key"
	case st.Run == nil:
		m.State = "applying"
	default:
		r := st.Run
		m.At = &r.At
		if slices.Contains(r.Networks, network) || r.State == "failed" || r.State == "rolled-back" {
			m.State, m.Message = r.State, r.Message
		} else {
			m.State = "applying"
		}
	}
}

func (s *Store) wgNetwork(ctx context.Context, id string) (WGNetwork, error) {
	if _, err := uuid.Parse(id); err != nil {
		return WGNetwork{}, ErrNoWGNetwork
	}
	return scanWGNetwork(s.db(ctx).QueryRow(ctx, `SELECT `+wgNetworkColumns+` FROM wg_networks WHERE id = $1`, id))
}

// CreateWGNetwork keeps a new network, waiting for approval.
func (s *Store) CreateWGNetwork(ctx context.Context, d WGNetworkDraft, by string) (WGNetwork, error) {
	var count int
	if err := s.db(ctx).QueryRow(ctx, `SELECT count(*) FROM wg_networks`).Scan(&count); err != nil {
		return WGNetwork{}, err
	}
	if count >= wgMaxNetworks {
		return WGNetwork{}, invalidWG("there are already %d networks", wgMaxNetworks)
	}
	b, err := json.Marshal(d)
	if err != nil {
		return WGNetwork{}, err
	}
	// The interface is named after the id; a clash on six hex digits is
	// rare and another id settles it.
	for range 5 {
		id := uuid.New()
		iface := "argus" + strings.ReplaceAll(id.String(), "-", "")[:6]
		n, err := scanWGNetwork(s.db(ctx).QueryRow(ctx, `
			INSERT INTO wg_networks (id, name, shape, cidr, port, iface, pending, pending_by, pending_at, requested_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, now(), $8)
			RETURNING `+wgNetworkColumns, id, d.Name, d.Shape, d.CIDR, d.Port, iface, b, by))
		if err != nil && isUniqueViolation(err) {
			if strings.Contains(err.Error(), "uq_wg_networks_name") {
				return n, ErrWGNameExists
			}
			continue
		}
		return n, err
	}
	return WGNetwork{}, errors.New("could not name the network's interface")
}

// ProposeWGChange keeps a change to an approved network, waiting for
// approval.
func (s *Store) ProposeWGChange(ctx context.Context, id string, d WGNetworkDraft, by string) (WGNetwork, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return WGNetwork{}, err
	}
	n, err := scanWGNetwork(s.db(ctx).QueryRow(ctx, `
		UPDATE wg_networks SET pending = $2::jsonb, pending_by = $3, pending_at = now(), updated_at = now()
		 WHERE id = $1 AND pending IS NULL
		RETURNING `+wgNetworkColumns, id, b, by))
	if errors.Is(err, ErrNoWGNetwork) {
		if _, gerr := s.wgNetwork(ctx, id); gerr == nil {
			return n, ErrWGPending
		}
	}
	return n, err
}

// ApproveWGNetwork makes the waiting change the network. It returns the
// network as it was before, and whether the change removed it.
func (s *Store) ApproveWGNetwork(ctx context.Context, id, by string) (WGNetwork, bool, error) {
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return WGNetwork{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	before, err := scanWGNetwork(tx.QueryRow(ctx, `SELECT `+wgNetworkColumns+` FROM wg_networks WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return before, false, err
	}
	d := before.Pending
	if d == nil {
		return before, false, ErrWGNoPending
	}
	if d.Remove {
		if _, err := tx.Exec(ctx, `DELETE FROM wg_networks WHERE id = $1`, id); err != nil {
			return before, false, err
		}
		return before, true, tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE wg_networks SET name = $2, shape = $3, cidr = $4, port = $5, state = 'active',
		       pending = NULL, pending_by = '', pending_at = NULL, approved_by = $6, approved_at = now(),
		       generation = generation + 1, updated_at = now()
		 WHERE id = $1`, id, d.Name, d.Shape, d.CIDR, d.Port, by); err != nil {
		if isUniqueViolation(err) {
			return before, false, ErrWGNameExists
		}
		return before, false, err
	}
	// Members keep their keys; the ones left out are dropped, which takes
	// the network off them on their next heartbeat.
	keep := make([]string, 0, len(d.Members))
	for _, m := range d.Members {
		keep = append(keep, m.AssetID)
		if _, err := tx.Exec(ctx, `
			INSERT INTO wg_members (network_id, asset_id, address, hub, endpoint, routes) VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (network_id, asset_id) DO UPDATE SET address = $3, hub = $4, endpoint = $5, routes = $6`,
			id, m.AssetID, m.Address, m.Hub, m.Endpoint, nonNilStrings(m.Routes)); err != nil {
			return before, false, err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM wg_members WHERE network_id = $1 AND NOT (asset_id::text = ANY($2))`, id, keep); err != nil {
		return before, false, err
	}
	return before, false, tx.Commit(ctx)
}

// DeclineWGNetwork drops the waiting change; a network never approved goes
// with it. It returns the network as it was, and whether it was deleted.
func (s *Store) DeclineWGNetwork(ctx context.Context, id string) (WGNetwork, bool, error) {
	before, err := s.wgNetwork(ctx, id)
	if err != nil {
		return before, false, err
	}
	if before.Pending == nil {
		return before, false, ErrWGNoPending
	}
	if before.State == "waiting" {
		_, err := s.db(ctx).Exec(ctx, `DELETE FROM wg_networks WHERE id = $1 AND state = 'waiting'`, id)
		return before, true, err
	}
	_, err = s.db(ctx).Exec(ctx, `
		UPDATE wg_networks SET pending = NULL, pending_by = '', pending_at = NULL, updated_at = now() WHERE id = $1`, id)
	return before, false, err
}

/* ── The heartbeat ───────────────────────────────────────────────────────── */

// The agent's types (agent/wgnet.go), as the control plane reads and writes
// them.
type (
	WGNetBeat struct {
		Supported bool              `json:"supported"`
		Reason    string            `json:"reason,omitempty"`
		Keys      map[string]string `json:"keys,omitempty"`
		Run       *WGNetRun         `json:"run,omitempty"`
	}
	WGNetRun struct {
		ID       string    `json:"id"`
		Want     string    `json:"want"`
		State    string    `json:"state"`
		Message  string    `json:"message,omitempty"`
		At       time.Time `json:"at"`
		Networks []string  `json:"networks,omitempty"`
	}
	WGNetOrders struct {
		Networks []WGNetSpec `json:"networks"`
	}
	WGNetSpec struct {
		ID         string      `json:"id"`
		Name       string      `json:"name"`
		Iface      string      `json:"iface"`
		Address    string      `json:"address"`
		ListenPort int         `json:"listen_port"`
		Forward    bool        `json:"forward,omitempty"`
		OpenPort   bool        `json:"open_port,omitempty"`
		Peers      []WGNetPeer `json:"peers"`
	}
	WGNetPeer struct {
		PublicKey  string   `json:"public_key"`
		Endpoint   string   `json:"endpoint,omitempty"`
		AllowedIPs []string `json:"allowed_ips"`
		Keepalive  int      `json:"keepalive,omitempty"`
	}
)

// recordWGNetBeat keeps what the agent said. It returns the run it last
// said, to tell when one has just ended.
func (s *Store) recordWGNetBeat(ctx context.Context, host, assetID string, b WGNetBeat) (*WGNetRun, error) {
	st := wgAgentState{Supported: b.Supported, Reason: clip(b.Reason, 500), Run: b.Run}
	if st.Run != nil {
		r := *st.Run
		r.Message = clip(r.Message, 1000)
		if len(r.Networks) > 64 {
			r.Networks = r.Networks[:64]
		}
		st.Run = &r
	}
	nb, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	var prevRaw []byte
	err = s.db(ctx).QueryRow(ctx, `
		WITH old AS (SELECT hostname, wgnet_state FROM agents WHERE lower(hostname) = lower($1) FOR UPDATE)
		UPDATE agents g SET wgnet_state = $2::jsonb FROM old
		 WHERE g.hostname = old.hostname
		RETURNING old.wgnet_state`, host, nb).Scan(&prevRaw)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	var prev *WGNetRun
	if len(prevRaw) > 0 {
		var p wgAgentState
		if json.Unmarshal(prevRaw, &p) == nil {
			prev = p.Run
		}
	}
	for network, key := range b.Keys {
		if _, err := uuid.Parse(network); err != nil || !validWGPublicKey(key) {
			continue
		}
		if _, err := s.db(ctx).Exec(ctx, `
			UPDATE wg_members SET public_key = $3, key_at = now()
			 WHERE network_id = $1 AND asset_id = $2 AND public_key <> $3`, network, assetID, key); err != nil {
			return prev, err
		}
	}
	return prev, nil
}

func validWGPublicKey(k string) bool {
	b, err := base64.StdEncoding.DecodeString(k)
	return err == nil && len(b) == 32
}

// wgMemberRow is one member of a network a server is in.
type wgMemberRow struct {
	network, name, shape, cidr, iface string
	port                              int
	asset, address, endpoint, key     string
	hub                               bool
	routes                            []string
}

// wgSpecsFor is every approved network asset is in, with all its members.
func (s *Store) wgSpecsFor(ctx context.Context, asset string) ([]WGNetSpec, error) {
	rows, err := s.db(ctx).Query(ctx, `
		SELECT n.id::text, n.name, n.shape, n.cidr, n.port, n.iface, m.asset_id::text, m.address, m.endpoint, m.public_key, m.hub, m.routes
		  FROM wg_networks n JOIN wg_members m ON m.network_id = n.id
		 WHERE n.state = 'active'
		   AND n.id IN (SELECT network_id FROM wg_members WHERE asset_id = $1)
		 ORDER BY n.id, m.address`, asset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var in []wgMemberRow
	for rows.Next() {
		var r wgMemberRow
		if err := rows.Scan(&r.network, &r.name, &r.shape, &r.cidr, &r.port, &r.iface, &r.asset, &r.address, &r.endpoint,
			&r.key, &r.hub, &r.routes); err != nil {
			return nil, err
		}
		in = append(in, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return buildWGSpecs(asset, in), nil
}

// buildWGSpecs is what self should have for each network in rows: its own
// address and port, and a peer for every other member whose key is known.
// A mesh peer carries the member's own address; in a hub network a spoke's
// one peer is the hub, carrying the whole range, and the hub forwards.
func buildWGSpecs(self string, rows []wgMemberRow) []WGNetSpec {
	byNet := map[string][]wgMemberRow{}
	var order []string
	for _, r := range rows {
		if _, ok := byNet[r.network]; !ok {
			order = append(order, r.network)
		}
		byNet[r.network] = append(byNet[r.network], r)
	}
	out := []WGNetSpec{}
	for _, id := range order {
		ms := byNet[id]
		var me *wgMemberRow
		for i := range ms {
			if ms[i].asset == self {
				me = &ms[i]
			}
		}
		pfx, err := netip.ParsePrefix(ms[0].cidr)
		if me == nil || err != nil {
			continue
		}
		hubShape := ms[0].shape == "hub"
		// A spoke reaches every other spoke's site through the hub.
		var spokeRoutes []string
		for _, o := range ms {
			if o.asset != self && !o.hub {
				spokeRoutes = append(spokeRoutes, o.routes...)
			}
		}
		spec := WGNetSpec{ID: id, Name: ms[0].name, Iface: ms[0].iface, Address: me.address + "/" + strconv.Itoa(pfx.Bits()),
			ListenPort: me.port, Forward: hubShape && me.hub || len(me.routes) > 0, OpenPort: me.endpoint != "", Peers: []WGNetPeer{}}
		for _, o := range ms {
			if o.asset == self || o.key == "" {
				continue
			}
			// A spoke talks to the hub alone.
			if hubShape && !me.hub && !o.hub {
				continue
			}
			p := WGNetPeer{PublicKey: o.key, AllowedIPs: append([]string{o.address + "/32"}, o.routes...)}
			if hubShape && o.hub {
				p.AllowedIPs = append(append([]string{pfx.String()}, o.routes...), spokeRoutes...)
			}
			if o.endpoint != "" {
				p.Endpoint = net.JoinHostPort(o.endpoint, strconv.Itoa(o.port))
				p.Keepalive = wgKeepalive
			}
			spec.Peers = append(spec.Peers, p)
		}
		out = append(out, spec)
	}
	return out
}

// heartbeatWGNet keeps what the agent on host said about its WireGuard
// networks and answers with the networks it should be in. It returns nil
// for an agent older than server networks, one that cannot set them up,
// and on a read that failed: the agent then keeps what it has, rather than
// taking down every network because the database blinked.
func (a *API) heartbeatWGNet(ctx context.Context, host string, b *WGNetBeat) *WGNetOrders {
	if b == nil {
		return nil
	}
	assetID, assetHost, _, err := a.store.assetForAgent(ctx, host)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			a.log.Error("heartbeat: cannot find the server for its networks", "hostname", host, "error", err)
		}
		return nil
	}
	prev, err := a.store.recordWGNetBeat(ctx, host, assetID, *b)
	if err != nil {
		a.log.Error("heartbeat: cannot keep the WireGuard networks", "hostname", host, "error", err)
	}
	if r := b.Run; r != nil && (r.State == "rolled-back" || r.State == "failed") && (prev == nil || prev.ID != r.ID || prev.State != r.State) {
		a.raiseAlert(ctx, ConsoleAlert{
			Kind: AlertNetwork, Severity: "warning", Audience: AudienceAdmins,
			Title:  "A WireGuard network change on " + assetHost + " was undone",
			Detail: r.Message,
			// News, not a problem the network watch keeps open.
			Link: "/networks",
		})
	}
	if !b.Supported {
		return nil
	}
	specs, err := a.store.wgSpecsFor(ctx, assetID)
	if err != nil {
		a.log.Error("heartbeat: cannot read the WireGuard networks to send", "hostname", host, "error", err)
		return nil
	}
	return &WGNetOrders{Networks: specs}
}
