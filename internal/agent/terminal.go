package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/creack/pty"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Sessions through the tunnel.

The control plane relays a browser terminal onto a "terminal" stream with a
signed grant in the header. The agent checks the grant itself, starts the
account's login shell on a pty -- as root it needs no key or password to do
that -- and records it exactly as it records a login through sshd: same file,
same chain, same kernel tracing, same report. The difference is that it knows
who the session is for, from the grant, rather than only which account.

Nothing runs before the recording is open. A session that cannot be recorded
is refused, whatever the host's recording scope: a session through Argus is
the one kind that is always recorded.
*/

// TerminalService is the name the control plane asks for.
const TerminalService = "terminal"

// TerminalConfig is what the terminal service needs.
type TerminalConfig struct {
	Collector *Collector
	Verifier  *tunnel.GrantVerifier
	Hostname  string
	Log       *slog.Logger
	// Spawn is how the shell is started; see SpawnMode. Empty is SpawnAuto.
	Spawn SpawnMode
}

// hangupWait is how long a shell gets to exit after its terminal goes
// away before it is killed.
const hangupWait = 5 * time.Second

// Terminal returns the terminal service.
func Terminal(cfg TerminalConfig) TunnelService {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return func(ctx context.Context, call *Call) {
		if call.GrantKeyErr != nil {
			call.Refuse(call.GrantKeyErr.Error())
			return
		}
		g, err := cfg.Verifier.Verify(call.GrantKey, call.Header.Grant)
		if err != nil {
			log.Warn("tunnel: refused a session", "error", err)
			call.Refuse(err.Error())
			return
		}
		t, err := startTerminal(cfg, g)
		if err != nil {
			log.Warn("tunnel: could not open a session", "principal", g.Principal,
				"for", g.Email, "error", err)
			call.Refuse(err.Error())
			return
		}
		log.Info("tunnel: session opened", "session", t.rec.ID(), "principal", g.Principal,
			"for", g.Email, "client", g.ClientAddr)
		if err := call.Accept(t.rec.ID()); err != nil {
			t.hangup()
		}
		t.run(call.Stream, log)
	}
}

type tunnelShell struct {
	proc shellProc
	ptmx *os.File
	rec  *TunnelRecording

	hangOnce sync.Once
	// exited is set once the shell has been waited for; a hangup after that
	// kills nothing, so a pid reused in the meantime is never touched.
	exited atomic.Bool
}

func startTerminal(cfg TerminalConfig, g tunnel.Grant) (*tunnelShell, error) {
	acct, err := lookupAccount(g.Principal)
	if err != nil {
		return nil, err
	}

	ptmx, tty, err := pty.Open()
	if err != nil {
		return nil, fmt.Errorf("cannot allocate a terminal: %w", err)
	}
	// The account owns its terminal, as it would after an sshd login, so
	// programs that reopen it by name work.
	_ = tty.Chown(acct.uid, acct.gid)
	_ = pty.Setsize(ptmx, &pty.Winsize{Cols: 80, Rows: 24})
	proc, err := spawnShell(cfg.Spawn, acct, tty, g.Email)
	_ = tty.Close()
	if err != nil {
		_ = ptmx.Close()
		return nil, err
	}

	t := &tunnelShell{proc: proc, ptmx: ptmx}
	rec, err := cfg.Collector.StartRecording(SessionStart{
		Type:       "start",
		Principal:  acct.name,
		ClientAddr: g.ClientAddr,
		Origin:     Brokered,
		OriginReason: "opened through the Argus tunnel on a session grant signed by " +
			"the control plane",
		TTY:       true,
		Cols:      80,
		Rows:      24,
		Hostname:  cfg.Hostname,
		StartedAt: time.Now().UTC(),
		PID:       proc.Pid(),
		Actor:     g.Email,
	}, t.hangup)
	if err != nil {
		// The shell was never released: nothing ran, so nothing went
		// unrecorded.
		proc.Abort()
		_ = ptmx.Close()
		proc.Wait()
		return nil, fmt.Errorf("the session could not be recorded, so it was not opened: %w", err)
	}
	t.rec = rec
	if err := proc.Release(); err != nil {
		proc.Abort()
		t.hangup()
		proc.Wait()
		t.exited.Store(true)
		rec.Seal(255)
		return nil, fmt.Errorf("cannot start the shell: %w", err)
	}
	return t, nil
}

// hangup ends the session as a dropped connection would: the terminal goes
// away and the shell is hung up, and killed if it is still running after
// hangupWait -- the browser is gone, and a shell left running would be one
// nobody is watching.
func (t *tunnelShell) hangup() {
	t.hangOnce.Do(func() {
		_ = t.ptmx.Close()
		if !t.exited.Load() {
			t.proc.Hangup()
		}
	})
}

// run relays the stream until the shell exits or the stream ends, then
// seals the recording and says how it ended.
func (t *tunnelShell) run(st tunnel.Stream, log *slog.Logger) {
	outputDone := make(chan struct{})
	go func() {
		defer close(outputDone)
		buf := make([]byte, 32<<10)
		for {
			n, err := t.ptmx.Read(buf)
			if n > 0 {
				// Recorded before it is shown: what the person saw is in the
				// chain, or they did not see it.
				if rerr := t.rec.Output(buf[:n]); rerr != nil {
					log.Error("tunnel: recording failed; ending the session",
						"session", t.rec.ID(), "error", rerr)
					t.hangup()
					return
				}
				if werr := tunnel.WriteFrame(st, tunnel.FrameOutput, buf[:n]); werr != nil {
					t.hangup()
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	go func() {
		fr := tunnel.NewFrameReader(st)
		defer t.hangup()
		for {
			typ, p, err := fr.Next()
			if err != nil {
				return
			}
			switch typ {
			case tunnel.FrameInput:
				if rerr := t.rec.Input(p); rerr != nil {
					log.Error("tunnel: recording failed; ending the session",
						"session", t.rec.ID(), "error", rerr)
					return
				}
				if _, werr := t.ptmx.Write(p); werr != nil {
					return
				}
			case tunnel.FrameResize:
				if cols, rows, ok := tunnel.ParseResize(p); ok {
					_ = pty.Setsize(t.ptmx, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
					_ = t.rec.Resize(cols, rows)
				}
			}
		}
	}()

	code := t.proc.Wait()
	if sum := t.proc.Summary(); sum != "" {
		log.Info("tunnel: session unit ended", "session", t.rec.ID(), "exit", code,
			"systemd", sum)
	}
	t.exited.Store(true)
	// The shell is gone; its terminal may still hold output it wrote last.
	// A shell that left children holding the terminal open is hung up.
	select {
	case <-outputDone:
	case <-time.After(time.Second):
		t.hangup()
		<-outputDone
	}
	t.hangup()
	head := t.rec.Seal(code)
	_ = tunnel.WriteFrame(st, tunnel.FrameExit, tunnel.ExitPayload(code, head))
	_ = st.Close()
}
