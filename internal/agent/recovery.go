package agent

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

// RecoveryKeyFile holds the fleet recovery key's public half, and
// RecoveryStateFile when the newest recovery order acted on was issued.
const (
	RecoveryKeyFile   = "recovery-key.pub"
	RecoveryStateFile = "recovery-state.json"
)

// Recovery holds the fleet recovery key (tunnel/recovery.go) and acts on
// what it signs: a rekey re-pins the grant key, a replacement re-pins the
// recovery key itself.
//
// The key is learned once, from a heartbeat reply the order gate accepted,
// and never from the control plane again: one that has been taken over can
// sign orders with the grant key, but not with this.
type Recovery struct {
	StateDir string
	Pin      *GrantPin

	mu    sync.Mutex
	key   ed25519.PublicKey
	state recoveryState
	read  bool
}

// recoveryState is when the newest order of each kind acted on was issued.
type recoveryState struct {
	Rekey   time.Time `json:"rekey"`
	Replace time.Time `json:"replace"`
}

func (r *Recovery) load() {
	if r.read {
		return
	}
	r.read = true
	if b, err := os.ReadFile(filepath.Join(r.StateDir, RecoveryKeyFile)); err == nil {
		if k, derr := tunnel.DecodeGrantKey(string(b)); derr == nil {
			r.key = k
		}
	}
	if b, err := os.ReadFile(filepath.Join(r.StateDir, RecoveryStateFile)); err == nil {
		_ = json.Unmarshal(b, &r.state)
	}
}

// Fingerprint names the pinned key, or "" before one is.
func (r *Recovery) Fingerprint() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.load()
	if r.key == nil {
		return ""
	}
	return tunnel.RecoveryFingerprint(r.key)
}

// Learn pins the key the control plane sends, if none is pinned yet. One that
// differs from the pinned key is ignored, and reported.
func (r *Recovery) Learn(sent string) (learned bool, err error) {
	k, err := tunnel.DecodeGrantKey(sent)
	if err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.load()
	switch {
	case r.key == nil:
		if err := writePrivate(filepath.Join(r.StateDir, RecoveryKeyFile), []byte(sent+"\n")); err != nil {
			return false, fmt.Errorf("cannot pin the fleet recovery key: %w", err)
		}
		r.key = k
		return true, nil
	case !bytes.Equal(r.key, k):
		return false, errors.New("the control plane sends a fleet recovery key other than the one " +
			"pinned here; it is ignored. A recovery key is only replaced by an order signed with " +
			"the one before it")
	}
	return false, nil
}

// Apply acts on a recovery order. It reports what changed, or nothing when
// the order was already acted on.
func (r *Recovery) Apply(token string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.load()
	if r.key == nil {
		return "", errors.New("a recovery order arrived, but this server has no fleet recovery key " +
			"pinned to check it with; it was not acted on")
	}
	o, err := tunnel.VerifyRecovery(r.key, token)
	if err != nil {
		return "", err
	}
	last := &r.state.Rekey
	if o.Kind == tunnel.RecoveryReplace {
		last = &r.state.Replace
	}
	if !o.IssuedAt.After(*last) {
		return "", nil
	}
	key, _ := tunnel.DecodeGrantKey(o.Key)
	what := ""
	switch o.Kind {
	case tunnel.RecoveryRekey:
		if r.Pin == nil {
			return "", errors.New("no grant key pin to move")
		}
		if !bytes.Equal(r.Pin.Pinned(), key) {
			if err := r.Pin.Repin(key); err != nil {
				return "", err
			}
			what = "the control plane's grant key was replaced by a rekey signed with the fleet recovery key"
		}
	case tunnel.RecoveryReplace:
		if err := writePrivate(filepath.Join(r.StateDir, RecoveryKeyFile), []byte(o.Key+"\n")); err != nil {
			return "", err
		}
		r.key = key
		what = "the fleet recovery key was replaced by an order signed with the one before it"
	}
	next := r.state
	if o.Kind == tunnel.RecoveryReplace {
		next.Replace = o.IssuedAt
	} else {
		next.Rekey = o.IssuedAt
	}
	b, _ := json.Marshal(next)
	if err := writePrivate(filepath.Join(r.StateDir, RecoveryStateFile), b); err != nil {
		return "", fmt.Errorf("cannot record the recovery order: %w", err)
	}
	r.state = next
	return what, nil
}
