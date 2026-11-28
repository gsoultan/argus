package control

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gsoultan/caduceus"
)

// People shared across organizations.
//
// A person belongs to one organization: the one that holds their password,
// their second factor and their recovery codes. That is their home. Another
// organization may add the same person without issuing them a second
// credential: it gets a users row of its own, with its own role, its own
// disabling and its own audit trail, that points home for the sign-in.
//
// Nothing crosses the boundary but the credential check. The organization
// that shared someone in sees that they sign in elsewhere and nothing about
// where; the home organization sees which organizations use its person's
// account, because its own audit log records each one. Neither reads the
// other's tables.
//
// Sharing is opt-in at home. An account can be added elsewhere only when the
// organization it belongs to has marked it shareable, so an owner typing an
// address into the "shared person" form learns nothing about addresses whose
// home has not allowed it: those look exactly like addresses nobody uses.

var (
	// ErrSharedAccount is returned when an operation that belongs to a
	// person's home organization is attempted where they were shared in.
	ErrSharedAccount = errors.New("this person signs in through another organization")
	// ErrNoShareable is the one answer for every address that cannot be
	// shared in: unknown, not shareable, disabled, or in no organization.
	ErrNoShareable = errors.New("no account that can be shared uses this address")
	// ErrAmbiguousHome means more than one organization has a shareable
	// account for the address, and the caller must say which.
	ErrAmbiguousHome = errors.New(
		"more than one organization has an account for this address; enter the one they sign in with")
	// ErrAccountExists means the organization already has its own account
	// for the address.
	ErrAccountExists = errors.New("already has an account in this organization")
	// ErrOwnAccountHere means the organization has an account of its own for
	// the address, with its own password, still in use. Adding the person
	// from another organization would take that account over, so it is
	// refused until the organization removes it.
	ErrOwnAccountHere = errors.New("already has an account of its own in this organization, " +
		"with its own password. Remove it there first (Teams, People, Remove), then add them again; " +
		"the removed account becomes the one they share, with its history")
	// ErrNotMember is returned by MemberOf when the signed-in identity has no
	// usable account in the target organization.
	ErrNotMember = errors.New("you are not a member of that organization")
)

// tenantByIDOrDefault resolves an organization id, the default included.
func (s *Store) tenantByIDOrDefault(ctx context.Context, id string) (*Tenant, error) {
	if id == DefaultTenantID {
		return s.DefaultTenant(ctx), nil
	}
	return s.TenantByID(ctx, id)
}

// homeContext returns the context of the organization that holds email's
// credentials: ctx itself for a person who belongs here.
//
// A disabled home organization is an error: its people cannot sign in
// anywhere, including where they were shared.
func (s *Store) homeContext(ctx context.Context, email string) (context.Context, error) {
	a, err := s.Account(ctx, email)
	if err != nil {
		return nil, err
	}
	if !a.Shared() {
		return ctx, nil
	}
	home, err := s.tenantByIDOrDefault(ctx, a.HomeTenant)
	if err != nil {
		return nil, fmt.Errorf("home organization of %s: %w", email, err)
	}
	if home.Disabled() {
		return nil, ErrTenantDisabled
	}
	return WithTenant(ctx, home), nil
}

// Credentials is the account as the sign-in path needs it: this
// organization's row for role, name and disabling, carrying the password,
// second factor and recovery codes from home. The returned context is the one
// to spend recovery codes and enrol second factors in.
//
// Disabled in either organization means disabled. The id is the home row's,
// which is what the recovery code table there is keyed by.
func (s *Store) Credentials(ctx context.Context, email string) (*Account, context.Context, error) {
	local, err := s.Account(ctx, email)
	if err != nil {
		return nil, nil, err
	}
	if !local.Shared() {
		return local, ctx, nil
	}
	home, err := s.tenantByIDOrDefault(ctx, local.HomeTenant)
	if err != nil {
		return nil, nil, fmt.Errorf("home organization of %s: %w", email, err)
	}
	if home.Disabled() {
		return nil, nil, ErrNoAccount
	}
	// A home that took this organization away has taken the person away.
	if ok, err := s.shareAllowed(ctx, email, home.ID, tenantIDOf(ctx)); err != nil {
		return nil, nil, err
	} else if !ok {
		return nil, nil, ErrNoAccount
	}
	hctx := WithTenant(ctx, home)
	cred, err := s.Account(hctx, email)
	if err != nil {
		return nil, nil, err
	}
	if cred.Shared() {
		// A home that itself points elsewhere is a corrupt index, not a
		// chain to follow.
		return nil, nil, fmt.Errorf("account %s: home organization %s does not hold it", email, home.Slug)
	}
	merged := *local
	merged.ID = cred.ID
	merged.PasswordHash = cred.PasswordHash
	merged.TOTPSecret = cred.TOTPSecret
	merged.MFAEnrolled = cred.MFAEnrolled
	merged.PasswordChangeRequired = cred.PasswordChangeRequired
	merged.PasswordExpiresAt = cred.PasswordExpiresAt
	merged.Disabled = local.Disabled || cred.Disabled
	return &merged, hctx, nil
}

// homeIDOf is the id of the organization holding this row's credentials.
func homeIDOf(ctx context.Context, a *Account) string {
	if a.Shared() {
		return a.HomeTenant
	}
	return tenantIDOf(ctx)
}

// recordMembership writes one row of the platform index inside a tenant's
// transaction. The qualified name reaches the registry whatever the
// connection's search_path.
func recordMembership(ctx context.Context, tx *caduceus.Tx, email, tenantID, homeID string) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO argus_platform.memberships (email, tenant_id, home_tenant)
		VALUES ($1, $2, $3)
		ON CONFLICT (email, tenant_id) DO UPDATE SET home_tenant = EXCLUDED.home_tenant`,
		normaliseEmail(email), tenantID, homeID); err != nil {
		return fmt.Errorf("record membership: %w", err)
	}
	return nil
}

// backfillMemberships fills the index from every organization's users table.
//
// Idempotent and run at every start, so an install upgraded to this version
// indexes the accounts it already had, and an index that was ever out of step
// with a users table is put right.
func (s *Store) backfillMemberships(ctx context.Context) error {
	list, err := s.Tenants(ctx)
	if err != nil {
		return err
	}
	for i := range list {
		t := &list[i]
		pool := s.pool
		if !t.IsDefault() {
			pool = s.tdb(WithTenant(ctx, t)).pool
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO argus_platform.memberships (email, tenant_id, home_tenant)
			SELECT lower(email), $1, COALESCE(home_tenant, $1)
			  FROM users
			 WHERE password_hash IS NOT NULL OR home_tenant IS NOT NULL
			ON CONFLICT (email, tenant_id) DO UPDATE SET home_tenant = EXCLUDED.home_tenant`,
			t.ID); err != nil {
			return fmt.Errorf("index people of organization %s: %w", t.Slug, err)
		}
	}
	return nil
}

// SharedMember is what adding a shared person takes.
type SharedMember struct {
	Email       string
	DisplayName string
	Role        string
	// HomeSlug names the organization they sign in with, needed only when
	// more than one has a shareable account for the address.
	HomeSlug string
}

// AddSharedMember adds a person who already signs in to another organization
// to ctx's organization, with the role given here.
//
// The home is found through the index and then checked against the home's
// own users table, which is the authority: the person must exist there, have
// a password, be enabled and be marked shareable. An address that fails any
// of those gets ErrNoShareable, the same answer as an address nobody uses.
// Returns the new row and the home organization, so the caller can write to
// both audit logs.
func (s *Store) AddSharedMember(ctx context.Context, in SharedMember) (*Account, *Tenant, error) {
	email := normaliseEmail(in.Email)
	target := tenantIDOf(ctx)

	// Our own row first: an existing account here is refused before the
	// index is consulted, so the error says what the caller can already see
	// in their own list and nothing about anywhere else. A removed account,
	// of their own or shared, is not refused: adding them again turns it
	// into the shared one (below), the way the caller expects after Remove.
	if existing, err := s.Account(ctx, email); err == nil && !existing.Disabled {
		if existing.Shared() {
			return nil, nil, ErrAccountExists
		}
		if existing.PasswordHash != "" {
			return nil, nil, ErrOwnAccountHere
		}
	} else if err != nil && !errors.Is(err, ErrNoAccount) {
		return nil, nil, err
	}

	rows, err := s.pool.Query(ctx, `
		SELECT m.home_tenant
		  FROM argus_platform.memberships m
		  JOIN argus_platform.tenants t ON t.id = m.home_tenant
		 WHERE m.email = $1 AND m.tenant_id = m.home_tenant
		   AND m.home_tenant <> $2 AND t.disabled_at IS NULL
		   AND ($3 = '' OR t.slug = $3)
		 ORDER BY m.home_tenant`, email, target, strings.ToLower(strings.TrimSpace(in.HomeSlug)))
	if err != nil {
		return nil, nil, err
	}
	var candidates []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, nil, err
		}
		candidates = append(candidates, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	var (
		home *Tenant
		cred *Account
	)
	for _, id := range candidates {
		t, err := s.tenantByIDOrDefault(ctx, id)
		if err != nil {
			continue
		}
		a, err := s.Account(WithTenant(ctx, t), email)
		if err != nil || a.Shared() || a.Disabled || a.PasswordHash == "" {
			continue
		}
		// Its home must have named this organization. The old "shareable
		// with anyone" flag is not an allowance.
		if ok, err := s.shareAllowed(ctx, email, t.ID, target); err != nil {
			return nil, nil, err
		} else if !ok {
			continue
		}
		if home != nil {
			return nil, nil, ErrAmbiguousHome
		}
		home, cred = t, a
	}
	if home == nil {
		return nil, nil, ErrNoShareable
	}

	displayName := strings.TrimSpace(in.DisplayName)
	if displayName == "" {
		displayName = cred.DisplayName
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var (
		id         string
		wasOwnHome bool
	)
	// A row with this address can already exist and become theirs:
	//   - no password and no home: someone put on a team before they had an
	//     account; pointing it home is giving it one;
	//   - removed (disabled), whether it had its own password or was shared:
	//     adding them again keeps the row, so the audit log, teams and
	//     grants keep pointing at one person, and drops every credential of
	//     its own, so only the home's password and second factor sign in.
	// An account in use is left alone; the WHERE re-checks it under the row
	// lock, in case it was turned back on since the check above.
	err = tx.QueryRow(ctx, `
		WITH before AS (
			SELECT home_tenant IS NULL AND password_hash IS NOT NULL AS own
			  FROM users WHERE email = $1)
		INSERT INTO users (email, display_name, role, home_tenant)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (email) DO UPDATE
		   SET display_name = EXCLUDED.display_name,
		       role         = EXCLUDED.role,
		       home_tenant  = EXCLUDED.home_tenant,
		       password_hash = NULL,
		       password_change_required = false,
		       password_expires_at = NULL,
		       totp_secret = NULL,
		       mfa_enrolled_at = NULL,
		       shareable = false,
		       disabled_at = NULL,
		       disabled_by = NULL
		 WHERE (users.password_hash IS NULL AND users.home_tenant IS NULL)
		    OR users.disabled_at IS NOT NULL
		RETURNING id::text, coalesce((SELECT own FROM before), false)`,
		email, displayName, in.Role, home.ID).Scan(&id, &wasOwnHome)
	if errors.Is(err, caduceus.ErrNoRows) {
		return nil, nil, ErrAccountExists
	}
	if err != nil {
		return nil, nil, fmt.Errorf("add shared person: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM recovery_codes WHERE user_id = $1`, id); err != nil {
		return nil, nil, fmt.Errorf("drop old recovery codes: %w", err)
	}
	if wasOwnHome {
		// This organization was their home for this address. It is not any
		// more, so what it allowed others is void: they now come from home.
		if _, err := tx.Exec(ctx, `
			DELETE FROM argus_platform.share_allowances WHERE email = $1 AND home_tenant = $2`,
			email, target); err != nil {
			return nil, nil, fmt.Errorf("drop old allowances: %w", err)
		}
	}
	if err := recordMembership(ctx, tx, email, target, home.ID); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	if err := s.ensurePersonalLoginNames(ctx); err != nil {
		return nil, nil, fmt.Errorf("name server account: %w", err)
	}
	a, err := s.Account(ctx, email)
	if err != nil {
		return nil, nil, err
	}
	return a, home, nil
}

// SetAccountShareable lets, or stops letting, other organizations add one of
// this organization's people. Only where they belong: a shared-in row has no
// say over the identity it borrows.
//
// Turning it off stops new sharing; organizations that already have the
// person keep them. Disabling the account here is what revokes them
// everywhere.
func (s *Store) SetAccountShareable(ctx context.Context, email string, shareable bool) error {
	a, err := s.Account(ctx, email)
	if err != nil {
		return err
	}
	if a.Shared() {
		return ErrSharedAccount
	}
	if a.PasswordHash == "" {
		return ErrNoAccount
	}
	if a.Shareable == shareable {
		return ErrAlreadyInState
	}
	_, err = s.db(ctx).Exec(ctx,
		`UPDATE users SET shareable = $2 WHERE lower(email) = lower($1)`, email, shareable)
	return err
}

// Membership is one organization a person can switch to.
type Membership struct {
	Tenant
	Role string
	// Home is true for the organization holding their credentials.
	Home bool
}

// OrganizationsFor lists the enabled organizations the signed-in identity --
// this address, with its credentials where this row says they are -- has an
// enabled account in. Each is confirmed against that organization's own
// users table, so the index can only ever offer less than the truth.
func (s *Store) OrganizationsFor(ctx context.Context, email string) ([]Membership, error) {
	local, err := s.Account(ctx, email)
	if err != nil {
		return nil, err
	}
	homeID := homeIDOf(ctx, local)
	rows, err := s.pool.Query(ctx, `
		SELECT `+qualify("t", tenantColumns)+`
		  FROM argus_platform.memberships m
		  JOIN argus_platform.tenants t ON t.id = m.tenant_id
		 WHERE m.email = $1 AND m.home_tenant = $2 AND t.disabled_at IS NULL
		   AND (m.tenant_id = m.home_tenant OR EXISTS (
		        SELECT 1 FROM argus_platform.share_allowances a
		         WHERE a.email = m.email AND a.home_tenant = m.home_tenant AND a.tenant_id = m.tenant_id))
		 ORDER BY (t.id <> 'default'), lower(t.name), t.id`, normaliseEmail(email), homeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tenants []Tenant
	for rows.Next() {
		t, err := scanTenant(rows)
		if err != nil {
			return nil, err
		}
		tenants = append(tenants, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Membership, 0, len(tenants))
	for i := range tenants {
		t := &tenants[i]
		if t.IsDefault() {
			t = defaultTenantFrom(t)
		}
		a, err := s.Account(WithTenant(ctx, t), email)
		if err != nil || a.Disabled || homeIDOf(WithTenant(ctx, t), a) != homeID {
			continue
		}
		out = append(out, Membership{Tenant: *t, Role: a.Role, Home: t.ID == homeID})
	}
	return out, nil
}

// MemberOf returns the signed-in identity's account in target, or ErrNotMember.
//
// The identity is the one signed in to ctx's organization: the same address
// with the same home. An organization that made its own account for the same
// address is somebody else's, and is not reachable from here.
func (s *Store) MemberOf(ctx context.Context, email string, target *Tenant) (*Account, error) {
	local, err := s.Account(ctx, email)
	if err != nil {
		return nil, ErrNotMember
	}
	if target.Disabled() {
		return nil, ErrNotMember
	}
	tctx := WithTenant(ctx, target)
	a, err := s.Account(tctx, email)
	if err != nil || a.Disabled || homeIDOf(tctx, a) != homeIDOf(ctx, local) {
		return nil, ErrNotMember
	}
	// Disabled at home refuses the switch too, as it refuses a sign-in.
	cred, _, err := s.Credentials(tctx, email)
	if err != nil || cred.Disabled || cred.PasswordHash == "" {
		return nil, ErrNotMember
	}
	return a, nil
}

// fillSharedMFA replaces the second-factor flag of every shared-in row with
// the one from its home, grouped by home so each is read once.
func (s *Store) fillSharedMFA(ctx context.Context, list []Account) error {
	byHome := map[string][]int{}
	for i := range list {
		if list[i].Shared() {
			byHome[list[i].HomeTenant] = append(byHome[list[i].HomeTenant], i)
		}
	}
	for homeID, idx := range byHome {
		home, err := s.tenantByIDOrDefault(ctx, homeID)
		if err != nil || home.Disabled() {
			continue
		}
		emails := make([]string, 0, len(idx))
		for _, i := range idx {
			emails = append(emails, strings.ToLower(list[i].Email))
		}
		rows, err := s.db(WithTenant(ctx, home)).Query(ctx, `
			SELECT lower(email), mfa_enrolled_at IS NOT NULL FROM users
			 WHERE lower(email) = ANY($1) AND home_tenant IS NULL`, emails)
		if err != nil {
			return err
		}
		enrolled := map[string]bool{}
		for rows.Next() {
			var e string
			var on bool
			if err := rows.Scan(&e, &on); err != nil {
				rows.Close()
				return err
			}
			enrolled[e] = on
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, i := range idx {
			list[i].MFAEnrolled = enrolled[strings.ToLower(list[i].Email)]
		}
	}
	return nil
}

// qualify prefixes each column of a comma-separated list with a table alias.
func qualify(alias, cols string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

// ── Which organizations may have a person ───────────────────────────────────
//
// A home names each organization that may have one of its people, one row in
// argus_platform.share_allowances per organization. There is no "any
// organization" setting: the old shareable flag only marks people who had it,
// so the console can ask their home to pick where.
//
// The allowance is checked where an identity is used, not only where it is
// added: adding them (AddSharedMember), signing in or switching (Credentials,
// MemberOf), every request on a cookie (SessionState) and the organization
// menu (OrganizationsFor). So a home that takes an organization away refuses
// its person there at their next request, without writing that
// organization's tables.
//
// An admin or owner of both organizations holds both decisions already, so
// they may allow and add in one step; both chains record who did it.

// ErrCannotManageThere is the answer for an organization the caller is not an
// admin or owner of, or not a member of at all: the same 404 either way.
var ErrCannotManageThere = errors.New("you are not an admin or owner of that organization")

// shareAllowed reports whether home allows email to be used by tenant. Home
// itself is always allowed.
func (s *Store) shareAllowed(ctx context.Context, email, home, tenant string) (bool, error) {
	if home == tenant {
		return true, nil
	}
	var ok bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM argus_platform.share_allowances
		                WHERE email = $1 AND home_tenant = $2 AND tenant_id = $3)`,
		normaliseEmail(email), home, tenant).Scan(&ok)
	return ok, err
}

// ManagesOrganization returns the signed-in identity's account in t when it
// is an enabled admin or owner there; ErrCannotManageThere otherwise.
func (s *Store) ManagesOrganization(ctx context.Context, actor string, t *Tenant) (*Account, error) {
	a, err := s.MemberOf(ctx, actor, t)
	if err != nil || (a.Role != "admin" && a.Role != "owner") {
		return nil, ErrCannotManageThere
	}
	return a, nil
}

// ShareAllowance is one organization a person's home allows to have them.
type ShareAllowance struct {
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	GrantedBy string    `json:"grantedBy"`
	GrantedAt time.Time `json:"grantedAt"`
	// Added is set once that organization has added them, with Role theirs
	// there; Disabled when it has since removed them.
	Added    bool   `json:"added"`
	Role     string `json:"role,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
}

// homePerson is one of ctx's own people: signs in here, has a password.
func (s *Store) homePerson(ctx context.Context, email string) (*Account, error) {
	a, err := s.Account(ctx, email)
	if err != nil {
		return nil, err
	}
	if a.Shared() {
		return nil, ErrSharedAccount
	}
	if a.PasswordHash == "" {
		return nil, ErrNoAccount
	}
	return a, nil
}

// ShareAllowances lists the organizations ctx's organization allows to have
// one of its people, and whether each has added them.
func (s *Store) ShareAllowances(ctx context.Context, email string) ([]ShareAllowance, error) {
	if _, err := s.homePerson(ctx, email); err != nil {
		return nil, err
	}
	home := tenantIDOf(ctx)
	rows, err := s.pool.Query(ctx, `
		SELECT `+qualify("t", tenantColumns)+`, a.granted_by, a.granted_at
		  FROM argus_platform.share_allowances a
		  JOIN argus_platform.tenants t ON t.id = a.tenant_id
		 WHERE a.email = $1 AND a.home_tenant = $2 AND t.disabled_at IS NULL
		 ORDER BY lower(t.name), t.id`, normaliseEmail(email), home)
	if err != nil {
		return nil, err
	}
	type row struct {
		t    Tenant
		by   string
		when time.Time
	}
	var list []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.t.ID, &r.t.Slug, &r.t.Name, &r.t.Schema, &r.t.CreatedAt, &r.t.CreatedBy,
			&r.t.DisabledAt, &r.t.DisabledBy, &r.by, &r.when); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]ShareAllowance, 0, len(list))
	for i := range list {
		t := &list[i].t
		if t.IsDefault() {
			t = defaultTenantFrom(t)
		}
		al := ShareAllowance{Slug: t.Slug, Name: t.Name, GrantedBy: list[i].by, GrantedAt: list[i].when}
		// Read from that organization's own table: the index can say a row
		// exists, only the table says what it is now.
		if a, err := s.Account(WithTenant(ctx, t), email); err == nil && a.HomeTenant == home {
			al.Added, al.Role, al.Disabled = true, a.Role, a.Disabled
		}
		out = append(out, al)
	}
	return out, nil
}

// AllowShare lets target have one of ctx's own people. It returns whether it
// added an allowance, so the caller audits only a real change. Choosing any
// organization retires the old "shareable with anyone" flag for them.
func (s *Store) AllowShare(ctx context.Context, actor, email string, target *Tenant) (bool, error) {
	a, err := s.homePerson(ctx, email)
	if err != nil {
		return false, err
	}
	if a.Disabled {
		return false, ErrNoShareable
	}
	home := tenantIDOf(ctx)
	if target.ID == home || target.Disabled() {
		return false, ErrUnknownTenant
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO argus_platform.share_allowances (email, home_tenant, tenant_id, granted_by)
		VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, normaliseEmail(email), home, target.ID, actor)
	if err != nil {
		return false, err
	}
	if a.Shareable {
		if _, err := s.db(ctx).Exec(ctx,
			`UPDATE users SET shareable = false WHERE lower(email) = lower($1)`, email); err != nil {
			return false, err
		}
	}
	return tag.RowsAffected() > 0, nil
}

// RevokeShare takes target away from one of ctx's own people. Their row there
// stays, for that organization's records, but nothing signs them in to it.
func (s *Store) RevokeShare(ctx context.Context, email string, target *Tenant) error {
	if _, err := s.homePerson(ctx, email); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM argus_platform.share_allowances
		 WHERE email = $1 AND home_tenant = $2 AND tenant_id = $3`,
		normaliseEmail(email), tenantIDOf(ctx), target.ID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrAlreadyInState
	}
	return nil
}

// OtherPerson is someone whose home is an organization the caller manages.
type OtherPerson struct {
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	Role        string `json:"role"`
}

// ManagedOrganization is another organization the signed-in identity is an
// admin or owner of, and its own people who are not in ctx's organization.
type ManagedOrganization struct {
	Slug     string        `json:"slug"`
	Name     string        `json:"name"`
	YourRole string        `json:"yourRole"`
	People   []OtherPerson `json:"people"`
}

// ManagedOrganizations lists every other organization the signed-in identity
// is an admin or owner of, each read from its own users table. An
// organization they only belong to with a lesser role, or not at all, is not
// named.
func (s *Store) ManagedOrganizations(ctx context.Context, actor string) ([]ManagedOrganization, error) {
	here := tenantIDOf(ctx)
	mine, err := s.OrganizationsFor(ctx, actor)
	if err != nil {
		return nil, err
	}
	present := map[string]bool{}
	rows, err := s.db(ctx).Query(ctx, `
		SELECT lower(email) FROM users
		 WHERE (password_hash IS NOT NULL OR home_tenant IS NOT NULL) AND disabled_at IS NULL`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			rows.Close()
			return nil, err
		}
		present[e] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := []ManagedOrganization{}
	for i := range mine {
		m := &mine[i]
		if m.ID == here || (m.Role != "admin" && m.Role != "owner") {
			continue
		}
		t := m.Tenant
		org := ManagedOrganization{Slug: t.Slug, Name: t.Name, YourRole: m.Role, People: []OtherPerson{}}
		rows, err := s.db(WithTenant(ctx, &t)).Query(ctx, `
			SELECT lower(email), display_name, role FROM users
			 WHERE disabled_at IS NULL AND home_tenant IS NULL AND password_hash IS NOT NULL
			 ORDER BY lower(display_name), lower(email)`)
		if err != nil {
			return nil, fmt.Errorf("people of organization %s: %w", t.Slug, err)
		}
		for rows.Next() {
			var p OtherPerson
			if err := rows.Scan(&p.Email, &p.DisplayName, &p.Role); err != nil {
				rows.Close()
				return nil, err
			}
			if !present[p.Email] {
				org.People = append(org.People, p)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		out = append(out, org)
	}
	return out, nil
}

// OtherOrganizationsOf maps each of ctx's own people to the other
// organizations that have added them and that their home still allows, by
// name, for the People list. Read from the index; ShareAllowances is the
// exact answer for one person.
func (s *Store) OtherOrganizationsOf(ctx context.Context) (map[string][]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT m.email, t.name
		  FROM argus_platform.memberships m
		  JOIN argus_platform.share_allowances a
		    ON a.email = m.email AND a.home_tenant = m.home_tenant AND a.tenant_id = m.tenant_id
		  JOIN argus_platform.tenants t ON t.id = m.tenant_id
		 WHERE m.home_tenant = $1 AND m.tenant_id <> $1 AND t.disabled_at IS NULL
		 ORDER BY lower(t.name)`, tenantIDOf(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var e, name string
		if err := rows.Scan(&e, &name); err != nil {
			return nil, err
		}
		out[e] = append(out[e], name)
	}
	return out, rows.Err()
}

// HasShareAllowances reports, for the People list, which of ctx's own people
// have any organization chosen; with the legacy shareable flag it tells "pick
// where" apart.
func (s *Store) HasShareAllowances(ctx context.Context) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT email FROM argus_platform.share_allowances WHERE home_tenant = $1`, tenantIDOf(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out[e] = true
	}
	return out, rows.Err()
}
