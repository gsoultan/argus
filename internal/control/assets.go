package control

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

/*
The inventory, as the console owns it.

An asset exists because an administrator entered it here, and a person reaches
one because an administrator assigned it to them. Both facts live in this
package: the gateway reads them, it does not hold them.

Assignment is permission to ask, not permission to have. An elevated principal
still needs an approved access request behind it — see authorize_routes.go.
*/

// assetSelect is the one place the asset column list is written.
//
// Shared by every read so a column added to one query cannot go missing from
// another; scanAsset below is its other half and the two change together.
const assetSelect = `
	SELECT a.id::text, a.hostname, a.address, a.port, a.protocol, a.os, a.tags,
	       a.group_name, a.source, a.credential_ref, a.domain,
	       a.credential_mode, a.credential_rotated_at, a.rotation_interval_days,
	       a.host_key_state, a.host_key_fingerprint, a.host_key_pinned_at,
	       a.health, a.last_checked_at, a.principals,
	       a.agent_state, a.agent_last_seen_at, a.bypass_posture,
	       a.unmanaged_key_count
	FROM assets a`

func scanAsset(rows pgx.Rows) (Asset, error) {
	var a Asset
	err := rows.Scan(&a.ID, &a.Hostname, &a.Address, &a.Port, &a.Protocol, &a.OS,
		&a.Tags, &a.GroupName, &a.Source, &a.CredentialRef, &a.Domain,
		&a.CredentialMode, &a.CredentialRotatedAt, &a.RotationIntervalDays,
		&a.HostKeyState, &a.HostKeyFingerprint, &a.HostKeyPinnedAt,
		&a.Health, &a.LastCheckedAt, &a.Principals,
		&a.AgentState, &a.AgentLastSeenAt, &a.BypassPosture, &a.UnmanagedKeyCount)
	return a, err
}

// ErrAssetNotFound means no live asset has that id.
var ErrAssetNotFound = errors.New("asset not found")

// ErrInvalidAsset carries what an administrator has to change to be allowed.
type ErrInvalidAsset struct{ Reason string }

func (e ErrInvalidAsset) Error() string { return e.Reason }

/* ── Input ───────────────────────────────────────────────────────────────── */

// AssetInput is what the console submits to create or replace an asset.
//
// Deliberately not the Asset type. Everything an agent or a gateway reports —
// host-key state, agent liveness, bypass posture, unmanaged keys — is observed,
// and letting the console write those fields would let an administrator declare
// a host verified rather than have it verified.
type AssetInput struct {
	Hostname   string   `json:"hostname"`
	Address    string   `json:"address"`
	Port       int      `json:"port"`
	Protocol   string   `json:"protocol"`
	OS         string   `json:"os"`
	Tags       []string `json:"tags"`
	GroupName  string   `json:"groupName"`
	Principals []string `json:"principals"`

	CredentialMode string `json:"credentialMode"`
	// CredentialRef names a file (SSH) or a directory (RDP) inside the
	// gateway's vault. A name, never a path: see validate below for why.
	CredentialRef string `json:"credentialRef"`
	Domain        string `json:"domain"`

	RotationIntervalDays *int `json:"rotationIntervalDays"`
}

var (
	// A DNS name, and nothing that could be read as anything else.
	reHostname = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
	// Hostname characters plus what a bracketed IPv6 literal needs: Asset.Addr
	// joins with a colon, so `[::1]` is the form that survives it.
	reAddress = regexp.MustCompile(`^[A-Za-z0-9.:\[\]_-]{1,253}$`)
	// An account name on the target. No colon: the SSH user string and the RDP
	// cookie are both `principal:target`, so a principal containing one would
	// be parsed as something else entirely.
	rePrincipal = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	// A vault entry name. No separator, no dot-dot, nothing that leaves the
	// directory it is resolved against.
	reCredentialRef = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	reTag           = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)
	reDomain        = regexp.MustCompile(`^[A-Za-z0-9.-]{1,253}$`)
)

const (
	maxPrincipals = 64
	maxTags       = 16
)

// normalise trims, defaults and validates, returning the input as it will be
// stored. Rejections name the field and what would be accepted, because a
// refusal an administrator cannot act on just becomes a support ticket.
func (in AssetInput) normalise() (AssetInput, error) {
	bad := func(format string, args ...any) (AssetInput, error) {
		return AssetInput{}, ErrInvalidAsset{Reason: fmt.Sprintf(format, args...)}
	}

	in.Hostname = strings.ToLower(strings.TrimSpace(in.Hostname))
	in.Address = strings.TrimSpace(in.Address)
	in.Protocol = strings.ToLower(strings.TrimSpace(in.Protocol))
	in.CredentialMode = strings.TrimSpace(in.CredentialMode)
	in.CredentialRef = strings.TrimSpace(in.CredentialRef)
	in.Domain = strings.TrimSpace(in.Domain)
	in.OS = strings.TrimSpace(in.OS)
	in.GroupName = strings.TrimSpace(in.GroupName)

	if !reHostname.MatchString(in.Hostname) {
		return bad("hostname must be a DNS name: letters, digits, dots and hyphens")
	}
	if in.Address == "" {
		in.Address = in.Hostname
	}
	if !reAddress.MatchString(in.Address) {
		return bad("address must be a hostname or an IP; write an IPv6 literal in brackets, as [::1]")
	}

	switch in.Protocol {
	case "":
		in.Protocol = "ssh"
	case "ssh", "rdp":
	default:
		return bad("protocol must be ssh or rdp")
	}

	if in.Port == 0 {
		in.Port = 22
		if in.Protocol == "rdp" {
			in.Port = 3389
		}
	}
	if in.Port < 1 || in.Port > 65535 {
		return bad("port must be between 1 and 65535")
	}

	if in.GroupName == "" {
		in.GroupName = "ungrouped"
	}

	// Credential mode and protocol are not independent. A certificate is an SSH
	// construct and the gateway has no password path for SSH, so three of the
	// six combinations cannot open a session — better refused here than
	// discovered by an operator whose connection fails at the target.
	if in.CredentialMode == "" {
		if in.Protocol == "rdp" {
			in.CredentialMode = "injected-password"
		} else {
			in.CredentialMode = "injected-key"
		}
	}
	switch {
	case in.Protocol == "ssh" && in.CredentialMode == "injected-password":
		return bad("SSH assets use an injected key or a certificate; there is no password path")
	case in.Protocol == "rdp" && in.CredentialMode != "injected-password":
		return bad("Remote Desktop assets use an injected password")
	case in.CredentialMode != "injected-key" &&
		in.CredentialMode != "injected-password" &&
		in.CredentialMode != "ca-certificate":
		return bad("credential mode must be injected-key, injected-password or ca-certificate")
	}

	if in.CredentialMode == "ca-certificate" {
		// Nothing to point at. Keeping a stale name here would say the host has
		// a standing credential when the whole point of this mode is that it
		// does not.
		in.CredentialRef = ""
	} else {
		if in.CredentialRef == "" {
			return bad("name the credential in the gateway's vault that should be injected")
		}
		if !reCredentialRef.MatchString(in.CredentialRef) {
			return bad("the credential is a name inside the gateway's vault, not a path: " +
				"letters, digits, dot, underscore and hyphen only")
		}
		if in.CredentialRef == "." || in.CredentialRef == ".." {
			return bad("the credential name must name a file, not a directory")
		}
	}

	if in.Domain != "" {
		if in.Protocol != "rdp" {
			return bad("a Windows domain only applies to a Remote Desktop asset")
		}
		if !reDomain.MatchString(in.Domain) {
			return bad("domain must be a DNS name")
		}
	}

	in.Principals = dedupe(in.Principals)
	if len(in.Principals) > maxPrincipals {
		return bad("an asset may list at most %d principals", maxPrincipals)
	}
	for _, p := range in.Principals {
		if !rePrincipal.MatchString(p) {
			return bad("principal %q is not an account name: letters, digits, dot, "+
				"underscore and hyphen, no colon", p)
		}
	}

	in.Tags = dedupe(in.Tags)
	if len(in.Tags) > maxTags {
		return bad("an asset may carry at most %d tags", maxTags)
	}
	for _, t := range in.Tags {
		if !reTag.MatchString(t) {
			return bad("tag %q is not a tag: letters, digits, dot, colon, underscore and hyphen", t)
		}
	}

	if in.RotationIntervalDays != nil && (*in.RotationIntervalDays < 1 || *in.RotationIntervalDays > 3650) {
		return bad("rotation interval must be between 1 and 3650 days")
	}
	if len(in.OS) > 128 || len(in.GroupName) > 64 {
		return bad("OS and group name must be shorter than 128 and 64 characters")
	}
	return in, nil
}

// dedupe trims, drops empties and sorts, so two inputs that mean the same thing
// are stored the same way.
func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

/* ── Asset writes ────────────────────────────────────────────────────────── */

// CreateAsset adds an asset the console owns.
//
// A hostname that already exists is an error rather than an update: an
// administrator who types a name that is already in the fleet is either
// looking at a different host or has lost track of one, and silently
// overwriting the existing row would hide both.
func (s *Store) CreateAsset(ctx context.Context, in AssetInput) (Asset, error) {
	v, err := in.normalise()
	if err != nil {
		return Asset{}, err
	}

	var id string
	err = s.pool.QueryRow(ctx, `
		INSERT INTO assets (hostname, address, port, protocol, os, tags, group_name,
			principals, credential_mode, credential_ref, domain,
			rotation_interval_days, source, health, host_key_state)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'console','reachable','unpinned')
		RETURNING id::text`,
		v.Hostname, v.Address, v.Port, v.Protocol, v.OS, v.Tags, v.GroupName,
		v.Principals, v.CredentialMode, v.CredentialRef, v.Domain,
		v.RotationIntervalDays).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return Asset{}, ErrInvalidAsset{
				Reason: v.Hostname + " is already in the inventory",
			}
		}
		return Asset{}, err
	}
	return s.AssetByID(ctx, id)
}

// UpdateAsset replaces the fields an administrator owns.
//
// Observed state is untouched: host-key pins, agent liveness and posture are
// facts reported by something that looked, not settings.
func (s *Store) UpdateAsset(ctx context.Context, id string, in AssetInput) (Asset, error) {
	v, err := in.normalise()
	if err != nil {
		return Asset{}, err
	}

	tag, err := s.pool.Exec(ctx, `
		UPDATE assets SET
			hostname = $2, address = $3, port = $4, protocol = $5, os = $6,
			tags = $7, group_name = $8, principals = $9, credential_mode = $10,
			credential_ref = $11, domain = $12, rotation_interval_days = $13,
			source = 'console', updated_at = now()
		WHERE id = $1::uuid AND archived_at IS NULL`,
		id, v.Hostname, v.Address, v.Port, v.Protocol, v.OS, v.Tags, v.GroupName,
		v.Principals, v.CredentialMode, v.CredentialRef, v.Domain,
		v.RotationIntervalDays)
	if err != nil {
		if isUniqueViolation(err) {
			return Asset{}, ErrInvalidAsset{
				Reason: v.Hostname + " is already in the inventory",
			}
		}
		return Asset{}, err
	}
	if tag.RowsAffected() == 0 {
		return Asset{}, ErrAssetNotFound
	}
	return s.AssetByID(ctx, id)
}

// ArchiveAsset retires an asset without deleting it.
//
// It leaves the inventory, the console and every gateway's next sync. Its
// sessions and audit entries keep pointing at a row that still exists, because
// the history of a host is the part of it that matters after it is gone.
func (s *Store) ArchiveAsset(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE assets SET archived_at = now(), updated_at = now()
		 WHERE id = $1::uuid AND archived_at IS NULL`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrAssetNotFound
	}
	return nil
}

// AssetByID reads one live asset.
func (s *Store) AssetByID(ctx context.Context, id string) (Asset, error) {
	rows, err := s.pool.Query(ctx, assetSelect+`
		WHERE a.id = $1::uuid AND a.archived_at IS NULL`, id)
	if err != nil {
		return Asset{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Asset{}, err
		}
		return Asset{}, ErrAssetNotFound
	}
	return scanAsset(rows)
}

func isUniqueViolation(err error) bool {
	return strings.Contains(err.Error(), "SQLSTATE 23505")
}

/* ── Assignment ──────────────────────────────────────────────────────────── */

// Assignment is one person's access to one asset.
type Assignment struct {
	AssetID    string    `json:"assetId"`
	Hostname   string    `json:"hostname"`
	UserEmail  string    `json:"userEmail"`
	Principals []string  `json:"principals"`
	GrantedBy  string    `json:"grantedBy"`
	GrantedAt  time.Time `json:"grantedAt"`
}

// Assignments lists who may reach one asset.
func (s *Store) Assignments(ctx context.Context, assetID string) ([]Assignment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT g.asset_id::text, a.hostname, g.user_email, g.principals,
		       g.granted_by, g.granted_at
		FROM asset_assignments g JOIN assets a ON a.id = g.asset_id
		WHERE g.asset_id = $1::uuid
		ORDER BY g.user_email`, assetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Assignment{}
	for rows.Next() {
		var g Assignment
		if err := rows.Scan(&g.AssetID, &g.Hostname, &g.UserEmail, &g.Principals,
			&g.GrantedBy, &g.GrantedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// SetAssignment records which principals a person may assume on an asset.
//
// Principals are checked against the asset's own list: assigning an account the
// asset does not permit would produce access that looks granted in the console
// and is refused at the gateway, which is the worst of both.
//
// An empty principal list is a removal, not an assignment of nothing. "Assigned
// but able to do nothing" is a state with no meaning and one more way for a
// list of who-can-reach-what to be misread.
func (s *Store) SetAssignment(
	ctx context.Context, assetID, email string, principals []string, actor string,
) error {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return ErrInvalidAsset{Reason: "an assignment needs an account to assign to"}
	}
	principals = dedupe(principals)
	if len(principals) == 0 {
		return s.RemoveAssignment(ctx, assetID, email)
	}
	if len(principals) > maxPrincipals {
		return ErrInvalidAsset{Reason: fmt.Sprintf(
			"an assignment may name at most %d principals", maxPrincipals)}
	}

	asset, err := s.AssetByID(ctx, assetID)
	if err != nil {
		return err
	}
	allowed := map[string]bool{}
	for _, p := range asset.Principals {
		allowed[p] = true
	}
	for _, p := range principals {
		if !allowed[p] {
			return ErrInvalidAsset{Reason: fmt.Sprintf(
				"%s does not permit the principal %q; add it to the asset first",
				asset.Hostname, p)}
		}
	}

	_, err = s.pool.Exec(ctx, `
		INSERT INTO asset_assignments (asset_id, user_email, principals, granted_by)
		VALUES ($1::uuid, $2, $3, $4)
		ON CONFLICT (asset_id, user_email) DO UPDATE SET
			principals = EXCLUDED.principals,
			granted_by = EXCLUDED.granted_by,
			granted_at = now()`,
		assetID, email, principals, strings.ToLower(actor))
	return err
}

// RemoveAssignment ends a person's access to an asset.
func (s *Store) RemoveAssignment(ctx context.Context, assetID, email string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM asset_assignments WHERE asset_id = $1::uuid AND user_email = $2`,
		assetID, strings.ToLower(strings.TrimSpace(email)))
	return err
}

// AssetsForUser lists the assets assigned to one person.
//
// Each asset's principal list is narrowed to what that person may actually
// assume, so the console shows them the accounts they can use rather than every
// account the host has. An administrator removing a principal from the asset
// takes it away here too, without having to revisit every assignment: the
// intersection is computed, not stored.
func (s *Store) AssetsForUser(ctx context.Context, email string) ([]Asset, error) {
	rows, err := s.pool.Query(ctx, assetSelect+`
		JOIN asset_assignments g ON g.asset_id = a.id
		WHERE a.archived_at IS NULL AND g.user_email = lower($1)
		ORDER BY a.hostname`, email)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	assets := []Asset{}
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		assets = append(assets, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	granted, err := s.grantedPrincipals(ctx, email)
	if err != nil {
		return nil, err
	}
	for i := range assets {
		assets[i].Principals = intersect(assets[i].Principals, granted[assets[i].ID])
	}
	return assets, nil
}

func (s *Store) grantedPrincipals(ctx context.Context, email string) (map[string][]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT asset_id::text, principals FROM asset_assignments WHERE user_email = lower($1)`,
		email)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string][]string{}
	for rows.Next() {
		var id string
		var ps []string
		if err := rows.Scan(&id, &ps); err != nil {
			return nil, err
		}
		out[id] = ps
	}
	return out, rows.Err()
}

func intersect(a, b []string) []string {
	in := map[string]bool{}
	for _, v := range b {
		in[v] = true
	}
	out := []string{}
	for _, v := range a {
		if in[v] {
			out = append(out, v)
		}
	}
	return out
}

// AssignmentAllows reports whether a person may open a session as a principal
// on a host.
//
// Both lists have to contain the principal: the assignment names what the
// person was given, and the asset names what it permits at all. Checking only
// the assignment would let an account removed from the asset survive in
// whatever assignments still mention it.
//
// The target may be the stored hostname or its short form, matching how the
// gateway resolves a target typed at an ssh(1) command line.
func (s *Store) AssignmentAllows(ctx context.Context, email, target, principal string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM assets a
			JOIN asset_assignments g ON g.asset_id = a.id
			WHERE a.archived_at IS NULL
			  AND (a.hostname = lower($2) OR split_part(a.hostname, '.', 1) = lower($2))
			  AND g.user_email = lower($1)
			  AND $3 = ANY(g.principals)
			  AND $3 = ANY(a.principals)
		)`, email, target, principal).Scan(&ok)
	return ok, err
}

// AssetIsManaged reports whether a live asset exists for a target.
//
// Used to tell "you are not assigned this host" apart from "this host is not in
// the inventory". They need different answers: one is a question for an
// administrator, the other is a host Argus does not broker at all.
func (s *Store) AssetIsManaged(ctx context.Context, target string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM assets a
			WHERE a.archived_at IS NULL
			  AND (a.hostname = lower($1) OR split_part(a.hostname, '.', 1) = lower($1))
		)`, target).Scan(&ok)
	return ok, err
}

/* ── What a gateway is told ──────────────────────────────────────────────── */

// InventoryAssignment is one person's access, as a gateway receives it.
type InventoryAssignment struct {
	Email      string   `json:"email"`
	Principals []string `json:"principals"`
}

// InventoryAsset is one host, as a gateway receives it.
//
// Field names match internal/gateway.Asset's JSON so the file inventory and the
// synced one are the same shape. The credential is a name; the gateway resolves
// it against its own vault directory and nothing here can make it look
// elsewhere.
type InventoryAsset struct {
	Hostname       string                `json:"hostname"`
	Address        string                `json:"address"`
	Port           int                   `json:"port"`
	Protocol       string                `json:"protocol"`
	Principals     []string              `json:"principals"`
	CredentialMode string                `json:"credential_mode"`
	CredentialRef  string                `json:"credential_ref"`
	Domain         string                `json:"domain,omitempty"`
	Assignments    []InventoryAssignment `json:"assignments"`
}

// Inventory is the whole answer to "what may this gateway broker, and for whom".
type Inventory struct {
	GeneratedAt time.Time        `json:"generatedAt"`
	Assets      []InventoryAsset `json:"assets"`
	// Unrestricted are the accounts assignment does not gate: admins and
	// owners. They are exempt in the console for the same reason — someone has
	// to be able to act when the approval chain itself is broken — and a
	// gateway that did not know this would refuse them on the ssh(1) path while
	// the browser let them through, which is two products, not one.
	Unrestricted []string `json:"unrestricted"`
}

// InventorySnapshot builds what every gateway syncs.
//
// One query for assets and one for assignments rather than a join: the
// assignment list is the part that grows, and stitching in memory keeps the
// asset row from being repeated once per assigned person.
func (s *Store) InventorySnapshot(ctx context.Context) (Inventory, error) {
	inv := Inventory{GeneratedAt: time.Now().UTC(), Assets: []InventoryAsset{}, Unrestricted: []string{}}

	rows, err := s.pool.Query(ctx, `
		SELECT id::text, hostname, address, port, protocol, principals,
		       credential_mode, credential_ref, domain
		FROM assets WHERE archived_at IS NULL ORDER BY hostname`)
	if err != nil {
		return Inventory{}, err
	}
	defer rows.Close()

	ids := []string{}
	byID := map[string]*InventoryAsset{}
	for rows.Next() {
		var id string
		var a InventoryAsset
		if err := rows.Scan(&id, &a.Hostname, &a.Address, &a.Port, &a.Protocol,
			&a.Principals, &a.CredentialMode, &a.CredentialRef, &a.Domain); err != nil {
			return Inventory{}, err
		}
		a.Assignments = []InventoryAssignment{}
		inv.Assets = append(inv.Assets, a)
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return Inventory{}, err
	}
	for i := range inv.Assets {
		byID[ids[i]] = &inv.Assets[i]
	}

	grants, err := s.pool.Query(ctx, `
		SELECT asset_id::text, user_email, principals
		FROM asset_assignments ORDER BY user_email`)
	if err != nil {
		return Inventory{}, err
	}
	defer grants.Close()

	for grants.Next() {
		var id string
		var g InventoryAssignment
		if err := grants.Scan(&id, &g.Email, &g.Principals); err != nil {
			return Inventory{}, err
		}
		if a, ok := byID[id]; ok {
			a.Assignments = append(a.Assignments, g)
		}
	}
	if err := grants.Err(); err != nil {
		return Inventory{}, err
	}

	admins, err := s.pool.Query(ctx,
		`SELECT lower(email) FROM users
		 WHERE role IN ('admin','owner') AND disabled_at IS NULL ORDER BY email`)
	if err != nil {
		return Inventory{}, err
	}
	defer admins.Close()

	for admins.Next() {
		var email string
		if err := admins.Scan(&email); err != nil {
			return Inventory{}, err
		}
		inv.Unrestricted = append(inv.Unrestricted, email)
	}
	return inv, admins.Err()
}
