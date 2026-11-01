package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/gsoultan/argus/internal/live"
	"github.com/gsoultan/argus/internal/tunnel"
	"github.com/gsoultan/argus/internal/wskeep"
)

/*
A database in the browser, through an agent's tunnel.

	browser ──wss──▶ argus-control ──"postgres" stream──▶ agent ──▶ postgres on the server

Opened in two steps, as a terminal is. POST /api/v1/databases/{id}/ticket
decides whether this person may open this database and at what level -- the
assignment, or an approved request for a level that needs one -- and issues a
single-use ticket. GET /api/v1/postgres redeems it, signs a grant naming the
database, the level and the policy, and relays frames.

The control plane keeps the session's recording as it arrives: every record
line the agent streams is appended here, so the statements are off the server
before the server can be asked to forget them. At the end the file is stored
like a shipped terminal recording and the session row points at it.
*/

const agentPostgresService = tunnel.PostgresService

// handleDatabaseTicket serves POST /api/v1/databases/{id}/ticket.
func (a *API) handleDatabaseTicket(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if sess.Role == "auditor" {
		writeErr(w, http.StatusForbidden, "your role can review sessions but not open them")
		return
	}
	db, err := a.store.Database(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrDatabaseNotFound) {
		writeErr(w, http.StatusNotFound, "no such database")
		return
	}
	if err != nil {
		a.fail(w, "database", err)
		return
	}
	var in struct {
		// Level is what the person wants to open at; empty is the highest
		// they hold.
		Level string `json:"level"`
	}
	if r.ContentLength != 0 {
		if err := decode(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	level, expires, err := a.databaseLevelFor(r.Context(), sess.Email, sess.Role, db.ID, in.Level)
	if err != nil {
		a.log.Error("refusing a database session: access could not be checked",
			"email", sess.Email, "database", db.ID, "error", err)
		writeErr(w, http.StatusServiceUnavailable,
			"the control plane could not check your access; nothing was opened")
		return
	}
	if level == "" {
		writeErr(w, http.StatusForbidden, "you are not assigned this database"+
			map[bool]string{true: " at that level", false: ""}[in.Level != ""])
		return
	}
	role := a.databaseRoleFor(r.Context(), sess.Email)
	if role == "" {
		writeErr(w, http.StatusBadRequest, "no Postgres role name can be made from your login")
		return
	}
	// The ticket binds the host and the role; the level travels in it as the
	// principal's suffix so the redeemer cannot be handed a different one.
	principal := role + "@" + level
	ticket, err := a.signer.IssueTicket(sess.Email, db.Hostname, principal, a.ticketTTL())
	if err != nil {
		a.fail(w, "issue ticket", err)
		return
	}
	detail := "Database session authorised on " + db.Name + " at " + level + " as " + role + "."
	if expires != nil {
		detail += " Under an approved request until " + expires.UTC().Format(time.RFC3339) + "."
	}
	if _, aerr := a.store.AppendAudit(r.Context(), AuditEvent{Action: "db.authorized",
		Severity: "notice", ActorEmail: sess.Email, Target: db.Hostname, Detail: detail}); aerr != nil {
		a.log.Error("audit append failed", "error", aerr)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ticket": ticket, "level": level, "role": role,
		"expiresIn": int(a.ticketTTL().Seconds())})
}

// databaseLevelFor decides the level email opens dbID at: the one asked for
// when they hold it, else the highest they hold. Admins and owners hold dba,
// as they are exempt from assignment on servers, and it is audited the same.
func (a *API) databaseLevelFor(ctx context.Context, email, role, dbID, want string) (string, *time.Time, error) {
	acc := DBAccess{Level: tunnel.LevelDBA}
	if role != "admin" && role != "owner" {
		var err error
		acc, err = a.store.DatabaseAccess(ctx, email, dbID)
		if err != nil {
			return "", nil, err
		}
	}
	if acc.Level == "" {
		return "", nil, nil
	}
	if want != "" {
		if tunnel.LevelRank(want) == 0 || tunnel.LevelRank(want) > tunnel.LevelRank(acc.Level) {
			return "", nil, nil
		}
		return want, acc.GrantExpires, nil
	}
	return acc.Level, acc.GrantExpires, nil
}

// databaseRoleFor is the Postgres role a person's sessions open as: their
// server login name when they have one, so the two agree, else one made
// from their email.
func (a *API) databaseRoleFor(ctx context.Context, email string) string {
	if acct, err := a.store.Account(ctx, email); err == nil && acct.LoginName != "" {
		if r := tunnel.PersonalRole(acct.LoginName); r != "" {
			return r
		}
	}
	return tunnel.PersonalRole(email)
}

// handlePostgres serves GET /api/v1/postgres?database=&ticket=.
func (a *API) handlePostgres(w http.ResponseWriter, r *http.Request) {
	dbID := r.URL.Query().Get("database")
	if dbID == "" {
		writeErr(w, http.StatusBadRequest, "database is required")
		return
	}
	if a.signer == nil || a.Tunnels == nil || a.Tunnels.GrantKey == nil {
		writeErr(w, http.StatusServiceUnavailable,
			"this control plane cannot open sessions through agents: set signing_secret")
		return
	}
	token := r.URL.Query().Get("ticket")
	if token == "" {
		writeErr(w, http.StatusUnauthorized, "no ticket supplied")
		return
	}
	db, err := a.store.Database(r.Context(), dbID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such database")
		return
	}
	// The ticket names the role and level; the redeem checks host and
	// principal match what was issued, so neither can be swapped here.
	principal := r.URL.Query().Get("principal")
	role, level, ok := strings.Cut(principal, "@")
	if !ok || !tunnel.ValidPersonalRole(role) || tunnel.LevelRank(level) == 0 {
		writeErr(w, http.StatusBadRequest, "principal must be role@level")
		return
	}
	t, err := a.signer.RedeemTicket(r.Context(), token, db.Hostname, principal)
	if err != nil {
		a.log.Warn("database session refused", "database", db.ID, "principal", principal, "error", err)
		writeErr(w, http.StatusUnauthorized, "unauthorized: "+err.Error())
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: originPatterns(a.AllowedOrigins),
	})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(int64(tunnel.MaxSQL) + 4096)
	a.relayPostgres(r.Context(), conn, t.Email, db, role, level, a.clientAddr(r))
}

// pgBrowserMessage is what crosses the websocket as text; row batches cross
// as binary, exactly as the agent sent them.
type pgBrowserMessage struct {
	Type string `json:"type"`
	// Toward the control plane: a request.
	Req *tunnel.PGRequest `json:"req,omitempty"`
	// Toward the browser: an answer.
	Ans *tunnel.PGAnswer `json:"ans,omitempty"`
	// Data is an error's text, or the session id on ready.
	Data string `json:"data,omitempty"`
}

func (a *API) relayPostgres(ctx context.Context, conn *websocket.Conn, email string, db Database,
	role, level, client string) {

	var wmu sync.Mutex
	send := func(typ websocket.MessageType, b []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return conn.Write(wctx, typ, b)
	}
	sendJSON := func(m pgBrowserMessage) error {
		b, _ := json.Marshal(m)
		return send(websocket.MessageText, b)
	}
	fail := func(why string) {
		_ = sendJSON(pgBrowserMessage{Type: "error", Data: why})
		_ = conn.Close(websocket.StatusNormalClosure, "refused")
	}

	g := tunnel.NewGrant(email, db.Hostname, role, client)
	g.Service = agentPostgresService
	g.DB = &tunnel.GrantDB{ID: db.ID, Name: db.Name, Host: db.Host, Port: db.Port, Level: level,
		StatementTimeoutMS: db.Policy.StatementTimeoutS * 1000, IdleTxTimeoutMS: db.Policy.IdleTxTimeoutS * 1000,
		MaxRows: db.Policy.MaxRows, IdleTimeoutS: db.Policy.IdleTimeoutS, MaxSessionS: db.Policy.MaxSessionS}
	grant, err := tunnel.SignGrant(a.Tunnels.GrantKey, g)
	if err != nil {
		fail("could not sign the session grant")
		return
	}
	octx, cancel := context.WithTimeout(ctx, terminalOpenWait)
	st, answer, err := a.Tunnels.OpenWith(octx, db.Hostname,
		tunnel.Header{Service: agentPostgresService, Grant: grant})
	cancel()
	switch {
	case errors.Is(err, ErrNoTunnel):
		fail("the agent on " + db.Hostname + " is not connected to this control plane, so the " +
			"database cannot open through it; check the agent is running and shows as connected under Servers")
		return
	case tunnel.IsRefused(err):
		a.log.Warn("postgres: the agent refused the session", "host", db.Hostname, "database", db.Name,
			"role", role, "email", email, "error", err)
		fail("the agent on " + db.Hostname + " refused the session: " + answer.Error)
		return
	case err != nil:
		fail("could not reach the agent on " + db.Hostname + ": " + err.Error())
		return
	}
	defer st.Close()

	sessionID := answer.Session
	started := time.Now().UTC()
	a.auditDB(ctx, "db.session_started", "notice", email, db.Hostname,
		"Database session "+sessionID+" opened on "+db.Name+" at "+level+" as "+role+
			" through the agent's tunnel; statements are recorded on the server and streamed here.")
	if err := a.store.UpsertSession(ctx, Session{ID: sessionID, UserEmail: email, AssetHostname: db.Hostname,
		Principal: role, Protocol: "postgres", Origin: "console", State: "active", StartedAt: started,
		ClientIP: client, Fidelity: "statement", RiskFlags: []string{}, ReportedBy: "control",
		Kind: "database", DatabaseID: &db.ID}); err != nil {
		a.log.Error("recording a database session failed", "session", sessionID, "error", err)
	}

	// The recording, as it arrives. A temp file, not memory: a long session
	// is many statements, and the console plane holds nothing per session
	// that grows with it.
	rec, err := os.CreateTemp("", "argus-pg-*.argussql")
	if err != nil {
		fail("could not keep the session's recording: " + err.Error())
		return
	}
	defer os.Remove(rec.Name())
	defer rec.Close()
	var (
		recMu     sync.Mutex
		chainHead string
		stmts     int
	)

	var stmu sync.Mutex
	writeStream := func(typ byte, p []byte) error {
		stmu.Lock()
		defer stmu.Unlock()
		return tunnel.WriteFrame(st, typ, p)
	}
	ls := &liveSession{id: sessionID, protocol: "postgres", hub: live.NewHub()}
	var endOnce sync.Once
	var endedBy, endReason string
	ls.terminate = func(by, reason string) bool {
		first := false
		endOnce.Do(func() {
			first = true
			stmu.Lock()
			endedBy, endReason = by, reason
			stmu.Unlock()
		})
		if !first {
			return true
		}
		// Ending the stream ends the session on the agent; it records the
		// close and locks the role.
		_ = sendJSON(pgBrowserMessage{Type: "error", Data: "this session was ended by " + by + ": " + reason})
		_ = st.Close()
		return true
	}
	a.live.add(ls)
	defer a.live.remove(ls)

	finish := func() {
		recMu.Lock()
		head, n := chainHead, stmts
		recMu.Unlock()
		_ = rec.Sync()
		end := time.Now().UTC()
		state := "ended"
		stmu.Lock()
		by, why := endedBy, endReason
		stmu.Unlock()
		s := Session{ID: sessionID, UserEmail: email, AssetHostname: db.Hostname, Principal: role,
			Protocol: "postgres", Origin: "console", State: state, StartedAt: started, EndedAt: &end,
			ClientIP: client, Fidelity: "statement", RiskFlags: []string{}, ReportedBy: "control",
			Kind: "database", DatabaseID: &db.ID, CommandCount: &n}
		if head != "" {
			s.ChainHead = &head
		}
		if by != "" {
			s.State, s.TerminatedBy, s.TerminationReason = "terminated", &by, &why
		}
		store := a.storeRecording
		if store == nil && a.recordings != nil {
			store = a.recordings.Upload
		}
		if store != nil && head != "" {
			if fi, err := rec.Stat(); err == nil {
				s.RecordingBytes = fi.Size()
			}
			if key, err := store(context.WithoutCancel(ctx), rec.Name(), sessionID, head, started); err != nil {
				a.log.Error("storing a database recording failed", "session", sessionID, "error", err)
			} else {
				s.RecordingPath = key
			}
		}
		if err := a.store.UpsertSession(context.WithoutCancel(ctx), s); err != nil {
			a.log.Error("ending a database session failed", "session", sessionID, "error", err)
		}
		a.auditDB(context.WithoutCancel(ctx), "db.session_ended", "notice", email, db.Hostname,
			"Database session "+sessionID+" on "+db.Name+" ended after "+itoa(n)+" statements; chain head "+head+".")
	}
	defer finish()

	if sendJSON(pgBrowserMessage{Type: "ready", Data: sessionID}) != nil {
		return
	}
	go wskeep.Run(ctx, conn)

	// Browser → agent.
	go func() {
		defer st.Close()
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if typ != websocket.MessageText {
				continue
			}
			var m pgBrowserMessage
			if json.Unmarshal(data, &m) != nil || m.Req == nil {
				continue
			}
			if len(m.Req.SQL) > tunnel.MaxSQL {
				_ = sendJSON(pgBrowserMessage{Type: "ans", Ans: &tunnel.PGAnswer{ID: m.Req.ID,
					Type: "error", Error: "the statement is too long"}})
				continue
			}
			b, _ := json.Marshal(m.Req)
			if writeStream(tunnel.FramePGRequest, b) != nil {
				return
			}
		}
	}()

	// Agent → browser.
	fr := tunnel.NewFrameReader(st)
	for {
		typ, p, err := fr.Next()
		if err != nil {
			_ = sendJSON(pgBrowserMessage{Type: "error",
				Data: "the connection to the agent ended; open the database again"})
			_ = conn.Close(websocket.StatusGoingAway, "agent gone")
			return
		}
		switch typ {
		case tunnel.FramePGRows:
			ls.hub.Broadcast(append([]byte{hubOutput}, p...))
			if send(websocket.MessageBinary, p) != nil {
				return
			}
		case tunnel.FramePGAnswer:
			var ans tunnel.PGAnswer
			if json.Unmarshal(p, &ans) != nil {
				continue
			}
			if sendJSON(pgBrowserMessage{Type: "ans", Ans: &ans}) != nil {
				return
			}
			if ans.Type == "closed" {
				_ = conn.Close(websocket.StatusNormalClosure, "closed")
				return
			}
		case tunnel.FramePGRecord:
			var r tunnel.PGRecord
			if json.Unmarshal(p, &r) != nil || r.Hash == "" {
				continue
			}
			recMu.Lock()
			// Appended as sent: the hash is over the line without its own
			// hash, so a verifier can recompute it from what is stored.
			line := append(bytes.Clone(p), '\n')
			_, werr := rec.Write(line)
			chainHead = r.Hash
			if r.Kind == "stmt" {
				stmts++
			}
			recMu.Unlock()
			if werr != nil {
				a.log.Error("writing a database recording failed", "session", sessionID, "error", werr)
			}
			// Watchers see each statement as it is sent.
			if r.Kind == "stmt" || r.Kind == "done" || r.Kind == "error" {
				ls.hub.Broadcast(append([]byte{hubRecord}, p...))
			}
		}
	}
}

// hubRecord marks a recording line on a live session's hub.
const hubRecord = 'l'

func (a *API) auditDB(ctx context.Context, action, severity, email, host, detail string) {
	if _, err := a.store.AppendAudit(ctx, AuditEvent{
		Action: action, Severity: severity, ActorEmail: email, Target: host, Detail: detail,
	}); err != nil {
		a.log.Error("audit append failed", "error", err, "action", action)
	}
}

// argussqlName is the recording file name for a database session.
func argussqlName(sessionID string) string { return filepath.Base(sessionID) + ".argussql" }
