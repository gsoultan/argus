package tunnel

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

/*
The fleet recovery key.

Every agent pins the control plane's grant key the first time it connects,
and acts only on orders signed with it. Whoever holds the control plane's
signing_secret therefore commands the fleet, and moving the fleet to a new
secret -- after a takeover, say -- used to mean deleting grant-key.pub on
every server by hand.

The recovery key is an ed25519 pair whose private half never touches the
control plane: it is printed or downloaded once and kept offline. Agents
learn the public half from a signed heartbeat reply and pin it, like the
grant key, and from then on a control plane cannot change it on its own.
Two things are signed with the private half, on a machine that holds it:

  - a rekey: "the grant key is now K", which agents verify against the
    pinned recovery key and re-pin to, so a rebuilt control plane with a
    new signing_secret takes the fleet back as servers connect;
  - a recovery key replacement: "the recovery key is now R".

Each carries when it was issued, and an agent refuses one older than the
last it acted on, so an old order cannot be replayed to move it back.
*/

// Recovery order kinds.
const (
	RecoveryRekey   = "rekey"
	RecoveryReplace = "recovery-key"
)

// recoveryDomain is signed ahead of every order, so a recovery signature can
// never be mistaken for any other this key could make.
const recoveryDomain = "argus fleet recovery v1\n"

// recoveryFileHeader starts the private key file.
const recoveryFileHeader = "ARGUS FLEET RECOVERY KEY v1"

// RecoveryOrder is something the recovery key signs.
type RecoveryOrder struct {
	Kind string `json:"kind"`
	// Key is the new grant key (rekey) or the new recovery key (replace),
	// base64 like EncodeGrantKey.
	Key      string    `json:"key"`
	IssuedAt time.Time `json:"issued_at"`
	// Tenant is the organization a rekey is for, for the record; an agent
	// checks the key, not this.
	Tenant string `json:"tenant,omitempty"`
}

// NewRecoveryKey makes a recovery key pair.
func NewRecoveryKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

// EncodeRecoveryPrivate is the private key as the file an admin keeps.
func EncodeRecoveryPrivate(k ed25519.PrivateKey) string {
	return recoveryFileHeader + "\n" +
		"# Keep this offline. Whoever holds it can move every Argus agent to a new control plane.\n" +
		"# Public key fingerprint: " + RecoveryFingerprint(k.Public().(ed25519.PublicKey)) + "\n" +
		base64.StdEncoding.EncodeToString(k.Seed()) + "\n"
}

// DecodeRecoveryPrivate reads the file EncodeRecoveryPrivate wrote.
func DecodeRecoveryPrivate(s string) (ed25519.PrivateKey, error) {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != recoveryFileHeader {
		return nil, errors.New("not an Argus fleet recovery key file")
	}
	for _, l := range lines[1:] {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		seed, err := base64.StdEncoding.DecodeString(l)
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, errors.New("the recovery key file is damaged")
		}
		return ed25519.NewKeyFromSeed(seed), nil
	}
	return nil, errors.New("the recovery key file holds no key")
}

// RecoveryFingerprint is a short name for a recovery public key, to compare
// what the console shows with what a server pinned.
func RecoveryFingerprint(k ed25519.PublicKey) string {
	sum := sha256.Sum256(k)
	h := hex.EncodeToString(sum[:8])
	return h[0:4] + "-" + h[4:8] + "-" + h[8:12] + "-" + h[12:16]
}

// SignRecovery signs an order with the recovery key, as a token
// "<base64 body>.<base64 signature>".
func SignRecovery(k ed25519.PrivateKey, o RecoveryOrder) (string, error) {
	if o.Kind != RecoveryRekey && o.Kind != RecoveryReplace {
		return "", fmt.Errorf("unknown recovery order %q", o.Kind)
	}
	if _, err := DecodeGrantKey(o.Key); err != nil {
		return "", err
	}
	o.IssuedAt = o.IssuedAt.UTC()
	body, err := json.Marshal(o)
	if err != nil {
		return "", err
	}
	sig := ed25519.Sign(k, append([]byte(recoveryDomain), body...))
	return base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// VerifyRecovery checks a token against the pinned recovery key.
func VerifyRecovery(pub ed25519.PublicKey, token string) (RecoveryOrder, error) {
	var o RecoveryOrder
	b64body, b64sig, ok := strings.Cut(token, ".")
	if !ok {
		return o, errors.New("recovery order: malformed")
	}
	body, err := base64.RawURLEncoding.DecodeString(b64body)
	if err != nil {
		return o, errors.New("recovery order: malformed body")
	}
	sig, err := base64.RawURLEncoding.DecodeString(b64sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return o, errors.New("recovery order: malformed signature")
	}
	if len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, append([]byte(recoveryDomain), body...), sig) {
		return o, errors.New("recovery order: not signed by this server's recovery key")
	}
	if err := json.Unmarshal(body, &o); err != nil {
		return o, errors.New("recovery order: malformed body")
	}
	if o.Kind != RecoveryRekey && o.Kind != RecoveryReplace {
		return o, fmt.Errorf("recovery order: unknown kind %q", o.Kind)
	}
	if _, err := DecodeGrantKey(o.Key); err != nil {
		return o, fmt.Errorf("recovery order: %w", err)
	}
	return o, nil
}
