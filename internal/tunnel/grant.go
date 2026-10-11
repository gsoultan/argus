package tunnel

import (
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

/*
Session grants.

The control plane decides whether a session may open; the agent opens it. A
grant is that decision, signed, so the agent can check it for itself before it
starts anything as anyone: whoever can put bytes on a tunnel stream -- a proxy
terminating TLS on the HTTPS fallback, or a bug in the relay -- still cannot
open a shell without the control plane's signature.

Ed25519, not the HMAC the console's tickets use, because the agent must be able
to verify without being able to sign. The key is derived from the control
plane's signing secret, so every replica holds the same one and an existing
configuration needs nothing new.
*/

// GrantKeyHeader carries the control plane's grant key on the upgrade
// response, base64. The QUIC hello's answer carries it in Answer.GrantKey.
const GrantKeyHeader = "Argus-Tunnel-Grant-Key"

// GrantTTL is how long a grant may be presented. Long enough for a slow
// path, short enough that a grant captured off the wire is useless.
const GrantTTL = 60 * time.Second

// maxGrantAhead bounds how far in the future a grant's expiry may be, by
// the agent's clock. A grant valid for longer than the control plane ever
// issues is not one it issued.
const maxGrantAhead = GrantTTL + 2*time.Minute

// Grant is the control plane's permission to open one session.
type Grant struct {
	ID string `json:"jti"`
	// Email is the person the session is for. The agent records it as the
	// actor: the local account is only who the shell runs as.
	Email     string `json:"email"`
	Host      string `json:"host"`
	Principal string `json:"principal"`
	// ClientAddr is the browser's address as the control plane saw it.
	ClientAddr string    `json:"client_addr,omitempty"`
	ExpiresAt  time.Time `json:"exp"`
}

// GrantKeyFromSecret derives the grant signing key from the control plane's
// signing secret.
func GrantKeyFromSecret(secret string) (ed25519.PrivateKey, error) {
	if len(secret) < 32 {
		return nil, errors.New("tunnel: signing secret is too short to derive a grant key")
	}
	seed, err := hkdf.Key(sha256.New, []byte(secret), nil, "argus tunnel grant v1",
		ed25519.SeedSize)
	if err != nil {
		return nil, err
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// NewGrant fills in an id and an expiry.
func NewGrant(email, host, principal, clientAddr string) Grant {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return Grant{ID: base64.RawURLEncoding.EncodeToString(b), Email: email,
		Host: host, Principal: principal, ClientAddr: clientAddr,
		ExpiresAt: time.Now().UTC().Add(GrantTTL)}
}

// SignGrant encodes g as payload.signature.
func SignGrant(key ed25519.PrivateKey, g Grant) (string, error) {
	payload, err := json.Marshal(g)
	if err != nil {
		return "", err
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	sig := ed25519.Sign(key, []byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// ErrGrant is a grant the agent will not honour, with why.
type ErrGrant struct{ Reason string }

func (e ErrGrant) Error() string { return "grant refused: " + e.Reason }

// GrantVerifier checks grants on the agent, once each.
type GrantVerifier struct {
	// Host is this agent's own name; a grant for another host is refused.
	Host string
	now  func() time.Time

	mu   sync.Mutex
	used map[string]time.Time
}

// maxUsedGrants bounds the replay set. Only signed grants reach it, so the
// control plane is the only party that can fill it, and an entry leaves
// when its grant expires.
const maxUsedGrants = 4096

// Verify checks the signature against key, the host, the expiry, and that
// this grant has not been presented before, and spends it.
func (v *GrantVerifier) Verify(key ed25519.PublicKey, token string) (Grant, error) {
	if len(key) != ed25519.PublicKeySize {
		return Grant{}, ErrGrant{"this agent does not know the control plane's grant key"}
	}
	body, sig, ok := strings.Cut(token, ".")
	if !ok {
		return Grant{}, ErrGrant{"malformed"}
	}
	rawSig, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !ed25519.Verify(key, []byte(body), rawSig) {
		return Grant{}, ErrGrant{"the signature is not the control plane's"}
	}
	payload, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return Grant{}, ErrGrant{"malformed"}
	}
	var g Grant
	if err := json.Unmarshal(payload, &g); err != nil || g.ID == "" {
		return Grant{}, ErrGrant{"malformed"}
	}
	now := time.Now()
	if v.now != nil {
		now = v.now()
	}
	if !strings.EqualFold(g.Host, v.Host) {
		return Grant{}, ErrGrant{fmt.Sprintf("it is for %s, and this host is %s", g.Host, v.Host)}
	}
	if !now.Before(g.ExpiresAt) {
		return Grant{}, ErrGrant{"it has expired; check this host's clock if this repeats"}
	}
	if g.ExpiresAt.Sub(now) > maxGrantAhead {
		return Grant{}, ErrGrant{"its expiry is too far ahead; check this host's clock"}
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.used == nil {
		v.used = map[string]time.Time{}
	}
	if _, seen := v.used[g.ID]; seen {
		return Grant{}, ErrGrant{"it has already been used"}
	}
	if len(v.used) >= maxUsedGrants {
		for id, exp := range v.used {
			if !now.Before(exp) {
				delete(v.used, id)
			}
		}
		if len(v.used) >= maxUsedGrants {
			// Full of live grants: refusing is safe, forgetting one is not.
			return Grant{}, ErrGrant{"too many sessions are opening at once"}
		}
	}
	v.used[g.ID] = g.ExpiresAt
	return g, nil
}

// EncodeGrantKey and DecodeGrantKey carry a public key in a header or a file.
func EncodeGrantKey(k ed25519.PublicKey) string { return base64.StdEncoding.EncodeToString(k) }

func DecodeGrantKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("tunnel: not a grant key")
	}
	return ed25519.PublicKey(b), nil
}
