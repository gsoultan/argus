package main

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gsoultan/argus/internal/control"
	"github.com/gsoultan/argus/internal/tunnel"
)

// rekeyDB is an empty database of its own, dropped after the test.
func rekeyDB(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("ARGUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ARGUS_TEST_DATABASE_URL is not set")
	}
	i := strings.LastIndex(dsn, "/")
	j := strings.Index(dsn[i:], "?")
	if j < 0 {
		t.Skip("cannot derive an admin DSN")
	}
	admin := dsn[:i+1] + "postgres" + dsn[i+j:]
	name := fmt.Sprintf("argus_rekey_%d", time.Now().UnixNano())
	exec := func(sql string) {
		conn, err := pgx.Connect(context.Background(), admin)
		if err != nil {
			t.Skipf("no maintenance database: %v", err)
		}
		defer conn.Close(context.Background())
		if _, err := conn.Exec(context.Background(), sql); err != nil {
			t.Fatal(err)
		}
	}
	exec("CREATE DATABASE " + name)
	t.Cleanup(func() { exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)") })
	return dsn[:i+1] + name + dsn[i+j:]
}

// rekey writes a new secret where the config takes it from, keeps the old
// one, and stores an order signed with the recovery key for the new key.
func TestRekeyReplacesTheSecretAndSignsAnOrder(t *testing.T) {
	dsn := rekeyDB(t)
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "signing_secret")
	oldSecret := "old-secret-0123456789abcdef0123456789abcdef"
	if err := os.WriteFile(secretPath, []byte(oldSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "control.yaml")
	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf("database_url: %q\nreporter_token: \"rt-0123456789abcdef0123456789abcdef\"\n"+
		"signing_secret: \"${file:%s}\"\n", dsn, secretPath)), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := control.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pub, priv, _ := tunnel.NewRecoveryKey()
	if err := store.CreateRecoveryKey(ctx, pub, "test"); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "recovery.key")
	_ = os.WriteFile(keyPath, []byte(tunnel.EncodeRecoveryPrivate(priv)), 0o600)

	// Another key changes nothing.
	_, wrong, _ := tunnel.NewRecoveryKey()
	wrongPath := filepath.Join(dir, "wrong.key")
	_ = os.WriteFile(wrongPath, []byte(tunnel.EncodeRecoveryPrivate(wrong)), 0o600)
	if err := runRekey(cfgPath, []string{"-recovery-key", wrongPath}); err == nil {
		t.Fatal("rekeyed with another recovery key")
	}
	if b, _ := os.ReadFile(secretPath); strings.TrimSpace(string(b)) != oldSecret {
		t.Fatal("a refused rekey changed the secret")
	}

	if err := runRekey(cfgPath, []string{"-recovery-key", keyPath}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(secretPath)
	newSecret := strings.TrimSpace(string(b))
	if newSecret == oldSecret || len(newSecret) < 32 {
		t.Fatalf("secret not replaced: %q", newSecret)
	}
	kept, _ := filepath.Glob(secretPath + ".before-rekey-*")
	if len(kept) != 1 {
		t.Fatalf("the old secret was not kept: %v", kept)
	}
	tok, err := store.RekeyOrder(ctx, control.DefaultTenantID)
	if err != nil || tok == "" {
		t.Fatalf("no order: %v", err)
	}
	o, err := tunnel.VerifyRecovery(pub, tok)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := tunnel.GrantKeyFromSecret(newSecret)
	if o.Key != tunnel.EncodeGrantKey(root.Public().(ed25519.PublicKey)) {
		t.Error("the order names another key than the new secret's")
	}
}

func TestRekeyNeedsTheSecretInItsOwnFile(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "control.yaml")
	_ = os.WriteFile(cfg, []byte("signing_secret: \"inline-secret-0123456789abcdef0123\"\n"), 0o600)
	if _, err := signingSecretFile(cfg); err == nil || !strings.Contains(err.Error(), "own file") {
		t.Errorf("inline secret: %v", err)
	}
}
