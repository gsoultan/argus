package agent

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/gsoultan/argus/internal/tunnel"
)

// GrantKeyFile holds the control plane's grant key, in the state directory.
const GrantKeyFile = "grant-key.pub"

// GrantPin remembers the first grant key the control plane sent and refuses
// any other afterwards.
//
// The first one arrives over a connection the agent already verifies against
// the certificate trusted at enrollment. Pinning it means that someone who
// later sits on that path -- a proxy terminating TLS on the HTTPS fallback,
// or a certificate mis-issued to an attacker -- still cannot sign a session
// onto this host.
type GrantPin struct {
	Path string

	mu  sync.Mutex
	key ed25519.PublicKey
}

// NewGrantPin pins in stateDir.
func NewGrantPin(stateDir string) *GrantPin {
	return &GrantPin{Path: filepath.Join(stateDir, GrantKeyFile)}
}

// Check returns the pinned key when sent matches it, pinning sent if
// nothing is pinned yet.
func (p *GrantPin) Check(sent ed25519.PublicKey) (ed25519.PublicKey, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.key == nil {
		b, err := os.ReadFile(p.Path)
		switch {
		case err == nil:
			k, derr := tunnel.DecodeGrantKey(string(b))
			if derr != nil {
				return nil, fmt.Errorf("%s is not a grant key: %w", p.Path, derr)
			}
			p.key = k
		case errors.Is(err, os.ErrNotExist):
			// Written before it is trusted: a key this agent cannot keep is
			// one it would re-learn from whoever answers next.
			if werr := os.WriteFile(p.Path, []byte(tunnel.EncodeGrantKey(sent)+"\n"), 0o600); werr != nil {
				return nil, fmt.Errorf("cannot pin the control plane's grant key: %w", werr)
			}
			p.key = sent
		default:
			return nil, err
		}
	}
	if !bytes.Equal(p.key, sent) {
		return nil, fmt.Errorf("the control plane's grant key is not the one pinned in %s. "+
			"If its signing_secret was changed on purpose, delete that file and restart "+
			"the agent; otherwise something between this host and the control plane is "+
			"posing as it", p.Path)
	}
	return p.key, nil
}
