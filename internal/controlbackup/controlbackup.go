// Package controlbackup is what the console and `argus-control backup run`
// share about the control plane's own backups.
//
// The two never talk directly. The control plane runs as argus with
// everything but its state directory read-only; the backup runs as
// argus-backup.service with everything but the backup directory read-only.
// So each writes only where it can, and reads the other's files:
//
//	<state>/backup/settings.json  the console's retention; the backup reads it
//	<state>/backup/request.json   "back up now"; argus-backup-now.path starts
//	                              argus-backup.service when it changes
//	<backups>/last-run.json       the backup's own record of its last run
//	<backups>/<timestamp>/        one directory per run, with manifest.json
package controlbackup

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	settingsFile = "settings.json"
	requestFile  = "request.json"
	lastRunFile  = "last-run.json"
	// ManifestFile marks a run that finished.
	ManifestFile = "manifest.json"
	// RunLayout names a run's directory.
	RunLayout = "20060102T150405Z"
	// DefaultDir is where backups go unless ARGUS_BACKUP_DIR says otherwise.
	DefaultDir = "/var/backups/argus"
	// MinKeep and MaxKeep bound the retention the console may set.
	MinKeep, MaxKeep = 1, 365
)

// Settings is what the console set. Keep, when not zero, wins over the
// unit's -keep: it is the newer, and the one people can see.
type Settings struct {
	Keep      int       `json:"keep"`
	UpdatedBy string    `json:"updatedBy,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Request asks for a backup now.
type Request struct {
	By string    `json:"by"`
	At time.Time `json:"at"`
}

// LastRun is the backup's record of its latest run. FinishedAt is zero while
// it runs.
type LastRun struct {
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
	OK         bool      `json:"ok"`
	Error      string    `json:"error,omitempty"`
	// Run is the directory this run wrote, once it has one.
	Run string `json:"run,omitempty"`
	// Pruned is how many older dumps this run removed.
	Pruned int `json:"pruned,omitempty"`
	Keep   int `json:"keep,omitempty"`
}

// Manifest is what a finished run leaves beside its dump.
type Manifest struct {
	CreatedAt    time.Time `json:"createdAt"`
	ChainHead    string    `json:"chainHead"`
	ChainEvents  int64     `json:"chainEvents"`
	Database     string    `json:"database"`
	DatabaseSHA  string    `json:"databaseSha256"`
	Recordings   int       `json:"recordings"`
	ArgusVersion string    `json:"argusVersion"`
}

// Run is one directory under the backup root.
type Run struct {
	Name string `json:"name"`
	// Complete is false for a run that stopped before its manifest: kept as
	// evidence of the failure, never counted or pruned.
	Complete      bool      `json:"complete"`
	At            time.Time `json:"at"`
	DatabaseBytes int64     `json:"databaseBytes"`
	Recordings    int       `json:"recordings"`
	ChainEvents   int64     `json:"chainEvents"`
	Version       string    `json:"version,omitempty"`
}

func readJSON(path string, v any) (bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(b, v)
}

// writeJSON replaces path whole: a reader never sees half a file.
func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadSettings reads the console's settings from the state's backup directory.
func ReadSettings(dir string) (Settings, bool, error) {
	var s Settings
	ok, err := readJSON(filepath.Join(dir, settingsFile), &s)
	return s, ok, err
}

func WriteSettings(dir string, s Settings) error {
	return writeJSON(filepath.Join(dir, settingsFile), s)
}

// ReadRequest reads the latest "back up now".
func ReadRequest(dir string) (Request, bool, error) {
	var r Request
	ok, err := readJSON(filepath.Join(dir, requestFile), &r)
	return r, ok, err
}

// WriteRequest asks for a backup now. Every write changes the file, which is
// what the path unit watches.
func WriteRequest(dir string, r Request) error {
	return writeJSON(filepath.Join(dir, requestFile), r)
}

func ReadLastRun(root string) (LastRun, bool, error) {
	var l LastRun
	ok, err := readJSON(filepath.Join(root, lastRunFile), &l)
	return l, ok, err
}

func WriteLastRun(root string, l LastRun) error {
	return writeJSON(filepath.Join(root, lastRunFile), l)
}

// List is every run under root, newest first. Directories that are not
// named like a run are not ours and are left out.
func List(root string) ([]Run, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return []Run{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Run{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		at, err := time.Parse(RunLayout, e.Name())
		if err != nil {
			continue
		}
		r := Run{Name: e.Name(), At: at}
		var m Manifest
		if ok, err := readJSON(filepath.Join(root, e.Name(), ManifestFile), &m); ok && err == nil {
			r.Complete, r.Recordings, r.ChainEvents, r.Version = true, m.Recordings, m.ChainEvents, m.ArgusVersion
			if !m.CreatedAt.IsZero() {
				r.At = m.CreatedAt
			}
			db := m.Database
			if db == "" {
				db = "argus.dump"
			}
			if info, err := os.Stat(filepath.Join(root, e.Name(), filepath.Base(db))); err == nil {
				r.DatabaseBytes = info.Size()
			}
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}
