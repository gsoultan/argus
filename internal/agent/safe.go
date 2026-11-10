package agent

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"
)

// A panic in any goroutine ends the whole process. On the agent that is every
// recording, every console session and the tunnel at once, for one bad input
// on one of them -- and the collector's socket is open to every local user.
// So each goroutine that serves a caller, or runs for the life of the daemon,
// recovers its own panics: the one session fails, the agent stays up, and the
// stack goes to the log so the bug can still be found and fixed.

// Recover stops a panic in the goroutine that deferred it and logs it with
// its stack. It must be deferred directly -- `defer agent.Recover(log, "x")`
// -- because recover only works in the deferred function itself.
func Recover(log *slog.Logger, what string) {
	if r := recover(); r != nil {
		logPanic(log, what, r)
	}
}

// Go runs fn on a new goroutine that recovers its own panic.
func Go(log *slog.Logger, what string, fn func()) {
	go func() {
		defer Recover(log, what)
		fn()
	}()
}

// Protect runs fn on the calling goroutine and turns a panic into an error,
// so a caller that has to answer someone can still answer.
func Protect(log *slog.Logger, what string, fn func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			logPanic(log, what, r)
			err = fmt.Errorf("%s: internal error: %v", what, r)
		}
	}()
	fn()
	return nil
}

// supervisorBackoff bounds how fast a crashing loop is restarted, so a panic
// on every pass costs a log line a minute rather than a spinning CPU.
var supervisorBackoff = struct{ first, max time.Duration }{time.Second, time.Minute}

// Supervise runs fn until ctx ends. A panic restarts it after a backoff that
// grows with each crash in a row; fn returning on its own ends Supervise, as
// it would have ended the goroutine.
//
// It is for the daemon's long-lived loops -- the tunnel, the watchers, the
// upload sweeper -- whose death would otherwise go unnoticed until someone
// asked why a host stopped reporting.
func Supervise(ctx context.Context, log *slog.Logger, what string, fn func(context.Context)) {
	wait := supervisorBackoff.first
	for {
		started := time.Now()
		if err := Protect(log, what, func() { fn(ctx) }); err == nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		// A loop that ran a good while before it fell over is not crashing in
		// a row; start the backoff over.
		if time.Since(started) > 10*supervisorBackoff.max {
			wait = supervisorBackoff.first
		}
		log.Warn("agent: restarting after a panic", "in", what, "after", wait)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		wait = min(wait*2, supervisorBackoff.max)
	}
}

func logPanic(log *slog.Logger, what string, r any) {
	if log == nil {
		log = slog.Default()
	}
	log.Error("agent: recovered from a panic", "in", what,
		"panic", fmt.Sprint(r), "stack", string(debug.Stack()))
}
