package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/gsoultan/argus/internal/control"
	"github.com/gsoultan/argus/internal/tunnel"
)

/*
The fleet recovery key's commands (internal/tunnel/recovery.go). Both need
the key's private half, which Argus never keeps, so both run on a machine
an admin brings it to -- the control plane host, from a USB stick.

	argus-control rekey -recovery-key /media/usb/argus-recovery.key
	argus-control recovery-key replace -old /media/usb/old.key -out /media/usb/new.key
*/

func runRecoveryCommand(configPath string, args []string) error {
	if len(args) == 0 || args[0] != "replace" {
		return errors.New("usage: argus-control recovery-key replace -old OLD.key -out NEW.key " +
			"(make the first key in Settings › Account security)")
	}
	fs := flag.NewFlagSet("recovery-key replace", flag.ContinueOnError)
	oldPath := fs.String("old", "", "the recovery key file being replaced")
	outPath := fs.String("out", "", "where to write the new recovery key file (must not exist)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *oldPath == "" || *outPath == "" {
		return errors.New("usage: argus-control recovery-key replace -old OLD.key -out NEW.key")
	}
	old, err := readRecoveryFile(*oldPath)
	if err != nil {
		return err
	}
	store, ctx, done, err := openStore(configPath)
	if err != nil {
		return err
	}
	defer done()
	pub, _, err := store.RecoveryPublic(ctx)
	if err != nil {
		return err
	}
	if pub == nil {
		return errors.New("this deployment has no fleet recovery key yet; make one in Settings › Account security")
	}
	if !pub.Equal(old.Public()) {
		return fmt.Errorf("%s is not this deployment's recovery key (%s)", *oldPath, tunnel.RecoveryFingerprint(pub))
	}
	newPub, newPriv, err := tunnel.NewRecoveryKey()
	if err != nil {
		return err
	}
	tok, err := tunnel.SignRecovery(old, tunnel.RecoveryOrder{Kind: tunnel.RecoveryReplace,
		Key: tunnel.EncodeGrantKey(newPub), IssuedAt: time.Now()})
	if err != nil {
		return err
	}
	// The new file first: a key the database names that nobody holds would
	// leave the fleet with no way back.
	f, err := os.OpenFile(*outPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("cannot write the new key: %w", err)
	}
	if _, err := f.WriteString(tunnel.EncodeRecoveryPrivate(newPriv)); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := store.ReplaceRecoveryKey(ctx, newPub, tok, hostActor()); err != nil {
		return fmt.Errorf("the new key was written to %s but not stored: %w", *outPath, err)
	}
	auditRecovery(ctx, store, "control.recovery_key_replaced", "argus-control", fmt.Sprintf(
		"Replaced the fleet recovery key %s with %s. Agents move to it at their next check-in.",
		tunnel.RecoveryFingerprint(pub), tunnel.RecoveryFingerprint(newPub)))
	fmt.Printf("New recovery key %s written to %s.\n", tunnel.RecoveryFingerprint(newPub), *outPath)
	fmt.Println("Agents move to it at their next check-in. Keep the new file offline; the old one")
	fmt.Println("still works on servers that have not checked in since, so keep it until they have.")
	return nil
}

// reSecretFile is a signing_secret kept in its own file.
var reSecretFile = regexp.MustCompile(`^\$\{file:(.+)\}$`)

// runRekey moves the fleet to a new signing_secret: it writes a new one and
// stores, for each organization, an order signed with the recovery key that
// tells agents the grant key derived from it.
func runRekey(configPath string, args []string) error {
	fs := flag.NewFlagSet("rekey", flag.ContinueOnError)
	keyPath := fs.String("recovery-key", "", "the fleet recovery key file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *keyPath == "" {
		return errors.New("usage: argus-control rekey -recovery-key /media/usb/argus-recovery.key")
	}
	recovery, err := readRecoveryFile(*keyPath)
	if err != nil {
		return err
	}
	secretPath, err := signingSecretFile(configPath)
	if err != nil {
		return err
	}
	store, ctx, done, err := openStore(configPath)
	if err != nil {
		return err
	}
	defer done()
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	oldSecret := cfg.SigningSecret

	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	secret := hex.EncodeToString(b)
	// Staged beside the old secret before any order is stored, so a write
	// that fails cannot leave agents moved to a key nobody holds.
	staged := secretPath + ".rekey"
	if err := writeAtomic(staged, []byte(secret+"\n"), 0o600); err != nil {
		return fmt.Errorf("cannot write the new signing secret: %w", err)
	}
	// Authenticator secrets are sealed with a key derived from the signing
	// secret, so they move with it and nobody enrols their authenticator
	// again. Moved before any order is stored, and moved back if anything
	// after fails, so a failed rekey leaves everything under the old secret.
	oldKey, newKey := control.MFAKeyFromSecret(oldSecret), control.MFAKeyFromSecret(secret)
	undo := func(cause error) error {
		_ = os.Remove(staged)
		if oldKey == nil {
			return cause
		}
		store.SetMFAKey(newKey)
		if _, err := store.ResealMFASecrets(ctx, oldKey); err != nil {
			return fmt.Errorf("%w; and the authenticator secrets could not be moved back (%v): "+
				"run rekey again, or reset the second factor of anyone who cannot sign in", cause, err)
		}
		return cause
	}
	if oldKey != nil {
		if _, err := store.ResealMFASecrets(ctx, newKey); err != nil {
			return undo(fmt.Errorf("cannot move the authenticator secrets to the new key: %w", err))
		}
	}
	now := time.Now()
	n, err := store.Rekey(ctx, recovery, secret, now)
	if err != nil {
		return undo(err)
	}
	kept := secretPath + ".before-rekey-" + now.UTC().Format("20060102T150405Z")
	if err := os.Rename(secretPath, kept); err != nil {
		return fmt.Errorf("the orders are stored, but the old secret could not be moved aside: %w; "+
			"move %s over %s by hand, then restart argus-control", err, staged, secretPath)
	}
	if err := os.Rename(staged, secretPath); err != nil {
		return fmt.Errorf("the orders are stored, but the new secret could not be put in place: %w; "+
			"move %s over %s by hand, then restart argus-control", err, staged, secretPath)
	}
	auditRecovery(ctx, store, "control.rekeyed", "argus-control", fmt.Sprintf(
		"Replaced the signing secret and signed a rekey for %d organizations with the fleet recovery key %s. "+
			"Agents move to the new key as they check in.", n, tunnel.RecoveryFingerprint(recovery.Public().(ed25519.PublicKey))))
	fmt.Printf("New signing secret written to %s (the old one is %s).\n", secretPath, kept)
	fmt.Printf("Signed a rekey for %d organizations with recovery key %s.\n\n", n,
		tunnel.RecoveryFingerprint(recovery.Public().(ed25519.PublicKey)))
	fmt.Println("Now:")
	fmt.Println("  1. Copy the new secret to every other control server, then restart argus-control on each.")
	fmt.Println("     Authenticator codes were moved to the new secret and work again once each is restarted.")
	fmt.Println("  2. Servers move to the new key at their next check-in, within a minute of reaching it.")
	fmt.Println("  3. Everyone signs in again: the secret also signs console sessions.")
	fmt.Println("  4. Keep the old secret: database backups made before now are opened with it.")
	return nil
}

// signingSecretFile is the file control.yaml takes signing_secret from.
func signingSecretFile(configPath string) (string, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return "", err
	}
	var raw struct {
		SigningSecret string `yaml:"signing_secret"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return "", err
	}
	m := reSecretFile.FindStringSubmatch(strings.TrimSpace(raw.SigningSecret))
	if m == nil {
		return "", fmt.Errorf("signing_secret in %s is not kept in its own file; rekey replaces that "+
			"file. Move it to one first: signing_secret: \"${file:/var/lib/argus-control/secrets/signing_secret}\"",
			configPath)
	}
	return m[1], nil
}

func readRecoveryFile(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return tunnel.DecodeRecoveryPrivate(string(b))
}

// openStore opens the database of the config at path.
func openStore(configPath string) (*control.Store, context.Context, func(), error) {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return nil, nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	store, err := control.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		cancel()
		return nil, nil, nil, fmt.Errorf("open store: %w", err)
	}
	ctx = control.PlatformContext(ctx)
	// The recovery commands read and write accounts too, so they hold the
	// key the running control plane seals authenticator secrets with.
	store.SetMFAKey(control.MFAKeyFromSecret(cfg.SigningSecret))
	return store, ctx, func() { store.Close(); cancel() }, nil
}
