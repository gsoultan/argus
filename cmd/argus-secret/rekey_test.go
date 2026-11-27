package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/gsoultan/argus/internal/vault"
)

func TestRekeyCommandRotatesTheRootKeyFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "secret.yaml")
	data := filepath.Join(dir, "data")
	if err := os.WriteFile(cfgPath, []byte("organization: impack\ndata_dir: "+data+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(data, "root.key")
	old, _, err := vault.LoadRootKey(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	s, err := vault.Open(data, bytes.Clone(old), "impack")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(vault.PutInput{Name: "prod/db", Type: vault.TypeText, Value: []byte("hunter2"),
		Actor: vault.Actor{Kind: vault.ActorPerson, ID: "a@x"}, Route: vault.RouteConsole}); err != nil {
		t.Fatal(err)
	}
	// While the store is open (argus-secret running), rekey refuses.
	if code := rekey([]string{"--config", cfgPath}); code == 0 {
		t.Fatal("rekey ran against a store in use")
	}
	s.Close()

	if code := rekey([]string{"--config", cfgPath, "--rotate-root", "--reason", "test"}); code != 0 {
		t.Fatalf("rekey exited %d", code)
	}
	if _, err := os.Stat(keyFile + ".new"); err == nil {
		t.Fatal("root.key.new was left behind")
	}
	now, err := os.ReadFile(keyFile)
	if err != nil || bytes.Equal(now, old) {
		t.Fatal("root.key was not replaced")
	}
	if _, err := vault.Open(data, old, "impack"); err == nil {
		t.Fatal("the old root key still opens the store")
	}
	s, err = vault.Open(data, now, "impack")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	v, _, err := s.Read(vault.ReadInput{Name: "prod/db", Actor: vault.Actor{Kind: vault.ActorPerson, ID: "a@x"}, Route: vault.RouteConsole})
	if err != nil || string(v) != "hunter2" {
		t.Fatalf("after rekey: %q %v", v, err)
	}
	st, _ := s.Encryption()
	if !st.CommitOnly || st.V2Values != 1 {
		t.Fatalf("encryption: %+v", st)
	}
}
