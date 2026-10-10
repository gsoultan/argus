package main

import (
	"os"
	"path/filepath"
	"testing"
)

// An object key comes from the bucket, which this process does not control.
// "../../etc/cron.d/x" is a valid key, and copying it verbatim would let
// whoever can write to the bucket write anywhere the backup runs as.
func TestARecordingKeyCannotLeaveTheBackup(t *testing.T) {
	root := t.TempDir()
	for _, key := range []string{"../escape.cast", "2026/10/../../../etc/passwd", "../../x"} {
		if p, err := safeJoin(root, key); err == nil {
			t.Errorf("key %q was written to %s", key, p)
		}
	}
	if _, err := safeJoin(root, "2026/10/10/abc.cast"); err != nil {
		t.Errorf("an ordinary key was refused: %v", err)
	}
}

// Pruning removes old dumps, and nothing else: not the recordings mirror, not
// a run that never finished, not a directory someone else put there.
func TestPruningTouchesOnlyFinishedDumps(t *testing.T) {
	root := t.TempDir()
	mk := func(name string, finished bool) {
		d := filepath.Join(root, name)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if finished {
			_ = os.WriteFile(filepath.Join(d, backupManifest), []byte("{}"), 0o600)
		}
	}
	mk("20260101T000000Z", true)
	mk("20260102T000000Z", true)
	mk("20260103T000000Z", true)
	mk("20260100T000000Z", false) // not a valid time: someone else's
	mk("20251231T000000Z", false) // a run that failed part-way
	mk("recordings", false)
	mk("notes", false)

	n, err := pruneBackups(root, 2)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("pruned %d, want 1", n)
	}
	for name, want := range map[string]bool{
		"20260101T000000Z": false, "20260102T000000Z": true, "20260103T000000Z": true,
		"20251231T000000Z": true, "recordings": true, "notes": true,
	} {
		_, err := os.Stat(filepath.Join(root, name))
		if got := err == nil; got != want {
			t.Errorf("%s present = %v, want %v", name, got, want)
		}
	}
}

// A drill that restores over the live database destroys what it was meant to
// prove could be recovered.
func TestTheScratchDatabaseCannotBeTheLiveOne(t *testing.T) {
	live := "postgres://argus:pw@db.internal:5432/argus?sslmode=require"
	for _, s := range []string{
		"postgres://other:pw@db.internal/argus",
		"postgres://argus:pw@DB.internal:5432/argus?sslmode=disable",
	} {
		if !sameDatabase(s, live) {
			t.Errorf("%s was not recognised as the live database", s)
		}
	}
	if sameDatabase("postgres://argus:pw@db.internal:5432/argus_drill", live) {
		t.Error("a different database was mistaken for the live one")
	}
}
