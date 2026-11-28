package vault

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRootKeyFromCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	want, _ := randomKey()
	file := filepath.Join(t.TempDir(), "root.key")
	for name, out := range map[string]string{
		"base64":     base64.StdEncoding.EncodeToString(want) + "\n",
		"base64 url": base64.RawURLEncoding.EncodeToString(want),
	} {
		got, src, created, err := LoadConfiguredRootKey(Config{RootKeyFile: file, RootKeyCommand: []string{"sh", "-c", "printf '%s' '" + out + "'"}})
		if err != nil || !bytes.Equal(got, want) || src != RootKeyFromCommand || created {
			t.Fatalf("%s: %v %s %v", name, err, src, created)
		}
	}
	if _, err := os.Stat(file); err == nil {
		t.Fatal("a root key file was made although a command gives the key")
	}
}

func TestRootKeyCommandRefusals(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	for name, argv := range map[string][]string{
		"fails":        {"sh", "-c", "echo nope >&2; exit 3"},
		"short":        {"sh", "-c", "printf 'abc'"},
		"empty output": {"true"},
		"not found":    {"/nonexistent/kms-decrypt"},
	} {
		if _, _, _, err := LoadConfiguredRootKey(Config{RootKeyFile: filepath.Join(t.TempDir(), "k"), RootKeyCommand: argv}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	c := Config{Organization: "o", ControlPublicKey: base64.StdEncoding.EncodeToString(make([]byte, 32)), RootKeyCommand: []string{" "}}
	c.Defaults()
	if err := c.Validate(); err == nil {
		t.Error("an empty program in root_key_command was accepted")
	}
}

// systemd's LoadCredentialEncrypted= decrypts the key (TPM, host key) into
// $CREDENTIALS_DIRECTORY: argus-secret takes it from there.
func TestRootKeyFromSystemdCredential(t *testing.T) {
	want, _ := randomKey()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, RootKeyCredential), want, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	file := filepath.Join(t.TempDir(), "root.key")
	got, src, created, err := LoadConfiguredRootKey(Config{RootKeyFile: file})
	if err != nil || !bytes.Equal(got, want) || src != RootKeyFromCredential || created {
		t.Fatalf("%v %s %v", err, src, created)
	}
	if _, err := os.Stat(file); err == nil {
		t.Fatal("a root key file was made although a credential gives the key")
	}
	// The credential is read-only: replace it rather than write over it,
	// which fails for anyone but root.
	cred := filepath.Join(dir, RootKeyCredential)
	if err := os.Remove(cred); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cred, []byte("short"), 0o400); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := LoadConfiguredRootKey(Config{RootKeyFile: file}); err == nil {
		t.Fatal("a malformed credential was accepted")
	}
}

func TestRootKeyFileIsTheLastResort(t *testing.T) {
	t.Setenv("CREDENTIALS_DIRECTORY", t.TempDir())
	file := filepath.Join(t.TempDir(), "root.key")
	k, src, created, err := LoadConfiguredRootKey(Config{RootKeyFile: file})
	if err != nil || src != RootKeyFromFile || !created || len(k) != KeySize {
		t.Fatalf("%v %s %v", err, src, created)
	}
}

func TestHardenedProcessIsNotDumpable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	hardenProcess(slog.Default())
	if dumpable() {
		t.Fatal("the process can still dump core")
	}
}
