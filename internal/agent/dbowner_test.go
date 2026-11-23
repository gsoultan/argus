package agent

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

func TestAddArgusLineKeepsTheFile(t *testing.T) {
	dir := t.TempDir()
	ini := filepath.Join(dir, "pgbouncer.ini")
	orig := "[databases]\nbilling = host=127.0.0.1 port=5432\n\n[pgbouncer]\nlisten_port = 6432\n"
	if err := os.WriteFile(ini, []byte(orig), 0o640); err != nil {
		t.Fatal(err)
	}
	line := tunnel.PoolerDatabaseLine("shop", "127.0.0.1", 5432)
	ok, err := addArgusLine(ini, "databases", "added by Argus for shop", line)
	if err != nil || !ok {
		t.Fatalf("add: %v %v", ok, err)
	}
	got, _ := os.ReadFile(ini)
	want := "[databases]\n" + tunnel.PoolerLineMarker + " added by Argus for shop\n" + line + "\nbilling = host=127.0.0.1 port=5432\n\n[pgbouncer]\nlisten_port = 6432\n"
	if string(got) != want {
		t.Fatalf("file:\n%s", got)
	}
	if bak, _ := os.ReadFile(ini + ".argus.bak"); string(bak) != orig {
		t.Fatalf("backup:\n%s", bak)
	}
	if st, _ := os.Stat(ini); runtime.GOOS != "windows" && st.Mode().Perm() != 0o640 {
		t.Fatalf("mode changed to %v", st.Mode().Perm())
	}
	// Twice is once.
	if ok, err := addArgusLine(ini, "databases", "again", line); ok || err != nil {
		t.Fatalf("added twice: %v %v", ok, err)
	}
	// A userlist.txt that is not there yet is made.
	ul := filepath.Join(dir, "userlist.txt")
	if ok, err := addArgusLine(ul, "", "owner of shop", `"shop_app" "SCRAM-SHA-256$v"`); !ok || err != nil {
		t.Fatalf("userlist: %v %v", ok, err)
	}
}

func TestEnsurePooler(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc/pgbouncer"), 0o755); err != nil {
		t.Fatal(err)
	}
	ini := "/etc/pgbouncer/pgbouncer.ini"
	if err := os.WriteFile(filepath.Join(root, ini), []byte("[databases]\nbilling = host=10.0.0.9 port=5432\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	reloaded := 0
	was := reloadBouncers
	reloadBouncers = func(pgLocator) error { reloaded++; return nil }
	t.Cleanup(func() { reloadBouncers = was })

	l := pgLocator{root: root}
	b := pgBouncerSeen{Config: ini, Port: 6432, AuthFile: "/etc/pgbouncer/userlist.txt",
		Targets: []tunnel.PoolerTarget{{Name: "billing", Host: "10.0.0.9", Port: 5432}}}
	c := tunnel.GrantCreate{Op: tunnel.CreateOpCreate, Engine: tunnel.EnginePostgres, Name: "shop", Host: "127.0.0.1", Port: 5432,
		Owner: &tunnel.CreateOwner{Kind: tunnel.OwnerNew, Name: "shop_app"}}
	steps, changed, manual := ensurePooler(l, b, c, "SCRAM-SHA-256$4096:abc")
	if len(manual) != 0 || len(steps) != 1 || !steps[0].OK || reloaded != 1 {
		t.Fatalf("steps %+v manual %v reloaded %d", steps, manual, reloaded)
	}
	text := strings.Join(changed, "\n")
	if !strings.Contains(text, "shop = host=10.0.0.9 port=5432 dbname=shop") || strings.Contains(text, "abc") {
		t.Fatalf("changed: %s", text)
	}
	ul, _ := os.ReadFile(filepath.Join(root, b.AuthFile))
	if !strings.Contains(string(ul), `"shop_app" "SCRAM-SHA-256$4096:abc"`) {
		t.Fatalf("userlist: %s", ul)
	}

	// Covered by "*", trusting nothing new: nothing to change.
	b.Targets = append(b.Targets, tunnel.PoolerTarget{Name: "*", Port: 5432})
	b.AuthType = "trust"
	if steps, changed, _ := ensurePooler(l, b, c, "v"); len(changed) != 0 || !steps[0].OK {
		t.Fatalf("covered: %+v %v", steps, changed)
	}

	// The reload refused: the lines to add by hand, nothing claimed.
	reloadBouncers = func(pgLocator) error { return errors.New("permission denied") }
	b.Targets, b.AuthType = b.Targets[:1], ""
	c.Name, c.Owner.Name = "crm", "crm_app"
	steps, changed, manual = ensurePooler(l, b, c, "SCRAM-SHA-256$x")
	if len(changed) != 0 || len(manual) < 3 || steps[0].OK || !strings.Contains(strings.Join(manual, "\n"), "crm = host=10.0.0.9") {
		t.Fatalf("refused: %+v %v %v", steps, changed, manual)
	}
}
