package control

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Authenticator secrets are kept sealed.
//
// A TOTP secret cannot be hashed the way a password is: checking a code needs
// the secret itself. Kept as text, a copy of the database was everyone's
// second factor, so whoever took one and cracked a single password had the
// whole sign-in. Sealed with AES-256-GCM under a key derived from
// signing_secret, which lives in the control plane's configuration and not in
// the database, a copy of the database alone yields nothing.
//
// The column stays text: a sealed value is mfaSealPrefix and the base64 of
// nonce and ciphertext. A value without the prefix is a secret written before
// sealing (base32, so it can never start with the prefix). It still verifies,
// so an upgrade asks nothing of anyone.
//
// Sealing starts late on purpose. An update from the console goes back to the
// version that was running when the new one does not come up, and that
// version reads a sealed secret as a wrong one: everyone's codes would stop
// working. So this version reads sealed secrets from the moment it starts,
// and only begins writing them, and sealing the ones already stored, once it
// has been serving for a while (StartSealingMFA). Migration 102 marks the
// release as one a version from before cannot simply go back over: the way
// back from then is the backup the update takes, which holds the secrets as
// they were.
//
// With no signing_secret there is no key, and secrets are kept as before:
// such a control plane signs no sessions, so it has no password sign-in for
// a second factor to guard.

const mfaSealPrefix = "sealed:v1:"

// mfaSealLabel binds a sealed value to what it is, so a value sealed for
// something else under a related key never opens as a second factor.
var mfaSealLabel = []byte("argus authenticator secret")

// errMFASealed is a sealed secret this control plane cannot open.
var errMFASealed = errors.New("the authenticator secret cannot be read; was signing_secret changed?")

// MFAKeyFromSecret derives the key that seals authenticator secrets.
func MFAKeyFromSecret(signingSecret string) []byte {
	if signingSecret == "" {
		return nil
	}
	sum := sha256.Sum256(append([]byte("argus authenticator secrets v1\x00"), signingSecret...))
	return sum[:]
}

// SetMFAKey sets the key authenticator secrets are opened and sealed with.
// Call it once, before serving; nil keeps them unsealed. New secrets are
// sealed only once StartSealingMFA has run.
func (s *Store) SetMFAKey(key []byte) { s.mfaKey = key }

// sealMFAWith seals secret under key. With no key the secret is kept as it is.
func sealMFAWith(key []byte, secret string) (string, error) {
	if len(key) == 0 || secret == "" {
		return secret, nil
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("seal authenticator secret: %w", err)
	}
	return mfaSealPrefix + base64.RawStdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(secret), mfaSealLabel)), nil
}

// openMFAWith opens a stored secret. One written before sealing comes back as
// it is.
func openMFAWith(key []byte, stored string) (string, error) {
	body, sealed := strings.CutPrefix(stored, mfaSealPrefix)
	if !sealed {
		return stored, nil
	}
	if len(key) == 0 {
		return "", errMFASealed
	}
	raw, err := base64.RawStdEncoding.DecodeString(body)
	if err != nil {
		return "", errMFASealed
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errMFASealed
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], mfaSealLabel)
	if err != nil {
		return "", errMFASealed
	}
	return string(plain), nil
}

// sealMFA is how a new secret is stored: sealed once sealing has started.
func (s *Store) sealMFA(secret string) (string, error) {
	if !s.mfaSealing.Load() {
		return secret, nil
	}
	return sealMFAWith(s.mfaKey, secret)
}

// openMFA opens a stored secret, and "" for one that cannot be opened: the
// account then has no code that verifies, which is the safe way to fail. Its
// holder signs in with a recovery code, or an administrator resets the
// second factor.
func (s *Store) openMFA(stored string) string {
	secret, err := openMFAWith(s.mfaKey, stored)
	if err != nil {
		return ""
	}
	return secret
}

// StartSealingMFA seals new authenticator secrets from now on, and seals, in
// every organization, those stored before. It returns how many it sealed. A
// row already sealed is left alone, and each update only replaces the value
// it read, so replicas doing this together cannot undo each other, and a
// failure part-way is finished by the next call.
func (s *Store) StartSealingMFA(ctx context.Context) (int, error) {
	if len(s.mfaKey) == 0 {
		return 0, nil
	}
	s.mfaSealing.Store(true)
	return s.resealMFA(ctx, func(stored string) (string, bool, error) {
		if stored == "" || strings.HasPrefix(stored, mfaSealPrefix) {
			return "", false, nil
		}
		sealed, err := sealMFAWith(s.mfaKey, stored)
		return sealed, true, err
	})
}

// ResealMFASecrets moves every authenticator secret from this store's key to
// newKey, for a rekey. Each organization moves in one transaction, and a
// secret already under newKey is left as it is, so running it again after a
// failure part-way finishes the move, and running it the other way undoes
// it. A secret that opens under neither key is left as it is.
func (s *Store) ResealMFASecrets(ctx context.Context, newKey []byte) (int, error) {
	if len(newKey) == 0 {
		return 0, errors.New("no key to move the authenticator secrets to")
	}
	return s.resealMFA(ctx, func(stored string) (string, bool, error) {
		if stored == "" {
			return "", false, nil
		}
		if _, err := openMFAWith(newKey, stored); err == nil && strings.HasPrefix(stored, mfaSealPrefix) {
			return "", false, nil
		}
		plain, err := openMFAWith(s.mfaKey, stored)
		if err != nil {
			return "", false, nil
		}
		sealed, err := sealMFAWith(newKey, plain)
		return sealed, true, err
	})
}

// resealMFA rewrites totp_secret and totp_pending_secret in every
// organization through change.
func (s *Store) resealMFA(ctx context.Context, change func(string) (string, bool, error)) (int, error) {
	tenants, err := s.Tenants(ctx)
	if err != nil {
		return 0, fmt.Errorf("list organizations: %w", err)
	}
	n := 0
	for i := range tenants {
		t := &tenants[i]
		if t.IsDefault() {
			t = defaultTenantFrom(t)
		}
		c, err := s.resealMFATenant(WithTenant(ctx, t), change)
		n += c
		if err != nil {
			return n, fmt.Errorf("organization %s: %w", t.Slug, err)
		}
	}
	return n, nil
}

func (s *Store) resealMFATenant(ctx context.Context, change func(string) (string, bool, error)) (int, error) {
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	type row struct{ id, secret, pending string }
	rows, err := tx.Query(ctx, `
		SELECT id::text, coalesce(totp_secret, ''), coalesce(totp_pending_secret, '')
		  FROM users
		 WHERE coalesce(totp_secret, '') <> '' OR coalesce(totp_pending_secret, '') <> ''
		   FOR UPDATE`)
	if err != nil {
		return 0, err
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.secret, &r.pending); err != nil {
			rows.Close()
			return 0, err
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, r := range all {
		for _, col := range []struct{ name, value string }{{"totp_secret", r.secret}, {"totp_pending_secret", r.pending}} {
			next, ok, err := change(col.value)
			if err != nil {
				return 0, err
			}
			if !ok {
				continue
			}
			tag, err := tx.Exec(ctx, `UPDATE users SET `+col.name+` = $2 WHERE id = $1::uuid AND `+col.name+` = $3`,
				r.id, next, col.value)
			if err != nil {
				return 0, err
			}
			n += int(tag.RowsAffected())
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return n, nil
}
