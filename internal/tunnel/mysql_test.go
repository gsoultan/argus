package tunnel

import (
	"strings"
	"testing"
)

func TestMySQLUser(t *testing.T) {
	if u := MySQLUser("ana_sari", LevelRead); u != "argus-ana_sari-read" {
		t.Fatal(u)
	}
	long := strings.Repeat("a", 40)
	a, b := MySQLUser(long, LevelOwner), MySQLUser(long+"b", LevelOwner)
	if len(a) > MySQLMaxUser || len(b) > MySQLMaxUser || a == b || !IsStoreUser(a) || !strings.HasSuffix(a, "-owner") {
		t.Fatalf("%q %q", a, b)
	}
}

func TestMySQLQuoting(t *testing.T) {
	if g := MySQLGrantDB("app_db%x`"); g != "`app\\_db\\%x```" {
		t.Fatal(g)
	}
	if s := MySQLString(`it's \ x`); s != `'it''s \\ x'` {
		t.Fatal(s)
	}
	q := MySQLGrantSQL("argus-a-dba", "localhost", "shop", LevelDBA)
	if len(q) != 2 || !strings.HasSuffix(q[0], "WITH GRANT OPTION") || !strings.HasPrefix(q[1], "GRANT PROCESS ON *.*") {
		t.Fatal(q)
	}
	for _, lvl := range []string{LevelRead, LevelWrite, LevelOwner} {
		for _, q := range MySQLGrantSQL("u", "h", "shop", lvl) {
			if strings.Contains(q, "*.*") || strings.Contains(q, "GRANT OPTION") || strings.Contains(q, "SUPER") {
				t.Fatalf("%s grants beyond its database: %s", lvl, q)
			}
		}
	}
}

func TestValidMySQLDB(t *testing.T) {
	for name, ok := range map[string]bool{"shop": true, "app_db": true, "Ünïcode": true, "": false, "mysql": false,
		"INFORMATION_SCHEMA": false, "a/b": false, "a.b": false, "trail ": false, strings.Repeat("x", 65): false} {
		if ValidMySQLDB(name) != ok {
			t.Errorf("%q", name)
		}
	}
}
