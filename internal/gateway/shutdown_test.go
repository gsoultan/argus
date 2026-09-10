package gateway

import (
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/live"
	"github.com/gsoultan/argus/internal/recorder"
)

/*
Shutting down without losing the evidence.

`systemctl restart` is a routine operation and it is where recordings were
being lost. Close waited for every session to end by itself, so one operator
with an idle shell held the shutdown open until systemd sent SIGKILL -- and a
SIGKILL means Session.Close never runs. The recordings were left on disk with
no chain head, never uploaded, their rows still marked active: present, but
unverifiable, which for evidence is the same as absent.
*/

// heldSession stands in for a session that is not going to end on its own.
// It occupies the wait group exactly as a real relay pair does, and unwinds
// when the session is terminated.
func heldSession(t *testing.T, srv *Server, id string) *Session {
	t.Helper()
	sess := &Session{
		ID: id, User: "lin@northwind.id", Principal: "ops",
		Target: Asset{Hostname: "db-01"}, StartedAt: time.Now().UTC(),
		srv: srv, hub: live.NewHub(), log: srv.log, pol: DefaultPolicy(),
	}
	rec, err := recorder.New(io.Discard, recorder.Header{Width: 80, Height: 24})
	if err != nil {
		t.Fatal(err)
	}
	sess.rec = rec

	srv.mu.Lock()
	srv.sessions[id] = sess
	srv.mu.Unlock()

	// One goroutine per session, released only by Terminate -- which is what
	// closing the client connection does to a real relay.
	srv.wg.Add(1)
	go func() {
		defer srv.wg.Done()
		for {
			if _, _, killed := sess.Killed(); killed {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	return sess
}

func drainServer(t *testing.T) *Server {
	t.Helper()
	srv := testServer(t, t.TempDir())
	srv.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	return srv
}

// A session that will not end on its own is ended, so its recording is sealed.
func TestShutdownTerminatesSessionsThatOutlastTheDrain(t *testing.T) {
	srv := drainServer(t)
	a := heldSession(t, srv, "held-1")
	b := heldSession(t, srv, "held-2")

	done := make(chan error, 1)
	go func() { done <- srv.CloseWithin(50 * time.Millisecond) }()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown never returned; this is what invites the SIGKILL " +
			"that loses the recordings")
	}

	for _, sess := range []*Session{a, b} {
		by, reason, killed := sess.Killed()
		if !killed {
			t.Errorf("%s was left running by a shutdown", sess.ID)
			continue
		}
		if by != "argus" {
			t.Errorf("%s terminated by %q, want argus", sess.ID, by)
		}
		if reason == "" {
			t.Errorf("%s has no termination reason", sess.ID)
		}
	}
}

// A shutdown with nothing in flight returns immediately, and must not sit out
// the whole drain window.
func TestShutdownIsImmediateWhenNothingIsInFlight(t *testing.T) {
	srv := drainServer(t)
	start := time.Now()
	if err := srv.CloseWithin(5 * time.Second); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("an idle shutdown took %s; it should not wait out the drain", d)
	}
}

// Sessions that finish inside the window are left alone: a restart should not
// terminate someone who was about to be done anyway.
func TestShutdownDoesNotTerminateSessionsThatFinishInTime(t *testing.T) {
	srv := drainServer(t)
	sess := heldSession(t, srv, "polite")

	go func() {
		time.Sleep(30 * time.Millisecond)
		// Ends by itself, the way a real session does when the user logs out.
		sess.mu.Lock()
		sess.killedBy, sess.killReason = "self", "finished"
		sess.mu.Unlock()
	}()

	if err := srv.CloseWithin(5 * time.Second); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if by, _, _ := sess.Killed(); by != "self" {
		t.Errorf("a session that finished on its own was terminated by %q", by)
	}
}

// Close must be safe to call twice: a signal handler and a defer both reach it.
func TestShutdownIsIdempotent(t *testing.T) {
	srv := drainServer(t)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = srv.Close() }()
	}
	waited := make(chan struct{})
	go func() { wg.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(15 * time.Second):
		t.Fatal("concurrent Close calls deadlocked")
	}
}
