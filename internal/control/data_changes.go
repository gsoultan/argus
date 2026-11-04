package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
	"github.com/jackc/pgx/v5"
)

/*
Data changes: who inserted, updated or deleted what, from the console.

The relay already sees each statement going to the agent and each answer
coming back, so it is where a change is noticed: a statement whose answer
says it changed rows (INSERT 0 3, UPDATE 1, DELETE 12, MERGE 2), emptied or
dropped something, or put something back from the trash. Outside a
transaction it is written at once; inside one, when COMMIT succeeds, and not
at all on ROLLBACK (or ROLLBACK TO a savepoint taken before it). Each commit
writes its statements to database_changes (migration 047) and raises one
alert: to admins, and to everyone else the database is assigned to, directly
or through a team, never to the person who made the change.

	GET /api/v1/databases/{id}/changes?before=<seq>   the log, newest first

Anyone who may open the database may read its log. The statement's text is
in it, as it is in the recording; its values and its results never are.
*/

const (
	dataChangeRetention = 180 * 24 * time.Hour
	maxChangeSQL        = 4000
	maxChangeRecipients = 200
	maxChangesPending   = 2000
	maxChangesListed    = 200
)

// AlertData is the bell's kind for data changes.
const AlertData = "data"

// DataChange is one committed statement that changed data.
type DataChange struct {
	Seq        int64     `json:"seq"`
	At         time.Time `json:"at"`
	SessionID  string    `json:"sessionId"`
	Actor      string    `json:"actor"`
	Verb       string    `json:"verb"`
	Objects    []string  `json:"objects"`
	Rows       int64     `json:"rows"`
	Tag        string    `json:"tag"`
	SQL        string    `json:"sql"`
	ApprovalID string    `json:"approvalId,omitempty"`
}

var (
	tagCount   = regexp.MustCompile(`^(INSERT \d+|UPDATE|DELETE|MERGE) (\d+)$`)
	insertInto = regexp.MustCompile(`(?is)^\s*(?:insert|merge)\s+into\s+((?:"[^"]+"|[\w$]+)(?:\s*\.\s*(?:"[^"]+"|[\w$]+))?)`)
	savepoint  = regexp.MustCompile(`(?is)^\s*(?:savepoint|release(?:\s+savepoint)?|rollback(?:\s+work|\s+transaction)?\s+to(?:\s+savepoint)?)\s+("[^"]+"|[\w$]+)`)
)

// changeOf says what a finished statement changed, or nil when it changed no
// data. trash is what the agent kept of what it removed.
func changeOf(sql, tag string, c tunnel.SQLClass, trash []tunnel.PGTrashItem) *DataChange {
	ch := &DataChange{Tag: tag, Rows: -1, Objects: c.Targets}
	switch m := tagCount.FindStringSubmatch(tag); {
	case m != nil:
		n, _ := strconv.ParseInt(m[2], 10, 64)
		if n == 0 {
			return nil
		}
		ch.Rows = n
		ch.Verb = strings.ToLower(strings.Fields(m[1])[0])
		if (ch.Verb == "insert" || ch.Verb == "merge") && len(ch.Objects) == 0 {
			if t := insertInto.FindStringSubmatch(sql); t != nil {
				ch.Objects = []string{strings.ReplaceAll(t[1], " ", "")}
			}
		}
	case strings.HasPrefix(tag, "TRUNCATE"):
		ch.Verb = "truncate"
		for _, it := range trash {
			ch.Rows = max(ch.Rows, 0) + it.Rows
		}
	case strings.HasPrefix(tag, "DROP "):
		ch.Verb = "drop"
	case strings.Contains(strings.ToLower(sql), "argus_trash_fn.restore") || strings.Contains(strings.ToLower(sql), "argus_trash_fn\".restore"):
		ch.Verb = "restore"
	case c.Writes && strings.HasPrefix(tag, "SELECT"):
		// A WITH whose parts change rows; the tag counts what it returned.
		ch.Verb = "write"
	default:
		return nil
	}
	if ch.Objects == nil {
		ch.Objects = []string{}
	}
	ch.SQL = clip(sql, maxChangeSQL)
	return ch
}

// changeTracker follows one console session's statements and answers.
type changeTracker struct {
	a       *API
	ctx     context.Context
	email   string
	db      Database
	session string

	mu      sync.Mutex
	sent    map[uint32]sentStmt
	inTx    bool
	pending []DataChange
	marks   []savepointMark
}

type sentStmt struct {
	sql      string
	approval string
	class    tunnel.SQLClass
}

type savepointMark struct {
	name string
	at   int
}

func newChangeTracker(ctx context.Context, a *API, email string, db Database, session string) *changeTracker {
	return &changeTracker{a: a, ctx: context.WithoutCancel(ctx), email: strings.ToLower(email), db: db, session: session,
		sent: map[uint32]sentStmt{}}
}

// statement notes a run request as it goes to the agent.
func (t *changeTracker) statement(req tunnel.PGRequest) {
	if req.Op != "run" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.sent) >= 1024 {
		// Answers that never came; a session only runs one at a time.
		clear(t.sent)
	}
	t.sent[req.ID] = sentStmt{sql: req.SQL, approval: req.Approval, class: tunnel.ClassifySQL(req.SQL)}
}

// answer reads an answer as it comes back from the agent.
func (t *changeTracker) answer(p []byte) {
	var ans struct {
		ID    uint32               `json:"id"`
		Type  string               `json:"type"`
		Tag   string               `json:"tag"`
		Trash []tunnel.PGTrashItem `json:"trash"`
	}
	if json.Unmarshal(p, &ans) != nil || (ans.Type != "done" && ans.Type != "error") {
		return
	}
	t.mu.Lock()
	s, ok := t.sent[ans.ID]
	delete(t.sent, ans.ID)
	if !ok {
		t.mu.Unlock()
		return
	}
	var flush []DataChange
	if ans.Type == "done" {
		flush = t.finished(s, ans.Tag, ans.Trash)
	}
	t.mu.Unlock()
	if len(flush) > 0 {
		go t.write(flush)
	}
}

// finished moves the transaction state on; it returns what is committed now.
func (t *changeTracker) finished(s sentStmt, tag string, trash []tunnel.PGTrashItem) []DataChange {
	switch s.class.Verb {
	case "begin", "start":
		t.inTx, t.pending, t.marks = true, nil, nil
		return nil
	case "commit", "end":
		out := t.pending
		t.inTx, t.pending, t.marks = false, nil, nil
		if tag == "ROLLBACK" {
			// COMMIT of a transaction that had failed rolls it back.
			return nil
		}
		return out
	case "abort":
		t.inTx, t.pending, t.marks = false, nil, nil
		return nil
	case "savepoint":
		if m := savepoint.FindStringSubmatch(s.sql); m != nil {
			t.marks = append(t.marks, savepointMark{name: strings.ToLower(m[1]), at: len(t.pending)})
		}
		return nil
	case "release":
		if m := savepoint.FindStringSubmatch(s.sql); m != nil {
			t.dropMark(strings.ToLower(m[1]), false)
		}
		return nil
	case "rollback":
		if m := savepoint.FindStringSubmatch(s.sql); m != nil {
			t.dropMark(strings.ToLower(m[1]), true)
			return nil
		}
		t.inTx, t.pending, t.marks = false, nil, nil
		return nil
	}
	ch := changeOf(s.sql, tag, s.class, trash)
	if ch == nil {
		return nil
	}
	ch.ApprovalID = s.approval
	if t.inTx {
		if len(t.pending) < maxChangesPending {
			t.pending = append(t.pending, *ch)
		}
		return nil
	}
	return []DataChange{*ch}
}

// dropMark ends a savepoint: released keeps what came after it, rolled back
// to forgets it (the savepoint itself stays, as in Postgres).
func (t *changeTracker) dropMark(name string, rollback bool) {
	for i := len(t.marks) - 1; i >= 0; i-- {
		if t.marks[i].name != name {
			continue
		}
		if rollback {
			t.pending = t.pending[:min(t.marks[i].at, len(t.pending))]
			t.marks = t.marks[:i+1]
		} else {
			t.marks = t.marks[:i]
		}
		return
	}
}

// write records committed changes and tells whoever should know.
func (t *changeTracker) write(chs []DataChange) {
	ctx, cancel := context.WithTimeout(t.ctx, 10*time.Second)
	defer cancel()
	if err := t.a.store.RecordDataChanges(ctx, t.db.ID, t.session, t.email, chs); err != nil {
		t.a.log.Error("recording data changes failed", "database", t.db.Name, "session", t.session, "error", err)
	}
	t.a.alertDataChanged(ctx, t.db, t.email, chs)
}

// RecordDataChanges writes committed changes to the log.
func (s *Store) RecordDataChanges(ctx context.Context, dbID, session, actor string, chs []DataChange) error {
	b := &pgx.Batch{}
	for _, c := range chs {
		b.Queue(`INSERT INTO database_changes (database_id, session_id, actor_email, verb, objects, rows, tag, sql, approval_id)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9)`,
			dbID, session, actor, c.Verb, c.Objects, c.Rows, c.Tag, c.SQL, c.ApprovalID)
	}
	return s.db(ctx).SendBatch(ctx, b).Close()
}

// DataChanges lists a database's log, newest first, before seq when given.
func (s *Store) DataChanges(ctx context.Context, dbID string, before int64, limit int) ([]DataChange, error) {
	if limit <= 0 || limit > maxChangesListed {
		limit = 100
	}
	rows, err := s.db(ctx).Query(ctx, `
		SELECT seq, at, session_id, actor_email, verb, objects, rows, tag, sql, approval_id
		FROM database_changes WHERE database_id = $1::uuid AND ($2 = 0 OR seq < $2)
		ORDER BY seq DESC LIMIT $3`, dbID, before, limit)
	if err != nil {
		return nil, fmt.Errorf("data changes: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (DataChange, error) {
		var c DataChange
		err := r.Scan(&c.Seq, &c.At, &c.SessionID, &c.Actor, &c.Verb, &c.Objects, &c.Rows, &c.Tag, &c.SQL, &c.ApprovalID)
		return c, err
	})
}

// SweepDataChanges deletes log rows past their retention.
func (s *Store) SweepDataChanges(ctx context.Context) (int64, error) {
	tag, err := s.db(ctx).Exec(ctx, `DELETE FROM database_changes WHERE at < now() - make_interval(secs => $1)`,
		dataChangeRetention.Seconds())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DataChangeRecipients is everyone assigned the database, directly or
// through a team, who is not told through the admins' alerts anyway, and is
// not actor.
func (s *Store) DataChangeRecipients(ctx context.Context, dbID, actor string) ([]string, error) {
	rows, err := s.db(ctx).Query(ctx, `
		SELECT DISTINCT lower(u.email)
		FROM (SELECT g.user_email AS email FROM database_assignments g
		      WHERE g.database_id = $1::uuid AND g.user_email IS NOT NULL
		      UNION
		      SELECT m.user_email FROM database_assignments g JOIN team_members m ON m.team_id = g.team_id
		      WHERE g.database_id = $1::uuid) x
		JOIN users u ON lower(u.email) = lower(x.email)
		WHERE u.disabled_at IS NULL AND u.role NOT IN ('admin', 'owner', 'auditor') AND lower(u.email) <> lower($2)
		ORDER BY 1 LIMIT $3`, dbID, actor, maxChangeRecipients)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

var changeWords = map[string][2]string{
	"insert": {"Inserted", "into"}, "update": {"Updated", "in"}, "delete": {"Deleted", "from"},
	"merge": {"Merged", "into"}, "write": {"Changed rows", "with"},
}

// describeChanges says what changed, in a sentence or a few.
func describeChanges(chs []DataChange) (string, bool) {
	serious := false
	var parts []string
	for _, c := range chs {
		on := strings.Join(c.Objects, ", ")
		switch c.Verb {
		case "truncate":
			serious = true
			parts = append(parts, "Emptied "+on)
		case "drop":
			serious = true
			parts = append(parts, "Dropped "+on)
		case "restore":
			parts = append(parts, "Put something back from the trash")
		default:
			w := changeWords[c.Verb]
			n := "rows"
			switch {
			case c.Rows == 1:
				n = "1 row"
			case c.Rows > 1:
				n = strconv.FormatInt(c.Rows, 10) + " rows"
			}
			if c.Rows >= 1000 || c.Verb == "delete" && c.Rows >= 100 {
				serious = true
			}
			if on == "" {
				parts = append(parts, w[0]+" "+n)
			} else {
				parts = append(parts, w[0]+" "+n+" "+w[1]+" "+on)
			}
		}
	}
	if len(parts) > 6 {
		parts = append(parts[:6], fmt.Sprintf("and %d more statements", len(parts)-6))
	}
	return strings.Join(parts, "; ") + ".", serious
}

// alertDataChanged tells admins, and the database's other people, of a commit.
func (a *API) alertDataChanged(ctx context.Context, db Database, actor string, chs []DataChange) {
	what, serious := describeChanges(chs)
	sev := "info"
	if serious {
		sev = "warning"
	}
	where := db.Name
	if db.Label == "production" {
		where += " (production)"
	}
	al := ConsoleAlert{Kind: AlertData, Severity: sev, Actor: actor, Title: actor + " changed data in " + where,
		Detail: what, Link: "/databases/" + db.ID + "?panel=changes"}
	al.Audience = AudienceAdmins
	a.raiseAlert(ctx, al)
	people, err := a.store.DataChangeRecipients(ctx, db.ID, actor)
	if err != nil {
		a.log.Error("finding who to tell of a data change failed", "database", db.Name, "error", err)
		return
	}
	for _, p := range people {
		al.Audience, al.Recipient = AudiencePerson, p
		a.raiseAlert(ctx, al)
	}
}

// GET /api/v1/databases/{id}/changes
func (a *API) getDataChanges(w http.ResponseWriter, r *http.Request, _ string, db Database, _ string) {
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := a.store.DataChanges(r.Context(), db.ID, before, limit)
	if err != nil {
		a.fail(w, "data changes", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"changes": list})
}
