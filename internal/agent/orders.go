package agent

import (
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

// SignedOrdersFile records that this agent has had a signed heartbeat reply
// from its control plane, and the issue time of the newest one it acted on.
// Once it exists, unsigned replies are refused: see OrderGate.
const SignedOrdersFile = "signed-orders.json"

// OrderGate decides which heartbeat replies an agent acts on.
//
// The upgrade has to work in both directions:
//
//   - An agent older than signed orders never reads the signature, which is a
//     response header, and acts on the reply as it always has.
//   - A control plane older than signed orders sends none. Until this agent
//     has seen a signature that verifies, it acts on an unsigned reply too,
//     or upgrading an agent before its control plane would leave it unable to
//     be told anything -- including to update again.
//
// The first verified reply is the switch: it is written to SignedOrdersFile,
// and from then on a reply that is unsigned, signed with another key, for
// another host, or older than the last one acted on changes nothing. A party
// on the link can therefore only matter to an agent that has never heard
// from a control plane that signs, and only until it first does.
//
// The key is the grant key the tunnel pinned (GrantKeyFile). An agent that
// has not pinned one yet -- its tunnel has never connected -- has nothing to
// check against and stays on unsigned replies until it has.
type OrderGate struct {
	StateDir string
	Host     string
	Now      func() time.Time

	mu     sync.Mutex
	loaded bool
	state  signedOrdersState
}

type signedOrdersState struct {
	// Since is when this agent first acted on a signed reply.
	Since time.Time `json:"since"`
	// Last is the control plane's issue time of the newest reply acted on.
	Last time.Time `json:"last"`
}

// Locked reports whether unsigned replies are refused.
func (g *OrderGate) Locked() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.load()
	return !g.state.Since.IsZero()
}

// Accept reports whether a heartbeat reply may be acted on. signature is its
// tunnel.OrdersHeader and body the bytes read; the error says why not.
func (g *OrderGate) Accept(signature string, body []byte) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.load()
	locked := !g.state.Since.IsZero()

	key, kerr := g.key()
	switch {
	case signature == "" && !locked:
		// A control plane older than signed orders.
		return true, nil
	case signature == "":
		return false, errors.New("the control plane's reply is not signed, and this " +
			"agent has had signed replies from it before; nothing in it was acted on. " +
			"Something between this host and the control plane may be posing as it, " +
			"or the control plane was downgraded")
	case kerr != nil && !locked:
		// Signed, but there is nothing pinned to check it with yet: the
		// tunnel has not connected. The same as before signing existed,
		// until it has.
		return true, nil
	case kerr != nil:
		return false, fmt.Errorf("the control plane's key cannot be read, so its "+
			"orders cannot be checked; nothing was acted on: %w", kerr)
	}

	now := time.Now()
	if g.Now != nil {
		now = g.Now()
	}
	o, err := tunnel.VerifyOrders(key, g.Host, signature, body, now)
	if err != nil {
		return false, err
	}
	if !g.state.Last.IsZero() && o.IssuedAt.Before(g.state.Last.Add(-tunnel.OrdersSkew)) {
		return false, tunnel.ErrOrders{Reason: fmt.Sprintf(
			"they were issued at %s, before the last ones acted on (%s): a reply replayed",
			o.IssuedAt.Format(time.RFC3339), g.state.Last.Format(time.RFC3339))}
	}

	next := g.state
	if next.Since.IsZero() {
		next.Since = now.UTC()
	}
	if o.IssuedAt.After(next.Last) {
		next.Last = o.IssuedAt
	}
	// Kept before acting: a switch this agent forgets on a restart is one a
	// party on the link could turn back.
	if err := g.save(next); err != nil {
		return false, fmt.Errorf("cannot record the signed reply, so it was not acted on: %w", err)
	}
	g.state = next
	return true, nil
}

func (g *OrderGate) path() string { return filepath.Join(g.StateDir, SignedOrdersFile) }

// load reads SignedOrdersFile once. A file that exists but cannot be read
// still counts as the switch having been made: refusing unsigned replies is
// the side to be wrong on.
func (g *OrderGate) load() {
	if g.loaded {
		return
	}
	g.loaded = true
	b, err := os.ReadFile(g.path())
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil || json.Unmarshal(b, &g.state) != nil || g.state.Since.IsZero() {
		g.state = signedOrdersState{Since: time.Unix(1, 0).UTC()}
	}
}

func (g *OrderGate) save(s signedOrdersState) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return writePrivate(g.path(), b)
}

func (g *OrderGate) key() (ed25519.PublicKey, error) {
	b, err := os.ReadFile(filepath.Join(g.StateDir, GrantKeyFile))
	if err != nil {
		return nil, err
	}
	return tunnel.DecodeGrantKey(string(b))
}
