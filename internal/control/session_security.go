package control

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gsoultan/argus/internal/auth"
	"github.com/jackc/pgx/v5"
)

// Server-side state behind a console cookie.
//
// The cookie is signed, which proves the control plane issued it. It does not
// prove the account is still allowed in: until this existed, disabling an
// account, demoting it, changing its password or signing out left every cookie
// it had working for the rest of its eight hours, with the role it was issued
// with. So every request now asks the database three things a signature cannot
// answer -- is the account active, what is its role now, and has this cookie
// or every cookie before some moment been revoked -- in one indexed query.
//
// Not cached: a cache is exactly the window in which a revoked cookie still
// works, and this is a primary-key lookup on a table of people.

// SessionAccount is what authenticate needs to know about a cookie's account.
type SessionAccount struct {
	Email       string
	DisplayName string
	Role        string
	Disabled    bool
	// ValidAfter is when sessions were last revoked for this account.
	ValidAfter *time.Time
	// Revoked is true when this one cookie was signed out.
	Revoked bool
	// MFAEnrolled is whether the account has a confirmed second factor.
	MFAEnrolled bool
}

// SessionState looks up the account behind a cookie and whether sid was
// revoked.
//
// The row is this organization's: its role, its display name, its disabling,
// and the sign-outs done here. A person shared in from another organization
// has their password, second factor and "sign out everywhere" at home, so
// that row is read as well, and the later of the two revocation moments wins.
// Disabled in either organization is disabled.
func (s *Store) SessionState(ctx context.Context, email, sid string) (*SessionAccount, error) {
	a, home, err := s.sessionRow(ctx, email, sid)
	if err != nil {
		return nil, err
	}
	if home == "" {
		if !a.hasPassword {
			return nil, ErrNoAccount
		}
		return &a.SessionAccount, nil
	}
	t, err := s.tenantByIDOrDefault(ctx, home)
	if err != nil {
		return nil, fmt.Errorf("home organization of %s: %w", email, err)
	}
	if t.Disabled() {
		return nil, ErrNoAccount
	}
	h, hh, err := s.sessionRow(WithTenant(ctx, t), email, "")
	if err != nil {
		return nil, err
	}
	if hh != "" || !h.hasPassword {
		// A home that points elsewhere, or holds no credential, is not
		// an account that can be signed in to.
		return nil, ErrNoAccount
	}
	a.Disabled = a.Disabled || h.Disabled
	a.MFAEnrolled = h.MFAEnrolled
	if h.ValidAfter != nil && (a.ValidAfter == nil || h.ValidAfter.After(*a.ValidAfter)) {
		a.ValidAfter = h.ValidAfter
	}
	return &a.SessionAccount, nil
}

type sessionRow struct {
	SessionAccount
	hasPassword bool
}

// sessionRow reads one organization's users row for a cookie. The second
// value is the organization holding the credentials when it is not this one.
func (s *Store) sessionRow(ctx context.Context, email, sid string) (sessionRow, string, error) {
	var a sessionRow
	var disabledAt *time.Time
	var home *string
	err := s.db(ctx).QueryRow(ctx, `
		SELECT u.email, u.display_name, u.role, u.disabled_at, u.sessions_valid_after,
		       ($2 <> '' AND EXISTS (SELECT 1 FROM revoked_sessions r WHERE r.sid = $2)),
		       u.mfa_enrolled_at IS NOT NULL, u.password_hash IS NOT NULL, u.home_tenant
		  FROM users u
		 WHERE lower(u.email) = lower($1)`, email, sid).
		Scan(&a.Email, &a.DisplayName, &a.Role, &disabledAt, &a.ValidAfter, &a.Revoked,
			&a.MFAEnrolled, &a.hasPassword, &home)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, "", ErrNoAccount
	}
	if err != nil {
		return a, "", fmt.Errorf("session state: %w", err)
	}
	a.Disabled = disabledAt != nil
	if home != nil {
		return a, *home, nil
	}
	return a, "", nil
}

// RevokeSession signs one cookie out, until it would have expired anyway.
func (s *Store) RevokeSession(ctx context.Context, sid string, expires time.Time) error {
	if sid == "" {
		return nil
	}
	// Expired rows are swept on the way in, so the table holds at most the
	// sign-outs of the last session lifetime.
	if _, err := s.db(ctx).Exec(ctx, `DELETE FROM revoked_sessions WHERE expires_at < now()`); err != nil {
		return fmt.Errorf("sweep revoked sessions: %w", err)
	}
	_, err := s.db(ctx).Exec(ctx, `
		INSERT INTO revoked_sessions (sid, expires_at) VALUES ($1, $2)
		ON CONFLICT (sid) DO NOTHING`, sid, expires)
	if err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	return nil
}

// RevokeAllSessions signs an account out everywhere: every cookie issued
// before now is refused from the next request on. Everywhere includes every
// organization a shared person signs in to: the moment is written at home,
// where their credentials are, and read from there on each request.
//
// The moment is this process's clock, not the database's, because it is
// compared with IssuedAt, which this process wrote. A caller that goes on to
// issue the person a fresh cookie gets one issued after it.
func (s *Store) RevokeAllSessions(ctx context.Context, email string) error {
	ctx, err := s.homeContext(ctx, email)
	if err != nil {
		return err
	}
	tag, err := s.db(ctx).Exec(ctx,
		`UPDATE users SET sessions_valid_after = $2 WHERE lower(email) = lower($1)`,
		email, revocationMoment())
	if err != nil {
		return fmt.Errorf("revoke sessions: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoAccount
	}
	return nil
}

// revocationMoment is now, at the precision Postgres keeps, so a cookie issued
// in the same microsecond is not mistaken for one issued after.
func revocationMoment() time.Time {
	return time.Now().UTC().Truncate(time.Microsecond).Add(time.Microsecond)
}

// ConsumeTOTP checks code against the account's confirmed secret and spends
// the time step it matched, so the same code cannot sign in twice.
//
// The update is conditional on the step being newer than the last accepted,
// so two concurrent sign-ins with one code cannot both succeed.
func (s *Store) ConsumeTOTP(ctx context.Context, acct *Account, code string, now time.Time) (bool, error) {
	if acct.TOTPSecret == "" || !acct.MFAEnrolled {
		return false, nil
	}
	step, ok := auth.VerifyTOTPStep(acct.TOTPSecret, code, now)
	if !ok {
		return false, nil
	}
	// acct.ID is the home row's; spend the step there.
	ctx, err := s.homeContext(ctx, acct.Email)
	if err != nil {
		return false, err
	}
	tag, err := s.db(ctx).Exec(ctx, `
		UPDATE users SET totp_last_step = $2
		 WHERE id = $1::uuid AND (totp_last_step IS NULL OR totp_last_step < $2)`,
		acct.ID, step)
	if err != nil {
		return false, fmt.Errorf("spend authenticator code: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ResetMFA removes an account's second factor and every recovery code, and
// signs it out everywhere.
//
// For an account holder who has lost their authenticator and their codes.
// Done by another administrator from the console or by someone with a shell
// on the control plane host, and always audited by the caller. The account
// must enrol again; the console prompts for it at the next sign-in.
func (s *Store) ResetMFA(ctx context.Context, email string) error {
	ctx, err := s.homeContext(ctx, email)
	if err != nil {
		return err
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id string
	err = tx.QueryRow(ctx, `
		UPDATE users SET totp_secret = NULL, totp_pending_secret = NULL,
		                 mfa_enrolled_at = NULL, mfa_enrolled = false,
		                 totp_last_step = NULL, sessions_valid_after = $2
		 WHERE lower(email) = lower($1)
		 RETURNING id::text`, email, revocationMoment()).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoAccount
	}
	if err != nil {
		return fmt.Errorf("reset second factor: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM recovery_codes WHERE user_id = $1::uuid`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
