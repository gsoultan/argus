package control

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/auth"
	"github.com/jackc/pgx/v5"
)

// Local accounts: the users Argus authenticates itself.
//
// Kept deliberately narrow. Everything here either answers "is this person who
// they say they are" or changes what that answer depends on, and each one
// writes to the audit chain, because a credential change is exactly the event
// an investigator needs after the fact.

// ErrNoAccount is returned when no usable account matches.
//
// One error for "no such address", "disabled" and "wrong password" on purpose:
// distinguishing them tells an attacker which addresses are worth attacking.
var ErrNoAccount = errors.New("invalid email or password")

// ErrMFARequired means the password was right and a second factor is owed.
var ErrMFARequired = errors.New("second factor required")

// Account is a local user as the authentication path needs it.
type Account struct {
	ID           string
	Email        string
	DisplayName  string
	Role         string
	PasswordHash string
	TOTPSecret   string
	MFAEnrolled  bool
	Disabled     bool
	// DisabledAt is when it was revoked, for a listing that says so rather
	// than only that it happened.
	DisabledAt *time.Time
}

// Account looks a user up by email, case-insensitively.
func (s *Store) Account(ctx context.Context, email string) (*Account, error) {
	var a Account
	var pw, secret *string
	var enrolledAt, disabledAt *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, email, display_name, role, password_hash, totp_secret,
		       mfa_enrolled_at, disabled_at
		  FROM users WHERE lower(email) = lower($1)`, email).
		Scan(&a.ID, &a.Email, &a.DisplayName, &a.Role, &pw, &secret, &enrolledAt, &disabledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoAccount
	}
	if err != nil {
		return nil, fmt.Errorf("look up account: %w", err)
	}
	if pw != nil {
		a.PasswordHash = *pw
	}
	if secret != nil {
		a.TOTPSecret = *secret
	}
	a.MFAEnrolled = enrolledAt != nil
	a.Disabled = disabledAt != nil
	return &a, nil
}

// Authenticate verifies an email and password.
//
// Returns ErrNoAccount for every failure, and spends the same work whether the
// address exists or not: without a dummy verification, a missing account
// returns in microseconds while a real one takes the full argon2 cost, and that
// difference alone enumerates the user list.
func (s *Store) Authenticate(ctx context.Context, email, password string) (*Account, error) {
	a, err := s.Account(ctx, email)
	if err != nil || a.Disabled || a.PasswordHash == "" {
		// Hash anyway, against a throwaway value, so the timing matches.
		_ = auth.VerifyPassword(dummyHash, password)
		return nil, ErrNoAccount
	}
	if err := auth.VerifyPassword(a.PasswordHash, password); err != nil {
		return nil, ErrNoAccount
	}
	return a, nil
}

// dummyHash is a real argon2id hash of a value nobody knows, used to spend the
// same time on a missing account as on a present one.
var dummyHash = mustHash()

func mustHash() string {
	h, err := auth.HashPassword("argus-timing-equaliser-not-a-credential")
	if err != nil {
		panic("cannot hash the timing equaliser: " + err.Error())
	}
	return h
}

// SetPassword sets or replaces an account's password.
func (s *Store) SetPassword(ctx context.Context, email, password string) error {
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE users SET password_hash = $2 WHERE lower(email) = lower($1)`, email, hash)
	if err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoAccount
	}
	// Impossible once 008_email_case.sql has run, and worth saying out loud if
	// it ever becomes possible again: this statement writes a credential, and
	// one address matching two rows means one password opening two roles.
	if tag.RowsAffected() > 1 {
		return fmt.Errorf(
			"%q matches %d accounts; refusing to set one password on all of them",
			email, tag.RowsAffected())
	}
	return nil
}

// BeginMFAEnrolment stores a secret without trusting it yet.
//
// Unconfirmed until a code proves the app holds the same secret. Marking it
// enrolled here would mean starting an enrolment and walking away left an
// account whose second factor nobody can produce.
func (s *Store) BeginMFAEnrolment(ctx context.Context, email string) (secret string, err error) {
	secret, err = auth.NewTOTPSecret()
	if err != nil {
		return "", err
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE users SET totp_secret = $2, mfa_enrolled_at = NULL
		 WHERE lower(email) = lower($1)`, email, secret)
	if err != nil {
		return "", fmt.Errorf("begin MFA enrolment: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return "", ErrNoAccount
	}
	return secret, nil
}

// ConfirmMFAEnrolment verifies a code against the pending secret and, if it
// matches, marks the account enrolled and issues recovery codes.
func (s *Store) ConfirmMFAEnrolment(
	ctx context.Context, email, code string,
) (recovery []string, err error) {
	a, err := s.Account(ctx, email)
	if err != nil {
		return nil, err
	}
	if a.TOTPSecret == "" {
		return nil, errors.New("no enrolment is in progress for this account")
	}
	if !auth.VerifyTOTP(a.TOTPSecret, code, time.Now()) {
		return nil, errors.New("that code is not correct; check the clock on your phone")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`UPDATE users SET mfa_enrolled_at = now(), mfa_enrolled = true WHERE id = $1::uuid`,
		a.ID); err != nil {
		return nil, fmt.Errorf("confirm MFA: %w", err)
	}
	// Re-issuing replaces any earlier set, so a re-enrolment does not leave
	// codes from a device the user no longer has.
	if _, err := tx.Exec(ctx, `DELETE FROM recovery_codes WHERE user_id = $1::uuid`, a.ID); err != nil {
		return nil, err
	}
	recovery = make([]string, 0, recoveryCodeCount)
	for range recoveryCodeCount {
		code, err := newRecoveryCode()
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO recovery_codes (user_id, code_hash) VALUES ($1::uuid, $2)`,
			a.ID, hashRecoveryCode(code)); err != nil {
			return nil, err
		}
		recovery = append(recovery, code)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return recovery, nil
}

const recoveryCodeCount = 10

// newRecoveryCode returns a code in the shape people can read off paper.
func newRecoveryCode() (string, error) {
	b := make([]byte, 10) // 80 bits
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	s := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	return strings.ToLower(s[:4] + "-" + s[4:8] + "-" + s[8:12] + "-" + s[12:16]), nil
}

// hashRecoveryCode stores a code as a digest.
//
// SHA-256 rather than argon2: these are 80 bits of uniform randomness, not a
// human-chosen password, so there is no dictionary to run and nothing for a
// slow hash to buy. What matters is that a leaked table is not a set of usable
// second factors.
func hashRecoveryCode(code string) string {
	sum := sha256.Sum256([]byte(normaliseRecoveryCode(code)))
	return hex.EncodeToString(sum[:])
}

func normaliseRecoveryCode(code string) string {
	return strings.ToLower(strings.TrimSpace(code))
}

// ConsumeRecoveryCode spends a code, returning whether it was valid.
//
// The update is conditional on the code still being unused, so two concurrent
// attempts cannot both succeed with the same code.
func (s *Store) ConsumeRecoveryCode(ctx context.Context, userID, code string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE recovery_codes SET used_at = now()
		 WHERE user_id = $1::uuid AND code_hash = $2 AND used_at IS NULL`,
		userID, hashRecoveryCode(code))
	if err != nil {
		return false, fmt.Errorf("consume recovery code: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// RemainingRecoveryCodes reports how many are left, so the console can say when
// they are nearly gone rather than when they have run out.
func (s *Store) RemainingRecoveryCodes(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM recovery_codes WHERE user_id = $1::uuid AND used_at IS NULL`,
		userID).Scan(&n)
	return n, err
}

// normaliseEmail is the one form an address is stored and compared in.
//
// Sign-in matches on lower(email); storing anything else means the uniqueness
// constraint guards a different string than the lookup reads, which is how
// Alice@corp.com became a second account answering to alice@corp.com's login.
func normaliseEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// CreateAccount adds a local user. Used to bootstrap the first administrator.
func (s *Store) CreateAccount(ctx context.Context, email, displayName, role, password string) (string, error) {
	hash, err := auth.HashPassword(password)
	if err != nil {
		return "", err
	}
	email = normaliseEmail(email)
	if displayName == "" {
		displayName = strings.SplitN(email, "@", 2)[0]
	}
	var id string
	err = s.pool.QueryRow(ctx, `
		INSERT INTO users (email, display_name, role, password_hash)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (email) DO UPDATE
		   SET display_name = EXCLUDED.display_name,
		       role         = EXCLUDED.role,
		       password_hash = EXCLUDED.password_hash
		RETURNING id::text`, email, displayName, role, hash).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("create account: %w", err)
	}
	return id, nil
}

// AnyAccountExists reports whether anyone can sign in yet.
//
// A fresh install has none, and a password form that cannot possibly succeed is
// a bad first five minutes -- someone retypes a password they never set. Not
// treated as sensitive: that a brand new deployment has no accounts is neither
// surprising nor useful to an attacker, and saying so is the difference between
// an operator running one command and filing a bug.
func (s *Store) AnyAccountExists(ctx context.Context) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE password_hash IS NOT NULL AND disabled_at IS NULL)`).
		Scan(&exists)
	return exists, err
}

// ErrLastAdmin means disabling this account would leave nobody able to
// administer the deployment.
var ErrLastAdmin = errors.New("this is the last account that can administer this deployment")

// ErrAlreadyInState means the account was already in the state asked for.
var ErrAlreadyInState = errors.New("the account is already in that state")

// SetAccountDisabled revokes or restores an account.
//
// The column has been read since it was added -- a disabled account cannot
// sign in, does not count toward accountsExist, and does not count as an
// administrator -- but nothing ever wrote it. So an account could be created
// and never revoked, which for a product about controlling access is the wrong
// half of the pair to have.
//
// Refuses to disable the last administrator. That is what CountAdmins was
// written for, and a deployment nobody can administer is not a safer one: the
// way out of it is direct SQL against the database, which is the thing this
// exists to avoid needing.
func (s *Store) SetAccountDisabled(ctx context.Context, email string, disabled bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var role string
	var already *time.Time
	err = tx.QueryRow(ctx,
		`SELECT role, disabled_at FROM users WHERE lower(email) = lower($1) FOR UPDATE`,
		email).Scan(&role, &already)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoAccount
	}
	if err != nil {
		return err
	}
	if (already != nil) == disabled {
		return ErrAlreadyInState
	}

	// Counted inside the transaction, with the row locked: two disables racing
	// each other could otherwise each see one other administrator and both
	// proceed.
	if disabled && (role == "admin" || role == "owner") {
		var others int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM users
			  WHERE role IN ('owner','admin') AND disabled_at IS NULL
			    AND lower(email) <> lower($1)`, email).Scan(&others); err != nil {
			return err
		}
		if others == 0 {
			return ErrLastAdmin
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE users SET disabled_at = CASE WHEN $2 THEN now() ELSE NULL END
		 WHERE lower(email) = lower($1)`, email, disabled); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Accounts lists every local account, disabled ones included.
//
// Disabled ones especially: an operator asking who can sign in needs to see
// that an account exists and is revoked, not to be shown a list it is missing
// from and conclude it was never there.
func (s *Store) Accounts(ctx context.Context) ([]Account, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT email, display_name, role, mfa_enrolled, disabled_at
		  FROM users ORDER BY lower(email)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Account{}
	for rows.Next() {
		var a Account
		var disabledAt *time.Time
		if err := rows.Scan(&a.Email, &a.DisplayName, &a.Role, &a.MFAEnrolled, &disabledAt); err != nil {
			return nil, err
		}
		a.DisabledAt = disabledAt
		a.Disabled = disabledAt != nil
		out = append(out, a)
	}
	return out, rows.Err()
}

// CountAdmins reports how many accounts can change policy, so the console can
// warn before the last one is removed or demoted.
func (s *Store) CountAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM users WHERE role IN ('owner','admin') AND disabled_at IS NULL`).Scan(&n)
	return n, err
}
