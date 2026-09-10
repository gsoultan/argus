package gateway

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

/*
Reports racing the shutdown drain.

Every session's final report goes out on a detached goroutine tracked by
s.reports, and DrainReports waits on that WaitGroup once every listener has
drained. But those drains are bounded -- main gives each 30s plus a seal grace
and then moves on -- so a session that outlives one is still unwinding when the
wait starts, and its report called Add on a WaitGroup already being waited on.

sync.WaitGroup panics on that. In the path whose entire job is getting the last
evidence out of a gateway that is about to stop existing.
*/

// reportServer is the smallest Server report and DrainReports need. Not
// drainServer: this one is built 400 times in a loop and does not want a
// temporary directory and a known_hosts file each time.
func reportServer() *Server {
	return &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// A report arriving while the drain is waiting is waited for, not written off.
//
// This is the difference between removing the hazard and dodging it. Shutting a
// gate before the wait also stops the panic, but it sends every report that
// follows to the spool -- including one that arrives a millisecond in, with
// seconds of grace left and a control plane answering normally. Spooling that
// costs a restart to deliver, and on an upgrade the restart is not soon.
func TestAReportArrivingDuringTheDrainIsStillDelivered(t *testing.T) {
	srv := reportServer()

	// Hold one report open so the drain is genuinely waiting when the next
	// one arrives.
	release := make(chan struct{})
	srv.report(func(context.Context) { <-release })

	// Read the context inside f. The deferred cancel fires the moment f
	// returns, so a check afterwards reads "cancelled" either way and would
	// pass against the very thing this is testing.
	late := make(chan error, 1)
	go func() {
		time.Sleep(50 * time.Millisecond) // the drain is parked by now
		srv.report(func(ctx context.Context) { late <- ctx.Err() })
		close(release)
	}()

	if !srv.DrainReports(5 * time.Second) {
		t.Fatal("DrainReports gave up while reports were still in flight")
	}

	select {
	case err := <-late:
		if err != nil {
			t.Errorf("a report that arrived with grace left was handed a dead "+
				"context (%v), so the reporter spooled it instead of delivering it", err)
		}
	default:
		t.Fatal("the drain returned without waiting for a report that arrived " +
			"while it was waiting -- that session's last word needs a restart " +
			"to reach the control plane")
	}
}

// The concurrent path, under the race detector.
//
// Not a reproduction: the panic this fix removes lives in a few instructions
// inside the runtime, between the counter reaching zero and the waiter state
// being cleared, and I could not drive a Go test onto it reliably. What this
// does is exercise begin/done/waitFor overlapping in every order the scheduler
// will produce, which is what a regression here would disturb.
func TestAReportRacingTheDrainDoesNotPanic(t *testing.T) {
	for i := 0; i < 400; i++ {
		srv := reportServer()

		// One report in flight, so DrainReports parks rather than returning
		// immediately on a zero counter -- a Wait that never blocks cannot be
		// raced.
		release := make(chan struct{})
		srv.report(func(context.Context) { <-release })

		waiting := make(chan struct{})
		go func() {
			close(waiting)
			srv.DrainReports(2 * time.Second)
		}()
		<-waiting

		// The counter falling to zero and a new report arriving, at once.
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); close(release) }()
		go func() { defer wg.Done(); srv.report(func(context.Context) {}) }()
		wg.Wait()
	}
}

// Past the gate the report still happens, and its context is already done.
//
// Dropping it would trade a rare panic for a quiet hole in the evidence. The
// dead context is what makes the reporter skip a round trip this gateway has no
// time left to finish and spool the report for the next start instead.
func TestAReportAfterTheDrainIsSpooledNotDropped(t *testing.T) {
	srv := reportServer()
	srv.DrainReports(time.Second)

	var got error
	ran := false
	srv.report(func(ctx context.Context) {
		got, ran = ctx.Err(), true
	})

	if !ran {
		t.Fatal("a report submitted after the drain never ran -- the session's " +
			"last word about itself was dropped, which is what the drain exists " +
			"to prevent")
	}
	if got == nil {
		t.Error("the context was still live; the reporter would attempt a round " +
			"trip instead of spooling, on a gateway seconds from exit")
	}
}

// The ordinary path is unchanged: a report before any drain is tracked, and the
// drain waits for it.
func TestTheDrainStillWaitsForReportsInFlight(t *testing.T) {
	srv := reportServer()

	finished := make(chan struct{})
	release := make(chan struct{})
	srv.report(func(context.Context) {
		<-release
		close(finished)
	})

	go func() {
		time.Sleep(50 * time.Millisecond)
		close(release)
	}()

	if !srv.DrainReports(2 * time.Second) {
		t.Fatal("DrainReports gave up on a report that was still going")
	}
	select {
	case <-finished:
	default:
		t.Error("DrainReports returned before the report it was waiting on finished")
	}
}
