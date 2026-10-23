package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The package installs a control.yaml of comments only, so a fresh install
// must read as "no configuration" and go to setup, while any real setting --
// or a file that does not parse -- must not.
func TestHasSettings(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"empty", "", false},
		{"comments", "# Argus control plane.\n#\n# Nothing to edit here.\n", false},
		{"setting", "listen: :8080\n", true},
		{"broken", "listen: [\n", true},
	} {
		got, err := hasSettings(write(tc.name, tc.body))
		if err != nil || got != tc.want {
			t.Errorf("%s: hasSettings = %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
	if got, err := hasSettings(filepath.Join(dir, "missing")); got || err != nil {
		t.Errorf("missing file: %v, %v", got, err)
	}
}

func TestLocateConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "control.yaml")
	loc, err := locateConfig(path)
	if err != nil || loc.Path != "" || loc.Managed != path {
		t.Fatalf("no file: %+v, %v; want setup writing %s", loc, err, path)
	}
	if err := os.WriteFile(path, []byte("listen: :8080\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if loc, _ = locateConfig(path); loc.Path != path {
		t.Fatalf("configured file not used: %+v", loc)
	}
	if loc.pki().Dir != filepath.Join(dir, "tls") || loc.secretsDir() != filepath.Join(dir, "secrets") {
		t.Errorf("files written beside the config: %+v", loc)
	}
	if _, err := commandConfig(filepath.Join(dir, "other.yaml")); err != errNotSetUp {
		t.Errorf("a command with nothing set up: %v, want errNotSetUp", err)
	}
}

// A save writes a secret under a new name, and only the one the saved file
// refers to survives.
func TestSecretStage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "smtp-password"), []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := &secretStage{dir: dir}
	ref, err := st.write("smtp-password", "new")
	if err != nil {
		t.Fatal(err)
	}
	refused := &secretStage{dir: dir}
	if _, err := refused.write("telegram-bot-token", "x"); err != nil {
		t.Fatal(err)
	}
	refused.discard()
	st.prune()
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || "${file:"+filepath.Join(dir, entries[0].Name())+"}" != ref {
		t.Fatalf("left %v, want only the file %s refers to", entries, ref)
	}
	if info, _ := entries[0].Info(); info.Mode().Perm() != 0o600 {
		t.Errorf("secret mode %v", info.Mode().Perm())
	}
}

func TestDurationText(t *testing.T) {
	for d, want := range map[time.Duration]string{
		8 * time.Hour: "8h", time.Minute: "1m", 90 * time.Second: "1m30s",
		90 * time.Minute: "1h30m", 0: "",
	} {
		if got := durationText(d); got != want {
			t.Errorf("durationText(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestChangedKeys(t *testing.T) {
	a := config{Listen: ":8080", LogLevel: "info", Console: &consoleConfig{GatewayURL: "https://a"}}
	b := a
	b.LogLevel = "debug"
	b.Console = &consoleConfig{GatewayURL: "https://b"}
	b.Tunnel = &tunnelConfig{QUICListen: ":8080"}
	got, err := changedKeys(a, b)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"console", "log_level", "tunnel"}
	if len(got) != len(want) {
		t.Fatalf("changed %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("changed %v, want %v", got, want)
		}
	}
}

// The file setup writes must load as the control plane reads it.
func TestWrittenConfigLoads(t *testing.T) {
	dir := t.TempDir()
	ref, err := writeSecret(filepath.Join(dir, "secrets"), "reporter-token", "tok-123")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "control.yaml")
	if err := writeConfig(path, config{Listen: ":8080", ReporterToken: ref,
		SessionTTL: 8 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReporterToken != "tok-123" || cfg.SessionTTL != 8*time.Hour {
		t.Errorf("loaded %+v", cfg)
	}
	raw, _ := os.ReadFile(path)
	if want := "${file:"; !strings.Contains(string(raw), want) || strings.Contains(string(raw), "tok-123") {
		t.Errorf("the file must hold the reference, not the secret:\n%s", raw)
	}
}
