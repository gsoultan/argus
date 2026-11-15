package agent

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
The order gate across an upgrade. An agent must keep taking orders from a
control plane that does not sign yet, and must stop taking unsigned ones once
it has had a signed one -- across a restart too.
*/

var gateBody = []byte(`{"status":"ok","uninstall":true}` + "\n")

func gateKey(t *testing.T, dir string) ed25519.PrivateKey {
	t.Helper()
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pin := NewGrantPin(dir)
	if _, err := pin.Check(k.Public().(ed25519.PublicKey)); err != nil {
		t.Fatal(err)
	}
	return k
}

func gateSign(t *testing.T, k ed25519.PrivateKey, host string, at time.Time) string {
	t.Helper()
	tok, err := tunnel.SignOrders(k, host, gateBody, at)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestAnOlderControlPlaneIsStillObeyed(t *testing.T) {
	dir := t.TempDir()
	gateKey(t, dir)
	g := &OrderGate{StateDir: dir, Host: "web-01"}
	if take, err := g.Accept("", gateBody); !take || err != nil {
		t.Fatalf("an unsigned reply before any signed one was refused: %v %v", take, err)
	}
	if g.Locked() {
		t.Error("an unsigned reply switched the gate on")
	}
}

func TestAnAgentThatHasNotPinnedAKeyYetTakesUnsignedReplies(t *testing.T) {
	dir := t.TempDir()
	_, k, _ := ed25519.GenerateKey(rand.Reader)
	g := &OrderGate{StateDir: dir, Host: "web-01"}
	if take, _ := g.Accept(gateSign(t, k, "web-01", time.Now()), gateBody); !take {
		t.Fatal("refused a reply with nothing pinned to check it with")
	}
	if g.Locked() {
		t.Error("switched on without having verified anything")
	}
}

func TestOnceSignedUnsignedRepliesAreRefused(t *testing.T) {
	dir := t.TempDir()
	k := gateKey(t, dir)
	g := &OrderGate{StateDir: dir, Host: "web-01"}
	if take, err := g.Accept(gateSign(t, k, "web-01", time.Now()), gateBody); !take {
		t.Fatalf("refused the control plane's own signed reply: %v", err)
	}
	if take, _ := g.Accept("", gateBody); take {
		t.Fatal("an unsigned reply was acted on after a signed one")
	}
	// A restart: the switch is on disk, not only in memory.
	again := &OrderGate{StateDir: dir, Host: "web-01"}
	if take, _ := again.Accept("", gateBody); take {
		t.Fatal("an unsigned reply was acted on after a restart")
	}
}

func TestASignedReplyIsNotReplayed(t *testing.T) {
	dir := t.TempDir()
	k := gateKey(t, dir)
	g := &OrderGate{StateDir: dir, Host: "web-01"}
	now := time.Now()
	old := gateSign(t, k, "web-01", now.Add(-10*time.Minute))
	if take, _ := g.Accept(old, gateBody); !take {
		t.Fatal("refused a fresh signed reply")
	}
	if take, _ := g.Accept(gateSign(t, k, "web-01", now), gateBody); !take {
		t.Fatal("refused a newer signed reply")
	}
	if take, _ := g.Accept(old, gateBody); take {
		t.Fatal("an older signed reply was acted on again")
	}
	// A replica a few seconds behind is not a replay.
	if take, err := g.Accept(gateSign(t, k, "web-01", now.Add(-5*time.Second)), gateBody); !take {
		t.Fatalf("refused a reply from a replica a few seconds behind: %v", err)
	}
}

func TestASignedReplyMustVerify(t *testing.T) {
	dir := t.TempDir()
	k := gateKey(t, dir)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	g := &OrderGate{StateDir: dir, Host: "web-01"}
	if take, _ := g.Accept(gateSign(t, other, "web-01", time.Now()), gateBody); take {
		t.Fatal("a reply signed with another key was acted on")
	}
	if take, _ := g.Accept(gateSign(t, k, "db-01", time.Now()), gateBody); take {
		t.Fatal("another host's reply was acted on")
	}
}

// A switch file that cannot be read counts as on: refusing unsigned replies
// is the side to be wrong on.
func TestAnUnreadableSwitchCountsAsOn(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, SignedOrdersFile), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := &OrderGate{StateDir: dir, Host: "web-01"}
	if take, _ := g.Accept("", gateBody); take {
		t.Fatal("an unsigned reply was acted on with the switch file unreadable")
	}
}
