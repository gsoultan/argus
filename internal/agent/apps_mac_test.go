package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseLsofListen(t *testing.T) {
	out := "p101\ncpostgres\nn127.0.0.1:5432\nn[::1]:5432\np202\ncsshd\nn*:22\nn*:22\nnbad\n"
	got := parseLsofListen(out)
	if len(got) != 3 {
		t.Fatalf("%+v", got)
	}
	if got[0].Process != "postgres" || got[0].Port != 5432 || got[0].pid != 101 {
		t.Fatalf("%+v", got[0])
	}
	if got[1].Address != "::1" {
		t.Fatalf("ipv6: %+v", got[1])
	}
	if got[2].Address != "0.0.0.0" || got[2].Process != "sshd" {
		t.Fatalf("wildcard: %+v", got[2])
	}
}

func TestCatalogueNames(t *testing.T) {
	got := catalogueNames("launchd\npostgres\npostgres\nredis-server\nFinder\n")
	if len(got) != 2 || got[0] != "postgres" || got[1] != "redis-server" {
		t.Fatalf("%v", got)
	}
}

func TestBrewCellar(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"postgresql@17/17.0", "postgresql@17/17.2", "redis/7.2.4", ".keepme/x"} {
		if err := os.MkdirAll(filepath.Join(dir, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got, ok := brewCellar(dir)
	if !ok || len(got) != 2 {
		t.Fatalf("%v %+v", ok, got)
	}
	if got[0].name != "postgresql" || got[0].version != "17.2" {
		t.Fatalf("%+v", got[0])
	}
	inv := classify(hostScan{packages: got}, time.Now())
	found := false
	for _, a := range inv.Applications {
		if a.Kind == "postgres" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a Homebrew postgresql@17 is not PostgreSQL: %+v", inv.Applications)
	}
	if _, ok := brewCellar(filepath.Join(dir, "missing")); ok {
		t.Fatal("a missing Cellar read as empty")
	}
}
