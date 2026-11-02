package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

func TestOpenBackup(t *testing.T) {
	dir := t.TempDir()
	priv, err := tunnel.DeriveBackupKey(strings.Repeat("ab", 32), "org-1")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "b.argusdump")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	hdr := tunnel.DumpHeader{BackupID: "0b2d2a6e-1c2f-4a0e-9b6d-5f1c2a3b4c5d", DatabaseID: "d", Organization: "org-1",
		DBName: "app", Host: "localhost", Format: "custom", StartedAt: time.Now()}
	w, _, err := tunnel.NewDumpWriter(f, priv.PublicKey(), hdr)
	if err != nil {
		t.Fatal(err)
	}
	plain := bytes.Repeat([]byte("PGDMP hello "), 100000)
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	open := func() *os.File {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	// Header only.
	if code := openBackup(open(), "", "", "", true); code != 0 {
		t.Fatalf("header: exit %d", code)
	}
	// Wrong secret.
	if code := openBackup(open(), strings.Repeat("cd", 32), "", "", true); code != 1 {
		t.Fatalf("wrong secret: exit %d", code)
	}
	out := filepath.Join(dir, "plain.dump")
	if code := openBackup(open(), strings.Repeat("ab", 32), "", out, true); code != 0 {
		t.Fatalf("decrypt: exit %d", code)
	}
	got, _ := os.ReadFile(out)
	if !bytes.Equal(got, plain) {
		t.Fatalf("plaintext differs: %d vs %d bytes", len(got), len(plain))
	}
	// A flipped byte in the body fails, and leaves no output.
	raw, _ := os.ReadFile(path)
	raw[len(raw)-10] ^= 1
	os.WriteFile(path, raw, 0o600)
	out2 := filepath.Join(dir, "plain2.dump")
	if code := openBackup(open(), strings.Repeat("ab", 32), "", out2, true); code != 1 {
		t.Fatalf("tampered: exit %d", code)
	}
	if _, err := os.Stat(out2); !os.IsNotExist(err) {
		t.Fatal("tampered output was left behind")
	}
}
