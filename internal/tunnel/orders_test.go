package tunnel

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"
)

/*
Signed heartbeat orders: what an agent refuses. Each is a way a party on the
link between a server and the control plane could otherwise tell the agent,
which runs as root, to do something.
*/

var orderBody = []byte(`{"status":"ok","personal_accounts":[{"name":"ana","sudo":true}]}` + "\n")

func signOrders(t *testing.T, k ed25519.PrivateKey, host string, body []byte, at time.Time) string {
	t.Helper()
	tok, err := SignOrders(k, host, body, at)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestSignedOrdersVerifyForTheirHostAndBody(t *testing.T) {
	k := testKey(t)
	now := time.Now()
	tok := signOrders(t, k, "WEB-01", orderBody, now)
	o, err := VerifyOrders(pub(k), "web-01", tok, orderBody, now)
	if err != nil {
		t.Fatalf("refused the control plane's own orders: %v", err)
	}
	if !o.IssuedAt.Equal(now.UTC()) {
		t.Errorf("IssuedAt = %v, want %v", o.IssuedAt, now.UTC())
	}
}

func TestOrdersRefusals(t *testing.T) {
	k := testKey(t)
	now := time.Now()
	tok := signOrders(t, k, "web-01", orderBody, now)
	_, other, _ := ed25519.GenerateKey(rand.Reader)

	cases := map[string]func() error{
		// Someone on the link adding sudo for themselves to a signed reply.
		"a changed body": func() error {
			changed := []byte(strings.Replace(string(orderBody), `"ana"`, `"eve"`, 1))
			_, err := VerifyOrders(pub(k), "web-01", tok, changed, now)
			return err
		},
		"another host's orders": func() error {
			_, err := VerifyOrders(pub(k), "db-01", tok, orderBody, now)
			return err
		},
		"another key": func() error {
			_, err := VerifyOrders(pub(k), "web-01",
				signOrders(t, other, "web-01", orderBody, now), orderBody, now)
			return err
		},
		"held back for longer than MaxOrdersAge": func() error {
			_, err := VerifyOrders(pub(k), "web-01", tok, orderBody, now.Add(MaxOrdersAge+time.Minute))
			return err
		},
		"no signature": func() error {
			_, err := VerifyOrders(pub(k), "web-01", "", orderBody, now)
			return err
		},
		"a signature cut short": func() error {
			_, err := VerifyOrders(pub(k), "web-01", tok[:len(tok)-4], orderBody, now)
			return err
		},
	}
	for name, try := range cases {
		err := try()
		var refused ErrOrders
		if !errors.As(err, &refused) {
			t.Errorf("%s: err = %v, want a refusal", name, err)
		}
	}
}

// The same key signs grants and orders. Neither may stand in for the other:
// a grant is permission to open one session, and orders are not.
func TestOrdersAndGrantsCannotBeSwapped(t *testing.T) {
	k := testKey(t)
	grant := sign(t, k, NewGrant("ana@example.com", "web-01", "deploy", ""))
	if _, err := VerifyOrders(pub(k), "web-01", grant, orderBody, time.Now()); err == nil {
		t.Error("a grant was accepted as heartbeat orders")
	}
	orders := signOrders(t, k, "web-01", orderBody, time.Now())
	v := &GrantVerifier{Host: "web-01"}
	if _, err := v.Verify(pub(k), orders); err == nil {
		t.Error("heartbeat orders were accepted as a grant")
	}
}
