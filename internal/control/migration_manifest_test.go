package control

import "testing"

// A migration that takes away something an older version uses must say so,
// because the console tells people whether going back after an update is
// clean from what the release's migrations say. See migration_manifest.go.
func TestMigrationsSayWhenTheyBreakRollback(t *testing.T) {
	list, err := MigrationManifest()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) == 0 {
		t.Fatal("no migrations")
	}
	for _, m := range list {
		body, err := migrationFS.ReadFile("migrations/" + m.Name)
		if err != nil {
			t.Fatal(err)
		}
		what := takesAwaySomething(string(body))
		if what != "" && !m.Breaking {
			t.Errorf("%s has %q, which a version going back cannot run against. Ship the code that "+
				"stops using it first and drop it a release later; then start the file with\n"+
				"\t%s what it takes away", m.Name, what, breakingMarker)
		}
		if m.Breaking && m.Why == "" {
			t.Errorf("%s is marked breaking without saying what it takes away", m.Name)
		}
	}
}

func TestTakesAwaySomething(t *testing.T) {
	cases := map[string]bool{
		`ALTER TABLE users DROP COLUMN IF EXISTS idp_subject;`:                                              true,
		`DROP TABLE old_things;`:                                                                            true,
		`ALTER TABLE users RENAME COLUMN a TO b;`:                                                           true,
		`ALTER TABLE users ALTER COLUMN a TYPE bigint;`:                                                     true,
		`ALTER TABLE users ALTER COLUMN a SET NOT NULL;`:                                                    true,
		`ALTER TABLE users ADD COLUMN a TEXT NOT NULL;`:                                                     true,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS a TEXT NOT NULL DEFAULT '';`:                            false,
		`ALTER TABLE users ADD COLUMN a TEXT;`:                                                              false,
		`ALTER TABLE d DROP CONSTRAINT IF EXISTS c; ALTER TABLE d ADD CONSTRAINT c CHECK (x IN ('a','b'));`: false,
		"-- we used to DROP COLUMN here\nCREATE TABLE t (id TEXT NOT NULL);":                                false,
		`CREATE INDEX i ON t (a); DROP INDEX IF EXISTS j;`:                                                  false,
	}
	for sql, want := range cases {
		if got := takesAwaySomething(sql) != ""; got != want {
			t.Errorf("%q: got %v, want %v", sql, got, want)
		}
	}
}
