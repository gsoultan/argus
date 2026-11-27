package main

import (
	"os/exec"
	"sort"
	"strings"
	"testing"
)

// argus-secret links none of argus-control and none of the agent. It holds
// the keys to every secret, so what it links is what an attacker who reaches
// it can use, and a control-plane change must never be a reason to restart it.
func TestSecretServerLinksNeitherControlNorAgent(t *testing.T) {
	root, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", "./cmd/argus-secret")
	cmd.Dir = strings.TrimSpace(string(root))
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	const module = "github.com/gsoultan/argus/"
	allowed := map[string]bool{
		module + "cmd/argus-secret":    true,
		module + "internal/vault":      true,
		module + "internal/secretwire": true,
		module + "internal/hashchain":  true,
		module + "internal/secrets":    true,
		module + "internal/tlsconfig":  true,
	}
	var bad []string
	for _, pkg := range strings.Fields(string(out)) {
		if strings.HasPrefix(pkg, module) && !allowed[pkg] {
			bad = append(bad, pkg)
		}
		for _, m := range []string{"github.com/gsoultan/storm", "github.com/jackc/pgx", "github.com/minio/minio-go"} {
			if strings.HasPrefix(pkg, m) {
				bad = append(bad, pkg)
			}
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Errorf("argus-secret links code it must not:\n  %s", strings.Join(bad, "\n  "))
	}
}
