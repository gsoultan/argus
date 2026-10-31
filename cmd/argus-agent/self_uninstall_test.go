//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gsoultan/argus/internal/agent"
	"github.com/gsoultan/argus/internal/reporter"
)

// The agent of a removed server revokes itself and deletes its credential,
// and keeps its recordings.
func TestARemovedServersAgentRevokesItselfAndKeepsTheEvidence(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/report/deregister" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		_ = json.NewDecoder(r.Body).Decode(&got)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"state":"revoked"}`)
	}))
	defer srv.Close()

	dir := t.TempDir()
	for _, f := range []string{agent.EnrollmentFile, agent.GrantKeyFile, "console-only"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.MkdirAll(filepath.Join(dir, "recordings"), 0o700)
	_ = os.WriteFile(filepath.Join(dir, "recordings", "a.cast"), []byte("rec"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "sessions.jsonl"), []byte("{}\n"), 0o600)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rep := reporter.New(srv.URL, "argus-agent-test", filepath.Join(dir, "report-spool.jsonl"), log)
	stopped := false
	u := &selfUninstall{stateDir: dir, rep: rep, uploads: agent.NewUploads(dir, rep, log), log: log,
		sshdHook: func() ([]string, []string, error) { return nil, nil, nil },
		waysIn:   func() ([]string, bool) { return []string{"ana with 1 key(s)"}, true },
		stopSelf: func() error { stopped = true; return nil }}
	if !u.run(context.Background()) {
		t.Fatal("the uninstall did not finish")
	}

	mu.Lock()
	defer mu.Unlock()
	if got == nil || got["ran_by"] != "the Argus console (server removed)" {
		t.Fatalf("the control plane was not told, or not why: %v", got)
	}
	if ways, _ := got["ssh_ways"].([]any); len(ways) != 1 || ways[0] != "ana with 1 key(s)" {
		t.Errorf("ssh_ways = %v", got["ssh_ways"])
	}
	if !stopped {
		t.Error("the service was not switched off")
	}
	for _, f := range []string{agent.EnrollmentFile, agent.GrantKeyFile, "console-only"} {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("%s survived the uninstall", f)
		}
	}
	for _, f := range []string{"recordings/a.cast", "sessions.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("evidence %s was deleted: %v", f, err)
		}
	}
}

// If the control plane cannot be told, nothing is deleted: the credential is
// still needed to tell it on the next try.
func TestASelfUninstallThatCannotRevokeKeepsTheCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, agent.EnrollmentFile), []byte("{}"), 0o600)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rep := reporter.New(srv.URL, "argus-agent-test", filepath.Join(dir, "report-spool.jsonl"), log)
	u := &selfUninstall{stateDir: dir, rep: rep, uploads: agent.NewUploads(dir, rep, log), log: log,
		sshdHook: func() ([]string, []string, error) { return nil, nil, nil },
		waysIn:   func() ([]string, bool) { return nil, false },
		stopSelf: func() error { t.Error("stopped without revoking"); return nil }}
	if u.run(context.Background()) {
		t.Fatal("reported done without revoking")
	}
	if _, err := os.Stat(filepath.Join(dir, agent.EnrollmentFile)); err != nil {
		t.Errorf("the credential was deleted before it was revoked: %v", err)
	}
}

// sshd first: if its configuration cannot be changed, nothing else happens,
// so the agent is still there to refuse or record logins and to try again.
func TestASelfUninstallStopsWhenSSHDCannotBeChanged(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
	}))
	defer srv.Close()
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rep := reporter.New(srv.URL, "argus-agent-test", filepath.Join(dir, "report-spool.jsonl"), log)
	u := &selfUninstall{stateDir: dir, rep: rep, uploads: agent.NewUploads(dir, rep, log), log: log,
		sshdHook: func() ([]string, []string, error) { return nil, nil, errors.New("sshd -t failed") },
		waysIn:   func() ([]string, bool) { return nil, false },
		stopSelf: func() error { t.Error("stopped"); return nil }}
	if u.run(context.Background()) || called {
		t.Fatalf("went on past a failed sshd change (control plane called: %v)", called)
	}
}
