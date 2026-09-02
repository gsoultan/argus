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
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	Subject   string    `json:"sub"`
	IssuedAt  time.Time `json:"iat"`
	ExpiresAt time.Time `json:"exp"`
}

// Ticket authorises exactly one terminal session.
type Ticket struct {
	ID        string    `json:"jti"`
	Email     string    `json:"email"`
	Target    string    `json:"target"`
	Principal string    `json:"principal"`
	ExpiresAt time.Time `json:"exp"`
}

// Signer mints and verifies both.
//
// HMAC rather than asymmetric signing: the control plane issues and the gateway
// verifies, and both are operated by the same party, so a shared secret is
// simpler with no meaningful loss. Move to asymmetric when a third party needs
// to verify without being able to mint.
type Signer struct {
	key []byte

	// Redeemed tickets, so a ticket cannot be replayed. Bounded by expiry
	// sweeping rather than growing forever — an unbounded map keyed by
	// attacker-supplied input is its own vulnerability.
	mu       sync.Mutex
	redeemed map[string]time.Time
}

// NewSigner builds a signer from a secret.
func NewSigner(secret string) (*Signer, error) {
	if len(secret) < 32 {
		// Short secrets make offline brute force practical against a signature
		// an attacker can capture from any request.
		return nil, fmt.Errorf("signing secret must be at least 32 characters")
	}
	s := &Signer{key: []byte(secret), redeemed: map[string]time.Time{}}
	go s.sweep()
	return s, nil
}

// sweep drops expired ticket ids so the replay set stays bounded.
func (s *Signer) sweep() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
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

/* ── Sessions ────────────────────────────────────────────────────────────── */

// IssueSession signs a session for the cookie.
func (s *Signer) IssueSession(sess Session, ttl time.Duration) (string, error) {
	sess.IssuedAt = time.Now().UTC()
	sess.ExpiresAt = sess.IssuedAt.Add(ttl)
	return s.sign(sess)
}

// VerifySession checks a cookie value.
func (s *Signer) VerifySession(token string) (Session, error) {
	var sess Session
	if err := s.verify(token, &sess); err != nil {
		return Session{}, err
	}
	if time.Now().After(sess.ExpiresAt) {
		return Session{}, ErrExpired
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
		Email:     email,
		Target:    target,
		Principal: principal,
		ExpiresAt: time.Now().UTC().Add(ttl),
	})
}

// RedeemTicket verifies and burns a ticket.
//
// Verifying and redeeming are one operation on purpose: separating them invites
// a check-then-use gap where two concurrent connections both pass the check.
func (s *Signer) RedeemTicket(token, target, principal string) (Ticket, error) {
	var t Ticket
	if err := s.verify(token, &t); err != nil {
		return Ticket{}, err
	}
	if time.Now().After(t.ExpiresAt) {
		return Ticket{}, ErrExpired
	}

	// A valid signature is not enough: the ticket must be for what is actually
	// being opened.
	if subtle.ConstantTimeCompare([]byte(t.Target), []byte(target)) != 1 ||
		subtle.ConstantTimeCompare([]byte(t.Principal), []byte(principal)) != 1 {
		return Ticket{}, fmt.Errorf("%w: ticket is for %s@%s, not %s@%s",
			ErrInvalid, t.Principal, t.Target, principal, target)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, seen := s.redeemed[t.ID]; seen {
		return Ticket{}, ErrUsed
	}
	s.redeemed[t.ID] = t.ExpiresAt

	return t, nil
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
