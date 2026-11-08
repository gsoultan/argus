package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

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

// TerminalResumeService picks a held terminal back up; see HeldTerminals.
const TerminalResumeService = tunnel.TerminalResumeService

// TerminalConfig is what the terminal service needs.
type TerminalConfig struct {
	// Asker reaches the person at a workstation; nil is this platform's.
	Asker     Asker
	Collector *Collector
	Verifier  *tunnel.GrantVerifier
	Hostname  string
	Log       *slog.Logger
	// Spawn is how the shell is started; see SpawnMode. Empty is SpawnAuto.
	Spawn SpawnMode
	// Held is where terminals wait for their browser to come back. Nil
	// holds nothing: a dropped stream hangs the shell up, as it did before
	// terminals could be held, and the control plane is told so.
	Held *HeldTerminals
}

// hangupWait is how long a shell gets to exit after its terminal goes
// away before it is killed.
const hangupWait = 5 * time.Second

/*
A dropped stream is not the end of a terminal.

	browser ── argus-control ─╳─ agent ══ shell (still running, still recorded)
	                 ⋮                ║
	browser ── argus-control ─────────╜ "terminal.resume", within the hold

The stream between the control plane and the agent is the session's weakest
link: it goes when argus-control restarts for an update, when the tunnel
reconnects after a network blip, when Cloudflare recycles a WebSocket. Each of
those used to hang the shell up -- and with it whatever was running, an
apt-get halfway through unpacking, a migration with its lock held. Nobody had
ended anything.

So a terminal opened with a Hold is kept when its stream drops without saying
FrameHangup: the shell runs on, its output is still recorded before anything
else happens to it, and the last backlogSize bytes are kept for whoever comes
back. "terminal.resume", on a fresh grant for the same person, host and
account, picks it up: the output it missed first, then live.

A shell nobody comes back to is hung up when the hold runs out -- unless a
command is still running in its foreground, in which case it is kept until
that command returns to the prompt or Hold.RunningS runs out. That is the
"keep running after I leave" a person would otherwise reach for tmux to get,
and it is still the one recorded session it always was.
*/

// HeldTerminals is every terminal this agent has open, by session id, so a
// resume can find one whose stream dropped.
type HeldTerminals struct {
	mu sync.Mutex
	m  map[string]*tunnelShell
}

// NewHeldTerminals returns an empty set.
func NewHeldTerminals() *HeldTerminals { return &HeldTerminals{m: map[string]*tunnelShell{}} }

func (h *HeldTerminals) add(t *tunnelShell) {
	h.mu.Lock()
	h.m[t.rec.ID()] = t
	h.mu.Unlock()
}

func (h *HeldTerminals) get(id string) *tunnelShell {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.m[id]
}

func (h *HeldTerminals) remove(t *tunnelShell) {
	h.mu.Lock()
	if h.m[t.rec.ID()] == t {
		delete(h.m, t.rec.ID())
	}
	h.mu.Unlock()
}

func (h *HeldTerminals) all() []*tunnelShell {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*tunnelShell, 0, len(h.m))
	for _, t := range h.m {
		out = append(out, t)
	}
	return out
}

// Count is how many terminals are open, attached or held.
func (h *HeldTerminals) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.m)
}

// Bounds on what a control plane may ask an agent to hold. The agent keeps
// its own limits: a shell nobody is attached to is still a logged-in shell.
const (
	maxHoldFor     = time.Hour
	maxKeepRunning = 24 * time.Hour
	// backlogSize is how much recent output a held terminal keeps: several
	// full screens, enough to redraw what the person was looking at.
	backlogSize = 128 << 10
	// busyCheckEvery is how often a held terminal past its hold looks again
	// at whether its command has finished.
	busyCheckEvery = 15 * time.Second
	// tombstoneFor is how long an ended terminal is remembered, so a browser
	// coming back to it hears how it ended instead of "no such session".
	tombstoneFor = time.Minute
)

func holdFrom(h *tunnel.TermHold) (forD, runningD time.Duration, ok bool) {
	if h == nil || h.ForS <= 0 {
		return 0, 0, false
	}
	forD = min(time.Duration(h.ForS)*time.Second, maxHoldFor)
	if h.RunningS > 0 {
		runningD = min(time.Duration(h.RunningS)*time.Second, maxKeepRunning)
	}
	return forD, runningD, true
}

// Terminal returns the terminal service.
func Terminal(cfg TerminalConfig) TunnelService {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	if cfg.Held != nil && cfg.Collector != nil {
		held := cfg.Held
		// The person at each terminal hears that the agent is about to
		// stop, while there is still time to finish what they are doing.
		cfg.Collector.OnDrain(func(d time.Duration) {
			for _, t := range held.all() {
				t.notice(tunnel.Notice{Kind: "restarting", EndsInS: int(d.Seconds()),
					Text: fmt.Sprintf("argus-agent on this server is stopping; this session "+
						"ends in %d seconds unless it finishes first", int(d.Seconds()))})
			}
		})
	}
	return func(ctx context.Context, call *Call) {
		g, ok := verifyTerminalGrant(cfg, log, call)
		if !ok {
			return
		}
		if err := askAtMachineFor(ctx, cfg.Asker, g, "open a terminal as "+g.Principal, log); err != nil {
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
		t.log = log
		t.email, t.principal = g.Email, g.Principal
		if cfg.Held != nil {
			t.holdFor, t.keepRunning, t.holds = holdFrom(call.Header.Hold)
		}
		log.Info("tunnel: session opened", "session", t.rec.ID(), "principal", g.Principal,
			"for", g.Email, "client", g.ClientAddr, "held_on_drop", t.holds)
		if cfg.Held != nil {
			cfg.Held.add(t)
		}
		go t.lifecycle(cfg.Held)

		t.wmu.Lock()
		err = call.AcceptWith(tunnel.Answer{Session: t.rec.ID(), Holds: t.holds})
		if err == nil {
			t.mu.Lock()
			t.st = call.Stream
			t.mu.Unlock()
		}
		t.wmu.Unlock()
		// Output is read only once the answer is out: it is the first thing
		// on the stream, and what the shell prints meanwhile waits in the pty.
		go t.pump()
		if err != nil {
			t.hangup()
		} else {
			t.serve(call.Stream)
		}
		// The handler lasts as long as the shell, held or not, so an update
		// waiting for nothing to be open waits for this one too.
		<-t.ended
	}
}

// TerminalResume returns the service that picks a held terminal back up.
func TerminalResume(cfg TerminalConfig) TunnelService {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return func(ctx context.Context, call *Call) {
		if cfg.Held == nil {
			call.Refuse("this agent does not keep terminals for a reconnect")
			return
		}
		g, ok := verifyTerminalGrant(cfg, log, call)
		if !ok {
			return
		}
		r := call.Header.Resume
		if r == nil || r.Session == "" {
			call.Refuse("no session named to resume")
			return
		}
		t := cfg.Held.get(r.Session)
		// A session id is not a credential: the grant must be for the same
		// person and account the session was opened for. Refused in the
		// same words as a session that does not exist, so an id cannot be
		// probed for.
		if t == nil || t.email != g.Email || t.principal != g.Principal {
			if t != nil {
				log.Warn("tunnel: refused to resume a session for someone else",
					"session", r.Session, "for", g.Email, "principal", g.Principal)
			}
			call.Refuse(ErrNoHeldSession.Error())
			return
		}
		if !t.attach(call, r.From) {
			return
		}
		log.Info("tunnel: session resumed", "session", t.rec.ID(), "for", g.Email,
			"client", g.ClientAddr)
		t.serve(call.Stream)
	}
}

// ErrNoHeldSession is a resume for a terminal this agent no longer has.
var ErrNoHeldSession = errors.New("no such session is open on this server; it has ended")

func verifyTerminalGrant(cfg TerminalConfig, log *slog.Logger, call *Call) (tunnel.Grant, bool) {
	if call.GrantKeyErr != nil {
		call.Refuse(call.GrantKeyErr.Error())
		return tunnel.Grant{}, false
	}
	g, err := cfg.Verifier.Verify(call.GrantKey, call.Header.Grant)
	if err != nil {
		log.Warn("tunnel: refused a session", "error", err)
		call.Refuse(err.Error())
		return tunnel.Grant{}, false
	}
	if !g.ForService(TerminalService) {
		log.Warn("tunnel: refused a session on a grant for another service",
			"service", g.Service, "for", g.Email)
		call.Refuse(tunnel.ErrGrant{Reason: "it is for " + g.Service + ", not a terminal"}.Error())
		return tunnel.Grant{}, false
	}
	return g, true
}

type tunnelShell struct {
	proc shellProc
	term terminalIO
	rec  *TunnelRecording
	log  *slog.Logger

	email, principal string
	// holds is set when a dropped stream keeps the shell, for holdFor, and
	// for up to keepRunning while a command runs in its foreground.
	holds       bool
	holdFor     time.Duration
	keepRunning time.Duration

	// wmu orders everything written to the attached stream -- output, the
	// backlog a resume starts with, notices, the exit -- and guards the
	// backlog, so a resume's backlog and the live output after it neither
	// overlap nor leave a gap. Taken before mu.
	wmu     sync.Mutex
	backlog []byte
	// sent is how many bytes of output the session has produced; backlog
	// holds the last len(backlog) of them.
	sent int64

	mu         sync.Mutex
	st         tunnel.Stream // attached, or nil while held
	timer      *time.Timer
	detachedAt time.Time
	busyNoted  bool
	// released is set while a stream is attached but nobody is behind it:
	// the control plane is keeping it open for watching and ending, and has
	// asked for the shell to go once its command is done.
	released bool
	over     bool
	exitCode int
	head     string

	outputDone chan struct{}
	ended      chan struct{}

	hangOnce sync.Once
	// exited is set once the shell has been waited for; a hangup after that
	// kills nothing, so a pid reused in the meantime is never touched.
	exited atomic.Bool
}

// A session's terminal starts at this size; the browser resizes it at once.
const startCols, startRows = 80, 24

func startTerminal(cfg TerminalConfig, g tunnel.Grant) (*tunnelShell, error) {
	term, proc, principal, err := openShell(cfg, g)
	if err != nil {
		return nil, err
	}

	t := &tunnelShell{proc: proc, term: term, log: slog.Default(),
		outputDone: make(chan struct{}), ended: make(chan struct{})}
	rec, err := cfg.Collector.StartRecording(SessionStart{
		Type:       "start",
		Principal:  principal,
		ClientAddr: g.ClientAddr,
		Origin:     Brokered,
		OriginReason: "opened through the Argus tunnel on a session grant signed by " +
			"the control plane",
		TTY:       true,
		Cols:      startCols,
		Rows:      startRows,
		Hostname:  cfg.Hostname,
		StartedAt: time.Now().UTC(),
		PID:       proc.Pid(),
		Actor:     g.Email,
	}, t.hangup)
	if err != nil {
		// The shell was never released: nothing ran, so nothing went
		// unrecorded.
		proc.Abort()
		_ = term.Close()
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

// hangup ends the session as a dropped connection used to: the terminal goes
// away and the shell is hung up, and killed if it is still running after
// hangupWait.
func (t *tunnelShell) hangup() {
	t.hangOnce.Do(func() {
		_ = t.term.Close()
		if !t.exited.Load() {
			t.proc.Hangup()
		}
	})
}

// pump reads the shell's output for its whole life, attached or not.
func (t *tunnelShell) pump() {
	defer close(t.outputDone)
	buf := make([]byte, 32<<10)
	for {
		n, err := t.term.Read(buf)
		if n > 0 {
			// Recorded before it is shown or kept: what the person saw is in
			// the chain, or they did not see it.
			if rerr := t.rec.Output(buf[:n]); rerr != nil {
				t.log.Error("tunnel: recording failed; ending the session",
					"session", t.rec.ID(), "error", rerr)
				t.hangup()
				return
			}
			t.wmu.Lock()
			t.keep(buf[:n])
			t.mu.Lock()
			st := t.st
			t.mu.Unlock()
			if st != nil {
				if werr := tunnel.WriteFrame(st, tunnel.FrameOutput, buf[:n]); werr != nil {
					t.dropped(st)
				}
			}
			t.wmu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// keep appends p to the backlog. Called with wmu held.
func (t *tunnelShell) keep(p []byte) {
	t.sent += int64(len(p))
	if len(p) >= backlogSize {
		t.backlog = append(t.backlog[:0], p[len(p)-backlogSize:]...)
		return
	}
	if over := len(t.backlog) + len(p) - backlogSize; over > 0 {
		t.backlog = append(t.backlog[:0], t.backlog[over:]...)
	}
	t.backlog = append(t.backlog, p...)
}

// serve reads one stream's frames until it ends.
func (t *tunnelShell) serve(st tunnel.Stream) {
	fr := tunnel.NewFrameReader(st)
	for {
		typ, p, err := fr.Next()
		if err != nil {
			t.dropped(st)
			return
		}
		if !t.current(st) {
			return
		}
		switch typ {
		case tunnel.FrameInput:
			if rerr := t.rec.Input(p); rerr != nil {
				t.log.Error("tunnel: recording failed; ending the session",
					"session", t.rec.ID(), "error", rerr)
				t.hangup()
				return
			}
			if _, werr := t.term.Write(p); werr != nil {
				t.hangup()
				return
			}
		case tunnel.FrameResize:
			if cols, rows, ok := tunnel.ParseResize(p); ok {
				_ = t.term.Resize(cols, rows)
				_ = t.rec.Resize(cols, rows)
			}
		case tunnel.FrameTerminate:
			if k, ok := tunnel.ParseTerminate(p); ok {
				t.log.Warn("tunnel: session ended from the console", "session", t.rec.ID(),
					"by", k.By, "reason", k.Reason)
				t.rec.Terminate(k.By, k.Reason)
				t.hangup()
				return
			}
		case tunnel.FrameHangup:
			// Closed on purpose: no hold.
			t.hangup()
			return
		case tunnel.FrameRelease:
			t.release(string(p) == "1")
		}
	}
}

func (t *tunnelShell) current(st tunnel.Stream) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.st == st
}

// dropped notes that st has gone. If it was the attached one, the shell is
// held for a resume when the session was opened with a hold, and hung up
// otherwise, as it always was.
func (t *tunnelShell) dropped(st tunnel.Stream) {
	_ = st.Close()
	t.mu.Lock()
	if t.st != st || t.over {
		t.mu.Unlock()
		return
	}
	t.st = nil
	if !t.holds {
		t.mu.Unlock()
		t.hangup()
		return
	}
	if t.released {
		// Nobody was behind it already; the wait for the command goes on
		// from when that started rather than a fresh hold.
		t.mu.Unlock()
		return
	}
	t.detachedAt = time.Now()
	t.busyNoted = false
	t.armLocked(t.holdFor)
	t.mu.Unlock()
	t.log.Info("tunnel: the session's stream dropped; keeping the shell for a reconnect",
		"session", t.rec.ID(), "for", t.email, "hold", t.holdFor,
		"while_running", t.keepRunning)
}

// armLocked (re)starts the hold timer. Called with mu held.
func (t *tunnelShell) armLocked(d time.Duration) {
	if t.timer != nil {
		t.timer.Stop()
	}
	t.timer = time.AfterFunc(d, t.holdExpired)
}

// holdExpired hangs up a held shell nobody came back to -- unless it is
// still running a command and may be kept for that.
func (t *tunnelShell) holdExpired() {
	t.mu.Lock()
	if (t.st != nil && !t.released) || t.over {
		t.mu.Unlock()
		return
	}
	held := time.Since(t.detachedAt)
	if t.keepRunning > 0 && held < t.keepRunning {
		if busy, ok := foregroundBusy(t.term, t.proc.Pid()); ok && busy {
			first := !t.busyNoted
			t.busyNoted = true
			t.armLocked(min(busyCheckEvery, t.keepRunning-held))
			t.mu.Unlock()
			if first {
				t.log.Info("tunnel: nobody came back, but a command is still running; "+
					"keeping the session until it finishes",
					"session", t.rec.ID(), "for", t.email, "at_most", t.keepRunning)
			}
			return
		}
	}
	t.mu.Unlock()
	t.log.Info("tunnel: nobody came back to the session; hanging up",
		"session", t.rec.ID(), "for", t.email, "after", held.Round(time.Second))
	t.hangup()
}

// release notes whether anyone is behind the attached stream. Nobody: the
// hold has already run its course on the control plane, so what is left is
// the wait for a running command.
func (t *tunnelShell) release(nobody bool) {
	t.mu.Lock()
	if t.over || t.st == nil {
		t.mu.Unlock()
		return
	}
	t.released = nobody
	if !nobody {
		if t.timer != nil {
			t.timer.Stop()
			t.timer = nil
		}
		t.mu.Unlock()
		return
	}
	t.detachedAt = time.Now()
	t.busyNoted = false
	t.mu.Unlock()
	t.holdExpired()
}

// attach makes call's stream the one showing the session, replacing any
// other, and sends it the output it is missing. It reports false when the
// stream has already been answered and closed.
func (t *tunnelShell) attach(call *Call, from int64) bool {
	// The stream being replaced may be stuck in a write to a tunnel that is
	// gone; closing it first lets that write fail, and the output writer go.
	t.mu.Lock()
	old := t.st
	t.mu.Unlock()
	if old != nil {
		_ = old.SetDeadline(time.Now().Add(2 * time.Second))
	}

	t.wmu.Lock()
	defer t.wmu.Unlock()
	if old != nil && t.current(old) {
		// Told why, so the control plane holding it does not take the
		// session straight back: another one has it now.
		_ = tunnel.WriteFrame(old, tunnel.FrameNotice, tunnel.NoticePayload(tunnel.Notice{
			Kind: "moved", Text: "this session was picked up elsewhere"}))
		t.dropped(old)
	}
	t.mu.Lock()
	if t.over {
		code, head := t.exitCode, t.head
		t.mu.Unlock()
		// Ended while nobody was attached: say how, as an attached stream
		// would have heard.
		if call.AcceptWith(tunnel.Answer{Session: t.rec.ID(), Holds: true, From: t.sent}) == nil {
			_ = tunnel.WriteFrame(call.Stream, tunnel.FrameExit, tunnel.ExitPayload(code, head))
		}
		_ = call.Stream.Close()
		return false
	}
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
	t.st = call.Stream
	t.released = false
	t.mu.Unlock()

	start := t.sent - int64(len(t.backlog))
	gap := false
	if from <= 0 || from < start {
		gap = from > 0
		from = start
	}
	if from > t.sent {
		from = t.sent
	}
	if err := call.AcceptWith(tunnel.Answer{Session: t.rec.ID(), Holds: true,
		From: from, Gap: gap}); err != nil {
		t.mu.Lock()
		t.st = nil
		t.mu.Unlock()
		t.dropped(call.Stream)
		return false
	}
	missed := t.backlog[from-start:]
	for len(missed) > 0 {
		n := min(len(missed), 32<<10)
		if tunnel.WriteFrame(call.Stream, tunnel.FrameOutput, missed[:n]) != nil {
			break
		}
		missed = missed[n:]
	}
	return true
}

// notice tells whoever is attached something that is not the shell's output.
func (t *tunnelShell) notice(n tunnel.Notice) {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	t.mu.Lock()
	st := t.st
	t.mu.Unlock()
	if st != nil {
		_ = tunnel.WriteFrame(st, tunnel.FrameNotice, tunnel.NoticePayload(n))
	}
}

// lifecycle waits for the shell, seals the recording and says how it ended
// to whoever is attached then, or to the next resume.
func (t *tunnelShell) lifecycle(held *HeldTerminals) {
	code := t.proc.Wait()
	if sum := t.proc.Summary(); sum != "" {
		t.log.Info("tunnel: session unit ended", "session", t.rec.ID(), "exit", code,
			"systemd", sum)
	}
	t.exited.Store(true)
	// The shell is gone; its terminal may still hold output it wrote last.
	// A shell that left children holding the terminal open is hung up.
	select {
	case <-t.outputDone:
	case <-time.After(time.Second):
		t.hangup()
		<-t.outputDone
	}
	t.hangup()
	head := t.rec.Seal(code)

	t.wmu.Lock()
	t.mu.Lock()
	t.over, t.exitCode, t.head = true, code, head
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
	st := t.st
	t.st = nil
	t.mu.Unlock()
	if st != nil {
		_ = tunnel.WriteFrame(st, tunnel.FrameExit, tunnel.ExitPayload(code, head))
		_ = st.Close()
	}
	t.wmu.Unlock()
	close(t.ended)

	if held != nil {
		if st == nil && t.holds {
			time.AfterFunc(tombstoneFor, func() { held.remove(t) })
		} else {
			held.remove(t)
		}
	}
}
