package control

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/curve25519"
)

/*
The staff VPN: a WireGuard network of shape staff. Its one server member is
the hub, set up and approved like any server network; its other peers are
people's devices.

  - A device profile is made by its person, with an authenticator code when
    the VPN asks for one. Argus makes the key pair, hands the private key
    over once in the profile, and keeps only the public key. A profile lasts
    the VPN's days and is renewed the same way, code and all; an expired
    one is simply left out of the hub.
  - What a device reaches is what its person's teams are given (grants),
    enforced on the hub (agent/wgstaff.go). SSH and RDP straight to those
    ranges are refused unless a team allows direct logins, which Argus does
    not record; logins through Argus are.
  - Traffic is company (the granted ranges) or full (everything, through the
    hub to the internet): the VPN's default, or a person's own setting.

A profile's routes are fixed when it is made: when someone's reach or
setting changes, the hub enforces the new one at once, and a new profile
routes it.
*/

const wgShapeStaff = "staff"

// WGStaffPolicy is a staff VPN's settings.
type WGStaffPolicy struct {
	// Default is company or full.
	Default string `json:"default"`
	// Days is how long a profile lasts.
	Days int `json:"days"`
	// MFA asks for an authenticator code to make or renew a profile.
	MFA bool `json:"mfa"`
}

// WGStaffGrant is what a team reaches.
type WGStaffGrant struct {
	TeamID    string    `json:"teamId"`
	TeamName  string    `json:"teamName"`
	Ranges    []string  `json:"ranges"`
	Logins    bool      `json:"logins"`
	UpdatedBy string    `json:"updatedBy,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// WGStaffDevice is a device profile, without its key.
type WGStaffDevice struct {
	ID        string    `json:"id"`
	NetworkID string    `json:"networkId"`
	UserEmail string    `json:"userEmail"`
	Name      string    `json:"name"`
	Address   string    `json:"address"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	Expired   bool      `json:"expired,omitempty"`
}

var (
	ErrNoStaffDevice = errors.New("no such device")
	ErrStaffNoReach  = errors.New("none of your teams is given anything on this VPN; ask an admin")
	ErrStaffNotReady = errors.New("the VPN's server has not set it up yet; try again once it shows applied")
	ErrStaffCode     = errors.New("that authenticator code is wrong or already used")
	ErrStaffNoMFA    = errors.New("this VPN asks for an authenticator code: set one up on your account first")
)

const (
	wgStaffMaxDevicesPerPerson = 10
	wgStaffMaxGrants           = 100
)

func wgIsStaff(rows []wgMemberRow, network string) bool {
	for _, r := range rows {
		if r.network == network {
			return r.shape == wgShapeStaff
		}
	}
	return false
}

// staffMode is what a person's traffic is on network: their own setting,
// or the network's default.
func (s *Store) staffModes(ctx context.Context, network string) (string, map[string]string, error) {
	var def string
	if err := s.db(ctx).QueryRow(ctx, `SELECT staff_default FROM wg_networks WHERE id = $1`, network).Scan(&def); err != nil {
		return "", nil, err
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT lower(user_email), mode FROM wg_staff_modes WHERE network_id = $1`, network)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	own := map[string]string{}
	for rows.Next() {
		var e, m string
		if err := rows.Scan(&e, &m); err != nil {
			return "", nil, err
		}
		own[e] = m
	}
	return def, own, rows.Err()
}

// staffReach is each person's reach on network, from their teams: the
// ranges with direct logins allowed, and those without.
type staffReach struct{ logins, plain []string }

func (s *Store) staffReachAll(ctx context.Context, network string) (map[string]*staffReach, error) {
	rows, err := s.db(ctx).Query(ctx, `
		SELECT lower(tm.user_email), g.ranges, g.logins
		  FROM wg_staff_grants g JOIN team_members tm ON tm.team_id = g.team_id
		 WHERE g.network_id = $1`, network)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*staffReach{}
	for rows.Next() {
		var (
			e      string
			ranges []string
			logins bool
		)
		if err := rows.Scan(&e, &ranges, &logins); err != nil {
			return nil, err
		}
		r := out[e]
		if r == nil {
			r = &staffReach{}
			out[e] = r
		}
		if logins {
			r.logins = append(r.logins, ranges...)
		} else {
			r.plain = append(r.plain, ranges...)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, r := range out {
		r.logins = uniqSorted(r.logins)
		// A range one team gives with logins is not also given without.
		r.plain = slices.DeleteFunc(uniqSorted(r.plain), func(x string) bool { return slices.Contains(r.logins, x) })
	}
	return out, nil
}

func uniqSorted(xs []string) []string {
	out := append([]string(nil), xs...)
	sort.Strings(out)
	return slices.Compact(out)
}

// addStaffHub gives a staff hub's spec its devices and their reach. Expired
// devices are left out, which is what ends them.
func (s *Store) addStaffHub(ctx context.Context, spec *WGNetSpec) error {
	def, own, err := s.staffModes(ctx, spec.ID)
	if err != nil {
		return err
	}
	reach, err := s.staffReachAll(ctx, spec.ID)
	if err != nil {
		return err
	}
	rows, err := s.db(ctx).Query(ctx, `
		SELECT lower(user_email), public_key, address FROM wg_staff_devices
		 WHERE network_id = $1 AND expires_at > now() ORDER BY address`, spec.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	st := &WGStaff{Rules: []WGStaffRule{}}
	for rows.Next() {
		var e, key, addr string
		if err := rows.Scan(&e, &key, &addr); err != nil {
			return err
		}
		host := addr + "/32"
		spec.Peers = append(spec.Peers, WGNetPeer{PublicKey: key, AllowedIPs: []string{host}})
		if r := reach[e]; r != nil {
			if len(r.logins) > 0 {
				st.Rules = append(st.Rules, WGStaffRule{From: host, To: r.logins, Logins: true})
			}
			if len(r.plain) > 0 {
				st.Rules = append(st.Rules, WGStaffRule{From: host, To: r.plain})
			}
		}
		mode := def
		if m, ok := own[e]; ok {
			mode = m
		}
		if mode == "full" {
			st.Internet = append(st.Internet, host)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	spec.Staff = st
	spec.Forward, spec.OpenPort = true, true
	return nil
}

/* ── Device keys and profiles ───────────────────────────────────────────── */

// wgKeyPair makes a WireGuard key pair.
func wgKeyPair() (priv, pub string, err error) {
	var k [32]byte
	if _, err := rand.Read(k[:]); err != nil {
		return "", "", err
	}
	k[0] &= 248
	k[31] = (k[31] & 127) | 64
	p, err := curve25519.X25519(k[:], curve25519.Basepoint)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(k[:]), base64.StdEncoding.EncodeToString(p), nil
}

// staffHub is a staff VPN's hub as a device needs it.
type staffHub struct {
	cidr      netip.Prefix
	port      int
	endpoint  string
	publicKey string
	address   string
	policy    WGStaffPolicy
	name      string
}

func (s *Store) staffHubOf(ctx context.Context, network string) (staffHub, error) {
	var (
		h     staffHub
		cidr  string
		shape string
		state string
	)
	err := s.db(ctx).QueryRow(ctx, `
		SELECT n.name, n.shape, n.state, n.cidr, n.port, n.staff_default, n.staff_days, n.staff_mfa,
		       coalesce(m.endpoint, ''), coalesce(m.public_key, ''), coalesce(m.address, '')
		  FROM wg_networks n LEFT JOIN wg_members m ON m.network_id = n.id AND m.hub
		 WHERE n.id::text = $1`, network).Scan(&h.name, &shape, &state, &cidr, &h.port, &h.policy.Default, &h.policy.Days,
		&h.policy.MFA, &h.endpoint, &h.publicKey, &h.address)
	if err != nil || shape != wgShapeStaff {
		return h, ErrNoWGNetwork
	}
	if h.cidr, err = netip.ParsePrefix(cidr); err != nil {
		return h, err
	}
	if state != "active" || h.publicKey == "" || h.endpoint == "" {
		return h, ErrStaffNotReady
	}
	return h, nil
}

// staffConfig is a device's profile: its key, its address, and the hub.
func staffConfig(h staffHub, priv, addr string, full bool, reach []string) string {
	allowed := []string{h.cidr.String()}
	if full {
		allowed = []string{"0.0.0.0/0"}
	} else {
		allowed = append(allowed, reach...)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Argus staff VPN %q. Keep this file private: it is the device's key.\n", h.name)
	b.WriteString("[Interface]\n")
	fmt.Fprintf(&b, "PrivateKey = %s\n", priv)
	fmt.Fprintf(&b, "Address = %s/32\n", addr)
	b.WriteString("\n[Peer]\n")
	fmt.Fprintf(&b, "PublicKey = %s\n", h.publicKey)
	fmt.Fprintf(&b, "Endpoint = %s\n", net.JoinHostPort(h.endpoint, strconv.Itoa(h.port)))
	fmt.Fprintf(&b, "AllowedIPs = %s\n", strings.Join(allowed, ", "))
	b.WriteString("PersistentKeepalive = 25\n")
	return b.String()
}

// nextStaffAddress is the first address in cidr that no device and no
// server member has.
func (s *Store) nextStaffAddress(ctx context.Context, network string, cidr netip.Prefix) (string, error) {
	rows, err := s.db(ctx).Query(ctx, `
		SELECT address FROM wg_staff_devices WHERE network_id = $1
		UNION SELECT address FROM wg_members WHERE network_id = $1`, network)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	used := map[string]bool{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return "", err
		}
		used[a] = true
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	last := lastAddr(cidr)
	for a := cidr.Masked().Addr().Next(); cidr.Contains(a) && a.Less(last); a = a.Next() {
		if !used[a.String()] {
			return a.String(), nil
		}
	}
	return "", invalidWG("%s has no address left for another device", cidr)
}

// StaffProfile is a new device and its profile, handed over once.
type StaffProfile struct {
	Device WGStaffDevice `json:"device"`
	Config string        `json:"config"`
	// Mode is company or full: what the profile routes.
	Mode string `json:"mode"`
}

// MakeStaffDevice makes a device profile for email on network. The caller
// has checked the code.
func (s *Store) MakeStaffDevice(ctx context.Context, network, email, name string) (StaffProfile, error) {
	name = strings.TrimSpace(name)
	if !reWGNetName.MatchString(name) || len(name) > 60 {
		return StaffProfile{}, invalidWG("name the device with up to 60 letters, digits, spaces and . _ ( ) -")
	}
	h, err := s.staffHubOf(ctx, network)
	if err != nil {
		return StaffProfile{}, err
	}
	reach, err := s.staffReachAll(ctx, network)
	if err != nil {
		return StaffProfile{}, err
	}
	r := reach[strings.ToLower(email)]
	if r == nil || len(r.logins)+len(r.plain) == 0 {
		return StaffProfile{}, ErrStaffNoReach
	}
	var n int
	if err := s.db(ctx).QueryRow(ctx, `SELECT count(*) FROM wg_staff_devices WHERE network_id = $1 AND lower(user_email) = lower($2)`,
		network, email).Scan(&n); err != nil {
		return StaffProfile{}, err
	}
	if n >= wgStaffMaxDevicesPerPerson {
		return StaffProfile{}, invalidWG("you have %d devices on this VPN already; remove one first", n)
	}
	def, own, err := s.staffModes(ctx, network)
	if err != nil {
		return StaffProfile{}, err
	}
	mode := def
	if m, ok := own[strings.ToLower(email)]; ok {
		mode = m
	}
	priv, pub, err := wgKeyPair()
	if err != nil {
		return StaffProfile{}, err
	}
	for range 5 {
		addr, err := s.nextStaffAddress(ctx, network, h.cidr)
		if err != nil {
			return StaffProfile{}, err
		}
		var d WGStaffDevice
		err = s.db(ctx).QueryRow(ctx, `
			INSERT INTO wg_staff_devices (network_id, user_email, name, public_key, address, expires_at)
			VALUES ($1, $2, $3, $4, $5, now() + make_interval(days => $6))
			RETURNING id::text, network_id::text, user_email, name, address, created_at, expires_at`,
			network, strings.ToLower(email), name, pub, addr, h.policy.Days).Scan(&d.ID, &d.NetworkID, &d.UserEmail, &d.Name,
			&d.Address, &d.CreatedAt, &d.ExpiresAt)
		if err != nil && isUniqueViolation(err) {
			continue // another device took the address first
		}
		if err != nil {
			return StaffProfile{}, err
		}
		reachAll := append(append([]string(nil), r.logins...), r.plain...)
		return StaffProfile{Device: d, Config: staffConfig(h, priv, addr, mode == "full", uniqSorted(reachAll)), Mode: mode}, nil
	}
	return StaffProfile{}, errors.New("could not give the device an address; try again")
}

func (s *Store) staffDevice(ctx context.Context, id string) (WGStaffDevice, error) {
	var d WGStaffDevice
	if _, err := uuid.Parse(id); err != nil {
		return d, ErrNoStaffDevice
	}
	err := s.db(ctx).QueryRow(ctx, `SELECT id::text, network_id::text, user_email, name, address, created_at, expires_at
		FROM wg_staff_devices WHERE id = $1`, id).Scan(&d.ID, &d.NetworkID, &d.UserEmail, &d.Name, &d.Address, &d.CreatedAt, &d.ExpiresAt)
	if err != nil {
		return d, ErrNoStaffDevice
	}
	d.Expired = !d.ExpiresAt.After(time.Now())
	return d, nil
}

// RenewStaffDevice gives a device the VPN's days again, from now.
func (s *Store) RenewStaffDevice(ctx context.Context, id string) (WGStaffDevice, error) {
	if _, err := s.db(ctx).Exec(ctx, `
		UPDATE wg_staff_devices d SET expires_at = now() + make_interval(days => n.staff_days)
		  FROM wg_networks n WHERE n.id = d.network_id AND d.id = $1`, id); err != nil {
		return WGStaffDevice{}, err
	}
	return s.staffDevice(ctx, id)
}

// RemoveStaffDevice deletes a device; the hub drops it on its next beat.
func (s *Store) RemoveStaffDevice(ctx context.Context, id string) error {
	_, err := s.db(ctx).Exec(ctx, `DELETE FROM wg_staff_devices WHERE id = $1`, id)
	return err
}

// StaffDevices lists a network's devices, everyone's or one person's.
func (s *Store) StaffDevices(ctx context.Context, network, email string) ([]WGStaffDevice, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT id::text, network_id::text, user_email, name, address, created_at, expires_at
		FROM wg_staff_devices WHERE network_id = $1 AND ($2 = '' OR lower(user_email) = lower($2))
		ORDER BY lower(user_email), created_at`, network, email)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WGStaffDevice{}
	now := time.Now()
	for rows.Next() {
		var d WGStaffDevice
		if err := rows.Scan(&d.ID, &d.NetworkID, &d.UserEmail, &d.Name, &d.Address, &d.CreatedAt, &d.ExpiresAt); err != nil {
			return nil, err
		}
		d.Expired = !d.ExpiresAt.After(now)
		out = append(out, d)
	}
	return out, rows.Err()
}

/* ── Policy, grants and people's settings ───────────────────────────────── */

func checkStaffPolicy(p *WGStaffPolicy) error {
	if p.Default != "company" && p.Default != "full" {
		return invalidWG("traffic is company or full")
	}
	if p.Days < 1 || p.Days > 365 {
		return invalidWG("a profile lasts between 1 and 365 days")
	}
	return nil
}

// SetStaffPolicy changes a staff VPN's settings. A shorter life shortens
// profiles already made, so none outlives the new policy.
func (s *Store) SetStaffPolicy(ctx context.Context, network string, p WGStaffPolicy) error {
	tag, err := s.db(ctx).Exec(ctx, `UPDATE wg_networks SET staff_default = $2, staff_days = $3, staff_mfa = $4, updated_at = now()
		WHERE id::text = $1 AND shape = 'staff'`, network, p.Default, p.Days, p.MFA)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNoWGNetwork
	}
	_, err = s.db(ctx).Exec(ctx, `UPDATE wg_staff_devices SET expires_at = least(expires_at, created_at + make_interval(days => $2))
		WHERE network_id::text = $1`, network, p.Days)
	return err
}

// checkStaffRanges checks a grant's ranges: private IPv4 ranges, at most 32.
func checkStaffRanges(in []string) ([]string, error) {
	var out []string
	for _, r := range in {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		p, err := netip.ParsePrefix(r)
		if err != nil || !p.Addr().Is4() {
			return nil, invalidWG("%q is not an IPv4 range such as 10.88.0.0/24", r)
		}
		if p != p.Masked() {
			return nil, invalidWG("%s is not the start of its range: did you mean %s?", r, p.Masked())
		}
		if !slices.ContainsFunc(wgPrivate, func(b netip.Prefix) bool { return b.Bits() <= p.Bits() && b.Contains(p.Addr()) }) {
			return nil, invalidWG("%s is not a private range; the internet is the full-traffic setting, not a grant", r)
		}
		out = append(out, p.String())
	}
	if len(out) > 32 {
		return nil, invalidWG("a team is given at most 32 ranges")
	}
	return uniqSorted(out), nil
}

// SetStaffGrant sets what a team reaches; no ranges removes the grant.
func (s *Store) SetStaffGrant(ctx context.Context, network, team string, ranges []string, logins bool, by string) error {
	if _, err := uuid.Parse(team); err != nil {
		return invalidWG("pick a team")
	}
	if len(ranges) == 0 {
		_, err := s.db(ctx).Exec(ctx, `DELETE FROM wg_staff_grants WHERE network_id::text = $1 AND team_id = $2`, network, team)
		return err
	}
	var n int
	if err := s.db(ctx).QueryRow(ctx, `SELECT count(*) FROM wg_staff_grants WHERE network_id::text = $1`, network).Scan(&n); err != nil {
		return err
	}
	if n >= wgStaffMaxGrants {
		return invalidWG("a staff VPN has at most %d team grants", wgStaffMaxGrants)
	}
	_, err := s.db(ctx).Exec(ctx, `
		INSERT INTO wg_staff_grants (network_id, team_id, ranges, logins, updated_by)
		SELECT n.id, $2, $3, $4, $5 FROM wg_networks n WHERE n.id::text = $1 AND n.shape = 'staff'
		ON CONFLICT (network_id, team_id) DO UPDATE SET ranges = $3, logins = $4, updated_by = $5, updated_at = now()`,
		network, team, ranges, logins, by)
	if err != nil && strings.Contains(err.Error(), "SQLSTATE 23503") {
		return invalidWG("no such team")
	}
	return err
}

// StaffGrants lists what each team reaches.
func (s *Store) StaffGrants(ctx context.Context, network string) ([]WGStaffGrant, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT g.team_id::text, t.name, g.ranges, g.logins, g.updated_by, g.updated_at
		FROM wg_staff_grants g JOIN teams t ON t.id = g.team_id WHERE g.network_id = $1 ORDER BY lower(t.name)`, network)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WGStaffGrant{}
	for rows.Next() {
		var g WGStaffGrant
		if err := rows.Scan(&g.TeamID, &g.TeamName, &g.Ranges, &g.Logins, &g.UpdatedBy, &g.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// SetStaffMode sets a person's own traffic setting; "" goes back to the
// default.
func (s *Store) SetStaffMode(ctx context.Context, network, email, mode, by string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	if !strings.Contains(email, "@") || len(email) > 320 {
		return invalidWG("give the person's email")
	}
	if mode == "" {
		_, err := s.db(ctx).Exec(ctx, `DELETE FROM wg_staff_modes WHERE network_id::text = $1 AND user_email = $2`, network, email)
		return err
	}
	if mode != "company" && mode != "full" {
		return invalidWG("traffic is company or full")
	}
	_, err := s.db(ctx).Exec(ctx, `
		INSERT INTO wg_staff_modes (network_id, user_email, mode, set_by)
		SELECT n.id, $2, $3, $4 FROM wg_networks n WHERE n.id::text = $1 AND n.shape = 'staff'
		ON CONFLICT (network_id, user_email) DO UPDATE SET mode = $3, set_by = $4, set_at = now()`, network, email, mode, by)
	return err
}

// StaffModes lists people's own settings.
func (s *Store) StaffModes(ctx context.Context, network string) (map[string]string, error) {
	_, own, err := s.staffModes(ctx, network)
	return own, err
}

// staffAccessItems is what the staff VPN gives a person, for their page under
// People: each VPN their teams reach ranges on, and each device of theirs.
func staffAccessItems(ctx context.Context, s *Store, email string) ([]AccessItem, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT n.id::text, n.name, array_agg(DISTINCT r ORDER BY r), bool_or(g.logins),
		array_agg(DISTINCT t.name ORDER BY t.name)
		FROM wg_staff_grants g
		JOIN wg_networks n ON n.id = g.network_id
		JOIN teams t ON t.id = g.team_id
		CROSS JOIN LATERAL unnest(g.ranges) r
		WHERE g.team_id IN (SELECT team_id FROM team_members WHERE user_email = $1)
		GROUP BY n.id, n.name ORDER BY lower(n.name)`, email)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccessItem
	for rows.Next() {
		var it AccessItem
		var ranges []string
		var logins bool
		if err := rows.Scan(&it.ID, &it.Name, &ranges, &logins, &it.Teams); err != nil {
			return nil, err
		}
		it.What = "reaches " + strings.Join(ranges, ", ") + "; SSH and RDP go through Argus"
		if logins {
			it.What = "reaches " + strings.Join(ranges, ", ") + ", SSH and RDP straight to servers included, which Argus does not record"
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	devs, err := s.db(ctx).Query(ctx, `SELECT d.id::text, d.name, n.name, d.expires_at FROM wg_staff_devices d
		JOIN wg_networks n ON n.id = d.network_id
		WHERE d.user_email = $1 AND d.expires_at > now() ORDER BY d.created_at`, email)
	if err != nil {
		return nil, err
	}
	defer devs.Close()
	for devs.Next() {
		var it AccessItem
		var until time.Time
		if err := devs.Scan(&it.ID, &it.Name, &it.Where, &until); err != nil {
			return nil, err
		}
		it.What, it.Own, it.Until, it.Teams = "a device that can connect", true, &until, []string{}
		out = append(out, it)
	}
	return out, devs.Err()
}
