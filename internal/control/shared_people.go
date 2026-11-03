package control

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
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
	// ErrNotMember is returned by MemberOf when the signed-in identity has no
	// usable account in the target organization.
	ErrNotMember = errors.New("you are not a member of that organization")
)

// tenantByIDOrDefault resolves an organization id, the default included.
func (s *Store) tenantByIDOrDefault(ctx context.Context, id string) (*Tenant, error) {
	if id == DefaultTenantID {
		return defaultTenant(), nil
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
func recordMembership(ctx context.Context, tx pgx.Tx, email, tenantID, homeID string) error {
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
	// in their own list and nothing about anywhere else.
	if existing, err := s.Account(ctx, email); err == nil {
		if existing.PasswordHash != "" || existing.Shared() {
			return nil, nil, ErrAccountExists
		}
	} else if !errors.Is(err, ErrNoAccount) {
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
		if err != nil || a.Shared() || !a.Shareable || a.Disabled || a.PasswordHash == "" {
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
	var id string
	// A row with this address and no password can exist -- someone put on a
	// team before they had an account -- and pointing it home is giving it
	// one. A row that already is an account is left alone.
	err = tx.QueryRow(ctx, `
		INSERT INTO users (email, display_name, role, home_tenant)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (email) DO UPDATE
		   SET display_name = EXCLUDED.display_name,
		       role         = EXCLUDED.role,
		       home_tenant  = EXCLUDED.home_tenant
		 WHERE users.password_hash IS NULL AND users.home_tenant IS NULL
		RETURNING id::text`, email, displayName, in.Role, home.ID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrAccountExists
	}
	if err != nil {
		return nil, nil, fmt.Errorf("add shared person: %w", err)
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
