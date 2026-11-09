package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
What Redis and RabbitMQ sessions share: verifying the grant, the recording,
the request loop with its limits, and closing. Each engine supplies how to
open, how to answer one request, and how to close; see redis.go and
rabbitmq.go.

The recording is the Postgres one, line for line (tunnel.PGRecord): every
command is written and sent to the control plane before it runs, and a command
the recording cannot take is not run.
*/

// StoreConfig is what both services take.
type StoreConfig struct {
	Verifier *tunnel.GrantVerifier
	Log      *slog.Logger
	// RecordDir is where recordings are written.
	RecordDir string
	// MaxRequestsPerMinute bounds one session. Zero is 120.
	MaxRequestsPerMinute int
}

// storeEngine is one open session's engine side.
type storeEngine interface {
	// ready is what the session opened as.
	ready() tunnel.StoreAnswer
	// do answers one request. It records what reads or changes data through
	// rec before running it. An error ends the session: the recording, the
	// stream or the engine's connection failed for good.
	do(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error)
	// close releases the engine's connection and the person's user.
	close(ctx context.Context)
}

// openStore is how an engine opens on a verified grant.
type openStore func(ctx context.Context, g tunnel.Grant, id string) (storeEngine, error)

// storeService is the tunnel service for one engine.
func storeService(cfg StoreConfig, service, engine string, open openStore) TunnelService {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	if cfg.MaxRequestsPerMinute <= 0 {
		cfg.MaxRequestsPerMinute = 120
	}
	return func(ctx context.Context, call *Call) {
		if call.GrantKeyErr != nil {
			call.Refuse(call.GrantKeyErr.Error())
			return
		}
		g, err := cfg.Verifier.Verify(call.GrantKey, call.Header.Grant)
		if err != nil {
			log.Warn("tunnel: refused a "+engine+" session", "error", err)
			call.Refuse(err.Error())
			return
		}
		if !g.ForService(service) || g.DB == nil || g.DB.EngineOf() != engine {
			log.Warn("tunnel: refused a "+engine+" session on a grant for another service",
				"service", g.Service, "for", g.Email)
			call.Refuse(tunnel.ErrGrant{Reason: "it is not for " + engine}.Error())
			return
		}
		switch {
		case !tunnel.ValidPersonalRole(g.Principal):
			call.Refuse("not a user Argus will sign in as: " + g.Principal)
			return
		case tunnel.LevelRank(g.DB.Level) == 0:
			call.Refuse("unknown access level " + g.DB.Level)
			return
		case g.DB.Port <= 0 || g.DB.Port > 65535 || g.DB.Name == "":
			call.Refuse("the grant does not say what to open")
			return
		}
		idb := make([]byte, 16)
		if _, err := rand.Read(idb); err != nil {
			call.Refuse(err.Error())
			return
		}
		id := sessionUUID(idb)
		rec, err := newStmtRecorder(cfg.RecordDir, id)
		if err != nil {
			call.Refuse("the session could not be recorded, so it was not opened: " + err.Error())
			return
		}
		rec.parts = g.DB.AnswerParts
		octx, cancel := context.WithTimeout(ctx, 45*time.Second)
		eng, err := open(octx, g, id)
		cancel()
		if err != nil {
			rec.discard()
			log.Warn("tunnel: "+engine+" session refused", "entry", g.DB.Name, "user", g.Principal,
				"for", g.Email, "error", err)
			call.Refuse(err.Error())
			return
		}
		if call.Accept(id) != nil {
			eng.close(context.WithoutCancel(ctx))
			rec.discard()
			_ = call.Stream.Close()
			return
		}
		log.Info("tunnel: "+engine+" session opened", "entry", g.DB.Name, "user", g.Principal,
			"level", g.DB.Level, "for", g.Email, "session", id)
		s := &storeSession{cfg: cfg, g: g, eng: eng, rec: rec, log: log, id: id}
		s.serve(ctx, call.Stream)
		_ = call.Stream.Close()
	}
}

type storeSession struct {
	cfg StoreConfig
	g   tunnel.Grant
	eng storeEngine
	rec *stmtRecorder
	log *slog.Logger
	id  string

	cmu    sync.Mutex
	cancel context.CancelFunc
	curID  uint32
}

func (s *storeSession) serve(ctx context.Context, st tunnel.Stream) {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	s.rec.attach(st)
	reason := "closed"
	defer func() {
		s.eng.close(context.WithoutCancel(ctx))
		s.rec.close(reason)
		s.log.Info("tunnel: store session closed", "session", s.id, "user", s.g.Principal, "reason", reason)
	}()

	if s.rec.write(tunnel.PGRecord{Kind: "open", Actor: s.g.Email, Role: s.g.Principal,
		Level: s.g.DB.Level, Grant: s.g.ID}) != nil {
		reason = "the recording failed"
		return
	}
	r := s.eng.ready()
	r.Type, r.Session, r.Level = "ready", s.id, s.g.DB.Level
	if s.rec.send(r) != nil {
		return
	}

	reqs := make(chan tunnel.StoreRequest, maxQueuedRequests)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		defer stop()
		s.read(ctx, st, reqs)
	}()

	idle := secs(s.g.DB.IdleTimeoutS, defaultIdleTimeout)
	deadline := time.NewTimer(secs(s.g.DB.MaxSessionS, defaultMaxSession))
	defer deadline.Stop()
	idleT := time.NewTimer(idle)
	defer idleT.Stop()
	limiter := newRateLimit(s.cfg.MaxRequestsPerMinute, time.Minute)
	end := func(why string) {
		reason = why
		_ = s.rec.send(tunnel.StoreAnswer{Type: "closed", Error: why})
		stop()
		<-readerDone
	}
	for {
		select {
		case <-ctx.Done():
			<-readerDone
			return
		case <-deadline.C:
			end("the session reached its longest allowed length")
			return
		case <-idleT.C:
			end("the session was idle too long")
			return
		case req := <-reqs:
			idleT.Reset(idle)
			if !limiter.allow(time.Now()) {
				_ = s.rec.send(tunnel.StoreAnswer{ID: req.ID, Type: "error",
					Error: "too many requests in a minute; wait a moment"})
				continue
			}
			rctx, cancel := context.WithCancel(ctx)
			s.cmu.Lock()
			s.cancel, s.curID = cancel, req.ID
			s.cmu.Unlock()
			start := time.Now()
			ans, err := s.eng.do(rctx, req, s.rec)
			s.cmu.Lock()
			s.cancel, s.curID = nil, 0
			s.cmu.Unlock()
			cancel()
			if errors.Is(err, tunnel.ErrRecordTooLong) {
				// Refused before anything was sent to the server: the
				// command could not be recorded whole. The session goes on.
				if s.rec.send(tunnel.StoreAnswer{ID: req.ID, Type: "error", Error: err.Error()}) != nil {
					return
				}
				continue
			}
			if err != nil {
				reason = err.Error()
				_ = s.rec.send(tunnel.StoreAnswer{ID: req.ID, Type: "error", Error: err.Error()})
				return
			}
			ans.ID = req.ID
			if ans.MS == 0 {
				ans.MS = time.Since(start).Milliseconds()
			}
			if ans.Type == "" {
				ans.Type = "result"
			}
			if s.rec.send(ans) != nil {
				return
			}
		}
	}
}

// read takes requests off the stream; a cancel acts at once.
func (s *storeSession) read(ctx context.Context, st tunnel.Stream, reqs chan<- tunnel.StoreRequest) {
	fr := tunnel.NewFrameReader(st)
	for {
		typ, p, err := fr.Next()
		if err != nil || typ != tunnel.FramePGRequest {
			return
		}
		var req tunnel.StoreRequest
		if json.Unmarshal(p, &req) != nil {
			return
		}
		if req.Op == "cancel" {
			s.cmu.Lock()
			if s.cancel != nil && (req.ID == 0 || req.ID == s.curID) {
				s.cancel()
			}
			s.cmu.Unlock()
			continue
		}
		select {
		case reqs <- req:
		case <-ctx.Done():
			return
		default:
			_ = s.rec.send(tunnel.StoreAnswer{ID: req.ID, Type: "error",
				Error: "the session is busy; wait for the running command or cancel it"})
		}
	}
}

// stmtRecorder is a session's recording: the file on this server and the
// copy streamed to the control plane, one hash-chained line per event.
type stmtRecorder struct {
	mu     sync.Mutex
	f      *os.File
	seq    int64
	prev   string
	stream tunnel.Stream
	closed bool
	// parts says an answer larger than a frame may go in parts: the
	// control plane that signed the grant joins them.
	parts bool
}

func newStmtRecorder(dir, id string) (*stmtRecorder, error) {
	if dir == "" {
		return nil, errors.New("no recording directory is configured")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, id+".argussql"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &stmtRecorder{f: f}, nil
}

func (r *stmtRecorder) attach(st tunnel.Stream) {
	r.mu.Lock()
	r.stream = st
	r.mu.Unlock()
}

// write appends one line and streams it.
func (r *stmtRecorder) write(rec tunnel.PGRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("the recording is closed")
	}
	r.seq++
	rec.Seq = r.seq
	rec.Time = time.Now().UTC().Format(time.RFC3339Nano)
	rec.Prev = r.prev
	rec.Hash = ""
	body, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	rec.Hash = hex.EncodeToString(sum[:])
	line, _ := json.Marshal(rec)
	if len(line) > tunnel.MaxRecordLine {
		// Not written; see pgSession.record.
		r.seq--
		return tunnel.ErrRecordTooLong
	}
	line = append(line, '\n')
	if _, err := r.f.Write(line); err != nil {
		return err
	}
	r.prev = rec.Hash
	if r.stream != nil {
		return tunnel.WriteFrame(r.stream, tunnel.FramePGRecord, line[:len(line)-1])
	}
	return nil
}

// stmt records a command before it runs; no record, no command.
func (r *stmtRecorder) stmt(id uint32, text string) error {
	if err := r.write(tunnel.PGRecord{Kind: "stmt", Request: id, SQL: text}); err != nil {
		return fmt.Errorf("the recording failed: %w", err)
	}
	return nil
}

// done records what a command answered: a short tag, how many items, and
// the hash of the answer exactly as it was sent.
func (r *stmtRecorder) done(id uint32, tag string, items int64, ms int64, answer any, truncated bool) error {
	b, _ := json.Marshal(answer)
	sum := sha256.Sum256(b)
	return r.write(tunnel.PGRecord{Kind: "done", Request: id, Tag: tag, Rows: items, MS: ms,
		ResultSHA256: hex.EncodeToString(sum[:]), Truncated: truncated})
}

// fail records a command's failure or refusal.
func (r *stmtRecorder) fail(id uint32, why string, ms int64) error {
	return r.write(tunnel.PGRecord{Kind: "error", Request: id, Error: why, MS: ms})
}

// refused records a command the agent would not send, and why.
func (r *stmtRecorder) refused(id uint32, text, why string) error {
	return r.write(tunnel.PGRecord{Kind: "error", Request: id, SQL: text, Error: "refused: " + why})
}

func (r *stmtRecorder) send(a tunnel.StoreAnswer) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return r.answer(b, func() ([]byte, error) {
		return json.Marshal(tunnel.StoreAnswer{ID: a.ID, Type: "error", Error: tunnel.ErrAnswerTooLarge.Error()})
	})
}

// answer sends one encoded answer, in parts when it is larger than a frame
// and the control plane joins parts. One that cannot be sent at all goes as
// what instead makes, an error naming the request: written as it was, it
// failed, and that failure ended the whole session over one large answer.
func (r *stmtRecorder) answer(b []byte, instead func() ([]byte, error)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stream == nil {
		return errors.New("no stream")
	}
	err := tunnel.WriteAnswer(r.stream, b, r.parts)
	if !errors.Is(err, tunnel.ErrAnswerTooLarge) {
		return err
	}
	nb, err := instead()
	if err != nil {
		return err
	}
	return tunnel.WriteFrame(r.stream, tunnel.FramePGAnswer, nb)
}

// frame sends one frame on the session's stream, in order with the
// recording's lines: a MySQL session's answers and row batches.
func (r *stmtRecorder) frame(typ byte, b []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stream == nil {
		return errors.New("no stream")
	}
	return tunnel.WriteFrame(r.stream, typ, b)
}

func (r *stmtRecorder) close(reason string) {
	_ = r.write(tunnel.PGRecord{Kind: "close", Error: reason})
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.closed = true
		_ = r.f.Sync()
		_ = r.f.Close()
	}
}

// discard removes a recording of a session that never opened.
func (r *stmtRecorder) discard() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.closed = true
		_ = r.f.Close()
		_ = os.Remove(r.f.Name())
	}
}

// userUse counts open sessions per engine user on this agent, so the last
// session to close is the one that removes the user, and a user with a live
// session is never swept. It holds the user's password while it is in use,
// for engines that allow only one.
type userUse struct {
	mu sync.Mutex
	n  map[string]int
	pw map[string]string
}

func (u *userUse) add(key string, d int) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.n == nil {
		u.n, u.pw = map[string]int{}, map[string]string{}
	}
	u.n[key] += d
	v := u.n[key]
	if v <= 0 {
		delete(u.n, key)
		delete(u.pw, key)
	}
	return v
}

func (u *userUse) live(key string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.n[key] > 0
}

// password returns the user's live password, making one with mk when there
// is none yet.
func (u *userUse) password(key string, mk func() (string, error)) (string, bool, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.pw == nil {
		u.n, u.pw = map[string]int{}, map[string]string{}
	}
	if pw, ok := u.pw[key]; ok {
		return pw, false, nil
	}
	pw, err := mk()
	if err != nil {
		return "", false, err
	}
	u.pw[key] = pw
	return pw, true, nil
}
