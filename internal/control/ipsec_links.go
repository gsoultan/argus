package control

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math/big"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
IPsec links from a server to a cloud VPN gateway or an office firewall.

Cloud gateways (AWS Site-to-Site VPN, Azure VPN Gateway, Google Cloud VPN)
and most office firewalls speak IPsec only, so Argus links them to an Argus
server with strongSwan (IKEv2) through that server's agent
(agent/ipsec.go). An admin names the server, the other end's address, the
ranges on each side, and either pastes the pre-shared key the cloud made or
lets Argus make one to paste into the cloud. Like server networks, nothing
reaches the server until a second admin approves, and every change after
that waits the same way.

The key is kept sealed (like Cloudflare tokens, with a key derived from
signing_secret). It leaves the control plane on two paths only: in the
heartbeat reply to the gateway server's agent, which writes it to a root-only
file, and to an admin who asks to see it to enter it in the cloud, which is
audited. It is never in a list, a log or an alert.

Health comes from two places: whether strongSwan on the server has the link
established, and, when the other end is a tunnel of a cloud VPN in a
connected account, what the cloud says about that tunnel.
*/

// IPsecLink is a link as the console shows it. The key is never in it.
type IPsecLink struct {
	ID           string      `json:"id"`
	Name         string      `json:"name"`
	Conn         string      `json:"conn"`
	AssetID      string      `json:"assetId"`
	Hostname     string      `json:"hostname"`
	Peer         string      `json:"peer"`
	LocalRanges  []string    `json:"localRanges"`
	RemoteRanges []string    `json:"remoteRanges"`
	Proposals    string      `json:"proposals"`
	State        string      `json:"state"`
	Pending      *IPsecDraft `json:"pending,omitempty"`
	PendingBy    string      `json:"pendingBy,omitempty"`
	PendingAt    *time.Time  `json:"pendingAt,omitempty"`
	RequestedBy  string      `json:"requestedBy"`
	ApprovedBy   string      `json:"approvedBy,omitempty"`
	ApprovedAt   *time.Time  `json:"approvedAt,omitempty"`
	CreatedAt    time.Time   `json:"createdAt"`
	ApproveOwn   bool        `json:"approveOwn,omitempty"`
	// Server is how the change went on the server (WGMember's states).
	Server        string     `json:"server"`
	ServerMessage string     `json:"serverMessage,omitempty"`
	ServerAt      *time.Time `json:"serverAt,omitempty"`
	// Link is up, down or unknown as strongSwan on the server says; ""
	// before the server has said.
	Link string `json:"link,omitempty"`
	// Cloud is the cloud VPN tunnel the other end is, when a connected
	// account has one at that address.
	Cloud *IPsecCloudEnd `json:"cloud,omitempty"`
}

// IPsecCloudEnd is the cloud's view of the other end.
type IPsecCloudEnd struct {
	Provider    string `json:"provider"`
	AccountName string `json:"accountName"`
	Region      string `json:"region"`
	Name        string `json:"name"`
	Up          bool   `json:"up"`
	Why         string `json:"why,omitempty"`
}

// IPsecDraft is a link as someone asked for it. The key, when it changes,
// is already sealed.
type IPsecDraft struct {
	Name         string   `json:"name"`
	AssetID      string   `json:"assetId"`
	Hostname     string   `json:"hostname,omitempty"`
	Peer         string   `json:"peer"`
	LocalRanges  []string `json:"localRanges"`
	RemoteRanges []string `json:"remoteRanges"`
	Proposals    string   `json:"proposals"`
	// NewKey says the change brings a new key (sealed in PSKSealed).
	NewKey    bool   `json:"newKey,omitempty"`
	PSKSealed []byte `json:"pskSealed,omitempty"`
	Remove    bool   `json:"remove,omitempty"`
}

// IPsecInput is what the console sends: a draft, and the key when one is
// pasted.
type IPsecInput struct {
	IPsecDraft
	// PSK is a key to use; "" with NewKey (or on a new link) has Argus
	// make one.
	PSK string `json:"psk,omitempty"`
}

var (
	ErrNoIPsecLink      = errors.New("no such IPsec link")
	ErrIPsecNameExists  = errors.New("another link already has that name")
	reIPsecPSK          = regexp.MustCompile(`^[A-Za-z0-9._]{16,64}$`)
	ipsecProposalChoice = []string{"strong", "compatible"}
)

const ipsecMaxLinks = 50

/* ── The key ─────────────────────────────────────────────────────────────── */

func (a *API) ipsecKey(ctx context.Context) ([]byte, error) {
	if a.Tunnels == nil || a.Tunnels.GrantKey == nil {
		return nil, ErrInvalidWGNetwork{"this control plane cannot keep pre-shared keys safe: set signing_secret in its configuration"}
	}
	sum := sha256.Sum256(append([]byte("argus ipsec keys v1\x00"), a.Tunnels.grantKeyFor(ctx).Seed()...))
	return sum[:], nil
}

func (a *API) sealPSK(ctx context.Context, psk string) ([]byte, error) {
	key, err := a.ipsecKey(ctx)
	if err != nil {
		return nil, err
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, []byte(psk), []byte("ipsec-psk")), nil
}

func (a *API) openPSK(ctx context.Context, sealed []byte) (string, error) {
	key, err := a.ipsecKey(ctx)
	if err != nil {
		return "", err
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	if len(sealed) < gcm.NonceSize() {
		return "", errors.New("the link's key cannot be read")
	}
	plain, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], []byte("ipsec-psk"))
	if err != nil {
		return "", errors.New("the link's key cannot be read; was signing_secret changed? Give the link a new key")
	}
	return string(plain), nil
}

// newPSK makes a key every cloud takes: 40 letters and digits, starting
// with a letter (AWS refuses one starting with 0).
func newPSK() (string, error) {
	const letters = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	const all = letters + "0123456789"
	b := make([]byte, 40)
	for i := range b {
		set := all
		if i == 0 {
			set = letters
		}
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(set))))
		if err != nil {
			return "", err
		}
		b[i] = set[n.Int64()]
	}
	return string(b), nil
}

/* ── Checking a draft ────────────────────────────────────────────────────── */

func parseRanges(side string, in []string) ([]string, []netip.Prefix, error) {
	var (
		out  []string
		pfxs []netip.Prefix
	)
	for _, r := range in {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		p, err := netip.ParsePrefix(r)
		if err != nil || !p.Addr().Is4() {
			return nil, nil, invalidWG("%s: %q is not an IPv4 range such as 10.30.0.0/16", side, r)
		}
		if p != p.Masked() {
			return nil, nil, invalidWG("%s: %s is not the start of its range: did you mean %s?", side, r, p.Masked())
		}
		if !slices.ContainsFunc(wgPrivate, func(b netip.Prefix) bool { return b.Bits() <= p.Bits() && b.Contains(p.Addr()) }) {
			return nil, nil, invalidWG("%s: %s is not a private range; a link carries private ranges only", side, r)
		}
		pfxs = append(pfxs, p)
		out = append(out, p.String())
	}
	if len(out) == 0 || len(out) > 16 {
		return nil, nil, invalidWG("give between 1 and 16 ranges for %s", side)
	}
	return out, pfxs, nil
}

// checkIPsecDraft checks d and fills in the gateway's hostname. own is each
// server's own addresses.
func checkIPsecDraft(d *IPsecDraft, hosts map[string]string, own map[string][]netip.Prefix) error {
	d.Name = strings.TrimSpace(d.Name)
	if !reWGNetName.MatchString(d.Name) {
		return invalidWG("give the link a name of up to 80 letters, digits, spaces and . _ ( ) -")
	}
	host, ok := hosts[strings.TrimSpace(d.AssetID)]
	if !ok {
		return invalidWG("pick the Argus server that is this side's gateway")
	}
	d.AssetID, d.Hostname = strings.TrimSpace(d.AssetID), host
	d.Peer = strings.TrimSpace(d.Peer)
	if ip := net.ParseIP(d.Peer); ip == nil && (!reWGHost.MatchString(d.Peer) || strings.HasPrefix(d.Peer, "-")) {
		return invalidWG("the other end is its public IPv4 address or hostname: the cloud tunnel's outside address, or the office firewall")
	}
	if d.Proposals == "" {
		d.Proposals = "strong"
	}
	if !slices.Contains(ipsecProposalChoice, d.Proposals) {
		return invalidWG("the ciphers are strong or compatible")
	}
	local, lp, err := parseRanges("this side", d.LocalRanges)
	if err != nil {
		return err
	}
	remote, rp, err := parseRanges("the other side", d.RemoteRanges)
	if err != nil {
		return err
	}
	for _, r := range rp {
		for _, l := range lp {
			if prefixesOverlap(r, l) {
				return invalidWG("%s is on both sides of the link", r)
			}
		}
		for _, o := range own[d.AssetID] {
			if r.Contains(o.Addr()) {
				return invalidWG("%s holds %s's own address %s: it would send the server's own traffic into the tunnel", r, host, o.Addr())
			}
		}
	}
	d.LocalRanges, d.RemoteRanges = local, remote
	return nil
}

/* ── The store ───────────────────────────────────────────────────────────── */

const ipsecColumns = `l.id::text, l.name, l.conn, l.asset_id::text, a.hostname, l.peer, l.local_ranges, l.remote_ranges, l.proposals,
	l.state, l.pending, l.pending_by, l.pending_at, l.requested_by, l.approved_by, l.approved_at, l.created_at`

func scanIPsec(row pgx.Row, extra ...any) (IPsecLink, error) {
	var (
		l       IPsecLink
		pending []byte
	)
	dest := append([]any{&l.ID, &l.Name, &l.Conn, &l.AssetID, &l.Hostname, &l.Peer, &l.LocalRanges, &l.RemoteRanges, &l.Proposals,
		&l.State, &pending, &l.PendingBy, &l.PendingAt, &l.RequestedBy, &l.ApprovedBy, &l.ApprovedAt, &l.CreatedAt}, extra...)
	err := row.Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return l, ErrNoIPsecLink
	}
	if err != nil {
		return l, err
	}
	if len(pending) > 0 {
		var d IPsecDraft
		if json.Unmarshal(pending, &d) == nil {
			d.PSKSealed = nil
			l.Pending = &d
		}
	}
	return l, nil
}

// IPsecLinks reads every link with how its server is doing.
func (s *Store) IPsecLinks(ctx context.Context) ([]IPsecLink, error) {
	rows, err := s.db(ctx).Query(ctx, `
		SELECT `+ipsecColumns+`, ag.wgnet_state
		  FROM ipsec_links l JOIN assets a ON a.id = l.asset_id
		  LEFT JOIN LATERAL (
		       SELECT wgnet_state FROM agents g
		        WHERE g.hostname = a.hostname OR g.hostname = a.agent_hostname
		           OR g.hostname = split_part(a.hostname, '.', 1)
		        ORDER BY g.last_seen_at DESC NULLS LAST LIMIT 1) ag ON TRUE
		 ORDER BY lower(l.name)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IPsecLink{}
	for rows.Next() {
		var state []byte
		l, err := scanIPsec(rows, &state)
		if err != nil {
			return nil, err
		}
		var st *wgAgentState
		if len(state) > 0 {
			st = &wgAgentState{}
			if json.Unmarshal(state, st) != nil {
				st = nil
			}
		}
		linkState(&l, st)
		out = append(out, l)
	}
	return out, rows.Err()
}

// linkState says how the link is on its server.
func linkState(l *IPsecLink, st *wgAgentState) {
	if l.State != "active" {
		return
	}
	m := WGMember{PublicKey: "-"}
	memberState(&m, l.ID, st)
	l.Server, l.ServerMessage, l.ServerAt = m.State, m.Message, m.At
	if st != nil {
		l.Link = st.IPsec[l.ID]
	}
}

// matchCloudEnds finds the cloud VPN tunnel at each link's other end.
func matchCloudEnds(links []IPsecLink, vpns []CloudItem) {
	for i := range links {
		for _, v := range vpns {
			for _, t := range parseTunnels(v.Resource.Details[cloud.DetailTunnels]) {
				if t.Address == links[i].Peer {
					links[i].Cloud = &IPsecCloudEnd{Provider: v.Provider, AccountName: v.AccountName, Region: v.Resource.Region,
						Name: v.Resource.Name, Up: t.Up, Why: t.Why}
				}
			}
		}
	}
}

func (s *Store) ipsecLink(ctx context.Context, id string) (IPsecLink, []byte, error) {
	if _, err := uuid.Parse(id); err != nil {
		return IPsecLink{}, nil, ErrNoIPsecLink
	}
	var sealed []byte
	l, err := scanIPsec(s.db(ctx).QueryRow(ctx, `SELECT `+ipsecColumns+`, l.psk_sealed
		  FROM ipsec_links l JOIN assets a ON a.id = l.asset_id WHERE l.id = $1`, id), &sealed)
	return l, sealed, err
}

// CreateIPsecLink keeps a new link, waiting for approval.
func (s *Store) CreateIPsecLink(ctx context.Context, d IPsecDraft, sealed []byte, by string) (IPsecLink, error) {
	var count int
	if err := s.db(ctx).QueryRow(ctx, `SELECT count(*) FROM ipsec_links`).Scan(&count); err != nil {
		return IPsecLink{}, err
	}
	if count >= ipsecMaxLinks {
		return IPsecLink{}, invalidWG("there are already %d links", ipsecMaxLinks)
	}
	pd := d
	pd.PSKSealed, pd.NewKey = nil, false
	b, err := json.Marshal(pd)
	if err != nil {
		return IPsecLink{}, err
	}
	for range 5 {
		id := uuid.New()
		conn := "argus-" + strings.ReplaceAll(id.String(), "-", "")[:8]
		_, err := s.db(ctx).Exec(ctx, `
			INSERT INTO ipsec_links (id, name, conn, asset_id, peer, local_ranges, remote_ranges, proposals, psk_sealed,
			                         pending, pending_by, pending_at, requested_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11, now(), $11)`,
			id, d.Name, conn, d.AssetID, d.Peer, d.LocalRanges, d.RemoteRanges, d.Proposals, sealed, b, by)
		if err != nil && isUniqueViolation(err) {
			if strings.Contains(err.Error(), "uq_ipsec_links_name") {
				return IPsecLink{}, ErrIPsecNameExists
			}
			continue
		}
		if err != nil {
			return IPsecLink{}, err
		}
		l, _, err := s.ipsecLink(ctx, id.String())
		return l, err
	}
	return IPsecLink{}, errors.New("could not name the link's connection")
}

// ProposeIPsecChange keeps a change to an approved link, waiting.
func (s *Store) ProposeIPsecChange(ctx context.Context, id string, d IPsecDraft, by string) error {
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	tag, err := s.db(ctx).Exec(ctx, `
		UPDATE ipsec_links SET pending = $2::jsonb, pending_by = $3, pending_at = now(), updated_at = now()
		 WHERE id = $1 AND pending IS NULL`, id, b, by)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrWGPending
	}
	return nil
}

// ApproveIPsecLink makes the waiting change the link. It returns the link
// as it was, the change, and whether it removed the link.
func (s *Store) ApproveIPsecLink(ctx context.Context, id, by string) (IPsecLink, *IPsecDraft, bool, error) {
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return IPsecLink{}, nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var raw []byte
	before, err := scanIPsec(tx.QueryRow(ctx, `SELECT `+ipsecColumns+`, l.pending
		  FROM ipsec_links l JOIN assets a ON a.id = l.asset_id WHERE l.id = $1 FOR UPDATE OF l`, id), &raw)
	if err != nil {
		return before, nil, false, err
	}
	if len(raw) == 0 {
		return before, nil, false, ErrWGNoPending
	}
	var d IPsecDraft
	if err := json.Unmarshal(raw, &d); err != nil {
		return before, nil, false, err
	}
	if d.Remove {
		if _, err := tx.Exec(ctx, `DELETE FROM ipsec_links WHERE id = $1`, id); err != nil {
			return before, &d, false, err
		}
		return before, &d, true, tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE ipsec_links SET name = $2, asset_id = $3, peer = $4, local_ranges = $5, remote_ranges = $6, proposals = $7,
		       psk_sealed = COALESCE($8, psk_sealed), state = 'active', pending = NULL, pending_by = '', pending_at = NULL,
		       approved_by = $9, approved_at = now(), updated_at = now()
		 WHERE id = $1`, id, d.Name, d.AssetID, d.Peer, d.LocalRanges, d.RemoteRanges, d.Proposals, d.PSKSealed, by); err != nil {
		if isUniqueViolation(err) {
			return before, &d, false, ErrIPsecNameExists
		}
		return before, &d, false, err
	}
	return before, &d, false, tx.Commit(ctx)
}

// DeclineIPsecLink drops the waiting change; a link never approved goes
// with it.
func (s *Store) DeclineIPsecLink(ctx context.Context, id string) (IPsecLink, bool, error) {
	before, _, err := s.ipsecLink(ctx, id)
	if err != nil {
		return before, false, err
	}
	if before.Pending == nil {
		return before, false, ErrWGNoPending
	}
	if before.State == "waiting" {
		_, err := s.db(ctx).Exec(ctx, `DELETE FROM ipsec_links WHERE id = $1 AND state = 'waiting'`, id)
		return before, true, err
	}
	_, err = s.db(ctx).Exec(ctx, `UPDATE ipsec_links SET pending = NULL, pending_by = '', pending_at = NULL, updated_at = now() WHERE id = $1`, id)
	return before, false, err
}

/* ── The heartbeat ───────────────────────────────────────────────────────── */

// IPsecSpec is agent.IPsecSpec, as the control plane writes it.
type IPsecSpec struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Conn         string   `json:"conn"`
	Peer         string   `json:"peer"`
	LocalRanges  []string `json:"local_ranges"`
	RemoteRanges []string `json:"remote_ranges"`
	Proposals    string   `json:"proposals"`
	PSK          string   `json:"psk"`
}

// ipsecSpecsFor is every approved link whose gateway is asset, keys opened.
func (a *API) ipsecSpecsFor(ctx context.Context, asset string) ([]IPsecSpec, error) {
	rows, err := a.store.db(ctx).Query(ctx, `
		SELECT id::text, name, conn, peer, local_ranges, remote_ranges, proposals, psk_sealed
		  FROM ipsec_links WHERE asset_id = $1 AND state = 'active' ORDER BY id`, asset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IPsecSpec
	for rows.Next() {
		var (
			sp     IPsecSpec
			sealed []byte
		)
		if err := rows.Scan(&sp.ID, &sp.Name, &sp.Conn, &sp.Peer, &sp.LocalRanges, &sp.RemoteRanges, &sp.Proposals, &sealed); err != nil {
			return nil, err
		}
		if sp.PSK, err = a.openPSK(ctx, sealed); err != nil {
			// One unreadable key would otherwise take down every link on
			// the server: send none of them, and keep what it has.
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// addIPsecProblems adds a problem for each approved link strongSwan says is
// down, and each change its server undid.
func addIPsecProblems(v *NetworksView, links []IPsecLink) {
	for _, l := range links {
		if l.State != "active" || v.quiet[l.AssetID] {
			continue
		}
		switch {
		case l.Link == "down":
			v.Problems = append(v.Problems, NetworkProblem{Severity: "down", Kind: "ipsec-down", Key: problemKey("ipsec-down", l.AssetID, l.ID),
				AssetID: l.AssetID, Hostname: l.Hostname, Title: "IPsec link " + l.Name + " from " + l.Hostname + " to " + l.Peer + " is down",
				Detail: "strongSwan on " + l.Hostname + " has no established tunnel. Check that the other end has the same key, ranges and ciphers, and that UDP 500 and 4500 reach it."})
		case l.Server == "failed" || l.Server == "rolled-back":
			v.Problems = append(v.Problems, NetworkProblem{Severity: "warn", Kind: "ipsec-undone", Key: problemKey("ipsec-undone", l.AssetID, l.ID),
				AssetID: l.AssetID, Hostname: l.Hostname, Title: "IPsec link " + l.Name + " could not be set up on " + l.Hostname,
				Detail: l.ServerMessage})
		}
	}
	sortProblems(v.Problems)
}
