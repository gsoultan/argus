package tunnel

import (
	"strings"
	"testing"
)

func TestMSSQLLogin(t *testing.T) {
	a := MSSQLLogin("ana_sari", LevelRead, "sales")
	if !strings.HasPrefix(a, "argus-ana_sari-read-") || !IsStoreUser(a) {
		t.Fatalf("login: %q", a)
	}
	// One login per database: a session on one never reaches another.
	if b := MSSQLLogin("ana_sari", LevelRead, "billing"); a == b {
		t.Fatalf("two databases share the login %q", a)
	}
	long := strings.Repeat("x", 200)
	if l := MSSQLLogin(long, LevelDBA, "sales"); len(l) > MSSQLMaxName || !IsStoreUser(l) ||
		l == MSSQLLogin(long+"y", LevelDBA, "sales") {
		t.Fatalf("long login: %q (%d)", l, len(l))
	}
}

func TestValidMSSQLDB(t *testing.T) {
	for name, ok := range map[string]bool{
		"sales": true, "Sales DB": true, "a]b": true, "日本": true,
		"": false, "master": false, "TempDB": false, "msdb": false, "model": false, "distribution": false,
		" lead": false, "trail ": false, "tab\there": false, strings.Repeat("a", 64): false,
	} {
		if ValidMSSQLDB(name) != ok {
			t.Errorf("%q: want %v", name, ok)
		}
	}
	if MSSQLIdent("a]b") != "[a]]b]" || MSSQLString("it's") != "N'it''s'" {
		t.Fatal("quoting")
	}
}

// A T-SQL batch holds statements with nothing between them, so whether it
// changes anything is read from every word, not the first.
func TestMSSQLWrites(t *testing.T) {
	for sql, writes := range map[string]bool{
		"SELECT * FROM items":                                  false,
		"select [update], [delete] from [insert]":              false,
		"SELECT 'DELETE FROM x' AS s -- DROP TABLE y":          false,
		"/* EXEC sp_who */ SELECT 1":                           false,
		"PRINT 'hello'":                                        false,
		"DECLARE @n int = 1; SELECT @n":                        false,
		"WITH c AS (SELECT 1 AS a) SELECT a FROM c":            false,
		"BEGIN TRANSACTION":                                    false,
		"SELECT 1 DELETE FROM items":                           true,
		"SELECT 1; DELETE FROM items":                          true,
		"SELECT * INTO copy FROM items":                        true,
		"EXEC sp_rename 'a', 'b'":                              true,
		"sp_configure":                                         true,
		"SELECT 1 EXECUTE ('DROP TABLE x')":                    true,
		"SELECT 1 ALTER INDEX ALL ON t REBUILD":                true,
		"SELECT 1 DBCC FREEPROCCACHE":                          true,
		"SELECT 1 GRANT SELECT ON t TO p":                      true,
		"SELECT * FROM OPENROWSET('SQLNCLI', 'x', 'SELECT 1')": true,
		"MERGE t USING s ON 1 = 1 WHEN MATCHED THEN DELETE;":   true,
		"BACKUP DATABASE x TO DISK = 'y'":                      true,
		"TRUNCATE TABLE t":                                     true,
	} {
		if got := MSSQLWrites(sql); got != writes {
			t.Errorf("%q: writes %v, want %v", sql, got, writes)
		}
	}
}

func TestClassifyTSQL(t *testing.T) {
	c := ClassifySQL("DELETE TOP (10) FROM [dbo].[items]")
	if c.Verb != "delete" || len(c.Targets) != 1 || c.Targets[0] != "dbo.items" || c.HasWhere {
		t.Fatalf("delete top: %+v", c)
	}
	c = ClassifySQL("UPDATE TOP (5) PERCENT items SET a = 1")
	if c.Verb != "update" || len(c.Targets) != 1 || c.Targets[0] != "items" {
		t.Fatalf("update top: %+v", c)
	}
	c = ClassifySQL("DROP TABLE [we]]ird]")
	if c.Object != "table" || len(c.Targets) != 1 || c.Targets[0] != "we]ird" {
		t.Fatalf("drop: %+v", c)
	}
	// Postgres's array subscripts still read as before.
	c = ClassifySQL("UPDATE t SET a[1] = 2 WHERE id = 1")
	if !c.HasWhere || c.Targets[0] != "t" {
		t.Fatalf("subscript: %+v", c)
	}
}
