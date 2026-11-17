package control

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
The fleet recovery key (recovery_key.go).

	ARGUS_TEST_DATABASE_URL=... go test ./internal/control/ -run Recovery
*/

// The key is made once, its private half is answered once and not kept,
// it rides the heartbeat, and servers holding it are counted.
func TestTheRecoveryKeyIsMadeOnceAndRidesTheHeartbeat(t *testing.T) {
	_, s, h, _ := livenessAPI(t)
	asset, cred := enrolledAsset(t, s, h)

	if w := do(t, h, http.MethodGet, "/api/v1/platform/recovery-key", "admin-token", ""); w.Code != http.StatusOK ||
		!json.Valid(w.Body.Bytes()) {
		t.Fatalf("view: %d %s", w.Code, w.Body)
	}
	w := do(t, h, http.MethodPost, "/api/v1/platform/recovery-key", "admin-token", "")
	if w.Code != http.StatusOK {
		t.Fatalf("make: %d %s", w.Code, w.Body)
	}
	var made struct{ Fingerprint, PrivateKey string }
	_ = json.Unmarshal(w.Body.Bytes(), &made)
	priv, err := tunnel.DecodeRecoveryPrivate(made.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if w := do(t, h, http.MethodPost, "/api/v1/platform/recovery-key", "admin-token", ""); w.Code != http.StatusConflict {
		t.Errorf("a second key: %d", w.Code)
	}
	if e := lastAuditOn(t, s, "argus-control"); e.Action != "control.recovery_key_created" {
		t.Errorf("audit: %+v", e)
	}

	beat := func(fp string) map[string]any {
		w := do(t, h, http.MethodPost, "/api/v1/report/heartbeat", cred,
			`{"hostname":"`+asset.Hostname+`","version":"1.0.0","active_sessions":0,"recovery_key":"`+fp+`"}`)
		var got map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		return got
	}
	got := beat("")
	if got["recovery_key"] != tunnel.EncodeGrantKey(priv.Public().(ed25519.PublicKey)) {
		t.Fatalf("the heartbeat carries %v", got["recovery_key"])
	}
	if _, ok := got["rekey"]; ok {
		t.Error("a rekey before any was made")
	}
	beat(made.Fingerprint)
	w = do(t, h, http.MethodGet, "/api/v1/platform/recovery-key", "admin-token", "")
	var v RecoveryKey
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	if !v.Set || v.Fingerprint != made.Fingerprint || v.Holding != 1 || v.Total < 1 {
		t.Errorf("view: %+v", v)
	}
}

// A rekey takes this deployment's recovery key, signs an order per
// organization for the key the new secret derives, and rides the heartbeat.
func TestARekeyIsSignedForEachOrganization(t *testing.T) {
	_, s, h, _ := livenessAPI(t)
	asset, cred := enrolledAsset(t, s, h)
	ctx := context.Background()
	pub, priv, _ := tunnel.NewRecoveryKey()
	_, wrong, _ := tunnel.NewRecoveryKey()
	secret := "a-brand-new-secret-0123456789abcdef0123456789"

	if _, err := s.Rekey(ctx, priv, secret, time.Now()); err == nil {
		t.Fatal("a rekey with no recovery key held")
	}
	if err := s.CreateRecoveryKey(ctx, pub, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rekey(ctx, wrong, secret, time.Now()); err == nil {
		t.Fatal("a rekey with another recovery key")
	}
	n, err := s.Rekey(ctx, priv, secret, time.Now())
	if err != nil || n < 1 {
		t.Fatalf("rekey: %d %v", n, err)
	}
	w := do(t, h, http.MethodPost, "/api/v1/report/heartbeat", cred,
		`{"hostname":"`+asset.Hostname+`","version":"1.0.0","active_sessions":0}`)
	var got struct {
		Rekey string `json:"rekey"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	o, err := tunnel.VerifyRecovery(pub, got.Rekey)
	if err != nil {
		t.Fatalf("the heartbeat's rekey: %v", err)
	}
	root, _ := tunnel.GrantKeyFromSecret(secret)
	if o.Key != tunnel.EncodeGrantKey(GrantPublicFor(root, DefaultTenantID)) {
		t.Error("the rekey names another key than the new secret's")
	}
}
