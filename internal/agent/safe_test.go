package agent

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

// lockedBuf is a log sink safe to read while goroutines still write to it.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func panicLog() (*slog.Logger, *lockedBuf) {
	buf := &lockedBuf{}
	return slog.New(slog.NewTextHandler(buf, nil)), buf
}

// A service that panics fails its own stream with an answer, and the agent
// -- this test process -- is still running to read it. Before, the panic
// ended the daemon: every recording and console session on the host with it.
func TestAPanickingTunnelServiceFailsOnlyItsStream(t *testing.T) {
	log, buf := panicLog()
	var open atomic.Int64
	cfg := TunnelConfig{
		Log:  log,
		Open: &open,
		Services: map[string]TunnelService{
			"boom": func(context.Context, *Call) {
				var m map[string]int
				m["x"] = 1 // nil map write: a runtime panic
			},
		},
	}
	agentEnd, cpEnd := net.Pipe()
	defer cpEnd.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleStream(context.Background(), &Call{Stream: agentEnd}, cfg, log)
	}()

	_ = cpEnd.SetDeadline(time.Now().Add(5 * time.Second))
	if err := tunnel.WriteJSON(cpEnd, tunnel.Header{Service: "boom"}); err != nil {
		t.Fatal(err)
	}
	var ans tunnel.Answer
	if err := tunnel.ReadJSON(cpEnd, &ans); err != nil {
		t.Fatalf("no answer after the panic: %v", err)
	}
	if ans.OK || !strings.Contains(ans.Error, "internal error") {
		t.Errorf("answer = %+v, want a refusal naming an internal error", ans)
	}
	<-done
	if n := open.Load(); n != 0 {
		t.Errorf("open session count = %d after the panic, want 0", n)
	}
	if !strings.Contains(buf.String(), "recovered from a panic") ||
		!strings.Contains(buf.String(), "tunnel service boom") {
		t.Errorf("the panic was not logged with its service: %s", buf.String())
	}
}

// Protect is what the collector, loops and refusals lean on: the panic comes
// back as an error, and the log keeps the stack.
func TestProtectTurnsAPanicIntoALoggedError(t *testing.T) {
	log, buf := panicLog()
	err := Protect(log, "collector connection", func() { panic("bad frame") })
	if err == nil || !strings.Contains(err.Error(), "bad frame") {
		t.Fatalf("Protect = %v, want the panic as an error", err)
	}
	if !strings.Contains(buf.String(), "stack=") {
		t.Errorf("no stack in the log: %s", buf.String())
	}
}

// A loop that panics is started again; one that returns is not.
func TestSuperviseRestartsAfterAPanicOnly(t *testing.T) {
	old := supervisorBackoff
	supervisorBackoff.first, supervisorBackoff.max = time.Millisecond, 5*time.Millisecond
	defer func() { supervisorBackoff = old }()
	log, _ := panicLog()

	var runs atomic.Int64
	Supervise(context.Background(), log, "flaky", func(context.Context) {
		if runs.Add(1) < 3 {
			panic("not yet")
		}
	})
	if n := runs.Load(); n != 3 {
		t.Errorf("ran %d times, want 3 (two panics, then a clean return)", n)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runs.Store(0)
	go func() {
		for runs.Load() < 2 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	Supervise(ctx, log, "always", func(context.Context) {
		runs.Add(1)
		panic("always")
	})
	if ctx.Err() == nil {
		t.Error("Supervise returned before its context ended")
	}
}
