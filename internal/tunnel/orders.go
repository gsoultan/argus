package tunnel

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

/*
Signed heartbeat orders.

A heartbeat's reply is where the control plane tells an agent what to do
between sessions: install or remove packages, reboot, upgrade the operating
system or the agent itself, create personal accounts (with sudo and an SSH
key), open direct SSH, uninstall. Each of those is root on the server. Until
these were signed they were protected by TLS alone, so whoever could read and
write that link -- a proxy terminating TLS such as a Cloudflare Tunnel, an
inspecting proxy whose CA the server trusts, anyone holding the control
plane's certificate key -- could do any of them, although the same party
could not open a shell, because a session needs a signed grant.

Now the control plane signs the reply's body, byte for byte, with the key that
signs grants, bound to the host it is for and to when it was issued, and sends
the signature in the OrdersHeader response header. The body itself is
unchanged, so an agent older than this reads it as it always has, and a reply
is no larger than before (a reply is read up to 64 KiB, and a signed copy in
the body would have pushed a large one past that on agents already installed).
An agent that has pinned the key acts on a reply only when the signature
covers exactly the bytes it read.

The signature is over a context string and the body, not the body alone, so
signed orders can never be presented as a grant, nor a grant as orders.
*/

// OrdersHeader is the heartbeat response header that carries the signature
// over the reply's body.
const OrdersHeader = "Argus-Signed-Orders"

// ordersContext is what an orders signature covers before the body.
const ordersContext = "argus heartbeat orders v1\n"

// MaxOrdersAge is how old, by the agent's clock, a signed reply may be. It is
// not the replay guard -- an agent refuses anything older than the last reply
// it accepted -- but a bound on a reply someone held back and delivers late.
// Generous, because before orders were signed an agent's clock did not
// matter to them at all, and an hour off is a clock nobody has set.
const MaxOrdersAge = time.Hour

// OrdersSkew is how far behind the last accepted reply a new one may be
// issued. Replicas stamp replies with their own clocks, which drift a little;
// a reply from one a few seconds behind another is not a replay.
const OrdersSkew = 30 * time.Second

// Orders is what an orders signature vouches for.
type Orders struct {
	// Host is the server the reply is for. An agent refuses another's.
	Host string `json:"host"`
	// IssuedAt is the control plane's clock when it signed.
	IssuedAt time.Time `json:"iat"`
	// Body is the SHA-256 of the reply's body, hex.
	Body string `json:"body"`
}

// SignOrders signs body, a heartbeat reply exactly as it will be written, for
// host.
func SignOrders(key ed25519.PrivateKey, host string, body []byte, now time.Time) (string, error) {
	if len(key) != ed25519.PrivateKeySize {
		return "", errors.New("tunnel: no key to sign heartbeat orders with")
	}
	sum := sha256.Sum256(body)
	payload, err := json.Marshal(Orders{Host: host, IssuedAt: now.UTC(),
		Body: hex.EncodeToString(sum[:])})
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding.EncodeToString(payload)
	sig := ed25519.Sign(key, []byte(ordersContext+enc))
	return enc + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// ErrOrders is a signed reply the agent will not act on, with why.
type ErrOrders struct{ Reason string }

func (e ErrOrders) Error() string { return "heartbeat orders refused: " + e.Reason }

// VerifyOrders checks token against key, host and body, and that it is no
// older than MaxOrdersAge by now. It does not check order against earlier
// replies; the caller holds that.
func VerifyOrders(key ed25519.PublicKey, host, token string, body []byte, now time.Time) (Orders, error) {
	if len(key) != ed25519.PublicKeySize {
		return Orders{}, ErrOrders{"this agent does not know the control plane's key"}
	}
	enc, sig, ok := strings.Cut(token, ".")
	if !ok {
		return Orders{}, ErrOrders{"malformed"}
	}
	rawSig, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !ed25519.Verify(key, []byte(ordersContext+enc), rawSig) {
		return Orders{}, ErrOrders{"the signature is not the control plane's"}
	}
	payload, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return Orders{}, ErrOrders{"malformed"}
	}
	var o Orders
	if err := json.Unmarshal(payload, &o); err != nil || o.IssuedAt.IsZero() || o.Body == "" {
		return Orders{}, ErrOrders{"malformed"}
	}
	sum := sha256.Sum256(body)
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(o.Body)) != 1 {
		return Orders{}, ErrOrders{"the reply is not the one the control plane signed"}
	}
	if !strings.EqualFold(o.Host, host) {
		return Orders{}, ErrOrders{fmt.Sprintf("they are for %s, and this host is %s", o.Host, host)}
	}
	if now.Sub(o.IssuedAt) > MaxOrdersAge {
		return Orders{}, ErrOrders{fmt.Sprintf("they were issued at %s, more than %s ago; "+
			"check this host's clock if this repeats", o.IssuedAt.Format(time.RFC3339), MaxOrdersAge)}
	}
	return o, nil
}
