package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigRefusesUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.yaml")
	if err := os.WriteFile(path, []byte("organization: impack\nbehind_tls_proxi: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), "behind_tls_proxi") {
		t.Fatalf("a mistyped key was accepted: %v", err)
	}
}

// The packaged example must load and, with a key filled in, validate.
func TestPackagedExampleLoads(t *testing.T) {
	cfg, err := loadConfig("../../packaging/config/secret.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Organization == "" || cfg.Listen == "" {
		t.Fatalf("example: %+v", cfg)
	}
}
