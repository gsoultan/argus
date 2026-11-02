// Package auth implements console authentication and terminal authorisation.
//
// Two distinct credentials, deliberately:
//
//   - A session cookie proves who you are. Long-ish lived, HttpOnly, carried on
//     every console request.
//   - A terminal ticket proves you were authorised to open one specific session
//     on one specific host. Single-use, short-lived, and safe to put in a URL
//     because it is worthless a minute later and worthless twice.
//
// The split exists because a browser cannot set headers on a WebSocket
// handshake, so something must travel in the query string. Putting a
// long-lived session credential there would leak it into server logs, proxy
// logs and browser history. A ticket that expires in 60 seconds and burns on
// first use does not matter if it leaks.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

var (
	// ErrInvalid means the value failed signature or format checks.
	ErrInvalid = errors.New("invalid token")
	// ErrExpired means the value was well-formed but too old.
	ErrExpired = errors.New("token expired")
	// ErrUsed means a single-use ticket was already redeemed.
	ErrUsed = errors.New("ticket already used")
)

// Session is what a console cookie carries.
type Session struct {
	// ID names this one sign-in, so it can be revoked on its own (sign out)
	// without signing the person out everywhere. Empty on a cookie issued
	// before session ids existed; such a cookie is still subject to every
	// account-level revocation, and expires on its own.
	ID string `json:"sid,omitempty"`
	// Tenant is the organization the account belongs to. Empty means the
	// default organization, which is what every cookie minted before there
	// were several says. Every signed value carries it, and the key that
	// signs it is derived from it, so a value minted for one organization
	// fails its signature check in another; see Signer.ForTenant.
	Tenant    string    `json:"org,omitempty"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	Subject   string    `json:"sub"`
	IssuedAt  time.Time `json:"iat"`
	ExpiresAt time.Time `json:"exp"`
	// Purpose is never set on a session. Read so that a token of another kind
	// that happens to carry an email and an expiry is refused as a cookie.
	Purpose string `json:"purpose,omitempty"`
	// TicketID is never set on a session either; a terminal ticket carries
	// one, and a ticket is not a cookie.
	TicketID string `json:"jti,omitempty"`
}

// Ticket authorises exactly one terminal session.
type Ticket struct {
	ID string `json:"jti"`
	// Tenant is the organization the ticket was minted in; see Session.
	Tenant string `json:"org,omitempty"`
	Email  string `json:"email"`
	// Scope is what the ticket permits. Empty means ScopeSession, so tickets
	// minted before scopes existed keep working as terminal tickets and cannot
	// be silently promoted into shadow or terminate rights.
	Scope     string `json:"scope,omitempty"`
	Target    string `json:"target"`
	Principal string `json:"principal"`
	// SessionID binds a shadow or terminate ticket to one live session. Empty
	// for terminal tickets, which are bound by target and principal instead.
	SessionID string    `json:"sid,omitempty"`
	ExpiresAt time.Time `json:"exp"`
}

// Ticket scopes.
//
// Watching someone else's privileged session and killing it are both more
// serious than opening your own, and they are separate from each other: a
// security analyst may need to observe without the power to disconnect. Giving
// each its own scope means a ticket for one can never be spent as another, so a
// bug in whoever mints them cannot quietly widen a permission.
const (
	// ScopeSession opens a terminal as a principal on a target.
	ScopeSession = "session"
	// ScopeShadow attaches a read-only viewer to a session already running.
	ScopeShadow = "shadow"
	// ScopeTerminate ends a session already running.
	ScopeTerminate = "terminate"
)

func scopeOf(t Ticket) string {
	if t.Scope == "" {
		return ScopeSession
	}
	return t.Scope
}

// Redeemer records which tickets have been used.
//
// An interface because the answer has to be the same on every control plane
// replica. The in-memory implementation is correct for exactly one instance and
// quietly wrong for two: a ticket burned on replica A stays valid on replica B,
// so "single use" silently becomes "single use per replica".
//
// Redeem returns alreadyUsed. An error means "cannot tell", and callers must
// refuse rather than assume the ticket is fresh.
type Redeemer interface {
	Redeem(ctx context.Context, t Ticket) (alreadyUsed bool, err error)
}

// Signer mints and verifies sessions and tickets.
//
// HMAC rather than asymmetric signing: the control plane both issues and
// verifies, so a shared secret is simpler with no meaningful loss. Move to
// asymmetric when a third party needs to verify without being able to mint.
type Signer struct {
	key []byte
	// tenant is the organization this signer's key was derived for; empty
	// for the root signer, which is the default organization's.
	tenant string
	// root is the signer this one was derived from, which holds the shared
	// redemption state. Nil on the root itself.
	root *Signer

	// redeemer is shared state when configured. Nil falls back to the
	// in-memory set below, which holds for one instance only.
	redeemer Redeemer

	// Local redemption set. Bounded by expiry sweeping rather than growing
	// forever — an unbounded map keyed by attacker-supplied input is its own
	// vulnerability.
	mu       sync.Mutex
	redeemed map[string]time.Time

	// stop ends the sweeper. Without it the goroutine ran for the life of the
	// process holding a reference to this Signer, so neither could ever be
	// collected -- and any test or caller that built a Signer leaked one.
	stop     chan struct{}
	stopOnce sync.Once
}

// SetRedeemer switches to shared redemption.
func (s *Signer) SetRedeemer(r Redeemer) { s.redeemer = r }

// SharedRedemption reports whether replay protection spans instances.
func (s *Signer) SharedRedemption() bool { return s.redeemer != nil }

// ForTenant returns the signer for one organization.
//
// Its key is HMAC-SHA256(root key, "argus tenant key v1:" || id), so a cookie,
// ticket or challenge minted in one organization is a forgery in every other
// one without any handler having to compare a field. The empty id, and the
// default organization's, is the root signer itself, which keeps every value
// minted before organizations existed valid.
//
// Redemption state is the root's: single-use is one property for the whole
// control plane, and a derived signer shares the set rather than keeping one.
func (s *Signer) ForTenant(id string) *Signer {
	if id == "" || id == "default" {
		return s
	}
	root := s
	if s.root != nil {
		root = s.root
	}
	m := hmac.New(sha256.New, root.key)
	m.Write([]byte("argus tenant key v1:" + id))
	return &Signer{key: m.Sum(nil), tenant: id, root: root}
}

// Tenant is the organization this signer mints for; "" is the default.
func (s *Signer) Tenant() string { return s.tenant }

// PeekTenant reads which organization a signed value claims to belong to,
// without verifying it.
//
// The claim chooses which key to verify with and nothing else. A value that
// lies about its organization is then checked with that organization's key
// and fails; one that tells the truth is checked with the right key. Either
// way nothing is believed until a signature has been.
func PeekTenant(token string) string {
	body, _, found := strings.Cut(token, ".")
	if !found {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return ""
	}
	var claim struct {
		Tenant string `json:"org"`
	}
	if json.Unmarshal(payload, &claim) != nil {
		return ""
	}
	return claim.Tenant
}

// NewSigner builds a signer from a secret.
// placeholderSecrets are values that ship in example configs and documentation.
//
// Length alone does not make a secret. Every one of these is public, so a
// deployment using one is signing every session cookie and every terminal
// ticket with a string an attacker can read off GitHub -- which means forging
// either is arithmetic, not an attack. Compared case-insensitively and after
// trimming, because a copied placeholder often arrives with different casing
// or a stray newline.
var placeholderSecrets = []string{
	"change-me",
	"changeme",
	"change-me-please",
	"replace-me",
	"secret",
	"password",
	"argus-dev-signing-secret-at-least-32-chars-long",
	"insert-a-long-random-string-here",
	"your-signing-secret-here",
}

// ErrPlaceholderSecret reports a signing secret that is public knowledge.
var ErrPlaceholderSecret = errors.New("signing secret is a known placeholder")

func NewSigner(secret string) (*Signer, error) {
	if len(secret) < 32 {
		// Short secrets make offline brute force practical against a signature
		// an attacker can capture from any request.
		return nil, fmt.Errorf("signing secret must be at least 32 characters")
	}
	// Checked after the length rule so the more basic mistake is reported
	// first, and refused rather than warned about: a warning at startup is how
	// a placeholder survives into production.
	norm := strings.ToLower(strings.TrimSpace(secret))
	for _, bad := range placeholderSecrets {
		// Prefix, not equality: hitting the 32-character rule and padding the
		// placeholder out to satisfy it is the common response to it, and the
		// result is no less public than the original.
		if strings.HasPrefix(norm, bad) {
			return nil, fmt.Errorf("%w: %q ships in example configuration and is public. "+
				"Generate one with: openssl rand -base64 48", ErrPlaceholderSecret, secret)
		}
	}
	// A secret made only of one repeated character has no more entropy than
	// that character, whatever its length.
	if distinctRunes(norm) < 8 {
		return nil, fmt.Errorf("signing secret has too little variety to be random; " +
			"generate one with: openssl rand -base64 48")
	}
	s := &Signer{
		key:      []byte(secret),
		redeemed: map[string]time.Time{},
		stop:     make(chan struct{}),
	}
	go s.sweep()
	return s, nil
}

// distinctRunes counts unique characters, as a cheap entropy floor.
func distinctRunes(s string) int {
	seen := map[rune]struct{}{}
	for _, r := range s {
		seen[r] = struct{}{}
	}
	return len(seen)
}

// Close stops the sweeper. Safe to call more than once, and safe on a nil
// Signer so a caller need not decide whether one was ever configured.
func (s *Signer) Close() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.stop) })
}

// sweep drops expired ticket ids so the replay set stays bounded.
func (s *Signer) sweep() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			now := time.Now()
			s.mu.Lock()
			for id, exp := range s.redeemed {
				if now.After(exp) {
					delete(s.redeemed, id)
				}
			}
			s.mu.Unlock()
		}
	}
}

/* ── Sessions ────────────────────────────────────────────────────────────── */

// IssueSession signs a session for the cookie.
func (s *Signer) IssueSession(sess Session, ttl time.Duration) (string, error) {
	if sess.ID == "" {
		id, err := randomID()
		if err != nil {
			return "", err
		}
		sess.ID = id
	}
	sess.Tenant = s.tenant
	sess.IssuedAt = time.Now().UTC()
	sess.ExpiresAt = sess.IssuedAt.Add(ttl)
	return s.sign(sess)
}

// VerifySession checks a cookie value.
//
// A valid signature proves the control plane issued it, not that it is still
// good: the caller must also check the account is active and the session has
// not been revoked. See control.API.authenticate.
func (s *Signer) VerifySession(token string) (Session, error) {
	var sess Session
	if err := s.verify(token, &sess); err != nil {
		return Session{}, err
	}
	// Every other signed type (tickets, challenges, device tokens) is also
	// JSON under the same key. A session must name an account and an expiry,
	// so one of those can never be presented as a cookie.
	if sess.Email == "" || sess.ExpiresAt.IsZero() || sess.Purpose != "" || sess.TicketID != "" {
		return Session{}, ErrInvalid
	}
	if time.Now().After(sess.ExpiresAt) {
		return Session{}, ErrExpired
	}
	if sess.Tenant != s.tenant {
		// Signed with this key and yet naming another organization: not a
		// shape the issuer produces.
		return Session{}, ErrInvalid
	}
	return sess, nil
}

/* ── Tickets ─────────────────────────────────────────────────────────────── */

// IssueTicket mints a single-use terminal ticket.
//
// The target and principal are baked in, so a ticket for a staging box cannot
// be replayed against a production one — the gateway checks that what the
// ticket says matches what was actually requested.
func (s *Signer) IssueTicket(email, target, principal string, ttl time.Duration) (string, error) {
	id, err := randomID()
	if err != nil {
		return "", err
	}
	return s.sign(Ticket{
		ID:        id,
		Tenant:    s.tenant,
		Email:     email,
		Scope:     ScopeSession,
		Target:    target,
		Principal: principal,
		ExpiresAt: time.Now().UTC().Add(ttl),
	})
}

// IssueSessionScopedTicket mints a single-use ticket for one live session.
//
// Used for ScopeShadow and ScopeTerminate, which act on a session that already
// exists rather than on a target the holder is entitled to open.
func (s *Signer) IssueSessionScopedTicket(email, scope, sessionID string, ttl time.Duration) (string, error) {
	switch scope {
	case ScopeShadow, ScopeTerminate:
	default:
		return "", fmt.Errorf("%w: scope %q is not session-scoped", ErrInvalid, scope)
	}
	if sessionID == "" {
		return "", fmt.Errorf("%w: no session id", ErrInvalid)
	}
	id, err := randomID()
	if err != nil {
		return "", err
	}
	return s.sign(Ticket{
		ID:        id,
		Tenant:    s.tenant,
		Email:     email,
		Scope:     scope,
		SessionID: sessionID,
		ExpiresAt: time.Now().UTC().Add(ttl),
	})
}

// RedeemSessionScoped verifies and burns a shadow or terminate ticket.
//
// Mirrors RedeemTicket, but binds to the session id instead of the target and
// principal: a ticket to watch one session must not be spendable on another.
func (s *Signer) RedeemSessionScoped(ctx context.Context, token, scope, sessionID string) (Ticket, error) {
	var t Ticket
	if err := s.verify(token, &t); err != nil {
		return Ticket{}, err
	}
	if time.Now().After(t.ExpiresAt) {
		return Ticket{}, ErrExpired
	}
	if t.Tenant != s.tenant {
		return Ticket{}, ErrInvalid
	}
	if scopeOf(t) != scope {
		return Ticket{}, fmt.Errorf("%w: ticket is scoped %q, not %q",
			ErrInvalid, scopeOf(t), scope)
	}
	if subtle.ConstantTimeCompare([]byte(t.SessionID), []byte(sessionID)) != 1 {
		return Ticket{}, fmt.Errorf("%w: ticket is for a different session", ErrInvalid)
	}
	return t, s.burn(ctx, t)
}

// RedeemTicket verifies and burns a ticket.
//
// Verifying and redeeming are one operation on purpose: separating them invites
// a check-then-use gap where two concurrent connections both pass the check.
func (s *Signer) RedeemTicket(ctx context.Context, token, target, principal string) (Ticket, error) {
	var t Ticket
	if err := s.verify(token, &t); err != nil {
		return Ticket{}, err
	}
	if time.Now().After(t.ExpiresAt) {
		return Ticket{}, ErrExpired
	}
	if t.Tenant != s.tenant {
		return Ticket{}, ErrInvalid
	}

	// A valid signature is not enough: the ticket must be for what is actually
	// being opened, and must be the kind of ticket that opens anything at all.
	if scopeOf(t) != ScopeSession {
		return Ticket{}, fmt.Errorf("%w: ticket is scoped %q and cannot open a terminal",
			ErrInvalid, scopeOf(t))
	}
	if subtle.ConstantTimeCompare([]byte(t.Target), []byte(target)) != 1 ||
		subtle.ConstantTimeCompare([]byte(t.Principal), []byte(principal)) != 1 {
		return Ticket{}, fmt.Errorf("%w: ticket is for %s@%s, not %s@%s",
			ErrInvalid, t.Principal, t.Target, principal, target)
	}

	return t, s.burn(ctx, t)
}

// burn marks a ticket used, or reports why it cannot be spent.
//
// One implementation for every scope on purpose. Single-use is the property
// that makes it acceptable to pass a ticket in a query string, and a second
// copy of this logic would be a second place for that property to be lost.
func (s *Signer) burn(ctx context.Context, t Ticket) error {
	if s.root != nil {
		return s.root.burn(ctx, t)
	}
	if s.redeemer != nil {
		used, err := s.redeemer.Redeem(ctx, t)
		if err != nil {
			// Cannot determine whether this was already used. Refusing is the
			// only safe answer: assuming "fresh" would make every gateway
			// accept replays the moment the shared store is unreachable.
			return fmt.Errorf("cannot verify ticket has not been used: %w", err)
		}
		if used {
			return ErrUsed
		}
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, seen := s.redeemed[t.ID]; seen {
		return ErrUsed
	}
	s.redeemed[t.ID] = t.ExpiresAt
	return nil
}

/* ── Signing ─────────────────────────────────────────────────────────────── */

// sign produces payload.signature, both base64url.
func (s *Signer) sign(v any) (string, error) {
	payload, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	return body + "." + s.mac(body), nil
}

func (s *Signer) verify(token string, out any) error {
	body, sig, found := strings.Cut(token, ".")
	if !found {
		return ErrInvalid
	}
	// Constant time: a byte-by-byte comparison leaks how much of a forged
	// signature was correct, which is enough to forge one incrementally.
	if !hmac.Equal([]byte(sig), []byte(s.mac(body))) {
		return ErrInvalid
	}
	payload, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return ErrInvalid
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return ErrInvalid
	}
	return nil
}

func (s *Signer) mac(body string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func randomID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

/* ── Second-factor challenge ─────────────────────────────────────────────── */

// mfaChallenge is the state carried between "your password was right" and
// "here is my code".
//
// Signed and short-lived rather than stored: a half-finished sign-in should
// cost the server nothing to remember and should expire whether or not the
// browser ever comes back. It is deliberately not a session -- it authenticates
// nothing on its own, and naming the type separately keeps it from being handed
// to something expecting one.
type mfaChallenge struct {
	Tenant    string    `json:"org,omitempty"`
	Email     string    `json:"email"`
	ExpiresAt time.Time `json:"expiresAt"`
	// Purpose pins what this token may be redeemed for, so a challenge cannot
	// be presented anywhere a session or a ticket is expected.
	Purpose string `json:"purpose"`
}

const mfaChallengePurpose = "mfa-challenge"

// IssueMFAChallenge returns a token proving a password was accepted.
func (s *Signer) IssueMFAChallenge(email string, ttl time.Duration) (string, error) {
	return s.sign(mfaChallenge{
		Tenant:    s.tenant,
		Email:     email,
		ExpiresAt: time.Now().Add(ttl),
		Purpose:   mfaChallengePurpose,
	})
}

// RedeemMFAChallenge returns the address a challenge was issued for.
//
// Not single-use: it is spent by the code that follows it, and the code is what
// carries the security. Making the challenge one-shot would mean a mistyped
// code sent the user back to the password screen, which teaches people to
// choose shorter passwords.
func (s *Signer) RedeemMFAChallenge(token string) (string, error) {
	var c mfaChallenge
	if err := s.verify(token, &c); err != nil {
		return "", err
	}
	if c.Purpose != mfaChallengePurpose {
		return "", fmt.Errorf("%w: token is not a second-factor challenge", ErrInvalid)
	}
	if c.Tenant != s.tenant {
		return "", ErrInvalid
	}
	if time.Now().After(c.ExpiresAt) {
		return "", ErrExpired
	}
	if c.Email == "" {
		return "", fmt.Errorf("%w: challenge carries no account", ErrInvalid)
	}
	return c.Email, nil
}

/* ── Known devices ───────────────────────────────────────────────────────── */

// deviceToken marks a browser that has completed a sign-in for an account.
//
// It is how a per-account lockout avoids becoming a way to lock someone out:
// when an account is being guessed at from many addresses, sign-ins without
// this cookie are refused for a while, and the account holder's own browser,
// which has it, is not. It proves nothing by itself -- the password and the
// second factor are still required -- it only exempts a browser from a
// throttle aimed at strangers.
type deviceToken struct {
	Tenant    string    `json:"org,omitempty"`
	Email     string    `json:"email"`
	ExpiresAt time.Time `json:"expiresAt"`
	Purpose   string    `json:"purpose"`
}

const devicePurpose = "known-device"

// IssueDeviceToken returns a token marking this browser as one that signed in
// as email.
func (s *Signer) IssueDeviceToken(email string, ttl time.Duration) (string, error) {
	return s.sign(deviceToken{
		Tenant: s.tenant,
		Email:  strings.ToLower(strings.TrimSpace(email)), ExpiresAt: time.Now().Add(ttl),
		Purpose: devicePurpose,
	})
}

// DeviceKnownFor reports whether token is a valid device token for email.
func (s *Signer) DeviceKnownFor(token, email string) bool {
	if token == "" {
		return false
	}
	var d deviceToken
	if err := s.verify(token, &d); err != nil {
		return false
	}
	return d.Purpose == devicePurpose && d.Tenant == s.tenant && time.Now().Before(d.ExpiresAt) &&
		d.Email == strings.ToLower(strings.TrimSpace(email))
}
